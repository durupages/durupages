// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package defaultscaler provides the default scaler.Scaler implementation: the
// target/max-concurrency algorithm documented in ARCHITECTURE section 6.4.
//
// DesiredPods is a pure function of the supplied TenantScaleState and returns
// in microseconds (no external I/O), as required for the request hot path.
// SelectScaleDown reports idle pods that have exceeded the tenant IdleTTL and
// may be drained, while never draining the last pod of a tenant that still has
// demand.
package defaultscaler

import (
	"context"
	"time"

	"github.com/durupages/durupages/pkg/scaler"
)

// Scaler is the default target/max-concurrency scaler. It is stateless and
// safe for concurrent use.
type Scaler struct{}

// New returns a new default Scaler.
func New() *Scaler {
	return &Scaler{}
}

// DesiredPods returns the number of pods the tenant should have.
//
//	demand  = InFlight + QueueDepth + 1
//	desired = ceil(demand / targetConcurrencyPerPod)
//	result  = clamp(desired, 1, maxPods)
//
// targetConcurrencyPerPod comes from Defaults.TargetConcurrencyPerPod. maxPods
// is the tenant Config.MaxConcurrency when positive, otherwise
// Defaults.DefaultMaxConcurrency. The result is a plain target; the controller
// compares it against ready+creating pods and only creates the shortfall, so
// creating pods are intentionally not subtracted here.
func (s *Scaler) DesiredPods(ctx context.Context, st scaler.TenantScaleState) int {
	target := st.Defaults.TargetConcurrencyPerPod
	if target < 1 {
		target = 1 // guard against divide-by-zero / misconfiguration
	}

	demand := st.InFlight + st.QueueDepth + 1
	if demand < 1 {
		demand = 1
	}
	desired := (demand + target - 1) / target // ceil(demand/target)

	maxPods := s.maxPods(st)
	if desired < 1 {
		desired = 1
	}
	if desired > maxPods {
		desired = maxPods
	}
	return desired
}

// SelectScaleDown returns the refs of ready pods that are idle (InFlight==0),
// have a non-zero IdleSince, and have been idle for longer than the tenant
// IdleTTL relative to st.Now. When demand (InFlight+QueueDepth) is greater than
// zero it keeps at least one ready pod alive, trimming the returned set as
// needed; when demand is zero every idle-expired pod may be returned.
func (s *Scaler) SelectScaleDown(ctx context.Context, st scaler.TenantScaleState) []scaler.PodRef {
	idleTTL := s.idleTTL(st)

	var candidates []scaler.PodRef
	for _, p := range st.ReadyPods {
		if p.InFlight != 0 {
			continue
		}
		if p.IdleSince.IsZero() {
			continue
		}
		if st.Now.Sub(p.IdleSince) > idleTTL {
			candidates = append(candidates, p.Ref)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	demand := st.InFlight + st.QueueDepth
	if demand > 0 {
		// Never leave the tenant with fewer than one ready pod.
		maxRemove := len(st.ReadyPods) - 1
		if maxRemove < 0 {
			maxRemove = 0
		}
		if len(candidates) > maxRemove {
			candidates = candidates[:maxRemove]
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates
}

// maxPods resolves the pod ceiling: the tenant override when positive, else the
// controller default, floored at 1.
func (s *Scaler) maxPods(st scaler.TenantScaleState) int {
	maxPods := st.Defaults.DefaultMaxConcurrency
	if st.Tenant != nil && st.Tenant.Config.MaxConcurrency > 0 {
		maxPods = st.Tenant.Config.MaxConcurrency
	}
	if maxPods < 1 {
		maxPods = 1
	}
	return maxPods
}

// idleTTL resolves the idle TTL: the tenant override when positive, else the
// controller default.
func (s *Scaler) idleTTL(st scaler.TenantScaleState) time.Duration {
	idleTTL := st.Defaults.DefaultIdleTTL
	if st.Tenant != nil && st.Tenant.Config.IdleTTL > 0 {
		idleTTL = st.Tenant.Config.IdleTTL
	}
	return idleTTL
}
