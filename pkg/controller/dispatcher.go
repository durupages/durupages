// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/queue"
	"github.com/durupages/durupages/pkg/scaler"
	"github.com/durupages/durupages/pkg/workerauth"
)

// podPhase is the lifecycle phase of a worker pod as tracked by the controller.
type podPhase int

const (
	// phaseCreating: Create issued (or reconcile-seeded), not yet Registered.
	phaseCreating podPhase = iota
	// phaseReady: Registered and accepting leases.
	phaseReady
	// phaseDraining: no new leases; awaiting in-flight drain then deletion.
	phaseDraining
)

// pod is the controller's view of a single worker pod. All fields are guarded
// by the owning tenant's mutex.
type pod struct {
	name     string
	phase    podPhase
	endpoint string
	// inFlight is the number of outstanding leases (the authoritative slot
	// count; heartbeat-reported in-flight is informational only).
	inFlight int
	// idleSince is set when inFlight drops to zero and cleared when it rises.
	idleSince time.Time
	// loaded maps pageID -> deploymentID as last reported by heartbeats; drives
	// the "page already loaded" assignment preference.
	loaded map[string]string
	// jwtExpiry is when the pod's worker JWT expires. Zero means "unknown"
	// (adopted pod) and triggers renewal on the next heartbeat.
	jwtExpiry time.Time
	createdAt time.Time
	// seeded marks a pod discovered by reconcile that has not registered yet.
	seeded bool
	// adoptDeadline is when a seeded pod is deleted if it never registers.
	adoptDeadline time.Time
	// drainDeadline bounds the graceful drain of a draining pod.
	drainDeadline time.Time
}

// tenant holds one tenant's pod pool and its dispatcher. pods and
// pendingPlacement are guarded by mu.
type tenant struct {
	id string
	c  *Controller

	mu   sync.Mutex
	pods map[string]*pod
	// pendingPlacement is true while the dispatcher holds a dequeued item that
	// is waiting for a free slot. It blocks the AcquireSlot immediate-assignment
	// fast path so queued waiters keep strict FIFO priority.
	pendingPlacement bool

	// slotFreed is a coalescing signal raised whenever pod capacity may have
	// increased (release, register, drain/delete).
	slotFreed chan struct{}
}

// waiter couples a queued request to the goroutine serving its AcquireSlot
// stream. The settle handshake guarantees a reserved slot is never leaked: at
// most one of {dispatcher delivers a lease, waiter times out} wins.
type waiter struct {
	ctx    context.Context
	pageID string

	mu      sync.Mutex
	settled bool
	result  chan *api.Lease // buffered (1)
}

// ---- signalling helpers -------------------------------------------------

// signalSlot raises the tenant's slotFreed signal without blocking.
func (t *tenant) signalSlot() {
	select {
	case t.slotFreed <- struct{}{}:
	default:
	}
}

// ---- slot reservation ---------------------------------------------------

// tryReserve picks the best ready pod with a free slot for pageID, reserves a
// slot on it (increments inFlight) and returns it, or nil when the whole pool
// is at the hard cap. The caller must hold t.mu.
//
// Preference: pods that already have pageID loaded win; among equally-preferred
// pods the least-loaded one wins (spreads load and minimises lazy loads).
func (t *tenant) tryReserve(pageID string) *pod {
	cap := t.c.opts.Defaults.MaxConcurrencyPerPod
	var best *pod
	bestLoaded := false
	for _, p := range t.pods {
		if p.phase != phaseReady || p.inFlight >= cap {
			continue
		}
		_, loaded := p.loaded[pageID]
		switch {
		case best == nil:
			best, bestLoaded = p, loaded
		case loaded != bestLoaded:
			if loaded {
				best, bestLoaded = p, true
			}
		case p.inFlight < best.inFlight:
			best = p
		}
	}
	if best == nil {
		return nil
	}
	best.inFlight++
	best.idleSince = time.Time{}
	return best
}

// releaseSlot decrements a pod's in-flight count (never below zero) and updates
// its idle timestamp. The caller must hold t.mu.
func (t *tenant) releaseSlot(p *pod) {
	if p.inFlight > 0 {
		p.inFlight--
	}
	if p.inFlight == 0 {
		p.idleSince = t.c.now()
	}
}

// ---- dispatcher loop ----------------------------------------------------

// dispatch is the single per-tenant goroutine that pairs Queue.Dequeue with
// freed slots. It runs until the controller context is cancelled.
func (t *tenant) dispatch() {
	c := t.c
	for {
		item, err := c.opts.Queue.Dequeue(c.ctx, t.id)
		if err != nil {
			return // controller shutting down
		}
		w := c.takeWaiter(item.ID)
		if w == nil {
			continue // requester already gone
		}

		t.mu.Lock()
		t.pendingPlacement = true
		t.mu.Unlock()

		p := t.reserveBlocking(w)

		t.mu.Lock()
		t.pendingPlacement = false
		t.mu.Unlock()

		if p == nil {
			continue // waiter timed out; it emits TIMEOUT on its own
		}
		lease, err := c.buildLease(t, p, item.PageID)
		if err != nil {
			t.mu.Lock()
			t.releaseSlot(p)
			t.mu.Unlock()
			t.signalSlot()
			continue
		}
		if !deliver(w, lease) {
			// Waiter vanished between reservation and delivery: roll back.
			c.forceReleaseLease(lease.LeaseId)
		}
	}
}

// reserveBlocking waits until a slot is free for the waiter's page or the
// waiter's context is done. It returns the reserved pod, or nil on timeout /
// shutdown.
func (t *tenant) reserveBlocking(w *waiter) *pod {
	for {
		t.mu.Lock()
		p := t.tryReserve(w.pageID)
		t.mu.Unlock()
		if p != nil {
			return p
		}
		select {
		case <-t.slotFreed:
		case <-w.ctx.Done():
			return nil
		case <-t.c.ctx.Done():
			return nil
		}
	}
}

// deliver hands lease to the waiter under its settle lock. It returns false if
// the waiter already settled (timed out), in which case the caller must release
// the reserved slot.
func deliver(w *waiter, lease *api.Lease) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.settled {
		return false
	}
	w.settled = true
	w.result <- lease // buffered, never blocks
	return true
}

// ---- waiter registry ----------------------------------------------------

func (c *Controller) registerWaiter(id string, w *waiter) {
	c.waiterMu.Lock()
	c.waiters[id] = w
	c.waiterMu.Unlock()
}

func (c *Controller) unregisterWaiter(id string) {
	c.waiterMu.Lock()
	delete(c.waiters, id)
	c.waiterMu.Unlock()
}

// takeWaiter removes and returns the waiter for id (or nil).
func (c *Controller) takeWaiter(id string) *waiter {
	c.waiterMu.Lock()
	defer c.waiterMu.Unlock()
	w := c.waiters[id]
	delete(c.waiters, id)
	return w
}

// ---- lease building & release -------------------------------------------

// buildLease reserves nothing (the slot is already reserved); it re-reads the
// page's active deployment at grant time, signs a lease and records it for
// release/watchdog. On error the caller releases the slot.
func (c *Controller) buildLease(t *tenant, p *pod, pageID string) (*api.Lease, error) {
	deploymentID := ""
	requestTimeout := c.opts.Defaults.RequestTimeout
	if pg, err := c.opts.Provider.GetPage(c.ctx, pageID); err == nil {
		deploymentID = pg.ActiveDeploymentID
		if pg.Config.RequestTimeout > 0 {
			requestTimeout = pg.Config.RequestTimeout
		}
	}

	leaseID := randID()
	requestID := randID()
	deadline := c.now().Add(requestTimeout)

	sig, err := workerauth.IssueLease(c.opts.SigningKey, workerauth.LeaseClaims{
		LeaseID:      leaseID,
		TenantID:     t.id,
		PageID:       pageID,
		DeploymentID: deploymentID,
		RequestID:    requestID,
	}, requestTimeout+c.opts.LeaseGrace)
	if err != nil {
		return nil, err
	}

	c.leaseMu.Lock()
	c.leases[leaseID] = &leaseRec{
		id:       leaseID,
		tenantID: t.id,
		podName:  p.name,
		deadline: deadline,
	}
	c.leaseMu.Unlock()

	return &api.Lease{
		LeaseId:        leaseID,
		Endpoint:       p.endpoint,
		PageId:         pageID,
		DeploymentId:   deploymentID,
		DeadlineUnixMs: deadline.UnixMilli(),
		RequestId:      requestID,
		Signature:      sig,
	}, nil
}

// releaseLease marks a lease released and decrements the owning pod's in-flight
// count. It is idempotent and treats an unknown lease as a no-op. Returns true
// if this call performed the release.
func (c *Controller) releaseLease(leaseID string) bool {
	c.leaseMu.Lock()
	lr := c.leases[leaseID]
	if lr == nil || lr.released {
		c.leaseMu.Unlock()
		return false
	}
	lr.released = true
	delete(c.leases, leaseID)
	tenantID, podName := lr.tenantID, lr.podName
	c.leaseMu.Unlock()

	t := c.getTenant(tenantID)
	t.mu.Lock()
	if p := t.pods[podName]; p != nil {
		t.releaseSlot(p)
	}
	t.mu.Unlock()
	t.signalSlot()
	return true
}

// forceReleaseLease is releaseLease used on the rollback path.
func (c *Controller) forceReleaseLease(leaseID string) { c.releaseLease(leaseID) }

// dropLeasesForPod forgets any leases pointing at a now-deleted pod so later
// ReleaseSlot calls stay no-ops.
func (c *Controller) dropLeasesForPod(podName string) {
	c.leaseMu.Lock()
	for id, lr := range c.leases {
		if lr.podName == podName {
			delete(c.leases, id)
		}
	}
	c.leaseMu.Unlock()
}

// ---- scale-up (per request) ---------------------------------------------

// maybeScaleUp runs the scaler's DesiredPods judgment for the tenant and, when
// desired exceeds ready+creating pods, creates the shortfall (clamped to the
// tenant pod ceiling). Creation is asynchronous but the creating entry is added
// synchronously so concurrent requests don't over-provision.
func (c *Controller) maybeScaleUp(t *tenant) {
	tenantObj := c.tenantConfig(c.ctx, t.id)
	maxPods := resolveMaxPods(tenantObj, c.opts.Defaults.MaxConcurrency)

	t.mu.Lock()
	st := c.scaleStateLocked(t, tenantObj)
	desired := c.opts.Scaler.DesiredPods(c.ctx, st)
	if desired > maxPods {
		desired = maxPods // controller enforces the ceiling
	}
	effective := t.readyCreatingCountLocked()
	specs := make([]PodSpec, 0)
	for effective < desired {
		name := podName(t.id)
		spec, err := c.buildPodSpec(tenantObj, t.id, name)
		if err != nil {
			break
		}
		t.pods[name] = &pod{
			name:      name,
			phase:     phaseCreating,
			loaded:    map[string]string{},
			jwtExpiry: c.now().Add(c.opts.WorkerJWTTTL),
			createdAt: c.now(),
		}
		specs = append(specs, spec)
		effective++
	}
	t.mu.Unlock()

	for _, spec := range specs {
		spec := spec
		go func() {
			if err := c.opts.Pods.Create(c.ctx, spec); err != nil {
				t.mu.Lock()
				delete(t.pods, spec.Name)
				t.mu.Unlock()
			}
		}()
	}
}

// scaleStateLocked builds a TenantScaleState snapshot. The caller must hold
// t.mu.
func (c *Controller) scaleStateLocked(t *tenant, tenantObj *provider.Tenant) scaler.TenantScaleState {
	var inFlight int
	var ready []scaler.PodState
	var creating []scaler.PodRef
	for _, p := range t.pods {
		switch p.phase {
		case phaseReady:
			inFlight += p.inFlight
			ready = append(ready, scaler.PodState{
				Ref:         scaler.PodRef{Name: p.name},
				InFlight:    p.inFlight,
				IdleSince:   p.idleSince,
				LoadedPages: pageKeys(p.loaded),
			})
		case phaseCreating:
			creating = append(creating, scaler.PodRef{Name: p.name})
		}
	}
	depth, _ := c.opts.Queue.Depth(c.ctx, t.id)
	return scaler.TenantScaleState{
		Tenant:       tenantObj,
		InFlight:     inFlight,
		QueueDepth:   depth,
		ReadyPods:    ready,
		CreatingPods: creating,
		Defaults: scaler.ScaleDefaults{
			TargetConcurrencyPerPod: c.opts.Defaults.TargetConcurrencyPerPod,
			MaxConcurrencyPerPod:    c.opts.Defaults.MaxConcurrencyPerPod,
			DefaultMaxConcurrency:   c.opts.Defaults.MaxConcurrency,
			DefaultIdleTTL:          c.opts.Defaults.IdleTTL,
		},
		Now: c.now(),
	}
}

// readyCreatingCountLocked counts pods that count toward the pod ceiling
// (ready + creating). The caller must hold t.mu.
func (t *tenant) readyCreatingCountLocked() int {
	n := 0
	for _, p := range t.pods {
		if p.phase == phaseReady || p.phase == phaseCreating {
			n++
		}
	}
	return n
}

// buildPodSpec assembles the PodSpec (env, labels, resources) for a new pod.
func (c *Controller) buildPodSpec(tenantObj *provider.Tenant, tenantID, name string) (PodSpec, error) {
	jwt, err := workerauth.Issue(c.opts.SigningKey, name, tenantID, c.opts.WorkerJWTTTL)
	if err != nil {
		return PodSpec{}, err
	}
	env := map[string]string{
		"DURUPAGES_TENANT_ID":       tenantID,
		"DURUPAGES_POD_NAME":        name,
		"DURUPAGES_CONTROLLER_ADDR": c.opts.ControllerAddr,
		"DURUPAGES_HUB_ADDR":        c.opts.HubAddr,
		"DURUPAGES_WORKER_JWT":      jwt,
		"DURUPAGES_LEASE_PUBKEY":    base64.RawStdEncoding.EncodeToString(c.pub),
	}
	if c.opts.HubLogAddr != "" {
		env["DURUPAGES_HUB_LOG_ADDR"] = c.opts.HubLogAddr
	}
	if c.opts.BundleMinIdle != "" {
		env["DURUPAGES_BUNDLE_MIN_IDLE"] = c.opts.BundleMinIdle
	}
	if c.opts.BundleCacheMaxBytes != "" {
		env["DURUPAGES_BUNDLE_CACHE_MAX_BYTES"] = c.opts.BundleCacheMaxBytes
	}
	if c.opts.BundleSweepInterval != "" {
		env["DURUPAGES_BUNDLE_SWEEP_INTERVAL"] = c.opts.BundleSweepInterval
	}

	spec := PodSpec{Name: name, TenantID: tenantID, Env: env}
	if tenantObj != nil {
		spec.Labels = tenantObj.Config.PodLabels
		spec.Annotations = tenantObj.Config.PodAnnotations
		spec.CPULimit = tenantObj.Config.WorkerCPULimit
		spec.MemLimit = tenantObj.Config.WorkerMemLimit
	}
	return spec, nil
}

// ---- RouterService server -----------------------------------------------

type routerServer struct {
	api.UnimplementedRouterServiceServer
	c *Controller
}

var _ api.RouterServiceServer = (*routerServer)(nil)

// ResolvePage maps a host to its page/tenant/deployment for the router cache.
func (s *routerServer) ResolvePage(ctx context.Context, req *api.ResolvePageRequest) (*api.ResolvePageResponse, error) {
	pg, err := s.c.opts.Provider.ResolvePage(ctx, req.GetHost())
	if err != nil {
		return nil, status.Error(codes.NotFound, "controller: host not found")
	}
	return &api.ResolvePageResponse{
		Page: &api.PageInfo{
			PageId:             pg.ID,
			TenantId:           pg.TenantID,
			ActiveDeploymentId: pg.ActiveDeploymentID,
			HasWorker:          pg.ActiveDeploymentID != "",
		},
		TtlSeconds: int64(defaultResolveTTL / time.Second),
	}, nil
}

const defaultResolveTTL = 10 * time.Second

// AcquireSlot enqueues a request for a worker slot and streams QUEUED ->
// GRANTED (or TIMEOUT).
func (s *routerServer) AcquireSlot(req *api.AcquireSlotRequest, stream api.RouterService_AcquireSlotServer) error {
	c := s.c
	pageID := req.GetPageId()
	page, err := c.opts.Provider.GetPage(stream.Context(), pageID)
	if err != nil {
		return status.Error(codes.NotFound, "controller: page not found")
	}
	tenantID := page.TenantID
	t := c.getTenant(tenantID)

	// Queue timeout: page value clamped to the controller maximum.
	qt := c.opts.Defaults.QueueTimeout
	if page.Config.QueueTimeout > 0 {
		qt = page.Config.QueueTimeout
	}
	if qt > c.opts.Defaults.MaxQueueTimeout {
		qt = c.opts.Defaults.MaxQueueTimeout
	}
	ctx, cancel := context.WithTimeout(stream.Context(), qt)
	defer cancel()

	// Scale judgment runs on every request (ARCHITECTURE 6.4).
	c.maybeScaleUp(t)

	// Immediate assignment fast path: only when nobody is waiting, to preserve
	// FIFO. Depth>0 or a pending placement means queued requests come first.
	t.mu.Lock()
	depth, _ := c.opts.Queue.Depth(ctx, tenantID)
	if depth == 0 && !t.pendingPlacement {
		if p := t.tryReserve(pageID); p != nil {
			t.mu.Unlock()
			lease, err := c.buildLease(t, p, pageID)
			if err != nil {
				t.mu.Lock()
				t.releaseSlot(p)
				t.mu.Unlock()
				t.signalSlot()
				return status.Error(codes.Internal, "controller: lease signing failed")
			}
			return stream.Send(grantedEvent(lease))
		}
	}
	t.mu.Unlock()

	// Enqueue and wait for the dispatcher to pair us with a slot.
	w := &waiter{ctx: ctx, pageID: pageID, result: make(chan *api.Lease, 1)}
	item := queue.Item{
		ID:         randID(),
		PageID:     pageID,
		EnqueuedAt: c.now(),
		Deadline:   c.now().Add(qt),
	}
	c.registerWaiter(item.ID, w)
	defer c.unregisterWaiter(item.ID)

	ticket, err := c.opts.Queue.Enqueue(ctx, tenantID, item)
	if err != nil {
		if ctx.Err() != nil {
			return stream.Send(timeoutEvent())
		}
		return status.Error(codes.Internal, "controller: enqueue failed")
	}
	if pos, perr := ticket.Position(ctx); perr == nil && pos >= 0 {
		if err := stream.Send(queuedEvent(pos)); err != nil {
			return err
		}
	}

	select {
	case lease := <-w.result:
		return stream.Send(grantedEvent(lease))
	case <-ctx.Done():
		w.mu.Lock()
		if w.settled {
			// The dispatcher delivered concurrently; honour the grant.
			w.mu.Unlock()
			lease := <-w.result
			return stream.Send(grantedEvent(lease))
		}
		w.settled = true
		w.mu.Unlock()
		return stream.Send(timeoutEvent())
	}
}

// ReleaseSlot releases a lease's slot. Unknown or already-released leases are a
// no-op (idempotent).
func (s *routerServer) ReleaseSlot(ctx context.Context, req *api.ReleaseSlotRequest) (*api.ReleaseSlotResponse, error) {
	s.c.releaseLease(req.GetLeaseId())
	return &api.ReleaseSlotResponse{}, nil
}

// ---- WorkerService server -----------------------------------------------

type workerServer struct {
	api.UnimplementedWorkerServiceServer
	c *Controller
}

var _ api.WorkerServiceServer = (*workerServer)(nil)

// Register authenticates the shim (worker JWT in the "authorization" metadata,
// verified with the controller public key; tenant/pod must match the claims),
// then moves the pod from creating to ready. Unknown pods bearing a valid JWT
// are adopted (the valid signature proves the controller created them).
func (s *workerServer) Register(ctx context.Context, req *api.RegisterRequest) (*api.RegisterResponse, error) {
	c := s.c
	claims, err := c.authClaims(ctx)
	if err != nil {
		return nil, err
	}
	if claims.Tenant != req.GetTenantId() || claims.Pod != req.GetPodName() {
		return nil, status.Error(codes.Unauthenticated, "controller: token does not match tenant/pod")
	}

	t := c.getTenant(req.GetTenantId())
	t.mu.Lock()
	p := t.pods[req.GetPodName()]
	if p == nil {
		// Adoption path (reconcile or restart): trust the valid JWT.
		p = &pod{name: req.GetPodName(), loaded: map[string]string{}, createdAt: c.now()}
		t.pods[req.GetPodName()] = p
	}
	p.endpoint = req.GetEndpoint()
	p.phase = phaseReady
	p.seeded = false
	// Adopted / reconcile-seeded pods keep a zero jwtExpiry, which makes the
	// first heartbeat reissue a fresh, controller-signed JWT. Pods this run
	// created already carry their real expiry and keep it.
	if p.inFlight == 0 {
		p.idleSince = c.now()
	}
	t.mu.Unlock()
	t.signalSlot()

	return &api.RegisterResponse{
		HeartbeatIntervalSeconds: int64(c.opts.HeartbeatInterval / time.Second),
	}, nil
}

// GetPageConfig returns a page's Env/Secret bindings for lazy loading. The
// page must belong to the tenant of the caller's worker JWT — Secret values
// never cross a tenant boundary.
func (s *workerServer) GetPageConfig(ctx context.Context, req *api.GetPageConfigRequest) (*api.GetPageConfigResponse, error) {
	c := s.c
	claims, err := c.authClaims(ctx)
	if err != nil {
		return nil, err
	}
	page, err := c.opts.Provider.GetPage(ctx, req.GetPageId())
	if err != nil {
		if errors.Is(err, provider.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "controller: unknown page")
		}
		return nil, status.Error(codes.Internal, "controller: provider error")
	}
	if page.TenantID != claims.Tenant {
		return nil, status.Error(codes.PermissionDenied, "controller: page belongs to another tenant")
	}
	return &api.GetPageConfigResponse{
		Env:    page.Config.Env,
		Secret: page.Config.Secret,
	}, nil
}

// Heartbeat streams shim liveness. It updates loaded pages, answers with a
// drain instruction while the pod is draining, and renews the worker JWT before
// it expires. When a draining pod reports no in-flight work it is deleted.
func (s *workerServer) Heartbeat(stream api.WorkerService_HeartbeatServer) error {
	c := s.c
	claims, err := c.authClaims(stream.Context())
	if err != nil {
		return err
	}
	t := c.getTenant(claims.Tenant)
	podName := claims.Pod

	for {
		req, err := stream.Recv()
		if err != nil {
			return nil // client closed or errored; end the stream
		}

		var resp api.HeartbeatResponse
		var deleteIdle bool

		t.mu.Lock()
		if p := t.pods[podName]; p != nil {
			loaded := make(map[string]string, len(req.GetLoadedPages()))
			for _, lp := range req.GetLoadedPages() {
				loaded[lp.GetPageId()] = lp.GetDeploymentId()
			}
			p.loaded = loaded

			if p.phase == phaseDraining {
				resp.Drain = true
				deleteIdle = p.inFlight == 0
			}
			if c.jwtNeedsRenewal(p) {
				if nj, ierr := workerauth.Issue(c.opts.SigningKey, podName, claims.Tenant, c.opts.WorkerJWTTTL); ierr == nil {
					resp.RenewedJwt = nj
					p.jwtExpiry = c.now().Add(c.opts.WorkerJWTTTL)
				}
			}
		}
		t.mu.Unlock()

		if err := stream.Send(&resp); err != nil {
			return err
		}
		if deleteIdle {
			go c.deletePod(t, podName)
		}
	}
}

// jwtNeedsRenewal reports whether the pod's worker JWT should be reissued: its
// expiry is unknown (adopted) or falls within the renewal window (15m).
func (c *Controller) jwtNeedsRenewal(p *pod) bool {
	if p.jwtExpiry.IsZero() {
		return true
	}
	return c.now().Add(15 * time.Minute).After(p.jwtExpiry)
}

// authClaims verifies the worker JWT carried in the incoming "authorization"
// metadata against the controller public key.
func (c *Controller) authClaims(ctx context.Context) (*workerauth.Claims, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "controller: missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return nil, status.Error(codes.Unauthenticated, "controller: missing authorization")
	}
	token := strings.TrimSpace(strings.TrimPrefix(vals[0], "Bearer "))
	claims, err := workerauth.Verify(c.pub, token)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "controller: invalid worker token")
	}
	return claims, nil
}

// ---- pod deletion -------------------------------------------------------

// deletePod removes a pod from the pool, deletes it via the PodManager and
// forgets its leases. Safe to call more than once for the same pod.
func (c *Controller) deletePod(t *tenant, name string) {
	t.mu.Lock()
	p := t.pods[name]
	if p == nil {
		t.mu.Unlock()
		return
	}
	delete(t.pods, name)
	t.mu.Unlock()

	_ = c.opts.Pods.Delete(c.ctx, name)
	c.dropLeasesForPod(name)
	t.signalSlot()
}

// ---- event constructors -------------------------------------------------

func grantedEvent(l *api.Lease) *api.AcquireSlotEvent {
	return &api.AcquireSlotEvent{Event: &api.AcquireSlotEvent_Granted{Granted: l}}
}

func queuedEvent(pos int) *api.AcquireSlotEvent {
	return &api.AcquireSlotEvent{Event: &api.AcquireSlotEvent_Queued_{Queued: &api.AcquireSlotEvent_Queued{Position: int64(pos)}}}
}

func timeoutEvent() *api.AcquireSlotEvent {
	return &api.AcquireSlotEvent{Event: &api.AcquireSlotEvent_Timeout_{Timeout: &api.AcquireSlotEvent_Timeout{}}}
}

// ---- misc helpers -------------------------------------------------------

// resolveMaxPods returns the tenant pod ceiling: the tenant override when
// positive, else the controller default.
func resolveMaxPods(tenantObj *provider.Tenant, def int) int {
	if tenantObj != nil && tenantObj.Config.MaxConcurrency > 0 {
		return tenantObj.Config.MaxConcurrency
	}
	if def < 1 {
		return 1
	}
	return def
}

// pageKeys returns the page IDs of a loaded map.
func pageKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// podName builds a unique worker pod name: dpw-<tenant>-<rand6>.
func podName(tenantID string) string {
	return "dpw-" + sanitizeLabel(tenantID) + "-" + randSuffix(6)
}

// sanitizeLabel lowercases tenantID and keeps only DNS-safe characters.
func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "t"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

const randAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randSuffix returns n random lowercase alphanumeric characters.
func randSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = randAlphabet[int(b[i])%len(randAlphabet)]
	}
	return string(b)
}

// randID returns a random identifier for lease/request/queue-item IDs.
func randID() string { return randSuffix(20) }
