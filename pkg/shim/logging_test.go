// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/durupages/durupages/pkg/api"
)

// TestLoadFailureIsLoggedWithRequestID is the regression test for the failure
// this logging exists for: a page answers "502 load failed" and the operator,
// holding only the X-DuruPages-Request-Id from the response, must be able to
// find out why from the server log.
func TestLoadFailureIsLoggedWithRequestID(t *testing.T) {
	h, _ := newHarness(t, harnessOpts{}) // hub has no bundle registered -> 404

	const requestID = "c6eznoiyv8v8ds015w7b"
	resp, body := h.proxyRequest("page-1", "dep-1", requestID)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(body, "load failed") {
		t.Fatalf("body = %q, want the unchanged wire contract %q", body, "load failed")
	}

	line := h.opsLine(logMsgLoadFailed)
	if line == nil {
		t.Fatalf("no %q log line; got %v", logMsgLoadFailed, h.opsLines())
	}
	// The correlation the operator actually performs: grep the request id.
	if !strings.Contains(h.opsBuf.String(), requestID) {
		t.Fatalf("request id %q absent from ops log: %s", requestID, h.opsBuf.String())
	}
	if got := line["requestId"]; got != requestID {
		t.Errorf("requestId = %v, want %q", got, requestID)
	}
	if got := line["pageId"]; got != "page-1" {
		t.Errorf("pageId = %v, want page-1", got)
	}
	if got := line["deploymentId"]; got != "dep-1" {
		t.Errorf("deploymentId = %v, want dep-1", got)
	}
	if got := line["level"]; got != "ERROR" {
		t.Errorf("level = %v, want ERROR (5xx)", got)
	}
	errStr, _ := line["error"].(string)
	if !strings.Contains(errStr, "status 404") {
		t.Errorf("error = %q, want the hub status code", errStr)
	}
	// The hub URL is what tells an operator which hub was contacted at all.
	if !strings.Contains(errStr, h.hub.URL) {
		t.Errorf("error = %q, want the hub URL %q", errStr, h.hub.URL)
	}
	if got, _ := line["stage"].(string); got != "fetchBundle" {
		t.Errorf("stage = %q, want fetchBundle", got)
	}

	// The proxy's own 502 line must carry the same request id, so either message
	// is a valid entry point for the grep.
	proxyLine := h.opsLines()[len(h.opsLines())-1]
	if proxyLine["requestId"] != requestID {
		t.Errorf("proxy 502 line lacks requestId: %v", proxyLine)
	}
	if proxyLine["hubAddr"] != h.hub.URL {
		t.Errorf("proxy 502 line hubAddr = %v, want %q", proxyLine["hubAddr"], h.hub.URL)
	}
}

// TestLoadFailureStages checks that each stage of the load path is reported
// with a distinguishable cause: hub unreachable, hub rejection and a runtime
// that will not start all used to collapse into the same opaque "load failed".
func TestLoadFailureStages(t *testing.T) {
	t.Run("hubUnreachable", func(t *testing.T) {
		h, hm := newHarness(t, harnessOpts{})
		hm.set("acme", "page-1", "dep-1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "page-1", dep: "dep-1"}))
		h.hub.Close() // connection refused from here on

		resp, _ := h.proxyRequest("page-1", "dep-1", "req-unreachable")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		line := h.opsLine(logMsgLoadFailed)
		if line == nil {
			t.Fatalf("no load-failure line; got %v", h.opsLines())
		}
		if got, _ := line["stage"].(string); got != "fetchBundle" {
			t.Errorf("stage = %q, want fetchBundle", got)
		}
		if errStr, _ := line["error"].(string); !strings.Contains(errStr, h.hub.URL) {
			t.Errorf("error = %q, want the hub URL", errStr)
		}
		if line["requestId"] != "req-unreachable" {
			t.Errorf("requestId = %v", line["requestId"])
		}
	})

	t.Run("hubRejects", func(t *testing.T) {
		h, _ := newHarness(t, harnessOpts{})
		// hubMux answers 404 with the standard Go body for unknown bundles.
		resp, _ := h.proxyRequest("page-1", "dep-1", "req-401")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		errStr, _ := h.opsLine(logMsgLoadFailed)["error"].(string)
		if !strings.Contains(errStr, "status 404") {
			t.Errorf("error = %q, want the hub status", errStr)
		}
		// The hub's own explanation is quoted back, not swallowed.
		if !strings.Contains(strings.ToLower(errStr), "not found") {
			t.Errorf("error = %q, want the hub error body snippet", errStr)
		}
	})

	t.Run("runtimeLaunchFails", func(t *testing.T) {
		h, hm := newHarness(t, harnessOpts{})
		hm.set("acme", "page-1", "dep-1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "page-1", dep: "dep-1"}))
		h.rt.setLaunchErr(errors.New("workerd: exit status 1"))

		resp, _ := h.proxyRequest("page-1", "dep-1", "req-workerd")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		line := h.opsLine(logMsgLoadFailed)
		if line == nil {
			t.Fatalf("no load-failure line; got %v", h.opsLines())
		}
		if got, _ := line["stage"].(string); got != "swap" {
			t.Errorf("stage = %q, want swap", got)
		}
		if errStr, _ := line["error"].(string); !strings.Contains(errStr, "workerd: exit status 1") {
			t.Errorf("error = %q, want the runtime error", errStr)
		}
		if line["requestId"] != "req-workerd" {
			t.Errorf("requestId = %v", line["requestId"])
		}
	})
}

// TestLoadSuccessIsLogged covers the info line that makes a slow first request
// explainable (and proves a load happened at all).
func TestLoadSuccessIsLogged(t *testing.T) {
	h, hm := newHarness(t, harnessOpts{})
	tarBytes := buildBundleTar(t, bundleSpec{
		tenant: "acme", page: "page-1", dep: "dep-1",
		static: map[string]string{"/a.txt": "hello"},
	})
	hm.set("acme", "page-1", "dep-1", tarBytes)

	resp, _ := h.proxyRequest("page-1", "dep-1", "req-ok")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	line := h.opsLine(logMsgLoaded)
	if line == nil {
		t.Fatalf("no %q line; got %v", logMsgLoaded, h.opsLines())
	}
	if line["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", line["level"])
	}
	if line["pageId"] != "page-1" || line["deploymentId"] != "dep-1" {
		t.Errorf("identifiers = %v", line)
	}
	if line["requestId"] != "req-ok" {
		t.Errorf("requestId = %v, want req-ok", line["requestId"])
	}
	if n, ok := line["bundleBytes"].(float64); !ok || n <= 0 {
		t.Errorf("bundleBytes = %v, want a positive size", line["bundleBytes"])
	}
	if _, ok := line["elapsedMs"].(float64); !ok {
		t.Errorf("elapsedMs missing: %v", line)
	}
}

// TestRejectionLoggingLevels asserts the level split (4xx warn, 5xx error), the
// unchanged response bodies and that the pre-lease paths still correlate via
// the request-id header.
func TestRejectionLoggingLevels(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(req *http.Request)
		wantCode  int
		wantBody  string
		wantLevel string
		wantMsg   string
	}{
		{
			name:      "missingLease",
			mutate:    func(req *http.Request) { req.Header.Del(api.HeaderLease) },
			wantCode:  http.StatusUnauthorized,
			wantBody:  "missing lease",
			wantLevel: "WARN",
			wantMsg:   logMsgProxyRejected,
		},
		{
			name:      "invalidLease",
			mutate:    func(req *http.Request) { req.Header.Set(api.HeaderLease, "not-a-jwt") },
			wantCode:  http.StatusForbidden,
			wantBody:  "invalid lease",
			wantLevel: "WARN",
			wantMsg:   logMsgProxyRejected,
		},
		{
			name:      "missingDeployment",
			mutate:    func(req *http.Request) { req.Header.Del(api.HeaderDeployment) },
			wantCode:  http.StatusBadRequest,
			wantBody:  "missing deployment",
			wantLevel: "WARN",
			wantMsg:   logMsgProxyRejected,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHarness(t, harnessOpts{})
			const requestID = "req-reject"
			req, err := http.NewRequest(http.MethodGet, "http://"+h.shim.ProxyAddr()+"/api/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(api.HeaderLease, h.lease("page-1", "dep-1", requestID))
			req.Header.Set(api.HeaderPage, "page-1")
			req.Header.Set(api.HeaderDeployment, "dep-1")
			req.Header.Set(api.HeaderRequestID, requestID)
			tc.mutate(req)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 512)
			n, _ := resp.Body.Read(buf)
			resp.Body.Close()

			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if !strings.Contains(string(buf[:n]), tc.wantBody) {
				t.Fatalf("body = %q, want %q", string(buf[:n]), tc.wantBody)
			}
			line := h.opsLine(tc.wantMsg)
			if line == nil {
				t.Fatalf("no %q line; got %v", tc.wantMsg, h.opsLines())
			}
			if line["level"] != tc.wantLevel {
				t.Errorf("level = %v, want %s", line["level"], tc.wantLevel)
			}
			if line["status"] != float64(tc.wantCode) {
				t.Errorf("status = %v, want %d", line["status"], tc.wantCode)
			}
			if line["path"] != "/api/x" {
				t.Errorf("path = %v, want /api/x", line["path"])
			}
			if line["requestId"] != requestID {
				t.Errorf("requestId = %v, want %q", line["requestId"], requestID)
			}
		})
	}
}

// TestLeaseTokenNeverLogged guards the "no credentials in logs" rule: a
// rejected lease must be explained without echoing the bearer token.
func TestLeaseTokenNeverLogged(t *testing.T) {
	h, _ := newHarness(t, harnessOpts{})
	tok := h.lease("page-1", "dep-1", "req-secret")
	// Corrupt the signature so verification fails but the token stays realistic.
	bad := tok[:len(tok)-4] + "AAAA"

	resp, _ := h.proxyRequestWithLease("page-1", "dep-1", bad)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	out := h.opsBuf.String()
	if strings.Contains(out, bad) || strings.Contains(out, tok) {
		t.Fatalf("lease token leaked into the ops log: %s", out)
	}
	line := h.opsLine(logMsgProxyRejected)
	if line == nil {
		t.Fatalf("no rejection line; got %v", h.opsLines())
	}
	if errStr, _ := line["error"].(string); !strings.Contains(errStr, "verify lease") {
		t.Errorf("error = %q, want the verification reason", errStr)
	}
}

// TestAssetsAndCollectorErrorsLogged covers the other two servers' error paths.
func TestAssetsAndCollectorErrorsLogged(t *testing.T) {
	h, _ := newHarness(t, harnessOpts{})

	resp, err := http.Get("http://" + h.shim.AssetsAddr() + "/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("assets status = %d, want 400", resp.StatusCode)
	}
	if line := h.opsLine(logMsgAssetsFailed); line == nil {
		t.Fatalf("no assets failure line; got %v", h.opsLines())
	} else if line["level"] != "WARN" {
		t.Errorf("assets level = %v, want WARN", line["level"])
	}

	resp, err = http.Post("http://"+h.shim.TailAddr()+"/", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tail status = %d, want 400", resp.StatusCode)
	}
	line := h.opsLine(logMsgTailRejected)
	if line == nil {
		t.Fatalf("no tail rejection line; got %v", h.opsLines())
	}
	if errStr, _ := line["error"].(string); errStr == "" {
		t.Errorf("tail rejection has no error attribute: %v", line)
	}
}

// TestNilLoggerResolvesDefaultLate pins the Options.Logger contract shared with
// pkg/adminapi: a nil logger means "use slog.Default() at logging time", so a
// slog.SetDefault performed after New still takes effect.
func TestNilLoggerResolvesDefaultLate(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := &Shim{} // Logger unset.
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	s.logResponseErr(nil, http.StatusBadGateway, logMsgLoadFailed, slog.String("requestId", "late"))

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("no line on the late default logger: %v (%q)", err, buf.String())
	}
	if line["msg"] != logMsgLoadFailed || line["requestId"] != "late" || line["level"] != "ERROR" {
		t.Fatalf("unexpected line: %v", line)
	}
}

// The 502 a broken page returns is diagnosed by following one request id from
// the router's log, through the shim's, into the hub's. The shim is the hop
// that has to carry it across: it holds the id from the lease, and the hub only
// logs the id it is sent. Without this the chain breaks exactly where the real
// cause (hub 401/404) is recorded.
func TestRequestIDReachesTheHub(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))

	const requestID = "c6eznoiyv8v8ds015w7b"
	resp, _ := h.proxyRequest("blog", "dep1", requestID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	ids := hub.requestIDs()
	if len(ids) == 0 {
		t.Fatal("the hub was never asked for the bundle")
	}
	for i, got := range ids {
		if got != requestID {
			t.Fatalf("hub fetch %d carried request id %q, want %q "+
				"(the router->shim->hub correlation chain is broken)", i, got, requestID)
		}
	}
}
