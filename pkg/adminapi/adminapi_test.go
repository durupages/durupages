// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// capturedLog is one request log line, decoded from the JSON slog handler the
// logging tests install.
type capturedLog struct {
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	Bytes      int64  `json:"bytes"`
	DurationMs int64  `json:"durationMs"`
	RemoteAddr string `json:"remoteAddr"`
	Error      string `json:"error"`
}

// logCapture is a slog logger writing JSON lines into a buffer.
type logCapture struct {
	buf    bytes.Buffer
	logger *slog.Logger
}

func newLogCapture() *logCapture {
	c := &logCapture{}
	c.logger = slog.New(slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return c
}

// lines decodes every captured log line.
func (c *logCapture) lines(t *testing.T) []capturedLog {
	t.Helper()
	var out []capturedLog
	for _, raw := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var e capturedLog
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		out = append(out, e)
	}
	return out
}

// only returns the single captured line, failing when there is not exactly one.
func (c *logCapture) only(t *testing.T) capturedLog {
	t.Helper()
	lines := c.lines(t)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %q", len(lines), c.buf.String())
	}
	return lines[0]
}

// fixedNow is the clock injected into every test server.
var fixedNow = time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

// newTestServer builds a Server, failing the test on a configuration error.
// Unless a test wires its own logger, requests are logged to io.Discard: the
// package default is slog.Default(), which would otherwise spray the test
// output with one line per request.
func newTestServer(t *testing.T, o Options) *Server {
	t.Helper()
	if o.Now == nil {
		o.Now = func() time.Time { return fixedNow }
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// do performs a request against the handler and returns the recorder.
func do(t *testing.T, h http.Handler, method, target string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// doJSON performs a request whose body is the JSON encoding of v (raw strings
// and byte slices are sent verbatim).
func doJSON(t *testing.T, h http.Handler, method, target string, v any) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	switch x := v.(type) {
	case nil:
	case string:
		body = strings.NewReader(x)
	case []byte:
		body = bytes.NewReader(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		body = bytes.NewReader(b)
	}
	return do(t, h, method, target, body, "application/json")
}

// requireStatus fails the test unless the recorded status matches.
func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, want, rec.Body.String())
	}
}

// decodeBody decodes the recorded JSON body into v.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

// errorCode returns the code of the recorded error envelope.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	decodeBody(t, rec, &env)
	if env.Error.Code == "" {
		t.Fatalf("body %q carries no error code", rec.Body.String())
	}
	return env.Error.Code
}

func TestNewRequiresProvider(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without a Provider must fail")
	}
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, Options{Provider: newFakeProvider()})
	rec := do(t, s, http.MethodGet, "/healthz", nil, "")
	requireStatus(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}
}

func TestTenantRoutes(t *testing.T) {
	f := newFakeProvider()
	s := newTestServer(t, Options{Provider: f, Admin: f})

	// Create.
	rec := doJSON(t, s, http.MethodPost, "/v1/tenants", map[string]any{
		"id": "acme",
		"config": map[string]any{
			"maxConcurrency": 4,
			"idleTTL":        "30s",
			"workerCPULimit": "1",
			"podLabels":      map[string]string{"team": "web"},
		},
	})
	requireStatus(t, rec, http.StatusOK)

	// Durations must round-trip as human-readable strings, not nanoseconds.
	if !strings.Contains(rec.Body.String(), `"idleTTL":"30s"`) {
		t.Fatalf("response %q does not carry idleTTL as a duration string", rec.Body.String())
	}
	stored, err := f.GetTenant(t.Context(), "acme")
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if stored.Config.IdleTTL != 30*time.Second || stored.Config.MaxConcurrency != 4 {
		t.Fatalf("stored tenant = %+v", stored.Config)
	}

	// Upsert: a second POST updates in place.
	rec = doJSON(t, s, http.MethodPost, "/v1/tenants", map[string]any{
		"id":     "acme",
		"config": map[string]any{"maxConcurrency": 9, "idleTTL": "1h"},
	})
	requireStatus(t, rec, http.StatusOK)
	stored, _ = f.GetTenant(t.Context(), "acme")
	if stored.Config.MaxConcurrency != 9 || stored.Config.IdleTTL != time.Hour {
		t.Fatalf("upsert did not update: %+v", stored.Config)
	}

	// Get.
	rec = do(t, s, http.MethodGet, "/v1/tenants/acme", nil, "")
	requireStatus(t, rec, http.StatusOK)
	var got tenantJSON
	decodeBody(t, rec, &got)
	if got.ID != "acme" || time.Duration(got.Config.IdleTTL) != time.Hour {
		t.Fatalf("get tenant = %+v", got)
	}

	// List.
	f.seedTenant("beta")
	rec = do(t, s, http.MethodGet, "/v1/tenants", nil, "")
	requireStatus(t, rec, http.StatusOK)
	var list tenantsResponse
	decodeBody(t, rec, &list)
	if len(list.Tenants) != 2 || list.Tenants[0].ID != "acme" || list.Tenants[1].ID != "beta" {
		t.Fatalf("list tenants = %+v", list.Tenants)
	}

	// Delete, then 404.
	rec = do(t, s, http.MethodDelete, "/v1/tenants/acme", nil, "")
	requireStatus(t, rec, http.StatusNoContent)
	rec = do(t, s, http.MethodGet, "/v1/tenants/acme", nil, "")
	requireStatus(t, rec, http.StatusNotFound)
	if code := errorCode(t, rec); code != codeNotFound {
		t.Fatalf("code = %q", code)
	}
}

func TestTenantPagesRoutes(t *testing.T) {
	f := newFakeProvider()
	f.seedTenant("acme")
	f.seedTenant("beta")
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme"})
	f.seedPage(provider.Page{ID: "shop", TenantID: "beta"})
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := do(t, s, http.MethodGet, "/v1/tenants/acme/pages", nil, "")
	requireStatus(t, rec, http.StatusOK)
	var list pagesResponse
	decodeBody(t, rec, &list)
	if len(list.Pages) != 1 || list.Pages[0].ID != "blog" {
		t.Fatalf("tenant pages = %+v", list.Pages)
	}

	rec = do(t, s, http.MethodGet, "/v1/pages", nil, "")
	requireStatus(t, rec, http.StatusOK)
	decodeBody(t, rec, &list)
	if len(list.Pages) != 2 {
		t.Fatalf("all pages = %+v", list.Pages)
	}

	rec = do(t, s, http.MethodGet, "/v1/tenants/nope/pages", nil, "")
	requireStatus(t, rec, http.StatusNotFound)
}

func TestPageRoutesAndSecrets(t *testing.T) {
	f := newFakeProvider()
	f.seedTenant("acme")
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPost, "/v1/pages", map[string]any{
		"id":       "blog",
		"tenantId": "acme",
		"config": map[string]any{
			"requestTimeout": "45s",
			"env":            map[string]string{"STAGE": "prod"},
			"secret":         map[string]string{"API_KEY": "s3cr3t", "DB_PASS": "hunter2"},
		},
	})
	requireStatus(t, rec, http.StatusOK)

	// Secret values must never be echoed; only their key list.
	body := rec.Body.String()
	for _, v := range []string{"s3cr3t", "hunter2", `"secret"`} {
		if strings.Contains(body, v) {
			t.Fatalf("response leaks %q: %s", v, body)
		}
	}
	var page pageJSON
	decodeBody(t, rec, &page)
	if len(page.Config.SecretKeys) != 2 ||
		page.Config.SecretKeys[0] != "API_KEY" || page.Config.SecretKeys[1] != "DB_PASS" {
		t.Fatalf("secretKeys = %v", page.Config.SecretKeys)
	}
	stored, _ := f.page("blog")
	if stored.Config.Secret["API_KEY"] != "s3cr3t" || stored.Config.RequestTimeout != 45*time.Second {
		t.Fatalf("stored page = %+v", stored.Config)
	}

	// GET does not echo secrets either.
	rec = do(t, s, http.MethodGet, "/v1/pages/blog", nil, "")
	requireStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatalf("GET leaks secrets: %s", rec.Body.String())
	}

	// Upsert without a secret field keeps the stored secrets (a client that
	// GETs and re-POSTs must not wipe them).
	rec = doJSON(t, s, http.MethodPost, "/v1/pages", map[string]any{
		"id":       "blog",
		"tenantId": "acme",
		"config":   map[string]any{"requestTimeout": "10s"},
	})
	requireStatus(t, rec, http.StatusOK)
	stored, _ = f.page("blog")
	if len(stored.Config.Secret) != 2 {
		t.Fatalf("secrets dropped by partial upsert: %+v", stored.Config.Secret)
	}
	if stored.Config.RequestTimeout != 10*time.Second {
		t.Fatalf("upsert did not update requestTimeout: %v", stored.Config.RequestTimeout)
	}

	// An explicit secret object replaces the whole set.
	rec = doJSON(t, s, http.MethodPost, "/v1/pages", map[string]any{
		"id":       "blog",
		"tenantId": "acme",
		"config":   map[string]any{"secret": map[string]string{"ONLY": "x"}},
	})
	requireStatus(t, rec, http.StatusOK)
	stored, _ = f.page("blog")
	if len(stored.Config.Secret) != 1 || stored.Config.Secret["ONLY"] != "x" {
		t.Fatalf("secret replacement failed: %+v", stored.Config.Secret)
	}

	// Delete, then 404 on read.
	rec = do(t, s, http.MethodDelete, "/v1/pages/blog", nil, "")
	requireStatus(t, rec, http.StatusNoContent)
	rec = do(t, s, http.MethodGet, "/v1/pages/blog", nil, "")
	requireStatus(t, rec, http.StatusNotFound)
}

func TestUpsertPagePreservesActiveDeployment(t *testing.T) {
	f := newFakeProvider()
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme", ActiveDeploymentID: "dep-1"})
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPost, "/v1/pages", map[string]any{"id": "blog", "tenantId": "acme"})
	requireStatus(t, rec, http.StatusOK)
	stored, _ := f.page("blog")
	if stored.ActiveDeploymentID != "dep-1" {
		t.Fatalf("active deployment lost: %q", stored.ActiveDeploymentID)
	}
}

// TestUpsertPageCustomDomains guards the seam between the handler and the
// AdminProvider contract: UpsertPage ignores CustomDomains, so the handler must
// apply an explicitly supplied list via SetCustomDomains. Omitting the field
// must leave the stored set alone.
func TestUpsertPageCustomDomains(t *testing.T) {
	f := newFakeProvider()
	f.seedTenant("acme")
	s := newTestServer(t, Options{Provider: f, Admin: f})

	// Create with domains — they must be persisted, not silently dropped.
	rec := doJSON(t, s, http.MethodPost, "/v1/pages", map[string]any{
		"id": "blog", "tenantId": "acme",
		"customDomains": []string{"www.example.com", "example.com"},
	})
	requireStatus(t, rec, http.StatusOK)
	stored, _ := f.page("blog")
	if len(stored.CustomDomains) != 2 {
		t.Fatalf("custom domains not persisted: %+v", stored.CustomDomains)
	}

	// Omitting the field keeps the stored set.
	rec = doJSON(t, s, http.MethodPost, "/v1/pages", map[string]any{"id": "blog", "tenantId": "acme"})
	requireStatus(t, rec, http.StatusOK)
	stored, _ = f.page("blog")
	if len(stored.CustomDomains) != 2 {
		t.Fatalf("custom domains lost on partial update: %+v", stored.CustomDomains)
	}

	// An explicit empty list clears them.
	rec = doJSON(t, s, http.MethodPost, "/v1/pages", map[string]any{
		"id": "blog", "tenantId": "acme", "customDomains": []string{},
	})
	requireStatus(t, rec, http.StatusOK)
	stored, _ = f.page("blog")
	if len(stored.CustomDomains) != 0 {
		t.Fatalf("custom domains not cleared: %+v", stored.CustomDomains)
	}
}

func TestUpsertPageValidation(t *testing.T) {
	f := newFakeProvider()
	s := newTestServer(t, Options{Provider: f, Admin: f})

	tests := []struct {
		name string
		body any
		want int
	}{
		{"missing id", map[string]any{"tenantId": "acme"}, http.StatusBadRequest},
		{"missing tenant", map[string]any{"id": "blog"}, http.StatusBadRequest},
		{"bad id", map[string]any{"id": "../evil", "tenantId": "acme"}, http.StatusBadRequest},
		{"unknown field", `{"id":"blog","tenantId":"acme","nope":1}`, http.StatusBadRequest},
		{"broken json", `{`, http.StatusBadRequest},
		{"bad duration", `{"id":"blog","tenantId":"acme","config":{"idleTTL":"soon"}}`, http.StatusBadRequest},
		{"unknown tenant", map[string]any{"id": "blog", "tenantId": "ghost"}, http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, "/v1/pages", tc.body)
			requireStatus(t, rec, tc.want)
			if code := errorCode(t, rec); code == "" {
				t.Fatal("no error code")
			}
		})
	}
}

func TestCustomDomains(t *testing.T) {
	f := newFakeProvider()
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme"})
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/custom-domains",
		map[string]any{"domains": []string{"blog.example.com", "www.example.com"}})
	requireStatus(t, rec, http.StatusOK)
	var page pageJSON
	decodeBody(t, rec, &page)
	if page.CustomDomains == nil || len(*page.CustomDomains) != 2 {
		t.Fatalf("custom domains = %v", page.CustomDomains)
	}
	stored, _ := f.page("blog")
	if len(stored.CustomDomains) != 2 || stored.CustomDomains[0] != "blog.example.com" {
		t.Fatalf("stored domains = %v", stored.CustomDomains)
	}

	// Replacing with an empty list clears them.
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/custom-domains", map[string]any{"domains": []string{}})
	requireStatus(t, rec, http.StatusOK)
	stored, _ = f.page("blog")
	if len(stored.CustomDomains) != 0 {
		t.Fatalf("domains not cleared: %v", stored.CustomDomains)
	}

	rec = doJSON(t, s, http.MethodPut, "/v1/pages/ghost/custom-domains", map[string]any{"domains": []string{"a.example"}})
	requireStatus(t, rec, http.StatusNotFound)
}

func TestDeploymentListAndActivate(t *testing.T) {
	f := newFakeProvider()
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme", ActiveDeploymentID: "dep-1"})
	f.seedDeployment(provider.Deployment{ID: "dep-1", PageID: "blog", CreatedAt: fixedNow})
	f.seedDeployment(provider.Deployment{ID: "dep-2", PageID: "blog", CreatedAt: fixedNow.Add(time.Minute)})
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := do(t, s, http.MethodGet, "/v1/pages/blog/deployments", nil, "")
	requireStatus(t, rec, http.StatusOK)
	var list deploymentsResponse
	decodeBody(t, rec, &list)
	if len(list.Deployments) != 2 || list.Deployments[0].ID != "dep-2" {
		t.Fatalf("deployments = %+v", list.Deployments)
	}
	if list.Deployments[0].Active || !list.Deployments[1].Active {
		t.Fatalf("active flag wrong: %+v", list.Deployments)
	}

	rec = do(t, s, http.MethodPost, "/v1/pages/blog/deployments/dep-2/activate", nil, "")
	requireStatus(t, rec, http.StatusOK)
	var page pageJSON
	decodeBody(t, rec, &page)
	if page.ActiveDeploymentID != "dep-2" {
		t.Fatalf("activate response = %+v", page)
	}
	if len(f.activate) != 1 || f.activate[0] != [2]string{"blog", "dep-2"} {
		t.Fatalf("SetActiveDeployment calls = %v", f.activate)
	}

	// Unknown deployment and unknown page both 404.
	rec = do(t, s, http.MethodPost, "/v1/pages/blog/deployments/dep-9/activate", nil, "")
	requireStatus(t, rec, http.StatusNotFound)
	rec = do(t, s, http.MethodPost, "/v1/pages/ghost/deployments/dep-1/activate", nil, "")
	requireStatus(t, rec, http.StatusNotFound)
	rec = do(t, s, http.MethodGet, "/v1/pages/ghost/deployments", nil, "")
	requireStatus(t, rec, http.StatusNotFound)
}

func TestNilAdminReturns501(t *testing.T) {
	f := newFakeProvider()
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme"})
	s := newTestServer(t, Options{Provider: f})

	cases := []struct {
		method, target string
	}{
		{http.MethodGet, "/v1/tenants"},
		{http.MethodPost, "/v1/tenants"},
		{http.MethodDelete, "/v1/tenants/acme"},
		{http.MethodGet, "/v1/tenants/acme/pages"},
		{http.MethodGet, "/v1/pages"},
		{http.MethodPost, "/v1/pages"},
		{http.MethodDelete, "/v1/pages/blog"},
		{http.MethodPut, "/v1/pages/blog/custom-domains"},
		{http.MethodGet, "/v1/pages/blog/deployments"},
		{http.MethodPost, "/v1/pages/blog/deployments"},
		{http.MethodPost, "/v1/pages/blog/deployments/dep-1/activate"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			rec := doJSON(t, s, c.method, c.target, `{}`)
			requireStatus(t, rec, http.StatusNotImplemented)
			if code := errorCode(t, rec); code != codeNotImplemented {
				t.Fatalf("code = %q", code)
			}
		})
	}

	// Read routes still work without an AdminProvider.
	requireStatus(t, do(t, s, http.MethodGet, "/v1/pages/blog", nil, ""), http.StatusOK)
	requireStatus(t, do(t, s, http.MethodGet, "/healthz", nil, ""), http.StatusOK)
}

func TestRouteErrors(t *testing.T) {
	f := newFakeProvider()
	s := newTestServer(t, Options{Provider: f, Admin: f})

	// Unknown route -> 404 envelope.
	rec := do(t, s, http.MethodGet, "/v1/nope", nil, "")
	requireStatus(t, rec, http.StatusNotFound)
	if code := errorCode(t, rec); code != codeNotFound {
		t.Fatalf("code = %q", code)
	}

	// Wrong method on a known route -> 405 envelope from the mux.
	rec = do(t, s, http.MethodPatch, "/v1/tenants", nil, "")
	requireStatus(t, rec, http.StatusMethodNotAllowed)
	if code := errorCode(t, rec); code != codeMethodNotAllowed {
		t.Fatalf("code = %q", code)
	}
	if got := rec.Header().Get("Allow"); got == "" {
		t.Fatal("no Allow header on 405")
	}

	// Oversized JSON body -> 413.
	big := `{"id":"a","tenantId":"b","config":{"env":{"K":"` + strings.Repeat("x", maxJSONBodyBytes+16) + `"}}}`
	rec = doJSON(t, s, http.MethodPost, "/v1/pages", big)
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
	if code := errorCode(t, rec); code != codeTooLarge {
		t.Fatalf("code = %q", code)
	}
}

func TestRequestLogging(t *testing.T) {
	logs := newLogCapture()
	f := newFakeProvider()
	s := newTestServer(t, Options{Provider: f, Admin: f, Logger: logs.logger})

	requireStatus(t, do(t, s, http.MethodGet, "/v1/tenants", nil, ""), http.StatusOK)
	entry := logs.only(t)
	if entry.Method != http.MethodGet || entry.Path != "/v1/tenants" || entry.Status != http.StatusOK {
		t.Fatalf("log entry = %+v", entry)
	}
	if entry.Bytes == 0 {
		t.Fatal("log entry records no response size")
	}
	if entry.RemoteAddr == "" {
		t.Fatal("log entry records no remote address")
	}
	if entry.Level != slog.LevelInfo.String() {
		t.Fatalf("level = %q, want %q", entry.Level, slog.LevelInfo)
	}
}

// TestRequestLogLevels pins the outcome-to-level mapping an operator alerts on.
func TestRequestLogLevels(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		wantStatus int
		wantLevel  slog.Level
	}{
		{"ok", "/v1/tenants", http.StatusOK, slog.LevelInfo},
		{"clientError", "/v1/nope", http.StatusNotFound, slog.LevelWarn},
		{"serverError", "/v1/tenants", http.StatusInternalServerError, slog.LevelError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := newLogCapture()
			f := newFakeProvider()
			if tc.wantStatus == http.StatusInternalServerError {
				f.listTenantsErr = errors.New("boom")
			}
			s := newTestServer(t, Options{Provider: f, Admin: f, Logger: logs.logger})

			requireStatus(t, do(t, s, http.MethodGet, tc.target, nil, ""), tc.wantStatus)
			entry := logs.only(t)
			if entry.Status != tc.wantStatus || entry.Level != tc.wantLevel.String() {
				t.Fatalf("entry = %+v, want status %d level %s", entry, tc.wantStatus, tc.wantLevel)
			}
		})
	}
}

// TestServerErrorIsLogged is the operational gap this package used to have: the
// cause of a 500 must reach the server log, not only the client's response.
func TestServerErrorIsLogged(t *testing.T) {
	logs := newLogCapture()
	f := newFakeProvider()
	f.listTenantsErr = errors.New("read-only file system")
	s := newTestServer(t, Options{Provider: f, Admin: f, Logger: logs.logger})

	rec := do(t, s, http.MethodGet, "/v1/tenants", nil, "")
	requireStatus(t, rec, http.StatusInternalServerError)
	if code := errorCode(t, rec); code != codeInternal {
		t.Fatalf("code = %q", code)
	}
	entry := logs.only(t)
	if !strings.Contains(entry.Error, "read-only file system") {
		t.Fatalf("log entry does not carry the cause: %+v", entry)
	}
	// The detail also stays in the response body: this is a private admin port
	// and the CLI is how operators normally see the failure.
	if !strings.Contains(rec.Body.String(), "read-only file system") {
		t.Fatalf("response body dropped the cause: %s", rec.Body.String())
	}
}

// TestSuccessNotLoggedAsError guards against every line gaining an "error"
// attribute once writeError participates in logging.
func TestSuccessNotLoggedAsError(t *testing.T) {
	logs := newLogCapture()
	f := newFakeProvider()
	s := newTestServer(t, Options{Provider: f, Admin: f, Logger: logs.logger})

	requireStatus(t, do(t, s, http.MethodGet, "/v1/nope", nil, ""), http.StatusNotFound)
	if entry := logs.only(t); entry.Error != "" {
		t.Fatalf("4xx must not carry a server-side error detail: %+v", entry)
	}
}

// TestQueryStringNotLogged pins the decision that the raw query never reaches
// the log, since operator middleware may carry a token there.
func TestQueryStringNotLogged(t *testing.T) {
	logs := newLogCapture()
	f := newFakeProvider()
	s := newTestServer(t, Options{Provider: f, Admin: f, Logger: logs.logger})

	requireStatus(t, do(t, s, http.MethodGet, "/v1/tenants?token=supersecret", nil, ""), http.StatusOK)
	if strings.Contains(logs.buf.String(), "supersecret") {
		t.Fatalf("query string leaked into the log: %q", logs.buf.String())
	}
}

// TestWriteErrorWithoutRecorder covers handlers driven by a bare
// httptest.ResponseRecorder: recording the server-side detail must be a no-op,
// never a panic.
func TestWriteErrorWithoutRecorder(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusInternalServerError, codeInternal, "boom")
	requireStatus(t, rec, http.StatusInternalServerError)
}

// ---- middleware extension point ----

// TestMiddlewareAuthenticates shows the intended use: an operator-supplied
// middleware rejects unauthenticated requests before any handler runs.
func TestMiddlewareAuthenticates(t *testing.T) {
	f := newFakeProvider()
	f.seedTenant("acme")
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer secret" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	s := newTestServer(t, Options{Provider: f, Admin: f, Middleware: []func(http.Handler) http.Handler{auth}})

	// Without credentials every route is refused, including reads...
	rec := do(t, s, http.MethodGet, "/v1/tenants", nil, "")
	requireStatus(t, rec, http.StatusUnauthorized)
	// ...and mutations never reach the provider.
	rec = doJSON(t, s, http.MethodPost, "/v1/tenants", map[string]any{"id": "evil"})
	requireStatus(t, rec, http.StatusUnauthorized)
	if _, err := f.GetTenant(t.Context(), "evil"); err == nil {
		t.Fatal("middleware did not prevent the write")
	}

	// With credentials the request is served normally.
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusOK)
}

// TestMiddlewareOrderAndCoverage pins the two contracts the doc promises:
// entries run outermost-first, and every route is wrapped (HealthPath too).
func TestMiddlewareOrderAndCoverage(t *testing.T) {
	f := newFakeProvider()
	var order []string
	mark := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	s := newTestServer(t, Options{Provider: f, Admin: f,
		Middleware: []func(http.Handler) http.Handler{mark("outer"), mark("inner")}})

	rec := do(t, s, http.MethodGet, HealthPath, nil, "")
	requireStatus(t, rec, http.StatusOK)
	if len(order) != 2 || order[0] != "outer" || order[1] != "inner" {
		t.Fatalf("middleware order = %v, want [outer inner] (and HealthPath must be wrapped)", order)
	}
}

// TestMiddlewareRejectionIsLogged verifies logging stays outermost, so requests
// a middleware refuses still produce a log line with its status.
func TestMiddlewareRejectionIsLogged(t *testing.T) {
	f := newFakeProvider()
	logs := newLogCapture()
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		})
	}
	s := newTestServer(t, Options{Provider: f, Admin: f, Logger: logs.logger,
		Middleware: []func(http.Handler) http.Handler{deny}})

	rec := do(t, s, http.MethodGet, "/v1/tenants", nil, "")
	requireStatus(t, rec, http.StatusForbidden)
	if entry := logs.only(t); entry.Status != http.StatusForbidden {
		t.Fatalf("rejected request not logged: %+v", entry)
	}
}

// TestMiddlewareNilRejected keeps a misconfiguration loud instead of panicking
// on the first request.
func TestMiddlewareNilRejected(t *testing.T) {
	f := newFakeProvider()
	if _, err := New(Options{Provider: f, Middleware: []func(http.Handler) http.Handler{nil}}); err == nil {
		t.Fatal("expected an error for a nil middleware entry")
	}
	returnsNil := func(http.Handler) http.Handler { return nil }
	if _, err := New(Options{Provider: f, Middleware: []func(http.Handler) http.Handler{returnsNil}}); err == nil {
		t.Fatal("expected an error for a middleware returning nil")
	}
}

// ---- temp dir configuration ----

// TestNewValidatesTempDir keeps a misconfigured upload directory (the classic
// case: a read-only root filesystem with no writable volume mounted) a startup
// failure instead of a 500 on the first deployment.
func TestNewValidatesTempDir(t *testing.T) {
	f := newFakeProvider()

	t.Run("writable", func(t *testing.T) {
		if _, err := New(Options{Provider: f, TempDir: t.TempDir()}); err != nil {
			t.Fatalf("New with a writable TempDir: %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if _, err := New(Options{Provider: f, TempDir: missing}); err == nil {
			t.Fatal("expected an error for a missing TempDir")
		}
	})

	t.Run("notADirectory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if _, err := New(Options{Provider: f, TempDir: file}); err == nil {
			t.Fatal("expected an error for a TempDir that is a regular file")
		}
	})

	t.Run("readOnly", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o555); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := New(Options{Provider: f, TempDir: dir}); err == nil {
			t.Fatal("expected an error for a read-only TempDir")
		}
	})

	t.Run("emptyLeavesTheDefault", func(t *testing.T) {
		if _, err := New(Options{Provider: f}); err != nil {
			t.Fatalf("New without a TempDir: %v", err)
		}
	})
}

// TestTempDirProbeIsCleanedUp makes sure startup validation leaves nothing
// behind in the upload volume.
func TestTempDirProbeIsCleanedUp(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(Options{Provider: newFakeProvider(), TempDir: dir}); err != nil {
		t.Fatalf("New: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe directory left behind: %v", entries)
	}
}
