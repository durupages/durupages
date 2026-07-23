// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/runtime"
	"github.com/durupages/durupages/pkg/workerauth"
)

// ---- fake clock ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- fake runtime ----

// fakeRuntime launches in-process httptest servers as instances. A hook lets a
// test observe requests and (e.g.) emit tail traces to the shim collector.
type fakeRuntime struct {
	mu        sync.Mutex
	specs     []runtime.InstanceSpec
	instances []*fakeInstance
	launchErr error
	hook      func(spec runtime.InstanceSpec, w http.ResponseWriter, r *http.Request)
}

func (f *fakeRuntime) Launch(ctx context.Context, spec runtime.InstanceSpec) (runtime.Instance, error) {
	f.mu.Lock()
	if f.launchErr != nil {
		err := f.launchErr
		f.mu.Unlock()
		return nil, err
	}
	f.specs = append(f.specs, spec)
	hook := f.hook
	f.mu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			hook(spec, w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok:%s", r.Header.Get(api.HeaderPage))
	}))
	inst := &fakeInstance{srv: srv, endpoint: srv.Listener.Addr().String()}
	f.mu.Lock()
	f.instances = append(f.instances, inst)
	f.mu.Unlock()
	return inst, nil
}

// setLaunchErr makes every subsequent Launch fail, simulating a workerd that
// refuses to start.
func (f *fakeRuntime) setLaunchErr(err error) {
	f.mu.Lock()
	f.launchErr = err
	f.mu.Unlock()
}

func (f *fakeRuntime) instanceAt(i int) *fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.instances) {
		return nil
	}
	return f.instances[i]
}

func (f *fakeRuntime) launchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

func (f *fakeRuntime) lastSpec() runtime.InstanceSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) == 0 {
		return runtime.InstanceSpec{}
	}
	return f.specs[len(f.specs)-1]
}

type fakeInstance struct {
	srv      *httptest.Server
	endpoint string
	mu       sync.Mutex
	inFlight func() int
	closed   bool
	drained  bool
}

func (i *fakeInstance) Endpoint() string                    { return i.endpoint }
func (i *fakeInstance) WaitReady(ctx context.Context) error { return nil }
func (i *fakeInstance) SetInFlightFunc(f func() int)        { i.mu.Lock(); i.inFlight = f; i.mu.Unlock() }

func (i *fakeInstance) wasDrained() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.drained
}

func (i *fakeInstance) Drain(ctx context.Context) error {
	i.mu.Lock()
	f := i.inFlight
	i.drained = true
	i.mu.Unlock()
	if f == nil {
		return nil
	}
	for {
		if f() <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (i *fakeInstance) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.mu.Unlock()
	i.srv.Close()
	return nil
}

// emitTrace POSTs a trace record to the shim's tail collector, simulating the
// generated tail worker.
func emitTrace(t *testing.T, tailEndpoint, requestID string, rec map[string]any) {
	t.Helper()
	if rec == nil {
		rec = map[string]any{}
	}
	// Ensure the correlation header is present in the event request headers.
	ev, _ := rec["event"].(map[string]any)
	if ev == nil {
		ev = map[string]any{"request": map[string]any{"headers": map[string]any{}}}
		rec["event"] = ev
	}
	body, _ := json.Marshal([]map[string]any{rec})
	resp, err := http.Post("http://"+tailEndpoint+"/", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Logf("emitTrace: %v", err)
		return
	}
	resp.Body.Close()
}

// ---- fake worker service (controller) ----

type fakeWorkerService struct {
	api.UnimplementedWorkerServiceServer
	mu         sync.Mutex
	registered *api.RegisterRequest
	heartbeats []*api.HeartbeatRequest
	sendDrain  bool
	pageConfig map[string]*api.GetPageConfigResponse
}

// setPageConfig scripts a GetPageConfig response for a page.
func (s *fakeWorkerService) setPageConfig(pageID string, resp *api.GetPageConfigResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pageConfig == nil {
		s.pageConfig = map[string]*api.GetPageConfigResponse{}
	}
	s.pageConfig[pageID] = resp
}

// GetPageConfig serves scripted page configs. Pages without an entry return
// Unimplemented, like a legacy controller, exercising the fallback path that
// keeps the bundle-provided env.json/secret.json.
func (s *fakeWorkerService) GetPageConfig(_ context.Context, req *api.GetPageConfigRequest) (*api.GetPageConfigResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resp, ok := s.pageConfig[req.GetPageId()]; ok {
		return resp, nil
	}
	return nil, status.Error(codes.Unimplemented, "not scripted")
}

func (s *fakeWorkerService) Register(ctx context.Context, req *api.RegisterRequest) (*api.RegisterResponse, error) {
	s.mu.Lock()
	s.registered = req
	s.mu.Unlock()
	return &api.RegisterResponse{HeartbeatIntervalSeconds: 5}, nil
}

func (s *fakeWorkerService) Heartbeat(stream grpc.BidiStreamingServer[api.HeartbeatRequest, api.HeartbeatResponse]) error {
	first := true
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		s.heartbeats = append(s.heartbeats, req)
		drain := s.sendDrain && first
		s.mu.Unlock()
		first = false
		if err := stream.Send(&api.HeartbeatResponse{Drain: drain}); err != nil {
			return err
		}
	}
}

func (s *fakeWorkerService) lastHeartbeat() *api.HeartbeatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.heartbeats) == 0 {
		return nil
	}
	return s.heartbeats[len(s.heartbeats)-1]
}

func newWorkerClient(t *testing.T, svc api.WorkerServiceServer) api.WorkerServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	api.RegisterWorkerServiceServer(srv, svc)
	go srv.Serve(lis)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); srv.Stop(); lis.Close() })
	return api.NewWorkerServiceClient(conn)
}

// ---- bundle tar builder ----

type bundleSpec struct {
	tenant, page, dep string
	worker            map[string]string // path under worker/ -> content
	static            map[string]string // request path -> content
	env               map[string]string
	secret            map[string]string
	manifest          *manifest.Manifest // optional override base
}

func buildBundleTar(t *testing.T, bs bundleSpec) []byte {
	t.Helper()
	m := manifest.Manifest{
		Version:      1,
		TenantID:     bs.tenant,
		PageID:       bs.page,
		DeploymentID: bs.dep,
		HasWorker:    true,
		Static:       map[string]manifest.StaticEntry{},
	}
	if bs.manifest != nil {
		m = *bs.manifest
		m.Version = 1
		if m.Static == nil {
			m.Static = map[string]manifest.StaticEntry{}
		}
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeFile := func(name string, content []byte) {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}

	if bs.worker == nil {
		bs.worker = map[string]string{"index.js": "export default { async fetch() { return new Response('hi'); } }\n"}
	}
	for name, content := range bs.worker {
		writeFile("worker/"+name, []byte(content))
	}
	for reqPath, content := range bs.static {
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		m.Static[reqPath] = manifest.StaticEntry{Hash: hash, Size: int64(len(content)), ContentType: "text/plain; charset=utf-8"}
		writeFile("static/"+hash, []byte(content))
	}

	mj, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile("manifest.json", mj)
	if bs.env != nil {
		ej, _ := json.Marshal(bs.env)
		writeFile("env.json", ej)
	}
	if bs.secret != nil {
		sj, _ := json.Marshal(bs.secret)
		writeFile("secret.json", sj)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ---- test harness ----

type harness struct {
	t      *testing.T
	shim   *Shim
	rt     *fakeRuntime
	hub    *httptest.Server
	worker *fakeWorkerService
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
	clock  *fakeClock
	logBuf *syncBuffer
	// opsBuf collects Options.Logger output (the shim's own operational log),
	// which is a different stream from logBuf (the tenant-facing pod log).
	opsBuf *syncBuffer
	ctx    context.Context
	cancel context.CancelFunc
	done   chan error
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hubMux serves the bundles registered by tests.
type hubMux struct {
	mu        sync.Mutex
	bundles   map[string][]byte // "tenant/page/dep" -> tar
	auth      []string
	requestID []string // X-DuruPages-Request-Id seen on each fetch
}

// requestIDs returns the correlation ids the hub side observed.
func (h *hubMux) requestIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requestID...)
}

func (h *hubMux) set(tenant, page, dep string, tar []byte) {
	h.mu.Lock()
	if h.bundles == nil {
		h.bundles = map[string][]byte{}
	}
	h.bundles[tenant+"/"+page+"/"+dep] = tar
	h.mu.Unlock()
}

func (h *hubMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /v1/tenants/{t}/pages/{p}/deployments/{d}/bundle.tar
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 8 || parts[7] != "bundle.tar" {
		http.NotFound(w, r)
		return
	}
	key := parts[2] + "/" + parts[4] + "/" + parts[6]
	h.mu.Lock()
	tarBytes, ok := h.bundles[key]
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	h.requestID = append(h.requestID, r.Header.Get(api.HeaderRequestID))
	h.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Write(tarBytes)
}

type harnessOpts struct {
	logClient   api.LogServiceClient
	sendDrain   bool
	runtimeHook func(spec runtime.InstanceSpec, w http.ResponseWriter, r *http.Request)
	tenantID    string
	minIdle     time.Duration
	cacheMax    int64
	sweep       time.Duration
	noStartRun  bool
}

func newHarness(t *testing.T, ho harnessOpts) (*harness, *hubMux) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	hm := &hubMux{}
	hub := httptest.NewServer(hm)
	t.Cleanup(hub.Close)

	rt := &fakeRuntime{hook: ho.runtimeHook}
	worker := &fakeWorkerService{sendDrain: ho.sendDrain}
	wc := newWorkerClient(t, worker)
	logBuf := &syncBuffer{}
	opsBuf := &syncBuffer{}

	tenant := ho.tenantID
	if tenant == "" {
		tenant = "acme"
	}

	opts := Options{
		TenantID:      tenant,
		PodName:       "pod-1",
		HubAddr:       hub.URL,
		WorkerJWT:     "worker-jwt",
		LeasePubKey:   pub,
		Runtime:       rt,
		BundleDir:     t.TempDir(),
		MinIdle:       ho.minIdle,
		CacheMaxBytes: ho.cacheMax,
		SweepInterval: ho.sweep,
		LogClient:     ho.logClient,
		LogWriter:     logBuf,
		Logger:        slog.New(slog.NewJSONHandler(opsBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           clock.now,
		ProxyAddr:     "127.0.0.1:0",
		AssetsAddr:    "127.0.0.1:0",
		TailAddr:      "127.0.0.1:0",
		HealthAddr:    "127.0.0.1:0",
		WorkerClient:  wc,
	}
	if opts.MinIdle == 0 {
		opts.MinIdle = time.Hour
	}
	if opts.SweepInterval == 0 {
		opts.SweepInterval = time.Hour // effectively manual in tests
	}
	if opts.CacheMaxBytes == 0 {
		opts.CacheMaxBytes = 1 << 40
	}

	sh, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		t: t, shim: sh, rt: rt, hub: hub, worker: worker,
		priv: priv, pub: pub, clock: clock, logBuf: logBuf, opsBuf: opsBuf,
		ctx: ctx, cancel: cancel, done: make(chan error, 1),
	}
	if !ho.noStartRun {
		go func() { h.done <- sh.Run(ctx) }()
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
		}
	})
	return h, hm
}

// lease issues a signed lease token for the request.
func (h *harness) lease(page, dep, requestID string) string {
	h.t.Helper()
	tok, err := workerauth.IssueLease(h.priv, workerauth.LeaseClaims{
		LeaseID:      "lease-1",
		TenantID:     h.shim.opts.TenantID,
		PageID:       page,
		DeploymentID: dep,
		RequestID:    requestID,
	}, time.Minute)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// proxyRequest sends a proxied request with a valid lease.
func (h *harness) proxyRequest(page, dep, requestID string) (*http.Response, string) {
	h.t.Helper()
	return h.proxyRequestWithLease(page, dep, h.lease(page, dep, requestID))
}

func (h *harness) proxyRequestWithLease(page, dep, lease string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+h.shim.ProxyAddr()+"/api/x", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if lease != "" {
		req.Header.Set(api.HeaderLease, lease)
	}
	req.Header.Set(api.HeaderPage, page)
	req.Header.Set(api.HeaderDeployment, dep)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("proxy request: %v", err)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	return resp, string(body[:n])
}

// usageLines returns the parsed pod-log usage events emitted so far.
func (h *harness) usageLines() []usageLine {
	var out []usageLine
	for _, line := range strings.Split(strings.TrimRight(h.logBuf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var u usageLine
		if json.Unmarshal([]byte(line), &u) == nil {
			out = append(out, u)
		}
	}
	return out
}

// opsLines returns the shim's operational log lines (Options.Logger) parsed as
// JSON objects, in emission order.
func (h *harness) opsLines() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(h.opsBuf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// opsLine returns the first operational log line whose "msg" equals msg, or nil.
func (h *harness) opsLine(msg string) map[string]any {
	for _, l := range h.opsLines() {
		if l["msg"] == msg {
			return l
		}
	}
	return nil
}

// ed25519GenerateKey returns a fresh key pair.
func ed25519GenerateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	t.Helper()
	return ed25519.GenerateKey(nil)
}

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// waitFor polls until cond is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
