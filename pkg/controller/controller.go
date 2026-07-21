// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package controller implements the DuruPages control plane: a single-replica
// library that manages the per-tenant request queue, worker pod lifecycle,
// autoscaling and reconcile. It is assembled from the replaceable extension
// points (PageProvider, Queue, Scaler) plus a PodManager abstraction over
// Kubernetes, and exposes the RouterService and WorkerService gRPC servers.
//
// The controller is designed to be fully testable with fakes: swap in an
// in-memory queue, a scripted scaler and a recording PodManager, and drive time
// with an injected clock (Options.Now).
//
// Concurrency model. Global maps (tenants, leases, waiters) are guarded by
// small dedicated mutexes on the Controller. All per-tenant pod state lives on
// a *tenant guarded by its own mutex; the two levels are never held in a cycle
// (a Controller mutex is released before a tenant mutex is taken). A single
// dispatcher goroutine per tenant pairs Queue.Dequeue with freed slots, which
// keeps FIFO ordering simple to reason about (see dispatcher.go).
package controller

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/queue"
	"github.com/durupages/durupages/pkg/queue/inmemory"
	"github.com/durupages/durupages/pkg/scaler"
	"github.com/durupages/durupages/pkg/scaler/defaultscaler"
)

// Defaults mirrors the controller settings from ARCHITECTURE section 10. Zero
// values are replaced by the documented defaults in New.
type Defaults struct {
	// QueueTimeout is the default per-page queue wait timeout (default 30s).
	QueueTimeout time.Duration
	// MaxQueueTimeout is the upper clamp on any page's queue timeout (120s).
	MaxQueueTimeout time.Duration
	// RequestTimeout is the default worker request timeout / lease deadline
	// horizon (60s).
	RequestTimeout time.Duration
	// MaxConcurrency is the default maximum number of worker pods per tenant (5).
	MaxConcurrency int
	// MaxConcurrencyPerPod is the hard admission cap of in-flight requests a
	// single pod accepts (256).
	MaxConcurrencyPerPod int
	// TargetConcurrencyPerPod is the scale-up target concurrency per pod (32).
	TargetConcurrencyPerPod int
	// IdleTTL is the default idle time before a pod becomes scale-down eligible
	// (60s).
	IdleTTL time.Duration
}

// Options configures a Controller.
type Options struct {
	// Provider is the source of truth for pages/tenants (required).
	Provider provider.PageProvider
	// Queue orders waiting requests per tenant. Defaults to the in-memory queue.
	Queue queue.Queue
	// Scaler decides scale up/down. Defaults to the target/max-concurrency scaler.
	Scaler scaler.Scaler
	// Pods manages worker pods (required).
	Pods PodManager
	// SigningKey signs worker JWTs and lease tokens (required). Its public half
	// verifies worker Register/Heartbeat auth.
	SigningKey ed25519.PrivateKey
	// Defaults holds the controller settings; zero fields are defaulted.
	Defaults Defaults
	// Now returns the current time; defaults to time.Now. Injectable for tests.
	Now func() time.Time

	// ControllerAddr / HubAddr are propagated to worker pods via env.
	ControllerAddr string
	HubAddr        string
	// HubLogAddr, when set, enables worker log ingest by propagating
	// DURUPAGES_HUB_LOG_ADDR; empty keeps workers in pod-log mode.
	HubLogAddr string

	// BundleMinIdle / BundleCacheMaxBytes / BundleSweepInterval, when set, are
	// propagated to worker pods as the DURUPAGES_BUNDLE_* tuning envs.
	BundleMinIdle       string
	BundleCacheMaxBytes string
	BundleSweepInterval string

	// ScaleDownInterval is the scale-down / reconcile loop period (default 30s).
	ScaleDownInterval time.Duration
	// HeartbeatInterval is advertised to shims and drives the reconcile adoption
	// window (2x this value). Default 10s.
	HeartbeatInterval time.Duration
	// WorkerJWTTTL is the lifetime of issued worker JWTs (default 1h).
	WorkerJWTTTL time.Duration
	// LeaseGrace is the slack added past a lease deadline before the watchdog
	// force-releases it (default 10s).
	LeaseGrace time.Duration
	// DrainGrace bounds how long a draining pod is kept for in-flight to finish
	// before forced deletion (default = RequestTimeout).
	DrainGrace time.Duration
}

// Controller is the control plane. Construct it with New.
type Controller struct {
	opts Options
	pub  ed25519.PublicKey
	now  func() time.Time

	// ctx/cancel bound the lifetime of per-tenant dispatcher goroutines. New
	// creates them; Run cancels on return.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	tenants map[string]*tenant

	leaseMu sync.Mutex
	leases  map[string]*leaseRec

	waiterMu sync.Mutex
	waiters  map[string]*waiter

	// seededAt records when reconcile ran, used to widen the orphan-delete grace
	// right after startup.
	seededAt time.Time
}

// leaseRec tracks a granted lease for ReleaseSlot and the deadline watchdog.
type leaseRec struct {
	id       string
	tenantID string
	podName  string
	deadline time.Time
	released bool
}

// New validates opts, applies defaults and returns a ready Controller. The
// returned controller's gRPC servers work immediately; call Run to start the
// scale-down/reconcile loop.
func New(opts Options) (*Controller, error) {
	if opts.Provider == nil {
		return nil, errors.New("controller: Provider is required")
	}
	if opts.Pods == nil {
		return nil, errors.New("controller: Pods is required")
	}
	if len(opts.SigningKey) != ed25519.PrivateKeySize {
		return nil, errors.New("controller: SigningKey must be a valid ed25519 private key")
	}
	if opts.Queue == nil {
		opts.Queue = inmemory.New()
	}
	if opts.Scaler == nil {
		opts.Scaler = defaultscaler.New()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	d := &opts.Defaults
	setDurDefault(&d.QueueTimeout, 30*time.Second)
	setDurDefault(&d.MaxQueueTimeout, 120*time.Second)
	setDurDefault(&d.RequestTimeout, 60*time.Second)
	setIntDefault(&d.MaxConcurrency, 5)
	setIntDefault(&d.MaxConcurrencyPerPod, 256)
	setIntDefault(&d.TargetConcurrencyPerPod, 32)
	setDurDefault(&d.IdleTTL, 60*time.Second)

	setDurDefault(&opts.ScaleDownInterval, 30*time.Second)
	setDurDefault(&opts.HeartbeatInterval, 10*time.Second)
	setDurDefault(&opts.WorkerJWTTTL, time.Hour)
	setDurDefault(&opts.LeaseGrace, 10*time.Second)
	setDurDefault(&opts.DrainGrace, d.RequestTimeout)

	pub, ok := opts.SigningKey.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("controller: signing key has no ed25519 public half")
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{
		opts:    opts,
		pub:     pub,
		now:     opts.Now,
		ctx:     ctx,
		cancel:  cancel,
		tenants: make(map[string]*tenant),
		leases:  make(map[string]*leaseRec),
		waiters: make(map[string]*waiter),
	}
	return c, nil
}

// RegisterServices registers the RouterService and WorkerService gRPC servers
// on s.
func (c *Controller) RegisterServices(s *grpc.Server) {
	api.RegisterRouterServiceServer(s, &routerServer{c: c})
	api.RegisterWorkerServiceServer(s, &workerServer{c: c})
}

// Run reconciles existing worker pods, then loops the scale-down / drift /
// lease-watchdog checks every ScaleDownInterval until ctx is cancelled. On
// return it stops all per-tenant dispatcher goroutines.
func (c *Controller) Run(ctx context.Context) error {
	defer c.cancel()

	c.reconcile(ctx)

	ticker := time.NewTicker(c.opts.ScaleDownInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.scaleDownOnce(ctx)
		}
	}
}

// PublicKey returns the ed25519 public key that verifies worker JWTs and lease
// signatures. The hub is configured with the same key.
func (c *Controller) PublicKey() ed25519.PublicKey { return c.pub }

// getTenant returns the tenant state for id, creating it (and starting its
// dispatcher goroutine) on first use.
func (c *Controller) getTenant(id string) *tenant {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tenants[id]
	if t == nil {
		t = &tenant{
			id:        id,
			c:         c,
			pods:      make(map[string]*pod),
			slotFreed: make(chan struct{}, 1),
		}
		c.tenants[id] = t
		go t.dispatch()
	}
	return t
}

// snapshotTenants returns the current set of tenants.
func (c *Controller) snapshotTenants() []*tenant {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*tenant, 0, len(c.tenants))
	for _, t := range c.tenants {
		out = append(out, t)
	}
	return out
}

func setDurDefault(v *time.Duration, d time.Duration) {
	if *v <= 0 {
		*v = d
	}
}

func setIntDefault(v *int, d int) {
	if *v <= 0 {
		*v = d
	}
}

// tenantConfig fetches the tenant's config from the provider, tolerating a
// missing tenant (returns nil).
func (c *Controller) tenantConfig(ctx context.Context, id string) *provider.Tenant {
	t, err := c.opts.Provider.GetTenant(ctx, id)
	if err != nil {
		return nil
	}
	return t
}
