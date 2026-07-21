// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package shim implements the worker-pod PID-1 library: the runtime-neutral
// control loop that turns a tenant worker pod into a Cloudflare Pages-compatible
// server. It owns the request proxy (:8080), the env.ASSETS service (:8081), the
// tail trace collector (:8082, loopback only) and the health endpoints (:9090),
// and it drives bundle lazy-loading, workerd graceful swap and LRU eviction on
// top of the runtime.Runtime interface.
//
// The shim is deliberately runtime-neutral: everything specific to workerd lives
// in pkg/runtime/workerdruntime. The shim only speaks the fixed contracts —
// the X-DuruPages-* headers, the per-deployment bundle layout and the pkg/usage
// event schema.
package shim

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/runtime"
)

// Default configuration values (also overridable via the environment).
const (
	defaultMinIdle       = time.Hour
	defaultCacheMaxBytes = 2 << 30 // 2 GiB
	defaultSweepInterval = 5 * time.Minute

	heartbeatFailWindow  = 5 * time.Minute
	traceCorrelationWait = 150 * time.Millisecond
)

// heartbeatInterval is the heartbeat send period. It is a variable so tests can
// shorten it.
var heartbeatInterval = 5 * time.Second

// Options configures a Shim.
type Options struct {
	TenantID       string
	PodName        string
	ControllerAddr string
	HubAddr        string
	WorkerJWT      string
	LeasePubKey    ed25519.PublicKey
	Runtime        runtime.Runtime
	BundleDir      string

	// MinIdle is the minimum idle time before a deployment may be evicted
	// (env DURUPAGES_BUNDLE_MIN_IDLE, default 1h).
	MinIdle time.Duration
	// CacheMaxBytes triggers over-budget eviction (env
	// DURUPAGES_BUNDLE_CACHE_MAX_BYTES, default 2GiB).
	CacheMaxBytes int64
	// SweepInterval is the LRU sweep period (env DURUPAGES_BUNDLE_SWEEP_INTERVAL,
	// default 5m).
	SweepInterval time.Duration

	// LogClient, when non-nil, receives RequestUsage batches over gRPC.
	// When nil the shim runs in pod-log mode and writes JSON lines to LogWriter.
	LogClient api.LogServiceClient
	// LogWriter receives pod-log JSON lines (default os.Stdout).
	LogWriter io.Writer

	// Now is the clock (default time.Now); tests inject a fake clock.
	Now func() time.Time

	// Listener addresses. Empty uses the production default; "127.0.0.1:0" (or
	// ":0") binds an ephemeral port for tests.
	ProxyAddr  string // default ":8080"
	AssetsAddr string // default "127.0.0.1:8081" (loopback only)
	TailAddr   string // default "127.0.0.1:8082" (loopback only)
	HealthAddr string // default ":9090"

	// WorkerClient, when non-nil, is used instead of dialing ControllerAddr
	// (tests inject a bufconn client).
	WorkerClient api.WorkerServiceClient
	// HTTPClient is used for hub bundle downloads (default http.DefaultClient).
	HTTPClient *http.Client
}

// Shim is the worker-pod control loop. Construct it with New and drive it with
// Run.
type Shim struct {
	opts       Options
	now        func() time.Time
	minIdle    time.Duration
	cacheMax   int64
	sweepEvery time.Duration
	httpClient *http.Client

	proxyLn, assetsLn, tailLn, healthLn net.Listener
	assetsEndpoint, tailEndpoint        string

	// mu guards deployments and active; current is swapped atomically.
	mu          sync.Mutex
	deployments map[string]*deployment // keyed by deploymentID
	active      map[string]string      // pageID -> deploymentID
	current     atomic.Pointer[liveInstance]

	// swapMu serializes graceful swaps (loads and evictions).
	swapMu sync.Mutex

	loadingMu sync.Mutex
	loading   map[string]*loadCall

	redactor  atomic.Pointer[redactor]
	cor       *correlator
	emitter   *emitter
	transport http.RoundTripper

	stateMu   sync.Mutex
	draining  bool
	workerJWT string

	cancel     context.CancelFunc
	terminated atomic.Bool
}

// liveInstance couples a runtime instance with its shim-tracked in-flight count.
type liveInstance struct {
	inst     runtime.Instance
	inFlight int64
}

// New validates opts, applies defaults and environment overrides, and binds the
// four listeners so their addresses are available before Run. It does not start
// serving.
func New(opts Options) (*Shim, error) {
	if opts.TenantID == "" {
		return nil, errors.New("shim: TenantID required")
	}
	if opts.Runtime == nil {
		return nil, errors.New("shim: Runtime required")
	}
	if opts.BundleDir == "" {
		return nil, errors.New("shim: BundleDir required")
	}
	if len(opts.LeasePubKey) != ed25519.PublicKeySize {
		return nil, errors.New("shim: LeasePubKey required")
	}

	s := &Shim{
		opts:        opts,
		now:         opts.Now,
		minIdle:     opts.MinIdle,
		cacheMax:    opts.CacheMaxBytes,
		sweepEvery:  opts.SweepInterval,
		httpClient:  opts.HTTPClient,
		deployments: map[string]*deployment{},
		active:      map[string]string{},
		loading:     map[string]*loadCall{},
		workerJWT:   opts.WorkerJWT,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.minIdle == 0 {
		s.minIdle = envDuration("DURUPAGES_BUNDLE_MIN_IDLE", defaultMinIdle)
	}
	if s.cacheMax == 0 {
		s.cacheMax = envBytes("DURUPAGES_BUNDLE_CACHE_MAX_BYTES", defaultCacheMaxBytes)
	}
	if s.sweepEvery == 0 {
		s.sweepEvery = envDuration("DURUPAGES_BUNDLE_SWEEP_INTERVAL", defaultSweepInterval)
	}
	if s.httpClient == nil {
		s.httpClient = http.DefaultClient
	}
	s.cor = newCorrelator(s.now)
	s.emitter = newEmitter(opts.LogClient, opts.LogWriter)
	s.redactor.Store(newRedactor(nil))
	s.transport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
	}

	if err := os.MkdirAll(opts.BundleDir, 0o700); err != nil {
		return nil, fmt.Errorf("shim: bundle dir: %w", err)
	}

	var err error
	if s.proxyLn, err = listen(opts.ProxyAddr, ":8080"); err != nil {
		return nil, err
	}
	if s.assetsLn, err = listen(opts.AssetsAddr, "127.0.0.1:8081"); err != nil {
		return nil, err
	}
	if s.tailLn, err = listen(opts.TailAddr, "127.0.0.1:8082"); err != nil {
		return nil, err
	}
	if s.healthLn, err = listen(opts.HealthAddr, ":9090"); err != nil {
		return nil, err
	}
	s.assetsEndpoint = loopbackHostPort(s.assetsLn)
	s.tailEndpoint = loopbackHostPort(s.tailLn)
	return s, nil
}

// ProxyAddr, AssetsAddr, TailAddr and HealthAddr expose the bound listener
// addresses (useful when an ephemeral port was requested in tests).
func (s *Shim) ProxyAddr() string  { return s.proxyLn.Addr().String() }
func (s *Shim) AssetsAddr() string { return s.assetsLn.Addr().String() }
func (s *Shim) TailAddr() string   { return s.tailLn.Addr().String() }
func (s *Shim) HealthAddr() string { return s.healthLn.Addr().String() }

// Run starts all servers and background loops and blocks until ctx is cancelled
// or the shim self-terminates after a sustained heartbeat outage. It always
// tears the servers and the current runtime instance down before returning.
func (s *Shim) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	servers := []struct {
		ln net.Listener
		h  http.Handler
	}{
		{s.proxyLn, http.HandlerFunc(s.serveProxy)},
		{s.assetsLn, http.HandlerFunc(s.serveAssets)},
		{s.tailLn, http.HandlerFunc(s.serveCollector)},
		{s.healthLn, s.healthMux()},
	}
	httpServers := make([]*http.Server, len(servers))
	var wg sync.WaitGroup
	for i, sv := range servers {
		srv := &http.Server{Handler: sv.h}
		httpServers[i] = srv
		wg.Add(1)
		go func(srv *http.Server, ln net.Listener) {
			defer wg.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// Serving errors are logged to the pod log; nothing to recover.
				fmt.Fprintf(os.Stderr, "[shim] server error: %v\n", err)
			}
		}(srv, sv.ln)
	}

	wg.Add(1)
	go func() { defer wg.Done(); s.runController(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); s.runSweep(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); s.cor.janitor(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); s.emitter.run(ctx) }()

	<-ctx.Done()

	// Graceful shutdown.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	for _, srv := range httpServers {
		_ = srv.Shutdown(shutCtx)
	}
	shutCancel()
	if li := s.current.Load(); li != nil {
		_ = li.inst.Close()
	}
	wg.Wait()

	if s.terminated.Load() {
		return errors.New("shim: self-terminated after heartbeat outage")
	}
	return nil
}

// selfTerminate is invoked when the controller heartbeat has been failing for
// longer than heartbeatFailWindow; it stops Run so the pod exits (no orphans).
func (s *Shim) selfTerminate() {
	s.terminated.Store(true)
	if s.cancel != nil {
		s.cancel()
	}
}

// workerClient resolves the controller WorkerService client, dialing
// ControllerAddr when no client was injected.
func (s *Shim) workerClient() (api.WorkerServiceClient, func() error, error) {
	if s.opts.WorkerClient != nil {
		return s.opts.WorkerClient, func() error { return nil }, nil
	}
	if s.opts.ControllerAddr == "" {
		return nil, nil, errors.New("shim: no controller client or address")
	}
	conn, err := grpc.NewClient(s.opts.ControllerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return api.NewWorkerServiceClient(conn), conn.Close, nil
}

// listen binds addr (or def when addr is empty).
func listen(addr, def string) (net.Listener, error) {
	if addr == "" {
		addr = def
	}
	return net.Listen("tcp", addr)
}

// loopbackHostPort returns a 127.0.0.1:port endpoint for a bound listener,
// suitable for embedding in the runtime config (workerd dials it over loopback).
func loopbackHostPort(ln net.Listener) string {
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return net.JoinHostPort("127.0.0.1", port)
}
