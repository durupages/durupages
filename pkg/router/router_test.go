// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/router/staticcache"
	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/storage/memstorage"
	"github.com/durupages/durupages/pkg/usage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const (
	testTenant = "acme"
	testPage   = "blog"
	testDep    = "dep_1"
	testHost   = "blog.example.com"
)

// --- fake controller (RouterService) -----------------------------------------

type pageResolve struct {
	page     *api.PageInfo
	ttl      int64
	notFound bool
}

type fakeController struct {
	api.UnimplementedRouterServiceServer

	pages   map[string]pageResolve
	acquire []*api.AcquireSlotEvent

	mu           sync.Mutex
	resolveCalls int
	released     []string
}

func (f *fakeController) ResolvePage(_ context.Context, req *api.ResolvePageRequest) (*api.ResolvePageResponse, error) {
	f.mu.Lock()
	f.resolveCalls++
	f.mu.Unlock()
	pr, ok := f.pages[req.GetHost()]
	if !ok || pr.notFound {
		return nil, status.Error(codes.NotFound, "unknown host")
	}
	return &api.ResolvePageResponse{Page: pr.page, TtlSeconds: pr.ttl}, nil
}

func (f *fakeController) AcquireSlot(_ *api.AcquireSlotRequest, stream grpc.ServerStreamingServer[api.AcquireSlotEvent]) error {
	for _, ev := range f.acquire {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeController) ReleaseSlot(_ context.Context, req *api.ReleaseSlotRequest) (*api.ReleaseSlotResponse, error) {
	f.mu.Lock()
	f.released = append(f.released, req.GetLeaseId())
	f.mu.Unlock()
	return &api.ReleaseSlotResponse{}, nil
}

func (f *fakeController) resolveCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolveCalls
}

func (f *fakeController) releasedLeases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

// --- fake log service --------------------------------------------------------

type fakeLogService struct {
	api.UnimplementedLogServiceServer
	mu       sync.Mutex
	statics  []usage.StaticAccess
	received chan struct{}
}

func (s *fakeLogService) Ingest(stream grpc.BidiStreamingServer[api.IngestBatch, api.IngestAck]) error {
	for {
		batch, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		for _, raw := range batch.GetStaticAccessJson() {
			var ev usage.StaticAccess
			if json.Unmarshal(raw, &ev) == nil {
				s.statics = append(s.statics, ev)
			}
		}
		s.mu.Unlock()
		if s.received != nil {
			select {
			case s.received <- struct{}{}:
			default:
			}
		}
		_ = stream.Send(&api.IngestAck{BatchId: batch.GetBatchId()})
	}
}

func (s *fakeLogService) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.statics)
}

// --- grpc plumbing -----------------------------------------------------------

func startGRPC(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc client: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})
	return conn
}

// --- seeding helpers ---------------------------------------------------------

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// addFile registers a static file in m and stores its bytes in store.
func addFile(t *testing.T, store storage.Storage, m *manifest.Manifest, path, content, contentType string) {
	t.Helper()
	h := hashOf(content)
	m.Static[path] = manifest.StaticEntry{Hash: h, Size: int64(len(content)), ContentType: contentType}
	key := sprintf(storage.StaticKeyFmt, m.TenantID, m.PageID, m.DeploymentID, h)
	if err := store.Put(context.Background(), key, strings.NewReader(content), int64(len(content)), contentType); err != nil {
		t.Fatalf("put static: %v", err)
	}
}

func seedManifest(t *testing.T, store storage.Storage, m *manifest.Manifest) {
	t.Helper()
	var buf strings.Builder
	if err := m.Encode(&buf); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	key := sprintf(storage.ManifestKeyFmt, m.TenantID, m.PageID, m.DeploymentID)
	if err := store.Put(context.Background(), key, strings.NewReader(buf.String()), int64(buf.Len()), "application/json"); err != nil {
		t.Fatalf("put manifest: %v", err)
	}
}

func newManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Version:      manifest.Version,
		TenantID:     testTenant,
		PageID:       testPage,
		DeploymentID: testDep,
		Static:       map[string]manifest.StaticEntry{},
	}
}

func defaultPage() *api.PageInfo {
	return &api.PageInfo{
		PageId:             testPage,
		TenantId:           testTenant,
		ActiveDeploymentId: testDep,
	}
}

// countingStorage wraps a Storage and counts Get calls per key.
type countingStorage struct {
	storage.Storage
	mu    sync.Mutex
	calls map[string]int
}

func newCountingStorage(inner storage.Storage) *countingStorage {
	return &countingStorage{Storage: inner, calls: map[string]int{}}
}

func (c *countingStorage) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	c.mu.Lock()
	c.calls[key]++
	c.mu.Unlock()
	return c.Storage.Get(ctx, key)
}

func (c *countingStorage) staticGets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, v := range c.calls {
		if strings.Contains(k, "/static/") {
			n += v
		}
	}
	return n
}

// --- harness -----------------------------------------------------------------

type harness struct {
	rt    *Router
	ctrl  *fakeController
	store *countingStorage
	// log is the USAGE log (pod-log StaticAccess JSON lines).
	log *strings.Builder
	// oplog is the OPERATIONAL log (Options.Logger), captured as JSON lines at
	// debug level so tests can assert on failure causes and access lines.
	oplog *logCapture
}

// logCapture is a slog logger writing JSON lines into a buffer.
type logCapture struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	logger *slog.Logger
}

func newLogCapture() *logCapture {
	c := &logCapture{}
	c.logger = slog.New(slog.NewJSONHandler(&syncWriter{c: c}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return c
}

// syncWriter serializes writes from concurrent requests into the buffer.
type syncWriter struct{ c *logCapture }

func (w *syncWriter) Write(b []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(b)
}

// logEntry is one decoded operational log line. Attributes the tests do not
// care about are ignored by encoding/json.
type logEntry struct {
	Level          string `json:"level"`
	Msg            string `json:"msg"`
	Method         string `json:"method"`
	Host           string `json:"host"`
	Path           string `json:"path"`
	Status         int    `json:"status"`
	UpstreamStatus int    `json:"upstreamStatus"`
	Bytes          int64  `json:"bytes"`
	Route          string `json:"route"`
	RequestID      string `json:"requestId"`
	TenantID       string `json:"tenantId"`
	PageID         string `json:"pageId"`
	DeploymentID   string `json:"deploymentId"`
	Endpoint       string `json:"endpoint"`
	LeaseID        string `json:"leaseId"`
	GRPCCode       string `json:"grpcCode"`
	Error          string `json:"error"`
	AssetHash      string `json:"assetHash"`
	Raw            string `json:"-"`
}

// entries decodes every line written so far.
func (c *logCapture) entries(t *testing.T) []logEntry {
	t.Helper()
	c.mu.Lock()
	raw := c.buf.String()
	c.mu.Unlock()
	var out []logEntry
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var e logEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal op log line %q: %v", line, err)
		}
		e.Raw = line
		out = append(out, e)
	}
	return out
}

// find returns the single entry with the given message, failing when there is
// not exactly one.
func (c *logCapture) find(t *testing.T, msg string) logEntry {
	t.Helper()
	var got []logEntry
	for _, e := range c.entries(t) {
		if e.Msg == msg {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("op log: %d lines with msg %q, want 1; log:\n%s", len(got), msg, c.dump(t))
	}
	return got[0]
}

func (c *logCapture) has(t *testing.T, msg string) bool {
	t.Helper()
	for _, e := range c.entries(t) {
		if e.Msg == msg {
			return true
		}
	}
	return false
}

func (c *logCapture) dump(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func newHarness(t *testing.T, ctrl *fakeController, m *manifest.Manifest) *harness {
	t.Helper()
	return newHarnessWith(t, ctrl, ctrl, m)
}

// newHarnessWith is newHarness for tests that need a RouterService
// implementation other than the plain fake (e.g. one that fails AcquireSlot).
// ctrl is the fake the harness reports on; srv is what is actually served.
func newHarnessWith(t *testing.T, srv api.RouterServiceServer, ctrl *fakeController, m *manifest.Manifest) *harness {
	t.Helper()
	mem := memstorage.New()
	store := newCountingStorage(mem)
	if m != nil {
		seedManifest(t, store, m)
	}
	conn := startGRPC(t, func(s *grpc.Server) {
		api.RegisterRouterServiceServer(s, srv)
	})
	cache, err := staticcache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var logbuf strings.Builder
	oplog := newLogCapture()
	rt, err := New(Options{
		Resolver:        api.NewRouterServiceClient(conn),
		Storage:         store,
		Cache:           cache,
		ResolveCacheTTL: 10 * time.Second,
		LogWriter:       &logbuf,
		// Captured rather than left to default, so tests can assert on the
		// operational log and do not spray slog.Default() over the test output.
		Logger: oplog.logger,
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	return &harness{rt: rt, ctrl: ctrl, store: store, log: &logbuf, oplog: oplog}
}

func (h *harness) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.rt.ServeHTTP(rec, req)
	return rec
}

func ctrlWithPage() *fakeController {
	return &fakeController{
		pages: map[string]pageResolve{
			testHost: {page: defaultPage(), ttl: 30},
		},
	}
}

func sprintf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}
