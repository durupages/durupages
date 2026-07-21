// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	log   *strings.Builder
}

func newHarness(t *testing.T, ctrl *fakeController, m *manifest.Manifest) *harness {
	t.Helper()
	mem := memstorage.New()
	store := newCountingStorage(mem)
	if m != nil {
		seedManifest(t, store, m)
	}
	conn := startGRPC(t, func(s *grpc.Server) {
		api.RegisterRouterServiceServer(s, ctrl)
	})
	cache, err := staticcache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var logbuf strings.Builder
	rt, err := New(Options{
		Resolver:        api.NewRouterServiceClient(conn),
		Storage:         store,
		Cache:           cache,
		ResolveCacheTTL: 10 * time.Second,
		LogWriter:       &logbuf,
		Now:             func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	return &harness{rt: rt, ctrl: ctrl, store: store, log: &logbuf}
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
