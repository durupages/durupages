// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// testFixture bundles a hub, its keys and storage for handler tests.
type testFixture struct {
	hub     *Hub
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	storage *countingStorage
	handler http.Handler
}

func newFixture(t *testing.T, cacheDir string, cacheMax int64) *testFixture {
	t.Helper()
	pub, priv, err := workerauth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	mem := memstorage.New()
	cs := &countingStorage{inner: mem}
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testFixture{hub: h, pub: pub, priv: priv, storage: cs, handler: h.HTTPHandler()}
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
	h, err := New(Options{Storage: cs, JWTPublicKey: pub, CacheDir: dir, CacheMaxBytes: 250})
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
