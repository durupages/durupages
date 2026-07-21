// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/runtime"
	"github.com/durupages/durupages/pkg/workerauth"
)

// TestLazyLoad verifies the first request for a page downloads its bundle and
// launches an instance serving it.
func TestLazyLoad(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))

	resp, body := h.proxyRequest("blog", "dep1", "req-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "ok:blog" {
		t.Errorf("body = %q, want ok:blog", body)
	}
	if h.rt.launchCount() != 1 {
		t.Errorf("launchCount = %d, want 1", h.rt.launchCount())
	}
	spec := h.rt.lastSpec()
	if len(spec.Pages) != 1 || spec.Pages[0].PageID != "blog" {
		t.Errorf("spec pages = %+v, want [blog]", spec.Pages)
	}

	// Second request for the same deployment reuses the instance (no swap).
	h.proxyRequest("blog", "dep1", "req-2")
	if h.rt.launchCount() != 1 {
		t.Errorf("launchCount after reuse = %d, want 1", h.rt.launchCount())
	}
}

// TestLeaseVerifyMatrix covers the lease authentication cases.
func TestLeaseVerifyMatrix(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))

	// Valid lease.
	if resp, _ := h.proxyRequest("blog", "dep1", "r"); resp.StatusCode != http.StatusOK {
		t.Errorf("valid lease: status = %d, want 200", resp.StatusCode)
	}

	// Missing lease -> 401.
	if resp, _ := h.proxyRequestWithLease("blog", "dep1", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing lease: status = %d, want 401", resp.StatusCode)
	}

	// Lease signed by a different key -> 403.
	_, wrongPriv, _ := ed25519GenerateKey(t)
	badTok, _ := workerauth.IssueLease(wrongPriv, workerauth.LeaseClaims{
		LeaseID: "l", TenantID: "acme", PageID: "blog", DeploymentID: "dep1", RequestID: "r",
	}, time.Minute)
	if resp, _ := h.proxyRequestWithLease("blog", "dep1", badTok); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad-key lease: status = %d, want 403", resp.StatusCode)
	}

	// Tenant mismatch -> 403.
	otherTenant, _ := workerauth.IssueLease(h.priv, workerauth.LeaseClaims{
		LeaseID: "l", TenantID: "other", PageID: "blog", DeploymentID: "dep1", RequestID: "r",
	}, time.Minute)
	if resp, _ := h.proxyRequestWithLease("blog", "dep1", otherTenant); resp.StatusCode != http.StatusForbidden {
		t.Errorf("tenant mismatch: status = %d, want 403", resp.StatusCode)
	}

	// Page header mismatch (lease is for blog, header says shop) -> 403.
	blogLease := h.lease("blog", "dep1", "r")
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.shim.ProxyAddr()+"/x", nil)
	req.Header.Set(api.HeaderLease, blogLease)
	req.Header.Set(api.HeaderPage, "shop")
	req.Header.Set(api.HeaderDeployment, "dep1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("page mismatch: status = %d, want 403", resp.StatusCode)
	}
}

// TestSecondPageSwapAndDrain verifies loading a second page relaunches with a
// both-page spec and drains the previous instance.
func TestSecondPageSwapAndDrain(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))
	hub.set("acme", "shop", "depS", buildBundleTar(t, bundleSpec{tenant: "acme", page: "shop", dep: "depS"}))

	h.proxyRequest("blog", "dep1", "r1")
	inst0 := h.rt.instanceAt(0)

	h.proxyRequest("shop", "depS", "r2")
	if h.rt.launchCount() != 2 {
		t.Fatalf("launchCount = %d, want 2", h.rt.launchCount())
	}
	spec := h.rt.lastSpec()
	if len(spec.Pages) != 2 {
		t.Fatalf("second spec pages = %d, want 2 (both pages)", len(spec.Pages))
	}
	pages := map[string]bool{}
	for _, p := range spec.Pages {
		pages[p.PageID] = true
	}
	if !pages["blog"] || !pages["shop"] {
		t.Errorf("second spec missing a page: %+v", pages)
	}
	if !waitFor(t, time.Second, inst0.wasDrained) {
		t.Error("old instance was not drained after swap")
	}
}

// TestDeploymentChangeSwaps verifies a lease pointing at a new deployment of an
// already-loaded page triggers a swap.
func TestDeploymentChangeSwaps(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))
	hub.set("acme", "blog", "dep2", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep2"}))

	h.proxyRequest("blog", "dep1", "r1")
	h.proxyRequest("blog", "dep2", "r2")

	if h.rt.launchCount() != 2 {
		t.Fatalf("launchCount = %d, want 2", h.rt.launchCount())
	}
	h.shim.mu.Lock()
	active := h.shim.active["blog"]
	h.shim.mu.Unlock()
	if active != "dep2" {
		t.Errorf("active blog deployment = %q, want dep2", active)
	}
}

// TestUsagePodLog verifies pod-log usage emission with the expected fields.
func TestUsagePodLog(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))

	h.proxyRequest("blog", "dep1", "req-123")

	if !waitFor(t, time.Second, func() bool { return len(h.usageLines()) >= 1 }) {
		t.Fatal("no usage line emitted")
	}
	lines := h.usageLines()
	u := lines[0]
	if u.Type != "request_usage" {
		t.Errorf("type = %q", u.Type)
	}
	if u.RequestID != "req-123" || u.TenantID != "acme" || u.PageID != "blog" || u.DeploymentID != "dep1" {
		t.Errorf("usage fields = %+v", u.RequestUsage)
	}
	if u.WorkerPod != "pod-1" {
		t.Errorf("workerPod = %q, want pod-1", u.WorkerPod)
	}
	if u.Event.Request.Method != "GET" {
		t.Errorf("method = %q, want GET", u.Event.Request.Method)
	}
	if u.Event.Response.Status != 200 {
		t.Errorf("status = %d, want 200", u.Event.Response.Status)
	}
	if !strings.HasSuffix(u.Event.Request.URL, "/api/x") {
		t.Errorf("url = %q, want .../api/x", u.Event.Request.URL)
	}
}

// TestTraceCorrelation verifies logs/cpu from a tail POST are merged into usage.
func TestTraceCorrelation(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{
		runtimeHook: func(spec runtime.InstanceSpec, w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(api.HeaderRequestID)
			emitTrace(t, spec.TailEndpoint, rid, map[string]any{
				"scriptName": "blog",
				"outcome":    "ok",
				"cpuTime":    12,
				"logs":       []map[string]any{{"timestamp": 1000, "level": "log", "message": "hello from worker"}},
				"event": map[string]any{
					"request": map[string]any{
						"url":     "http://blog/api/x",
						"method":  "GET",
						"headers": map[string]any{"x-durupages-request-id": rid},
					},
					"response": map[string]any{"status": 200},
				},
			})
			w.WriteHeader(200)
			w.Write([]byte("ok"))
		},
	})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))

	h.proxyRequest("blog", "dep1", "corr-1")

	if !waitFor(t, 2*time.Second, func() bool {
		ls := h.usageLines()
		return len(ls) >= 1 && len(ls[0].Logs) >= 1
	}) {
		t.Fatal("trace not correlated into usage")
	}
	u := h.usageLines()[0]
	if u.CPUTime != 12*time.Millisecond {
		t.Errorf("cpuTime = %v, want 12ms", u.CPUTime)
	}
	if len(u.Logs) != 1 || u.Logs[0].Message != "hello from worker" {
		t.Errorf("logs = %+v", u.Logs)
	}
}

// TestRedaction verifies secret-value and header-name redaction in usage.
func TestRedaction(t *testing.T) {
	secretVal := "supersecretvalue"
	h, hub := newHarness(t, harnessOpts{
		runtimeHook: func(spec runtime.InstanceSpec, w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(api.HeaderRequestID)
			emitTrace(t, spec.TailEndpoint, rid, map[string]any{
				"scriptName": "blog",
				"logs":       []map[string]any{{"timestamp": 1000, "level": "error", "message": "leaked " + secretVal + " oops"}},
				"exceptions": []map[string]any{{"timestamp": 1000, "name": "Error", "message": "boom " + secretVal, "stack": "at x " + secretVal}},
				"event": map[string]any{
					"request":  map[string]any{"url": "http://blog/x", "method": "GET", "headers": map[string]any{"x-durupages-request-id": rid}},
					"response": map[string]any{"status": 200},
				},
			})
			w.WriteHeader(200)
			w.Write([]byte("ok"))
		},
	})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{
		tenant: "acme", page: "blog", dep: "dep1",
		secret: map[string]string{"TOKEN": secretVal},
	}))

	// Send a request carrying an Authorization header (redacted by name).
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.shim.ProxyAddr()+"/api/x", nil)
	req.Header.Set(api.HeaderLease, h.lease("blog", "dep1", "red-1"))
	req.Header.Set(api.HeaderPage, "blog")
	req.Header.Set(api.HeaderDeployment, "dep1")
	req.Header.Set("Authorization", "Bearer "+secretVal)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !waitFor(t, 2*time.Second, func() bool {
		ls := h.usageLines()
		return len(ls) >= 1 && len(ls[0].Logs) >= 1
	}) {
		t.Fatal("no usage with logs emitted")
	}
	u := h.usageLines()[0]
	if strings.Contains(mustJSON(t, u), secretVal) {
		t.Errorf("secret value leaked into usage:\n%s", mustJSON(t, u))
	}
	if u.Logs[0].Message != "leaked [REDACTED:TOKEN] oops" {
		t.Errorf("log not redacted: %q", u.Logs[0].Message)
	}
	if got := u.Event.Request.Headers["Authorization"]; got != "[REDACTED]" {
		t.Errorf("authorization header = %q, want [REDACTED]", got)
	}
	if len(u.Exceptions) != 1 || strings.Contains(u.Exceptions[0].Stack, secretVal) {
		t.Errorf("exception stack not redacted: %+v", u.Exceptions)
	}
}

// TestHeartbeatDrainHonored verifies a controller drain instruction stops the
// proxy from accepting new requests.
func TestHeartbeatDrainHonored(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{sendDrain: true})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))

	if !waitFor(t, 2*time.Second, h.shim.isDraining) {
		t.Fatal("shim did not enter draining after heartbeat drain instruction")
	}
	resp, _ := h.proxyRequest("blog", "dep1", "r")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status while draining = %d, want 503", resp.StatusCode)
	}
}

// TestRegisterAndHeartbeat verifies registration and heartbeat reporting.
func TestRegisterAndHeartbeat(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = old })

	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))
	h.proxyRequest("blog", "dep1", "r")

	if !waitFor(t, 2*time.Second, func() bool {
		hb := h.worker.lastHeartbeat()
		if hb == nil {
			return false
		}
		for _, p := range hb.LoadedPages {
			if p.PageId == "blog" && p.DeploymentId == "dep1" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("heartbeat did not report loaded page blog/dep1")
	}
	h.worker.mu.Lock()
	reg := h.worker.registered
	h.worker.mu.Unlock()
	if reg == nil || reg.TenantId != "acme" || reg.PodName != "pod-1" {
		t.Errorf("register = %+v", reg)
	}
}

// TestPageConfigFromController verifies that Env/Secret bindings fetched from
// the controller's GetPageConfig RPC override bundle-provided defaults and
// reach the runtime instance spec, and that secret values feed redaction.
func TestPageConfigFromController(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{
		tenant: "acme", page: "blog", dep: "dep1",
		env: map[string]string{"MODE": "file", "KEEP": "yes"},
	}))
	h.worker.setPageConfig("blog", &api.GetPageConfigResponse{
		Env:    map[string]string{"MODE": "rpc"},
		Secret: map[string]string{"API_KEY": "supersecret1"},
	})

	if resp, _ := h.proxyRequest("blog", "dep1", "req-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	spec := h.rt.lastSpec()
	if len(spec.Pages) != 1 {
		t.Fatalf("spec pages = %d, want 1", len(spec.Pages))
	}
	pw := spec.Pages[0]
	if pw.Env["MODE"] != "rpc" {
		t.Errorf("Env[MODE] = %q, want rpc (RPC overrides bundle file)", pw.Env["MODE"])
	}
	if pw.Secret["API_KEY"] != "supersecret1" {
		t.Errorf("Secret[API_KEY] = %q, want supersecret1", pw.Secret["API_KEY"])
	}
}
