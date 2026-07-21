// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/assets"
	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/router/staticcache"
	"github.com/durupages/durupages/pkg/storage/memstorage"
	"github.com/durupages/durupages/pkg/usage"
	"google.golang.org/grpc"
)

func req(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}

func TestUnknownHost404(t *testing.T) {
	ctrl := &fakeController{pages: map[string]pageResolve{}}
	h := newHarness(t, ctrl, nil)
	rec := h.do(req("GET", "http://nope.example.com/"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestResolveCaching(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/index.html", "<h1>hi</h1>", "text/html")

	for i := 0; i < 3; i++ {
		rec := h.do(req("GET", "http://blog.example.com/"))
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d status = %d", i, rec.Code)
		}
	}
	if got := ctrl.resolveCallCount(); got != 1 {
		t.Fatalf("resolve calls = %d, want 1 (cached)", got)
	}
}

func TestStaticServeAndCacheHit(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/index.html", "<h1>hi</h1>", "text/html")

	rec := h.do(req("GET", "http://blog.example.com/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "<h1>hi</h1>" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html" {
		t.Fatalf("content-type = %q", ct)
	}
	wantETag := assets.ETag(m.Static["/index.html"])
	if rec.Header().Get("ETag") != wantETag {
		t.Fatalf("etag = %q, want %q", rec.Header().Get("ETag"), wantETag)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing default header X-Content-Type-Options")
	}

	// Second request must be served from the on-disk cache: no extra static Get.
	_ = h.do(req("GET", "http://blog.example.com/"))
	if got := h.store.staticGets(); got != 1 {
		t.Fatalf("static storage gets = %d, want 1 (cache hit on 2nd)", got)
	}
}

func Test304NotModified(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/index.html", "hello", "text/html")

	r := req("GET", "http://blog.example.com/")
	r.Header.Set("If-None-Match", assets.ETag(m.Static["/index.html"]))
	rec := h.do(r)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 body = %q, want empty", rec.Body.String())
	}
}

func TestPrettyURLRedirect(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/contact.html", "contact", "text/html")

	rec := h.do(req("GET", "http://blog.example.com/contact.html?ref=x"))
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/contact?ref=x" {
		t.Fatalf("location = %q, want /contact?ref=x", loc)
	}
}

func TestRedirectsRuleWithQuery(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	m.Redirects = []manifest.Redirect{{Source: "/old", Destination: "/new", Status: 301}}
	h := newHarness(t, ctrl, m)

	rec := h.do(req("GET", "http://blog.example.com/old?a=1&b=2"))
	if rec.Code != 301 {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/new?a=1&b=2" {
		t.Fatalf("location = %q, want /new?a=1&b=2", loc)
	}
}

func Test200Rewrite(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	m.Redirects = []manifest.Redirect{{Source: "/proxied", Destination: "/target.txt", Status: 200}}
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/target.txt", "TARGET", "text/plain")

	rec := h.do(req("GET", "http://blog.example.com/proxied"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "TARGET" {
		t.Fatalf("body = %q, want TARGET (rewritten asset)", rec.Body.String())
	}
}

func TestHeaderSetAndUnsetDefault(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	m.Headers = []manifest.HeaderRule{{
		Pattern: "/*",
		Set:     map[string]string{"X-Custom": "yes", "Cache-Control": "max-age=31536000"},
		Unset:   []string{"X-Content-Type-Options"},
	}}
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/index.html", "hi", "text/html")

	rec := h.do(req("GET", "http://blog.example.com/"))
	if rec.Header().Get("X-Custom") != "yes" {
		t.Fatalf("X-Custom = %q", rec.Header().Get("X-Custom"))
	}
	// set replaces the default Cache-Control (single value, not appended).
	if cc := rec.Header().Values("Cache-Control"); len(cc) != 1 || cc[0] != "max-age=31536000" {
		t.Fatalf("Cache-Control = %v, want [max-age=31536000]", cc)
	}
	// `!` unset removed a default header.
	if _, ok := rec.Header()["X-Content-Type-Options"]; ok {
		t.Fatal("X-Content-Type-Options should have been unset")
	}
}

func TestSPAFallback(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	// No 404.html anywhere -> SPA: unmatched paths serve /index.html with 200.
	seedStatic(t, h, m, "/index.html", "SPA-ROOT", "text/html")

	rec := h.do(req("GET", "http://blog.example.com/deep/link"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", rec.Code)
	}
	if rec.Body.String() != "SPA-ROOT" {
		t.Fatalf("body = %q, want SPA-ROOT", rec.Body.String())
	}
}

func TestCustom404(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/404.html", "NOT-FOUND-PAGE", "text/html")

	rec := h.do(req("GET", "http://blog.example.com/missing"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec.Body.String() != "NOT-FOUND-PAGE" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMethodNotAllowedOnStatic(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/index.html", "hi", "text/html")

	rec := h.do(req("POST", "http://blog.example.com/"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestWorkerRouteProxied(t *testing.T) {
	// Fake worker backend records the internal headers it receives.
	var mu sync.Mutex
	var gotHeaders http.Header
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeaders = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("from-worker"))
	}))
	t.Cleanup(worker.Close)

	lease := &api.Lease{
		LeaseId:      "lease-42",
		Endpoint:     worker.URL,
		PageId:       testPage,
		DeploymentId: testDep,
		Signature:    "sig-good",
		RequestId:    "req-99",
	}
	ctrl := ctrlWithPage()
	ctrl.acquire = []*api.AcquireSlotEvent{grantedEvent(lease)}

	m := newManifest()
	m.HasWorker = true // Routes nil => every path routes to the worker.
	h := newHarness(t, ctrl, m)

	r := req("GET", "http://blog.example.com/api/x")
	r.Header.Set(api.HeaderLease, "evil-injected") // must be stripped
	rec := h.do(r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "from-worker" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get(api.HeaderRequestID) != "req-99" {
		t.Fatalf("echoed request id = %q, want req-99", rec.Header().Get(api.HeaderRequestID))
	}

	mu.Lock()
	hdr := gotHeaders
	mu.Unlock()
	if hdr.Get(api.HeaderPage) != testPage {
		t.Fatalf("worker X-DuruPages-Page = %q", hdr.Get(api.HeaderPage))
	}
	if hdr.Get(api.HeaderDeployment) != testDep {
		t.Fatalf("worker X-DuruPages-Deployment = %q", hdr.Get(api.HeaderDeployment))
	}
	if hdr.Get(api.HeaderRequestID) != "req-99" {
		t.Fatalf("worker X-DuruPages-Request-Id = %q", hdr.Get(api.HeaderRequestID))
	}
	// The inbound spoofed lease was stripped and replaced by the real one.
	if hdr.Get(api.HeaderLease) != "sig-good" {
		t.Fatalf("worker X-DuruPages-Lease = %q, want sig-good", hdr.Get(api.HeaderLease))
	}

	if rel := ctrl.releasedLeases(); len(rel) != 1 || rel[0] != "lease-42" {
		t.Fatalf("released leases = %v, want [lease-42]", rel)
	}
}

func TestQueueTimeout429(t *testing.T) {
	ctrl := ctrlWithPage()
	ctrl.acquire = []*api.AcquireSlotEvent{queuedEvent(3), timeoutEvent()}

	m := newManifest()
	m.HasWorker = true
	h := newHarness(t, ctrl, m)

	rec := h.do(req("GET", "http://blog.example.com/api/x"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
	}
}

func TestPodLogStaticAccess(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/index.html", "hello", "text/html")

	r := req("GET", "http://blog.example.com/")
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Cookie", "sid=1")
	r.Header.Set("User-Agent", "test-agent")
	rec := h.do(r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	line := strings.TrimSpace(h.log.String())
	if line == "" {
		t.Fatal("no pod-log line written")
	}
	var got struct {
		Type string `json:"type"`
		usage.StaticAccess
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal log line %q: %v", line, err)
	}
	if got.Type != "static_access" {
		t.Fatalf("type = %q, want static_access", got.Type)
	}
	if got.TenantID != testTenant || got.PageID != testPage || got.DeploymentID != testDep {
		t.Fatalf("attribution = %+v", got.StaticAccess)
	}
	if got.Event.Response.Status != http.StatusOK {
		t.Fatalf("status field = %d", got.Event.Response.Status)
	}
	if got.BytesSent != int64(len("hello")) {
		t.Fatalf("bytesSent = %d, want 5", got.BytesSent)
	}
	if _, ok := got.Event.Request.Headers["authorization"]; ok {
		t.Fatal("authorization must not be recorded")
	}
	if _, ok := got.Event.Request.Headers["cookie"]; ok {
		t.Fatal("cookie must not be recorded")
	}
	if got.Event.Request.Headers["user-agent"] != "test-agent" {
		t.Fatalf("user-agent = %q", got.Event.Request.Headers["user-agent"])
	}
	if got.Event.Request.Headers["host"] != testHost {
		t.Fatalf("host = %q", got.Event.Request.Headers["host"])
	}
}

func TestLogServiceMode(t *testing.T) {
	logsvc := &fakeLogService{received: make(chan struct{}, 4)}
	ctrl := ctrlWithPage()
	m := newManifest()

	mem := newCountingStorage(memstorage.New())
	seedStaticInto(t, mem, m, "/index.html", "hello", "text/html")
	seedManifest(t, mem, m)

	conn := startGRPC(t, func(s *grpc.Server) {
		api.RegisterRouterServiceServer(s, ctrl)
		api.RegisterLogServiceServer(s, logsvc)
	})
	cache, err := staticcache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		Resolver:  api.NewRouterServiceClient(conn),
		Storage:   mem,
		Cache:     cache,
		LogClient: api.NewLogServiceClient(conn),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req("GET", "http://blog.example.com/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	select {
	case <-logsvc.received:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for LogService ingest")
	}
	if logsvc.count() == 0 {
		t.Fatal("LogService received no StaticAccess events")
	}
}

// --- helpers -----------------------------------------------------------------

func grantedEvent(l *api.Lease) *api.AcquireSlotEvent {
	return &api.AcquireSlotEvent{Event: &api.AcquireSlotEvent_Granted{Granted: l}}
}

func queuedEvent(pos int64) *api.AcquireSlotEvent {
	return &api.AcquireSlotEvent{Event: &api.AcquireSlotEvent_Queued_{Queued: &api.AcquireSlotEvent_Queued{Position: pos}}}
}

func timeoutEvent() *api.AcquireSlotEvent {
	return &api.AcquireSlotEvent{Event: &api.AcquireSlotEvent_Timeout_{Timeout: &api.AcquireSlotEvent_Timeout{}}}
}

// seedStatic stores a static file both in the manifest map and the harness
// storage. The manifest was already seeded at harness construction, so files
// added here are for content the test resolves at request time.
func seedStatic(t *testing.T, h *harness, m *manifest.Manifest, path, content, contentType string) {
	t.Helper()
	seedStaticInto(t, h.store, m, path, content, contentType)
	// Re-seed the manifest so the on-storage copy includes the new file.
	seedManifest(t, h.store, m)
}

func seedStaticInto(t *testing.T, store *countingStorage, m *manifest.Manifest, path, content, contentType string) {
	t.Helper()
	addFile(t, store, m, path, content, contentType)
}
