// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/storage/memstorage"
	"github.com/durupages/durupages/pkg/workerauth"
)

const (
	testTenant     = "tenant-a"
	testPage       = "page-1"
	testDeployment = "dep-xyz"
)

// countingStorage wraps a Storage and counts Get calls, for asserting cache
// behavior.
type countingStorage struct {
	inner storage.Storage
	mu    sync.Mutex
	gets  int
}

func (c *countingStorage) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.inner.Get(ctx, key)
}
func (c *countingStorage) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	return c.inner.Put(ctx, key, r, size, ct)
}
func (c *countingStorage) Delete(ctx context.Context, key string) error {
	return c.inner.Delete(ctx, key)
}
func (c *countingStorage) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	return c.inner.List(ctx, prefix)
}
func (c *countingStorage) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

// logCapture collects the hub's structured log output for assertions. It is
// mutex-guarded because handler tests issue concurrent requests.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (c *logCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

func (c *logCapture) raw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// records decodes every captured line.
func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.raw()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// lastBundleRecord returns the most recent bundle request line.
func (c *logCapture) lastBundleRecord(t *testing.T) map[string]any {
	t.Helper()
	recs := c.records(t)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i]["msg"] == bundleLogMessage {
			return recs[i]
		}
	}
	t.Fatalf("no %q log line found in:\n%s", bundleLogMessage, c.raw())
	return nil
}

// wantAttrs asserts that rec carries exactly these values for these keys.
func wantAttrs(t *testing.T, rec map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		got, ok := rec[k]
		if !ok {
			t.Errorf("log line is missing attribute %q; got %v", k, rec)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(v) {
			t.Errorf("log attribute %q = %v, want %v", k, got, v)
		}
	}
}

// testFixture bundles a hub, its keys and storage for handler tests.
type testFixture struct {
	hub     *Hub
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	storage *countingStorage
	handler http.Handler
	logs    *logCapture
}

func newFixture(t *testing.T, cacheDir string, cacheMax int64) *testFixture {
	t.Helper()
	pub, priv, err := workerauth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	mem := memstorage.New()
	cs := &countingStorage{inner: mem}
	logs := &logCapture{}
	// Seed the bundle and manifest for the test deployment.
	bundleKey := fmt.Sprintf(storage.WorkerBundleKeyFmt, testTenant, testPage, testDeployment)
	manifestKey := fmt.Sprintf(storage.ManifestKeyFmt, testTenant, testPage, testDeployment)
	if err := mem.Put(context.Background(), bundleKey, strings.NewReader("BUNDLE-CONTENT"), -1, "application/x-tar"); err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), manifestKey, strings.NewReader(`{"version":1}`), -1, "application/json"); err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{
		Storage:       cs,
		JWTPublicKey:  pub,
		CacheDir:      cacheDir,
		CacheMaxBytes: cacheMax,
		Logger:        logs.logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testFixture{hub: h, pub: pub, priv: priv, storage: cs, handler: h.HTTPHandler(), logs: logs}
}

func bundleURL(tenant, page, dep, file string) string {
	return fmt.Sprintf("/v1/tenants/%s/pages/%s/deployments/%s/%s", tenant, page, dep, file)
}

func (f *testFixture) token(t *testing.T, tenant string) string {
	t.Helper()
	tok, err := workerauth.Issue(f.priv, "pod-1", tenant, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func doGet(f *testFixture, url, token, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthMatrix(t *testing.T) {
	f := newFixture(t, "", 0)
	url := bundleURL(testTenant, testPage, testDeployment, "bundle.tar")

	t.Run("no token 401", func(t *testing.T) {
		rec := doGet(f, url, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
	})

	t.Run("bad signature 401", func(t *testing.T) {
		_, otherPriv, _ := workerauth.GenerateKey()
		badTok, _ := workerauth.Issue(otherPriv, "pod-1", testTenant, time.Hour)
		rec := doGet(f, url, badTok, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
	})

	t.Run("expired 401", func(t *testing.T) {
		expTok, _ := workerauth.Issue(f.priv, "pod-1", testTenant, -time.Minute)
		rec := doGet(f, url, expTok, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
	})

	t.Run("tenant mismatch 403", func(t *testing.T) {
		tok := f.token(t, "other-tenant")
		rec := doGet(f, url, tok, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", rec.Code)
		}
	})

	t.Run("ok 200", func(t *testing.T) {
		tok := f.token(t, testTenant)
		rec := doGet(f, url, tok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != "BUNDLE-CONTENT" {
			t.Fatalf("body = %q", got)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/x-tar" {
			t.Fatalf("content-type = %q", ct)
		}
		if et := rec.Header().Get("ETag"); et != `"`+testDeployment+`"` {
			t.Fatalf("etag = %q", et)
		}
	})
}

func TestManifestRoute(t *testing.T) {
	f := newFixture(t, "", 0)
	url := bundleURL(testTenant, testPage, testDeployment, "manifest.json")
	rec := doGet(f, url, f.token(t, testTenant), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"version":1}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestNotFound(t *testing.T) {
	f := newFixture(t, "", 0)
	url := bundleURL(testTenant, testPage, "missing-dep", "bundle.tar")
	rec := doGet(f, url, f.token(t, testTenant), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestIfNoneMatch304(t *testing.T) {
	f := newFixture(t, "", 0)
	url := bundleURL(testTenant, testPage, testDeployment, "bundle.tar")
	etag := `"` + testDeployment + `"`
	rec := doGet(f, url, f.token(t, testTenant), etag)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("code = %d, want 304", rec.Code)
	}
	// A 304 must not touch storage (immutable deployments).
	if f.storage.getCount() != 0 {
		t.Fatalf("storage Get called %d times on 304", f.storage.getCount())
	}
	// Wildcard should also match.
	rec = doGet(f, url, f.token(t, testTenant), "*")
	if rec.Code != http.StatusNotModified {
		t.Fatalf("wildcard code = %d, want 304", rec.Code)
	}
}

func TestCacheHitSkipsStorage(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, dir, DefaultCacheMaxBytes)
	url := bundleURL(testTenant, testPage, testDeployment, "bundle.tar")
	tok := f.token(t, testTenant)

	rec := doGet(f, url, tok, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "BUNDLE-CONTENT" {
		t.Fatalf("first request: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if f.storage.getCount() != 1 {
		t.Fatalf("after first request, storage Get count = %d, want 1", f.storage.getCount())
	}

	rec = doGet(f, url, tok, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "BUNDLE-CONTENT" {
		t.Fatalf("second request: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Served from disk cache: no additional storage read.
	if f.storage.getCount() != 1 {
		t.Fatalf("after cache hit, storage Get count = %d, want 1", f.storage.getCount())
	}
}

func TestCacheConcurrentSingleflight(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, dir, DefaultCacheMaxBytes)
	url := bundleURL(testTenant, testPage, testDeployment, "bundle.tar")
	tok := f.token(t, testTenant)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			rec := doGet(f, url, tok, "")
			if rec.Code != http.StatusOK {
				t.Errorf("code = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	// Singleflight should collapse the concurrent misses to at most a couple
	// of storage reads (ideally 1).
	if got := f.storage.getCount(); got > 2 {
		t.Fatalf("storage Get count = %d, want singleflight to collapse misses", got)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := workerauth.GenerateKey()
	mem := memstorage.New()
	cs := &countingStorage{inner: mem}

	// Each bundle is 100 bytes; budget of 250 holds at most 2 entries.
	deployments := []string{"dep-1", "dep-2", "dep-3"}
	body := strings.Repeat("x", 100)
	for _, dep := range deployments {
		key := fmt.Sprintf(storage.WorkerBundleKeyFmt, testTenant, testPage, dep)
		if err := mem.Put(context.Background(), key, strings.NewReader(body), -1, "application/x-tar"); err != nil {
			t.Fatal(err)
		}
	}
	h, err := New(Options{Storage: cs, JWTPublicKey: pub, CacheDir: dir, CacheMaxBytes: 250,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	handler := h.HTTPHandler()
	tok, _ := workerauth.Issue(priv, "pod-1", testTenant, time.Hour)

	get := func(dep string) {
		req := httptest.NewRequest(http.MethodGet, bundleURL(testTenant, testPage, dep, "bundle.tar"), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("dep %s: code = %d", dep, rec.Code)
		}
	}

	get("dep-1") // storage: 1
	get("dep-2") // storage: 2
	get("dep-1") // cache hit, dep-1 now MRU; storage still 2
	get("dep-3") // storage: 3, evicts LRU (dep-2)
	if cs.getCount() != 3 {
		t.Fatalf("storage Get count = %d, want 3", cs.getCount())
	}
	get("dep-1") // still cached (was MRU)
	if cs.getCount() != 3 {
		t.Fatalf("dep-1 should be cached, storage Get count = %d, want 3", cs.getCount())
	}
	get("dep-2") // evicted earlier -> refetch
	if cs.getCount() != 4 {
		t.Fatalf("dep-2 should have been evicted, storage Get count = %d, want 4", cs.getCount())
	}
}

func TestParamValidation(t *testing.T) {
	f := newFixture(t, "", 0)
	tok := f.token(t, testTenant)
	// A traversal attempt in the deployment segment. net/http may resolve some
	// dot segments; use an encoded form that reaches the handler with "..".
	cases := []string{
		"/v1/tenants/" + testTenant + "/pages/" + testPage + "/deployments/../bundle.tar",
		"/v1/tenants/" + testTenant + "/pages/" + testPage + "/deployments/%2e%2e/bundle.tar",
	}
	for _, url := range cases {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("url %q returned 200, expected rejection", url)
		}
	}
}

// --- observability -------------------------------------------------------
//
// The bundle route is the one a worker shim calls on cold start, and a failure
// here surfaces to the user as a bare 502. These tests pin down that the hub's
// own log says why.

// doGetWithHeaders issues a bundle request with arbitrary extra headers.
func doGetWithHeaders(f *testFixture, url, token string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// TestNotFoundLogsStorageKey is the deployment-succeeded-but-worker-404s case:
// the storage key the hub looked up is the only evidence of a key mismatch, so
// a 404 must always name it.
func TestNotFoundLogsStorageKey(t *testing.T) {
	f := newFixture(t, "", 0)
	url := bundleURL(testTenant, testPage, "missing-dep", "bundle.tar")
	rec := doGetWithHeaders(f, url, f.token(t, testTenant), map[string]string{
		api.HeaderRequestID: "req-404",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "not found" {
		t.Fatalf("body = %q, want the unchanged wire contract %q", body, "not found")
	}

	line := f.logs.lastBundleRecord(t)
	wantKey := fmt.Sprintf(storage.WorkerBundleKeyFmt, testTenant, testPage, "missing-dep")
	wantAttrs(t, line, map[string]any{
		"level":        "WARN",
		"status":       404,
		"method":       http.MethodGet,
		"path":         url,
		"tenantId":     testTenant,
		"pageId":       testPage,
		"deploymentId": "missing-dep",
		"requestId":    "req-404",
		"key":          wantKey,
		"reason":       reasonBundleNotFound,
	})
	if errText, _ := line["error"].(string); !strings.Contains(errText, "not found") {
		t.Errorf("error attribute = %q, want the storage error", line["error"])
	}
}

// TestNotFoundLogsStorageKeyThroughCache covers the same guarantee on the
// cached path, which reaches storage through a different call site.
func TestNotFoundLogsStorageKeyThroughCache(t *testing.T) {
	f := newFixture(t, t.TempDir(), DefaultCacheMaxBytes)
	url := bundleURL(testTenant, testPage, "missing-dep", "manifest.json")
	rec := doGet(f, url, f.token(t, testTenant), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
	line := f.logs.lastBundleRecord(t)
	wantKey := fmt.Sprintf(storage.ManifestKeyFmt, testTenant, testPage, "missing-dep")
	wantAttrs(t, line, map[string]any{
		"status": 404,
		"key":    wantKey,
		"reason": reasonBundleNotFound,
	})
}

// TestJWTRejectionReasonsAreDistinguishable pins the 401/403 diagnosis: an
// expired worker token, a hub holding the wrong public key and a token minted
// for another tenant must not all look the same in the log.
func TestJWTRejectionReasonsAreDistinguishable(t *testing.T) {
	f := newFixture(t, "", 0)
	url := bundleURL(testTenant, testPage, testDeployment, "bundle.tar")
	_, otherPriv, _ := workerauth.GenerateKey()

	cases := []struct {
		name       string
		token      func() string
		wantStatus int
		wantLevel  string
		wantReason string
		wantExtra  map[string]any
	}{
		{
			name:       "no token",
			token:      func() string { return "" },
			wantStatus: http.StatusUnauthorized,
			wantLevel:  "WARN",
			wantReason: reasonMissingBearerToken,
		},
		{
			name:       "expired",
			token:      func() string { tok, _ := workerauth.Issue(f.priv, "pod-1", testTenant, -time.Minute); return tok },
			wantStatus: http.StatusUnauthorized,
			wantLevel:  "WARN",
			wantReason: reasonTokenExpired,
		},
		{
			name:       "signed by another key",
			token:      func() string { tok, _ := workerauth.Issue(otherPriv, "pod-1", testTenant, time.Hour); return tok },
			wantStatus: http.StatusUnauthorized,
			wantLevel:  "WARN",
			wantReason: reasonSignatureInvalid,
		},
		{
			name:       "garbage",
			token:      func() string { return "not-a-jwt" },
			wantStatus: http.StatusUnauthorized,
			wantLevel:  "WARN",
			wantReason: reasonTokenMalformed,
		},
		{
			name:       "tenant mismatch",
			token:      func() string { return f.token(t, "other-tenant") },
			wantStatus: http.StatusForbidden,
			wantLevel:  "WARN",
			wantReason: reasonTenantMismatch,
			wantExtra:  map[string]any{"tokenTenant": "other-tenant", "pod": "pod-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.logs.reset()
			tok := tc.token()
			rec := doGet(f, url, tok, "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("code = %d, want %d", rec.Code, tc.wantStatus)
			}
			line := f.logs.lastBundleRecord(t)
			want := map[string]any{
				"level":    tc.wantLevel,
				"status":   tc.wantStatus,
				"reason":   tc.wantReason,
				"tenantId": testTenant,
			}
			for k, v := range tc.wantExtra {
				want[k] = v
			}
			wantAttrs(t, line, want)
			if _, ok := line["error"]; !ok {
				t.Errorf("rejection line carries no error attribute: %v", line)
			}
			// The credential itself must never reach the log.
			if tok != "" && strings.Contains(f.logs.raw(), tok) {
				t.Errorf("the worker token leaked into the log:\n%s", f.logs.raw())
			}
			if strings.Contains(f.logs.raw(), "Bearer") {
				t.Errorf("the Authorization header leaked into the log:\n%s", f.logs.raw())
			}
		})
	}
}

func TestSuccessfulRequestIsLogged(t *testing.T) {
	f := newFixture(t, "", 0)
	url := bundleURL(testTenant, testPage, testDeployment, "bundle.tar")
	rec := doGetWithHeaders(f, url, f.token(t, testTenant), map[string]string{
		api.HeaderRequestID: "req-abc",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	line := f.logs.lastBundleRecord(t)
	wantAttrs(t, line, map[string]any{
		"level":        "INFO",
		"status":       200,
		"method":       http.MethodGet,
		"path":         url,
		"bytes":        len("BUNDLE-CONTENT"),
		"tenantId":     testTenant,
		"pageId":       testPage,
		"deploymentId": testDeployment,
		"requestId":    "req-abc",
		"key":          fmt.Sprintf(storage.WorkerBundleKeyFmt, testTenant, testPage, testDeployment),
	})
	if _, ok := line["durationMs"]; !ok {
		t.Errorf("request line is missing durationMs: %v", line)
	}
	if _, ok := line["error"]; ok {
		t.Errorf("successful request carries an error attribute: %v", line)
	}
}

// failingStorage returns a non-ErrNotFound failure, standing in for an S3
// outage or a bad bucket/credential configuration.
type failingStorage struct {
	err error
}

func (s failingStorage) Get(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error) {
	return nil, storage.ObjectInfo{}, s.err
}
func (failingStorage) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (failingStorage) Delete(context.Context, string) error                        { return nil }
func (failingStorage) List(context.Context, string) ([]storage.ObjectInfo, error)  { return nil, nil }

func TestStorageFailureLogsCause(t *testing.T) {
	pub, priv, _ := workerauth.GenerateKey()
	logs := &logCapture{}
	h, err := New(Options{
		Storage:      failingStorage{err: errors.New("s3: NoSuchBucket: bucket does not exist")},
		JWTPublicKey: pub,
		Logger:       logs.logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := workerauth.Issue(priv, "pod-1", testTenant, time.Hour)
	req := httptest.NewRequest(http.MethodGet, bundleURL(testTenant, testPage, testDeployment, "bundle.tar"), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "internal error" {
		t.Fatalf("body = %q, want the unchanged wire contract %q", body, "internal error")
	}
	line := logs.lastBundleRecord(t)
	wantAttrs(t, line, map[string]any{
		"level":  "ERROR",
		"status": 500,
		"reason": reasonStorageError,
		"key":    fmt.Sprintf(storage.WorkerBundleKeyFmt, testTenant, testPage, testDeployment),
	})
	if errText, _ := line["error"].(string); !strings.Contains(errText, "NoSuchBucket") {
		t.Errorf("error attribute = %q, want the underlying storage error", line["error"])
	}
}

func TestInvalidPathParameterIsLogged(t *testing.T) {
	f := newFixture(t, "", 0)
	url := "/v1/tenants/" + testTenant + "/pages/" + testPage + "/deployments/%2e%2e/bundle.tar"
	rec := doGetWithHeaders(f, url, f.token(t, testTenant), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	line := f.logs.lastBundleRecord(t)
	wantAttrs(t, line, map[string]any{
		"level":  "WARN",
		"status": 400,
		"reason": reasonInvalidPathParam,
	})
	if errText, _ := line["error"].(string); !strings.Contains(errText, "deploymentId") {
		t.Errorf("error attribute = %q, want it to name the offending parameter", line["error"])
	}
	// No storage lookup happened, so no key should be claimed.
	if _, ok := line["key"]; ok {
		t.Errorf("400 line claims a storage key it never looked up: %v", line)
	}
}

// TestNilLoggerUsesDefaultAtLogTime is the adminapi convention: an embedder
// that calls slog.SetDefault after hub.New still gets the hub's log.
func TestNilLoggerUsesDefaultAtLogTime(t *testing.T) {
	pub, priv, _ := workerauth.GenerateKey()
	h, err := New(Options{Storage: memstorage.New(), JWTPublicKey: pub}) // Logger unset
	if err != nil {
		t.Fatal(err)
	}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	logs := &logCapture{}
	slog.SetDefault(logs.logger()) // installed only after New

	tok, _ := workerauth.Issue(priv, "pod-1", testTenant, time.Hour)
	req := httptest.NewRequest(http.MethodGet, bundleURL(testTenant, testPage, testDeployment, "bundle.tar"), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.HTTPHandler().ServeHTTP(httptest.NewRecorder(), req)

	line := logs.lastBundleRecord(t)
	wantAttrs(t, line, map[string]any{"status": 404, "reason": reasonBundleNotFound})
}

func TestValidParam(t *testing.T) {
	valid := []string{"tenant-a", "dep_1", "abc123", "a.b"}
	for _, s := range valid {
		if !validParam(s) {
			t.Errorf("validParam(%q) = false, want true", s)
		}
	}
	invalid := []string{"", ".", "..", "a/b", "a\\b", "..hidden..traverse/..", "a..b"}
	for _, s := range invalid {
		if validParam(s) {
			t.Errorf("validParam(%q) = true, want false", s)
		}
	}
}
