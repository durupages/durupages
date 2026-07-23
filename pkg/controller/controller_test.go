// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"context"
	"crypto/ed25519"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/provider/memprovider"
	"github.com/durupages/durupages/pkg/scaler"
	"github.com/durupages/durupages/pkg/workerauth"
)

// ---- test harness -------------------------------------------------------

type testEnv struct {
	c     *Controller
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	prov  *memprovider.Provider
	pm    *fakePodManager
	clock *fakeClock

	router api.RouterServiceClient
	worker api.WorkerServiceClient
}

func setup(t *testing.T, mutate func(o *Options)) *testEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	prov := memprovider.New(memprovider.Options{PagesDomain: "pages.example.com"})
	pm := newFakePodManager()
	clock := newFakeClock()

	opts := Options{
		Provider:          prov,
		Pods:              pm,
		SigningKey:        priv,
		Now:               clock.Now,
		HeartbeatInterval: 50 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	c.RegisterServices(srv)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		c.cancel()
	})

	return &testEnv{
		c:      c,
		priv:   priv,
		pub:    pub,
		prov:   prov,
		pm:     pm,
		clock:  clock,
		router: api.NewRouterServiceClient(conn),
		worker: api.NewWorkerServiceClient(conn),
	}
}

// putTenant/putPage are provider fixtures.
func (e *testEnv) putTenant(id string, maxConcurrency int) {
	e.prov.PutTenant(provider.Tenant{ID: id, Config: provider.TenantConfig{MaxConcurrency: maxConcurrency}})
}

func (e *testEnv) putPage(id, tenant, deployment string, queueTimeout time.Duration) {
	e.prov.PutPage(provider.Page{
		ID:                 id,
		TenantID:           tenant,
		ActiveDeploymentID: deployment,
		Config:             provider.PageConfig{QueueTimeout: queueTimeout, RequestTimeout: 5 * time.Second},
	})
}

// workerCtx attaches a valid worker JWT to ctx.
func (e *testEnv) workerCtx(ctx context.Context, pod, tenant string) context.Context {
	jwt, err := workerauth.Issue(e.priv, pod, tenant, time.Hour)
	if err != nil {
		panic(err)
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+jwt)
}

func (e *testEnv) register(t *testing.T, pod, tenant, endpoint string) {
	t.Helper()
	ctx := e.workerCtx(context.Background(), pod, tenant)
	if _, err := e.worker.Register(ctx, &api.RegisterRequest{TenantId: tenant, PodName: pod, Endpoint: endpoint}); err != nil {
		t.Fatalf("register %s: %v", pod, err)
	}
}

type acquireResult struct {
	lease   *api.Lease
	timeout bool
	err     error
}

// acquire drives one AcquireSlot stream to completion (GRANTED or TIMEOUT).
func (e *testEnv) acquire(ctx context.Context, tenant, page string) acquireResult {
	stream, err := e.router.AcquireSlot(ctx, &api.AcquireSlotRequest{TenantId: tenant, PageId: page})
	if err != nil {
		return acquireResult{err: err}
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return acquireResult{err: err}
		}
		switch ev.Event.(type) {
		case *api.AcquireSlotEvent_Granted:
			return acquireResult{lease: ev.GetGranted()}
		case *api.AcquireSlotEvent_Timeout_:
			return acquireResult{timeout: true}
		case *api.AcquireSlotEvent_Queued_:
			// keep waiting
		}
	}
}

func (e *testEnv) acquireAsync(ctx context.Context, tenant, page string) <-chan acquireResult {
	ch := make(chan acquireResult, 1)
	go func() { ch <- e.acquire(ctx, tenant, page) }()
	return ch
}

func (e *testEnv) podInFlight(tenant, pod string) int {
	tn := e.c.getTenant(tenant)
	tn.mu.Lock()
	defer tn.mu.Unlock()
	if p := tn.pods[pod]; p != nil {
		return p.inFlight
	}
	return -1
}

func (e *testEnv) pendingPlacement(tenant string) bool {
	tn := e.c.getTenant(tenant)
	tn.mu.Lock()
	defer tn.mu.Unlock()
	return tn.pendingPlacement
}

func (e *testEnv) queueDepth(tenant string) int {
	d, _ := e.c.opts.Queue.Depth(context.Background(), tenant)
	return d
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// ---- New / defaults -----------------------------------------------------

func TestNewValidationAndDefaults(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	prov := memprovider.New(memprovider.Options{})
	pm := newFakePodManager()

	if _, err := New(Options{Pods: pm, SigningKey: priv}); err == nil {
		t.Fatal("expected error without Provider")
	}
	if _, err := New(Options{Provider: prov, SigningKey: priv}); err == nil {
		t.Fatal("expected error without Pods")
	}
	if _, err := New(Options{Provider: prov, Pods: pm}); err == nil {
		t.Fatal("expected error without SigningKey")
	}

	c, err := New(Options{Provider: prov, Pods: pm, SigningKey: priv})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := c.opts.Defaults
	if d.QueueTimeout != 30*time.Second || d.MaxQueueTimeout != 120*time.Second ||
		d.RequestTimeout != 60*time.Second || d.MaxConcurrency != 5 ||
		d.MaxConcurrencyPerPod != 256 || d.TargetConcurrencyPerPod != 32 || d.IdleTTL != 60*time.Second {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	if c.opts.ScaleDownInterval != 30*time.Second {
		t.Fatalf("scale-down interval default = %v", c.opts.ScaleDownInterval)
	}
	c.cancel()
}

// ---- cold start: create -> register -> grant ----------------------------

func TestAcquireSlotColdStartGrantsLease(t *testing.T) {
	e := setup(t, nil)
	e.putTenant("acme", 0)
	e.putPage("blog", "acme", "dep1", 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, "acme", "blog")

	// A pod should be created by the scale-up judgment.
	waitFor(t, 2*time.Second, "pod create", func() bool { return len(e.pm.createdNames()) >= 1 })
	specs := e.pm.createdSpecs()
	podName := specs[0].Name

	// Verify injected env.
	env := specs[0].Env
	if env["DURUPAGES_TENANT_ID"] != "acme" || env["DURUPAGES_POD_NAME"] != podName {
		t.Fatalf("bad env: %v", env)
	}
	if env["DURUPAGES_WORKER_JWT"] == "" {
		t.Fatal("missing worker JWT env")
	}
	if env["DURUPAGES_LEASE_PUBKEY"] == "" {
		t.Fatal("missing lease pubkey env")
	}

	// Shim registers; the queued request is then granted.
	e.register(t, podName, "acme", "10.0.0.7:8080")

	var r acquireResult
	select {
	case r = <-res:
	case <-time.After(3 * time.Second):
		t.Fatal("acquire did not complete")
	}
	if r.err != nil || r.lease == nil {
		t.Fatalf("expected grant, got %+v", r)
	}
	if r.lease.Endpoint != "10.0.0.7:8080" || r.lease.PageId != "blog" || r.lease.DeploymentId != "dep1" {
		t.Fatalf("bad lease: %+v", r.lease)
	}
	// The lease signature must verify with the controller public key.
	claims, err := workerauth.VerifyLease(e.pub, r.lease.Signature)
	if err != nil {
		t.Fatalf("verify lease: %v", err)
	}
	if claims.TenantID != "acme" || claims.PageID != "blog" || claims.LeaseID != r.lease.LeaseId {
		t.Fatalf("bad lease claims: %+v", claims)
	}
}

// ---- loaded-page preference ---------------------------------------------

func TestAcquireSlotPrefersPodWithPageLoaded(t *testing.T) {
	e := setup(t, nil)
	e.putTenant("acme", 5)
	e.putPage("blog", "acme", "dep1", 3*time.Second)

	e.register(t, "podA", "acme", "epA:8080")
	e.register(t, "podB", "acme", "epB:8080")

	// podB reports blog as loaded via a heartbeat.
	hbCtx := e.workerCtx(context.Background(), "podB", "acme")
	hb, err := e.worker.Heartbeat(hbCtx)
	if err != nil {
		t.Fatalf("heartbeat open: %v", err)
	}
	if err := hb.Send(&api.HeartbeatRequest{State: "ready", LoadedPages: []*api.LoadedPage{{PageId: "blog", DeploymentId: "dep1"}}}); err != nil {
		t.Fatalf("heartbeat send: %v", err)
	}
	if _, err := hb.Recv(); err != nil {
		t.Fatalf("heartbeat recv: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := e.acquire(ctx, "acme", "blog")
	if r.lease == nil {
		t.Fatalf("expected grant, got %+v", r)
	}
	if r.lease.Endpoint != "epB:8080" {
		t.Fatalf("expected grant on loaded pod epB, got %s", r.lease.Endpoint)
	}
}

// ---- hard cap: queue then release frees slot (FIFO) ---------------------

func TestHardCapQueueingReleaseFIFO(t *testing.T) {
	e := setup(t, func(o *Options) {
		o.Defaults.MaxConcurrencyPerPod = 1
	})
	e.putTenant("acme", 1) // a single pod
	e.putPage("blog", "acme", "dep1", 5*time.Second)

	e.register(t, "pod1", "acme", "ep1:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// #1 grabs the only slot immediately.
	r1 := e.acquire(ctx, "acme", "blog")
	if r1.lease == nil {
		t.Fatalf("first acquire not granted: %+v", r1)
	}
	if got := e.podInFlight("acme", "pod1"); got != 1 {
		t.Fatalf("inFlight = %d, want 1", got)
	}

	// #2 queues and becomes the dispatcher's held (pending) item.
	c2 := e.acquireAsync(ctx, "acme", "blog")
	waitFor(t, 2*time.Second, "#2 pending", func() bool { return e.pendingPlacement("acme") })

	// #3 queues strictly behind #2.
	c3 := e.acquireAsync(ctx, "acme", "blog")
	waitFor(t, 2*time.Second, "#3 queued", func() bool { return e.queueDepth("acme") == 1 })

	// Releasing #1 must free the slot to #2 (the first waiter).
	if _, err := e.router.ReleaseSlot(ctx, &api.ReleaseSlotRequest{LeaseId: r1.lease.LeaseId}); err != nil {
		t.Fatalf("release #1: %v", err)
	}
	var r2 acquireResult
	select {
	case r2 = <-c2:
	case <-time.After(3 * time.Second):
		t.Fatal("#2 not granted after release")
	}
	if r2.lease == nil {
		t.Fatalf("#2 not granted: %+v", r2)
	}

	// #3 must still be waiting.
	select {
	case r3 := <-c3:
		t.Fatalf("#3 granted out of order: %+v", r3)
	default:
	}

	// Releasing #2 finally serves #3.
	if _, err := e.router.ReleaseSlot(ctx, &api.ReleaseSlotRequest{LeaseId: r2.lease.LeaseId}); err != nil {
		t.Fatalf("release #2: %v", err)
	}
	select {
	case r3 := <-c3:
		if r3.lease == nil {
			t.Fatalf("#3 not granted: %+v", r3)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("#3 not granted after release")
	}
}

// ---- queue timeout ------------------------------------------------------

func TestAcquireSlotQueueTimeout(t *testing.T) {
	e := setup(t, nil)
	e.putTenant("acme", 1)
	e.putPage("blog", "acme", "dep1", 120*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// No pod ever registers, so the wait times out.
	r := e.acquire(ctx, "acme", "blog")
	if r.err != nil {
		t.Fatalf("unexpected err: %v", r.err)
	}
	if !r.timeout {
		t.Fatalf("expected TIMEOUT, got %+v", r)
	}
}

// ---- ReleaseSlot idempotency --------------------------------------------

func TestReleaseSlotIdempotent(t *testing.T) {
	e := setup(t, nil)
	e.putTenant("acme", 5)
	e.putPage("blog", "acme", "dep1", 3*time.Second)
	e.register(t, "pod1", "acme", "ep1:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := e.acquire(ctx, "acme", "blog")
	if r.lease == nil {
		t.Fatalf("no grant: %+v", r)
	}
	if got := e.podInFlight("acme", "pod1"); got != 1 {
		t.Fatalf("inFlight = %d, want 1", got)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.router.ReleaseSlot(ctx, &api.ReleaseSlotRequest{LeaseId: r.lease.LeaseId}); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
	// Unknown lease is also a no-op.
	if _, err := e.router.ReleaseSlot(ctx, &api.ReleaseSlotRequest{LeaseId: "does-not-exist"}); err != nil {
		t.Fatalf("release unknown: %v", err)
	}
	if got := e.podInFlight("acme", "pod1"); got != 0 {
		t.Fatalf("inFlight = %d, want 0 (not negative)", got)
	}
}

// ---- scale-down: select -> drain via heartbeat -> delete ----------------

func TestScaleDownDrainsViaHeartbeatThenDeletes(t *testing.T) {
	drainAll := &scriptedScaler{
		selectDrain: func(st scaler.TenantScaleState) []scaler.PodRef {
			var refs []scaler.PodRef
			for _, p := range st.ReadyPods {
				refs = append(refs, p.Ref)
			}
			return refs
		},
	}
	e := setup(t, func(o *Options) { o.Scaler = drainAll })
	e.putTenant("acme", 5)
	e.register(t, "pod1", "acme", "ep1:8080")

	// Run a heartbeat loop so the shim observes the drain instruction.
	hbCtx := e.workerCtx(context.Background(), "pod1", "acme")
	hb, err := e.worker.Heartbeat(hbCtx)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	sawDrain := make(chan struct{}, 1)
	go func() {
		for {
			if err := hb.Send(&api.HeartbeatRequest{State: "ready"}); err != nil {
				return
			}
			resp, err := hb.Recv()
			if err != nil {
				return
			}
			if resp.Drain {
				select {
				case sawDrain <- struct{}{}:
				default:
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Trigger the scale-down decision.
	e.c.scaleDownOnce(context.Background())

	select {
	case <-sawDrain:
	case <-time.After(3 * time.Second):
		t.Fatal("shim never saw drain instruction")
	}
	waitFor(t, 3*time.Second, "pod deleted", func() bool {
		for _, n := range e.pm.deletedNames() {
			if n == "pod1" {
				return true
			}
		}
		return false
	})
}

// ---- reconcile: adopt window then delete orphan -------------------------

func TestReconcileDeletesUnadoptedPod(t *testing.T) {
	e := setup(t, nil)
	e.pm.setList([]ExistingPod{{
		Name:     "orphan-1",
		TenantID: "acme",
		Labels:   map[string]string{labelAppName: appNameWorker, labelTenantID: "acme"},
	}})

	// Reconcile seeds the pod with an adoption deadline; it never registers.
	e.c.reconcile(context.Background())

	// Past the adoption window, the periodic pass deletes it.
	e.clock.Advance(500 * time.Millisecond)
	e.c.scaleDownOnce(context.Background())

	waitFor(t, 2*time.Second, "orphan deleted", func() bool {
		for _, n := range e.pm.deletedNames() {
			if n == "orphan-1" {
				return true
			}
		}
		return false
	})
}

// A pod discovered at reconcile time that has already failed (RestartPolicyNever
// makes this terminal) must never be adopted: adopting it would only earn it an
// adoption window it can never use while it sits there indefinitely, since a
// normally-created (non-seeded) phaseCreating pod has no timeout of its own.
func TestReconcileExcludesFailedPod(t *testing.T) {
	e := setup(t, nil)
	e.pm.setList([]ExistingPod{{
		Name:     "dead-1",
		TenantID: "acme",
		Labels:   map[string]string{labelAppName: appNameWorker, labelTenantID: "acme"},
		Failed:   true,
	}})

	e.c.reconcile(context.Background())

	tn := e.c.getTenant("acme")
	tn.mu.Lock()
	_, adopted := tn.pods["dead-1"]
	tn.mu.Unlock()
	if adopted {
		t.Fatal("a failed pod was adopted into the tenant's pool")
	}
	waitFor(t, 2*time.Second, "failed pod deleted", func() bool {
		for _, n := range e.pm.deletedNames() {
			if n == "dead-1" {
				return true
			}
		}
		return false
	})
}

// The bug this guards: a pod that fails AFTER maybeScaleUp creates it (bad
// image, crash on boot -- never seeded, so the adoption-window cleanup never
// applied to it at all) used to sit in phaseCreating forever. It kept counting
// toward the tenant's pod ceiling (readyCreatingCountLocked), so maybeScaleUp
// believed capacity was already met and never created a replacement: every
// request queued and timed out against a pod that would never register, with
// no way out short of a manual delete. scaleDownOnce must recognize the failure
// from the pod list and delete it itself, on any phase, seeded or not.
func TestScaleDownReplacesFailedCreatingPod(t *testing.T) {
	e := setup(t, nil)
	e.putTenant("acme", 1) // ceiling of 1: a stuck failed pod fully blocks scale-up
	e.putPage("blog", "acme", "dep1", 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	res := e.acquireAsync(ctx, "acme", "blog")

	// maybeScaleUp creates the one pod the ceiling allows. It never registers --
	// its container is about to crash.
	waitFor(t, 2*time.Second, "pod create", func() bool { return len(e.pm.createdNames()) >= 1 })
	podName := e.pm.createdNames()[0]

	tn := e.c.getTenant("acme")
	tn.mu.Lock()
	p := tn.pods[podName]
	if p == nil || p.phase != phaseCreating || p.seeded {
		t.Fatalf("expected a non-seeded phaseCreating pod, got %+v", p)
	}
	tn.mu.Unlock()

	// The pod now reports Failed on the next list -- RestartPolicyNever, the
	// container exited non-zero, it will never call Register.
	e.pm.setList([]ExistingPod{{
		Name:     podName,
		TenantID: "acme",
		Labels:   map[string]string{labelAppName: appNameWorker, labelTenantID: "acme"},
		Failed:   true,
	}})
	e.c.scaleDownOnce(context.Background())

	waitFor(t, 2*time.Second, "failed pod deleted", func() bool {
		for _, n := range e.pm.deletedNames() {
			if n == podName {
				return true
			}
		}
		return false
	})
	tn.mu.Lock()
	_, stillTracked := tn.pods[podName]
	tn.mu.Unlock()
	if stillTracked {
		t.Fatal("the failed pod is still tracked; it will keep blocking scale-up")
	}

	// The slot is free, but scale-up is request-driven (ARCHITECTURE 6.4): only
	// deleting the pod is not enough, something has to ask again. That is
	// exactly what a client retrying after its own queue timeout -- or any
	// other concurrent request for the tenant -- does in production; simulate
	// it with a second request rather than reaching in and calling
	// maybeScaleUp directly.
	e.pm.setList(nil)
	res2 := e.acquireAsync(ctx, "acme", "blog")

	waitFor(t, 2*time.Second, "replacement pod create", func() bool {
		return len(e.pm.createdNames()) >= 2
	})
	replacement := e.pm.createdNames()[len(e.pm.createdNames())-1]
	if replacement == podName {
		t.Fatal("no replacement pod was created")
	}
	e.register(t, replacement, "acme", "10.0.0.9:8080")

	// Both the original (queued first) and the retriggering request are
	// eventually granted from the replacement pod.
	for _, ch := range []<-chan acquireResult{res, res2} {
		select {
		case r := <-ch:
			if r.err != nil || r.lease == nil {
				t.Fatalf("expected a grant once the replacement registered, got %+v", r)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("request never granted; it is still stuck behind the failed pod's slot")
		}
	}
}

// ---- ResolvePage --------------------------------------------------------

func TestResolvePage(t *testing.T) {
	e := setup(t, nil)
	e.putTenant("acme", 0)
	e.putPage("blog", "acme", "dep1", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := e.router.ResolvePage(ctx, &api.ResolvePageRequest{Host: "blog.pages.example.com"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resp.Page.PageId != "blog" || resp.Page.TenantId != "acme" || resp.Page.ActiveDeploymentId != "dep1" {
		t.Fatalf("bad page info: %+v", resp.Page)
	}
	if _, err := e.router.ResolvePage(ctx, &api.ResolvePageRequest{Host: "nope.pages.example.com"}); err == nil {
		t.Fatal("expected NotFound for unknown host")
	}
}

// ---- Register auth ------------------------------------------------------

func TestRegisterRejectsBadAuth(t *testing.T) {
	e := setup(t, nil)

	// No metadata at all.
	if _, err := e.worker.Register(context.Background(), &api.RegisterRequest{TenantId: "acme", PodName: "p1"}); err == nil {
		t.Fatal("expected error without auth")
	}
	// Token for a different pod than the request claims.
	ctx := e.workerCtx(context.Background(), "other-pod", "acme")
	if _, err := e.worker.Register(ctx, &api.RegisterRequest{TenantId: "acme", PodName: "p1"}); err == nil {
		t.Fatal("expected error on pod mismatch")
	}
}

// ---- GetPageConfig ------------------------------------------------------

func TestGetPageConfig(t *testing.T) {
	e := setup(t, nil)
	e.putTenant("acme", 2)
	e.prov.PutPage(provider.Page{
		ID: "blog", TenantID: "acme", ActiveDeploymentID: "dep1",
		Config: provider.PageConfig{
			Env:    map[string]string{"MODE": "prod"},
			Secret: map[string]string{"API_KEY": "supersecret1"},
		},
	})
	e.putTenant("rival", 1)
	e.prov.PutPage(provider.Page{ID: "shop", TenantID: "rival"})

	ctx := e.workerCtx(context.Background(), "p1", "acme")
	resp, err := e.worker.GetPageConfig(ctx, &api.GetPageConfigRequest{PageId: "blog"})
	if err != nil {
		t.Fatalf("GetPageConfig: %v", err)
	}
	if resp.GetEnv()["MODE"] != "prod" || resp.GetSecret()["API_KEY"] != "supersecret1" {
		t.Fatalf("unexpected config: %+v", resp)
	}

	// Page of another tenant must be refused.
	if _, err := e.worker.GetPageConfig(ctx, &api.GetPageConfigRequest{PageId: "shop"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	// Unknown page.
	if _, err := e.worker.GetPageConfig(ctx, &api.GetPageConfigRequest{PageId: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
	// Missing auth.
	if _, err := e.worker.GetPageConfig(context.Background(), &api.GetPageConfigRequest{PageId: "blog"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}
