// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/manifest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestUpstream5xxLogged is the regression this package was missing: a worker
// that answers 502 ("load failed") used to reach the client with nothing at all
// on the server side. The router must now record the upstream status, the
// worker endpoint and the lease request ID — the same ID the client got back in
// X-DuruPages-Request-Id and the shim logged on its own side.
func TestUpstream5xxLogged(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("load failed"))
	}))
	t.Cleanup(worker.Close)

	lease := &api.Lease{
		LeaseId:      "lease-7",
		Endpoint:     worker.URL,
		PageId:       testPage,
		DeploymentId: testDep,
		Signature:    "sig-secret",
		RequestId:    "c6eznoiyv8v8ds015w7b",
	}
	ctrl := ctrlWithPage()
	ctrl.acquire = []*api.AcquireSlotEvent{grantedEvent(lease)}
	m := newManifest()
	m.HasWorker = true
	h := newHarness(t, ctrl, m)

	rec := h.do(req("GET", "http://blog.example.com/api/x?token=shh"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := rec.Header().Get(api.HeaderRequestID); got != lease.RequestId {
		t.Fatalf("echoed request id = %q, want %q", got, lease.RequestId)
	}

	e := h.oplog.find(t, msgUpstream5xx)
	if e.Level != slog.LevelError.String() {
		t.Fatalf("level = %q, want ERROR", e.Level)
	}
	if e.RequestID != lease.RequestId {
		t.Fatalf("requestId = %q, want %q (the id the client was given)", e.RequestID, lease.RequestId)
	}
	if e.UpstreamStatus != http.StatusBadGateway || e.Status != http.StatusBadGateway {
		t.Fatalf("status/upstreamStatus = %d/%d, want 502/502", e.Status, e.UpstreamStatus)
	}
	if e.Endpoint != worker.URL {
		t.Fatalf("endpoint = %q, want %q", e.Endpoint, worker.URL)
	}
	if e.Host != testHost || e.Path != "/api/x" || e.Method != "GET" {
		t.Fatalf("request context = %+v", e)
	}
	if e.PageID != testPage || e.TenantID != testTenant || e.DeploymentID != testDep {
		t.Fatalf("attribution = %+v", e)
	}
	if e.LeaseID != "lease-7" {
		t.Fatalf("leaseId = %q", e.LeaseID)
	}
	// The lease signature is a bearer credential and the query may carry a
	// token: neither may appear anywhere in the log.
	if dump := h.oplog.dump(t); strings.Contains(dump, "sig-secret") || strings.Contains(dump, "token=shh") {
		t.Fatalf("sensitive value leaked into the operational log:\n%s", dump)
	}
}

// TestProxyFailureLogged covers an unreachable worker (connection refused): the
// router itself fails to proxy, which must be distinguishable from the worker
// answering 5xx above.
func TestProxyFailureLogged(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := worker.URL
	worker.Close() // nothing is listening on dead anymore

	lease := &api.Lease{LeaseId: "lease-8", Endpoint: dead, PageId: testPage, DeploymentId: testDep, RequestId: "req-dead"}
	ctrl := ctrlWithPage()
	ctrl.acquire = []*api.AcquireSlotEvent{grantedEvent(lease)}
	m := newManifest()
	m.HasWorker = true
	h := newHarness(t, ctrl, m)

	rec := h.do(req("GET", "http://blog.example.com/api/x"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	e := h.oplog.find(t, msgProxyFailed)
	if e.Level != slog.LevelError.String() {
		t.Fatalf("level = %q, want ERROR", e.Level)
	}
	if e.RequestID != "req-dead" || e.Endpoint != dead {
		t.Fatalf("requestId/endpoint = %q/%q", e.RequestID, e.Endpoint)
	}
	if e.Error == "" {
		t.Fatal("proxy failure logged without the underlying error")
	}
	if h.oplog.has(t, msgUpstream5xx) {
		t.Fatal("a transport failure must not be reported as a worker 5xx")
	}
}

// TestBadEndpointLogged covers a lease whose endpoint does not parse (the
// scheme-less "host:port" case durupages-router works around).
func TestBadEndpointLogged(t *testing.T) {
	lease := &api.Lease{LeaseId: "lease-9", Endpoint: "://nope", PageId: testPage, RequestId: "req-bad"}
	ctrl := ctrlWithPage()
	ctrl.acquire = []*api.AcquireSlotEvent{grantedEvent(lease)}
	m := newManifest()
	m.HasWorker = true
	h := newHarness(t, ctrl, m)

	rec := h.do(req("GET", "http://blog.example.com/api/x"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	e := h.oplog.find(t, msgBadEndpoint)
	if e.Level != slog.LevelError.String() || e.Endpoint != "://nope" || e.RequestID != "req-bad" {
		t.Fatalf("entry = %+v", e)
	}
}

// TestAcquireSlotFailureLogged covers the controller refusing or dropping the
// slot request: the cause (a gRPC status) must reach the log.
func TestAcquireSlotFailureLogged(t *testing.T) {
	ctrl := &failingAcquireController{fakeController: ctrlWithPage()}
	m := newManifest()
	m.HasWorker = true
	h := newHarnessWith(t, ctrl, ctrl.fakeController, m)

	rec := h.do(req("GET", "http://blog.example.com/api/x"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	e := h.oplog.find(t, msgAcquireStream)
	if e.Level != slog.LevelError.String() {
		t.Fatalf("level = %q, want ERROR", e.Level)
	}
	if e.GRPCCode != codes.ResourceExhausted.String() {
		t.Fatalf("grpcCode = %q, want ResourceExhausted", e.GRPCCode)
	}
	if e.Status != http.StatusBadGateway || e.PageID != testPage || e.TenantID != testTenant {
		t.Fatalf("entry = %+v", e)
	}
	if e.Error == "" {
		t.Fatal("acquire failure logged without the underlying error")
	}
}

// TestQueueTimeoutLogged: 429 is capacity pressure, logged at warn.
func TestQueueTimeoutLogged(t *testing.T) {
	ctrl := ctrlWithPage()
	ctrl.acquire = []*api.AcquireSlotEvent{queuedEvent(3), timeoutEvent()}
	m := newManifest()
	m.HasWorker = true
	h := newHarness(t, ctrl, m)

	rec := h.do(req("GET", "http://blog.example.com/api/x"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	e := h.oplog.find(t, msgQueueTimeout)
	if e.Level != slog.LevelWarn.String() || e.Status != http.StatusTooManyRequests {
		t.Fatalf("entry = %+v", e)
	}
}

// TestUnknownHostLogged: a host the controller does not know is a warn, with
// the host that failed to resolve.
func TestUnknownHostLogged(t *testing.T) {
	ctrl := &fakeController{pages: map[string]pageResolve{}}
	h := newHarness(t, ctrl, nil)

	rec := h.do(req("GET", "http://nope.example.com/x"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	e := h.oplog.find(t, msgUnknownHost)
	if e.Level != slog.LevelWarn.String() {
		t.Fatalf("level = %q, want WARN", e.Level)
	}
	if e.Host != "nope.example.com" || e.Path != "/x" || e.Status != http.StatusNotFound {
		t.Fatalf("entry = %+v", e)
	}
}

// TestManifestMissingLogged: the page resolves but its active deployment's
// manifest is not in storage. The client sees a bare 404; the log has to name
// the deployment and the storage key.
func TestManifestMissingLogged(t *testing.T) {
	ctrl := ctrlWithPage()
	h := newHarness(t, ctrl, nil) // nil manifest => nothing seeded in storage

	rec := h.do(req("GET", "http://blog.example.com/"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	e := h.oplog.find(t, msgManifestMissing)
	if e.DeploymentID != testDep || e.PageID != testPage || e.TenantID != testTenant {
		t.Fatalf("attribution = %+v", e)
	}
	if !strings.Contains(e.Error, testDep) {
		t.Fatalf("error = %q, want it to name the missing manifest object", e.Error)
	}
}

// TestStaticFetchFailureLogged: the manifest promises an asset that storage
// cannot produce.
func TestStaticFetchFailureLogged(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	// Register the file in the manifest only: its bytes never reach storage.
	m.Static["/index.html"] = manifest.StaticEntry{Hash: hashOf("ghost"), Size: 5, ContentType: "text/html"}
	seedManifest(t, h.store, m)

	rec := h.do(req("GET", "http://blog.example.com/"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	e := h.oplog.find(t, msgStaticFetch)
	if e.Level != slog.LevelError.String() {
		t.Fatalf("level = %q, want ERROR", e.Level)
	}
	if e.AssetHash != hashOf("ghost") || e.Error == "" {
		t.Fatalf("entry = %+v", e)
	}
}

// TestStaticNotFoundLoggedAtInfo: a plain missing asset is ordinary traffic and
// must not be a warn, but must still be visible at the default level.
func TestStaticNotFoundLoggedAtInfo(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/other.txt", "x", "text/plain")

	rec := h.do(req("GET", "http://blog.example.com/missing.txt"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	e := h.oplog.find(t, msgStaticNotFound)
	if e.Level != slog.LevelInfo.String() {
		t.Fatalf("level = %q, want INFO", e.Level)
	}
}

// TestAccessLogAtDebug: one line per request at debug level, carrying the
// pipeline branch taken and the response size.
func TestAccessLogAtDebug(t *testing.T) {
	ctrl := ctrlWithPage()
	m := newManifest()
	h := newHarness(t, ctrl, m)
	seedStatic(t, h, m, "/index.html", "hello", "text/html")

	rec := h.do(req("GET", "http://blog.example.com/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	e := h.oplog.find(t, accessLogMessage)
	if e.Level != slog.LevelDebug.String() {
		t.Fatalf("level = %q, want DEBUG", e.Level)
	}
	if e.Route != routeStatic || e.Status != http.StatusOK || e.Bytes != int64(len("hello")) {
		t.Fatalf("entry = %+v", e)
	}
	if e.Method != "GET" || e.Host != testHost || e.Path != "/" {
		t.Fatalf("entry = %+v", e)
	}
	if !strings.Contains(e.Raw, "durationMs") {
		t.Fatalf("access line has no durationMs: %s", e.Raw)
	}
}

// TestAccessLogSuppressedAtInfo: at the default level the access line is gone
// while the failure line remains, which is the whole point of putting the
// access log at debug.
func TestAccessLogSuppressedAtInfo(t *testing.T) {
	ctrl := &fakeController{pages: map[string]pageResolve{}}
	h := newHarness(t, ctrl, nil)
	quiet := newLogCapture()
	quiet.logger = slog.New(slog.NewJSONHandler(&syncWriter{c: quiet}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h.rt.logger = quiet.logger

	if rec := h.do(req("GET", "http://nope.example.com/")); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if quiet.has(t, accessLogMessage) {
		t.Fatal("access line must not be emitted at info level")
	}
	if !quiet.has(t, msgUnknownHost) {
		t.Fatal("the failure cause must still be emitted at info level")
	}
}

// TestNilLoggerUsesDefaultLate verifies the pkg/adminapi convention: a Router
// built with no Logger resolves slog.Default() when it logs, so a
// slog.SetDefault performed after New is honoured.
func TestNilLoggerUsesDefaultLate(t *testing.T) {
	ctrl := &fakeController{pages: map[string]pageResolve{}}
	h := newHarness(t, ctrl, nil)
	h.rt.logger = nil // as if Options.Logger had been left unset

	capture := newLogCapture()
	prev := slog.Default()
	slog.SetDefault(capture.logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	if rec := h.do(req("GET", "http://nope.example.com/")); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !capture.has(t, msgUnknownHost) {
		t.Fatalf("nil Logger did not fall through to the current slog.Default():\n%s", capture.dump(t))
	}
}

// --- helpers -----------------------------------------------------------------

// failingAcquireController grants no slot: AcquireSlot ends the stream with a
// gRPC error, as an overloaded or shutting-down controller would. Host
// resolution still works (promoted from the embedded fake), so the request
// reaches the dynamic route before failing.
type failingAcquireController struct {
	*fakeController
}

func (f *failingAcquireController) AcquireSlot(_ *api.AcquireSlotRequest, _ grpc.ServerStreamingServer[api.AcquireSlotEvent]) error {
	return status.Error(codes.ResourceExhausted, "no capacity")
}
