// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"context"
	"log/slog"
)

// reconcile is run once at startup (ARCHITECTURE 6.5). It lists live worker
// pods and seeds each into its tenant's pool as a pending "creating" pod with
// an adoption deadline. A pod that registers within the window is adopted (its
// Register moves it to ready); one that never registers is deleted by
// scaleDownOnce after the window elapses. The controller thus recovers without
// leaving orphan workers, and keeps adopting pods a previous controller created.
func (c *Controller) reconcile(ctx context.Context) {
	c.seededAt = c.now()
	adoptWindow := 2 * c.opts.HeartbeatInterval

	existing, err := c.opts.Pods.List(ctx)
	if err != nil {
		return
	}
	for _, ep := range existing {
		if ep.TenantID == "" {
			// No tenant label: cannot own it safely — delete as an orphan.
			_ = c.opts.Pods.Delete(ctx, ep.Name)
			continue
		}
		if ep.Failed {
			// Already dead (RestartPolicyNever, non-zero exit) and never
			// registered, or it would already be tracked from a prior run.
			// Adopting it would only earn it an adoption window it can never use
			// while it sits there counting against the tenant's pod ceiling
			// (readyCreatingCountLocked) -- excluding it here beats discovering
			// that after the window expires.
			slog.Warn("controller: reconcile excluding failed worker pod",
				"tenant", ep.TenantID, "pod", ep.Name)
			_ = c.opts.Pods.Delete(ctx, ep.Name)
			continue
		}
		t := c.getTenant(ep.TenantID)
		t.mu.Lock()
		if _, ok := t.pods[ep.Name]; !ok {
			t.pods[ep.Name] = &pod{
				name:          ep.Name,
				phase:         phaseCreating,
				loaded:        map[string]string{},
				seeded:        true,
				createdAt:     c.now(),
				adoptDeadline: c.now().Add(adoptWindow),
			}
		}
		t.mu.Unlock()
	}
}

// scaleDownOnce performs one periodic maintenance pass:
//   - force-release leases past their deadline (+grace) and replace the pod;
//   - ask the scaler which idle pods may be drained and mark them draining;
//   - delete pods whose drain grace elapsed or whose adoption window lapsed;
//   - reconcile drift against the live pod list (delete unknown pods, forget
//     pods that have vanished from the cluster).
func (c *Controller) scaleDownOnce(ctx context.Context) {
	c.leaseWatchdog()

	listed, err := c.opts.Pods.List(ctx)
	listOK := err == nil
	listedNames := make(map[string]struct{}, len(listed))
	failedNames := make(map[string]struct{})
	for _, ep := range listed {
		listedNames[ep.Name] = struct{}{}
		if ep.Failed {
			failedNames[ep.Name] = struct{}{}
		}
	}

	knownNames := make(map[string]struct{})

	for _, t := range c.snapshotTenants() {
		tenantObj := c.tenantConfig(ctx, t.id)

		t.mu.Lock()
		st := c.scaleStateLocked(t, tenantObj)
		var toDelete []string
		for _, p := range t.pods {
			knownNames[p.name] = struct{}{}
			if _, failed := failedNames[p.name]; failed {
				// RestartPolicyNever makes this terminal: the pod will never
				// register (or, having already registered, never serve again).
				// Deleting it now -- rather than waiting for phaseCreating's
				// adoption window, which only ever applied to seeded pods
				// anyway -- frees the slot it was occupying in
				// readyCreatingCountLocked so the next maybeScaleUp actually
				// replaces it instead of believing capacity is already met.
				toDelete = append(toDelete, p.name)
				continue
			}
			switch p.phase {
			case phaseDraining:
				if p.inFlight == 0 || c.now().After(p.drainDeadline) {
					toDelete = append(toDelete, p.name)
				}
			case phaseCreating:
				// Any creating pod past its registration deadline, seeded or
				// not. A fresh pod that never becomes Ready (unschedulable, or
				// stuck pulling) stays Pending -- never PodFailed -- so the
				// failedNames path above cannot catch it; this does. The
				// IsZero guard is essential: without it a pod that somehow
				// carried no deadline would be After(zero)==true and deleted on
				// the spot.
				if !p.adoptDeadline.IsZero() && c.now().After(p.adoptDeadline) {
					toDelete = append(toDelete, p.name)
				}
			}
		}
		t.mu.Unlock()

		// Ask the scaler which idle pods to drain. Draining is announced to the
		// shim via the next Heartbeat (drain=true); the pod is deleted once it
		// reports no in-flight work, or by a later pass once its drain grace
		// elapses. It is deliberately not deleted in this same pass.
		for _, ref := range c.opts.Scaler.SelectScaleDown(ctx, st) {
			t.mu.Lock()
			if p := t.pods[ref.Name]; p != nil && p.phase == phaseReady {
				p.phase = phaseDraining
				p.drainDeadline = c.now().Add(c.opts.DrainGrace)
			}
			t.mu.Unlock()
		}

		for _, name := range toDelete {
			c.deletePod(t, name)
		}
	}

	if !listOK {
		return
	}

	// Drift: unknown pods with our label that we don't track are orphans.
	// Right after startup, give reconcile-seeded pods their adoption window
	// before treating anything as an orphan.
	orphanGrace := c.now().Before(c.seededAt.Add(2 * c.opts.HeartbeatInterval))
	for _, ep := range listed {
		if _, known := knownNames[ep.Name]; known {
			continue
		}
		if orphanGrace {
			continue
		}
		_ = c.opts.Pods.Delete(ctx, ep.Name)
	}

	// Drift: pods we track that have vanished from the cluster are forgotten,
	// but only after a grace so freshly-created pods (not yet listed) survive.
	forgetOlderThan := c.now().Add(-2 * c.opts.HeartbeatInterval)
	for _, t := range c.snapshotTenants() {
		t.mu.Lock()
		for name, p := range t.pods {
			if _, ok := listedNames[name]; ok {
				continue
			}
			if p.createdAt.Before(forgetOlderThan) {
				delete(t.pods, name)
			}
		}
		t.mu.Unlock()
	}
}

// leaseWatchdog force-releases leases whose deadline (plus grace) has passed and
// marks the owning pod suspect: it is drained and deleted, since its state can
// no longer be trusted (ARCHITECTURE 5.3). A replacement is created on demand by
// the next request's scale-up judgment.
func (c *Controller) leaseWatchdog() {
	now := c.now()
	type expired struct {
		leaseID  string
		tenantID string
		podName  string
	}
	var dead []expired

	c.leaseMu.Lock()
	for id, lr := range c.leases {
		if lr.released {
			continue
		}
		if now.After(lr.deadline.Add(c.opts.LeaseGrace)) {
			dead = append(dead, expired{leaseID: id, tenantID: lr.tenantID, podName: lr.podName})
		}
	}
	c.leaseMu.Unlock()

	for _, d := range dead {
		c.releaseLease(d.leaseID)
		t := c.getTenant(d.tenantID)
		t.mu.Lock()
		if p := t.pods[d.podName]; p != nil && p.phase != phaseDraining {
			p.phase = phaseDraining
			p.drainDeadline = now.Add(c.opts.DrainGrace)
		}
		t.mu.Unlock()
		c.deletePod(t, d.podName)
	}
}
