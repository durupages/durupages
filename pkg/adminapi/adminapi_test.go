// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// fixedNow is the clock injected into every test server.
var fixedNow = time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

// newTestServer builds a Server, failing the test on a configuration error.
func newTestServer(t *testing.T, o Options) *Server {
	t.Helper()
	if o.Now == nil {
		o.Now = func() time.Time { return fixedNow }
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
	var buf bytes.Buffer
	f := newFakeProvider()
	s := newTestServer(t, Options{Provider: f, Admin: f, LogWriter: &buf})

	requireStatus(t, do(t, s, http.MethodGet, "/v1/tenants", nil, ""), http.StatusOK)
	var entry logEntry
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	if entry.Method != http.MethodGet || entry.Path != "/v1/tenants" || entry.Status != http.StatusOK {
		t.Fatalf("log entry = %+v", entry)
	}
	if entry.Bytes == 0 {
		t.Fatal("log entry records no response size")
	}
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
	var logs strings.Builder
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		})
	}
	s := newTestServer(t, Options{Provider: f, Admin: f, LogWriter: &logs,
		Middleware: []func(http.Handler) http.Handler{deny}})

	rec := do(t, s, http.MethodGet, "/v1/tenants", nil, "")
	requireStatus(t, rec, http.StatusForbidden)
	if !strings.Contains(logs.String(), `"status":403`) {
		t.Fatalf("rejected request not logged: %q", logs.String())
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
