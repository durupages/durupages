// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// seedSecretPage installs a page with two secrets and a full configuration, so
// a secrets change can be checked for collateral damage.
func seedSecretPage(t *testing.T, f *fakeProvider) {
	t.Helper()
	f.seedTenant("acme")
	f.seedPage(provider.Page{
		ID:                 "blog",
		TenantID:           "acme",
		ActiveDeploymentID: "dep-1",
		CustomDomains:      []string{"blog.example.com"},
		Config: provider.PageConfig{
			QueueTimeout:   5 * time.Second,
			RequestTimeout: 45 * time.Second,
			Env:            map[string]string{"STAGE": "prod"},
			Secret:         map[string]string{"API_KEY": "s3cr3t", "DB_PASS": "hunter2"},
		},
	})
}

// requireNoSecretValues fails the test when a response body contains any of the
// given secret values: no route may ever echo one back.
func requireNoSecretValues(t *testing.T, rec *httptest.ResponseRecorder, values ...string) {
	t.Helper()
	for _, v := range values {
		if bytes.Contains(rec.Body.Bytes(), []byte(v)) {
			t.Fatalf("response leaks secret value %q: %s", v, rec.Body.String())
		}
	}
}

func TestPutSecretCreatesAndOverwrites(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	// Create a new key: the others survive.
	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/NEW_ONE", map[string]any{"value": "v1"})
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, "v1", "s3cr3t", "hunter2")

	var page pageJSON
	decodeBody(t, rec, &page)
	if got := strings.Join(page.Config.SecretKeys, ","); got != "API_KEY,DB_PASS,NEW_ONE" {
		t.Fatalf("secretKeys = %v", page.Config.SecretKeys)
	}
	stored, _ := f.page("blog")
	if stored.Config.Secret["NEW_ONE"] != "v1" || stored.Config.Secret["API_KEY"] != "s3cr3t" {
		t.Fatalf("stored secrets = %+v", stored.Config.Secret)
	}

	// Overwrite an existing key: the value changes, the key set does not.
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/API_KEY", map[string]any{"value": "rotated"})
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, "rotated", "s3cr3t")
	decodeBody(t, rec, &page)
	if got := strings.Join(page.Config.SecretKeys, ","); got != "API_KEY,DB_PASS,NEW_ONE" {
		t.Fatalf("secretKeys after overwrite = %v", page.Config.SecretKeys)
	}
	stored, _ = f.page("blog")
	if stored.Config.Secret["API_KEY"] != "rotated" || len(stored.Config.Secret) != 3 {
		t.Fatalf("stored secrets = %+v", stored.Config.Secret)
	}

	// An empty value is a legitimate secret, not a delete.
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/EMPTY", map[string]any{"value": ""})
	requireStatus(t, rec, http.StatusOK)
	stored, _ = f.page("blog")
	if v, ok := stored.Config.Secret["EMPTY"]; !ok || v != "" {
		t.Fatalf("empty value not stored: %+v", stored.Config.Secret)
	}
}

// TestPutSecretOnPageWithoutSecrets covers the nil stored map.
func TestPutSecretOnPageWithoutSecrets(t *testing.T) {
	f := newFakeProvider()
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme"})
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/_FIRST", map[string]any{"value": "v"})
	requireStatus(t, rec, http.StatusOK)
	stored, _ := f.page("blog")
	if stored.Config.Secret["_FIRST"] != "v" {
		t.Fatalf("stored secrets = %+v", stored.Config.Secret)
	}
}

// TestSecretChangePreservesPage pins that a secret write is surgical: nothing
// else on the page moves, and the response still reports it.
func TestSecretChangePreservesPage(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/API_KEY", map[string]any{"value": "rotated"})
	requireStatus(t, rec, http.StatusOK)

	var page pageJSON
	decodeBody(t, rec, &page)
	if page.ActiveDeploymentID != "dep-1" {
		t.Fatalf("activeDeploymentId = %q", page.ActiveDeploymentID)
	}
	if page.CustomDomains == nil || len(*page.CustomDomains) != 1 || (*page.CustomDomains)[0] != "blog.example.com" {
		t.Fatalf("customDomains = %v", page.CustomDomains)
	}
	if page.Config.Env["STAGE"] != "prod" {
		t.Fatalf("env = %v", page.Config.Env)
	}
	if time.Duration(page.Config.RequestTimeout) != 45*time.Second ||
		time.Duration(page.Config.QueueTimeout) != 5*time.Second {
		t.Fatalf("timeouts = %+v", page.Config)
	}

	// The stored page must agree. CustomDomains is the interesting one: the
	// AdminProvider contract says UpsertPage ignores the field (see the
	// fakeProvider comment), so the handler must neither re-apply nor lose it.
	stored, _ := f.page("blog")
	if len(stored.CustomDomains) != 1 || stored.CustomDomains[0] != "blog.example.com" {
		t.Fatalf("stored customDomains = %v", stored.CustomDomains)
	}
	if stored.ActiveDeploymentID != "dep-1" || stored.Config.Env["STAGE"] != "prod" ||
		stored.Config.RequestTimeout != 45*time.Second || stored.Config.QueueTimeout != 5*time.Second {
		t.Fatalf("stored page = %+v", stored)
	}
}

func TestDeleteSecret(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	// Existing key.
	rec := do(t, s, http.MethodDelete, "/v1/pages/blog/secrets/API_KEY", nil, "")
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, "s3cr3t", "hunter2")
	var page pageJSON
	decodeBody(t, rec, &page)
	if len(page.Config.SecretKeys) != 1 || page.Config.SecretKeys[0] != "DB_PASS" {
		t.Fatalf("secretKeys = %v", page.Config.SecretKeys)
	}
	stored, _ := f.page("blog")
	if _, ok := stored.Config.Secret["API_KEY"]; ok {
		t.Fatalf("secret not deleted: %+v", stored.Config.Secret)
	}
	if stored.Config.Secret["DB_PASS"] != "hunter2" {
		t.Fatalf("sibling secret lost: %+v", stored.Config.Secret)
	}

	// Missing key: idempotent, page unchanged.
	rec = do(t, s, http.MethodDelete, "/v1/pages/blog/secrets/GHOST", nil, "")
	requireStatus(t, rec, http.StatusOK)
	decodeBody(t, rec, &page)
	if len(page.Config.SecretKeys) != 1 || page.Config.SecretKeys[0] != "DB_PASS" {
		t.Fatalf("idempotent delete changed the page: %v", page.Config.SecretKeys)
	}
	after, _ := f.page("blog")
	if len(after.Config.Secret) != 1 || after.Config.Secret["DB_PASS"] != "hunter2" {
		t.Fatalf("stored secrets = %+v", after.Config.Secret)
	}

	// Deleting the last one empties the set.
	rec = do(t, s, http.MethodDelete, "/v1/pages/blog/secrets/DB_PASS", nil, "")
	requireStatus(t, rec, http.StatusOK)
	// A fresh value: secretKeys is omitted when empty, so decoding into the
	// previous one would keep its stale list.
	var emptied pageJSON
	decodeBody(t, rec, &emptied)
	if len(emptied.Config.SecretKeys) != 0 {
		t.Fatalf("secretKeys = %v", emptied.Config.SecretKeys)
	}
	after, _ = f.page("blog")
	if len(after.Config.Secret) != 0 {
		t.Fatalf("stored secrets = %+v", after.Config.Secret)
	}
}

func TestReplaceSecrets(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets", map[string]any{
		"secrets": map[string]string{"ONE": "a", "TWO": "b"},
	})
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, `"a"`, `"b"`, "s3cr3t", "hunter2")
	var page pageJSON
	decodeBody(t, rec, &page)
	if got := strings.Join(page.Config.SecretKeys, ","); got != "ONE,TWO" {
		t.Fatalf("secretKeys = %v", page.Config.SecretKeys)
	}
	stored, _ := f.page("blog")
	if len(stored.Config.Secret) != 2 || stored.Config.Secret["ONE"] != "a" {
		t.Fatalf("stored secrets = %+v", stored.Config.Secret)
	}
	// The rest of the page is untouched.
	if stored.ActiveDeploymentID != "dep-1" || len(stored.CustomDomains) != 1 ||
		stored.Config.Env["STAGE"] != "prod" {
		t.Fatalf("bulk replace disturbed the page: %+v", stored)
	}

	// An explicit empty object clears every secret.
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets", map[string]any{"secrets": map[string]string{}})
	requireStatus(t, rec, http.StatusOK)
	// A fresh value: secretKeys is omitted when empty, so decoding into the
	// previous one would keep its stale list.
	var cleared pageJSON
	decodeBody(t, rec, &cleared)
	if len(cleared.Config.SecretKeys) != 0 {
		t.Fatalf("secretKeys = %v", cleared.Config.SecretKeys)
	}
	stored, _ = f.page("blog")
	if len(stored.Config.Secret) != 0 {
		t.Fatalf("secrets not cleared: %+v", stored.Config.Secret)
	}
	if stored.Config.Env["STAGE"] != "prod" {
		t.Fatalf("clearing secrets disturbed env: %+v", stored.Config.Env)
	}
}

// TestReplaceSecretsRequiresField keeps "clear everything" deliberate: a body
// without a secrets object is a 400, never an accidental wipe.
func TestReplaceSecretsRequiresField(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	for _, body := range []string{`{}`, `{"secrets":null}`} {
		rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets", body)
		requireStatus(t, rec, http.StatusBadRequest)
		if code := errorCode(t, rec); code != codeInvalidRequest {
			t.Fatalf("body %s: code = %q", body, code)
		}
	}
	stored, _ := f.page("blog")
	if len(stored.Config.Secret) != 2 {
		t.Fatalf("rejected request still changed the page: %+v", stored.Config.Secret)
	}
}

func TestReplaceSecretsLimit(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	atLimit := make(map[string]string, maxBulkSecrets)
	for i := 0; i < maxBulkSecrets; i++ {
		atLimit[fmt.Sprintf("K%d", i)] = "v"
	}
	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets", map[string]any{"secrets": atLimit})
	requireStatus(t, rec, http.StatusOK)
	stored, _ := f.page("blog")
	if len(stored.Config.Secret) != maxBulkSecrets {
		t.Fatalf("stored %d secrets", len(stored.Config.Secret))
	}

	overLimit := make(map[string]string, maxBulkSecrets+1)
	for i := 0; i <= maxBulkSecrets; i++ {
		overLimit[fmt.Sprintf("K%d", i)] = "v"
	}
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets", map[string]any{"secrets": overLimit})
	requireStatus(t, rec, http.StatusBadRequest)
	if code := errorCode(t, rec); code != codeInvalidRequest {
		t.Fatalf("code = %q", code)
	}
	stored, _ = f.page("blog")
	if len(stored.Config.Secret) != maxBulkSecrets {
		t.Fatalf("over-limit request was applied: %d secrets", len(stored.Config.Secret))
	}
}

func TestListSecrets(t *testing.T) {
	f := newFakeProvider()
	f.seedTenant("acme")
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme", Config: provider.PageConfig{
		Secret: map[string]string{"ZULU": "z", "alpha": "a", "MIKE": "m"},
	}})
	f.seedPage(provider.Page{ID: "bare", TenantID: "acme"})
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := do(t, s, http.MethodGet, "/v1/pages/blog/secrets", nil, "")
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, `"z"`, `"a"`, `"m"`)
	var got secretKeysResponse
	decodeBody(t, rec, &got)
	if want := []string{"MIKE", "ZULU", "alpha"}; strings.Join(got.SecretKeys, ",") != strings.Join(want, ",") {
		t.Fatalf("secretKeys = %v, want %v (sorted)", got.SecretKeys, want)
	}

	// A page without secrets reports an empty list, not null.
	rec = do(t, s, http.MethodGet, "/v1/pages/bare/secrets", nil, "")
	requireStatus(t, rec, http.StatusOK)
	if body := strings.TrimSpace(rec.Body.String()); body != `{"secretKeys":[]}` {
		t.Fatalf("body = %s", body)
	}
}

func TestSecretNameValidation(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	oversize := strings.Repeat("A", maxSecretNameLen+1)
	bad := []string{"1BAD", "has-dash", "has.dot", "has%20space", oversize, "a%2Fb"}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/"+name, map[string]any{"value": "v"})
			requireStatus(t, rec, http.StatusBadRequest)
			if code := errorCode(t, rec); code != codeInvalidRequest {
				t.Fatalf("code = %q", code)
			}
			rec = do(t, s, http.MethodDelete, "/v1/pages/blog/secrets/"+name, nil, "")
			requireStatus(t, rec, http.StatusBadRequest)

			rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets",
				map[string]any{"secrets": map[string]string{name: "v"}})
			requireStatus(t, rec, http.StatusBadRequest)
			if code := errorCode(t, rec); code != codeInvalidRequest {
				t.Fatalf("bulk code = %q", code)
			}
		})
	}

	// Names at the edges of the pattern are accepted.
	for _, name := range []string{"A", "_", "_x9", "A" + strings.Repeat("B", maxSecretNameLen-1)} {
		rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/"+name, map[string]any{"value": "v"})
		requireStatus(t, rec, http.StatusOK)
	}

	// An empty name is not a secret route at all: the {name} wildcard does not
	// match an empty segment, so the router answers its own 404 envelope.
	if validSecretName("") {
		t.Fatal("empty secret name must be invalid")
	}
	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/", map[string]any{"value": "v"})
	requireStatus(t, rec, http.StatusNotFound)
	if code := errorCode(t, rec); code != codeNotFound {
		t.Fatalf("code = %q", code)
	}

	// A malformed page id is rejected before anything is read.
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/..%2Fevil/secrets/OK", map[string]any{"value": "v"})
	requireStatus(t, rec, http.StatusBadRequest)

	// The bad requests left the page alone.
	stored, _ := f.page("blog")
	for _, name := range bad {
		if _, ok := stored.Config.Secret[name]; ok {
			t.Fatalf("invalid name %q was stored", name)
		}
	}
}

func TestSecretRoutesUnknownPage(t *testing.T) {
	f := newFakeProvider()
	f.seedTenant("acme")
	s := newTestServer(t, Options{Provider: f, Admin: f})

	cases := []struct {
		method, target string
		body           any
	}{
		{http.MethodGet, "/v1/pages/ghost/secrets", nil},
		{http.MethodPut, "/v1/pages/ghost/secrets", map[string]any{"secrets": map[string]string{"A": "b"}}},
		{http.MethodPut, "/v1/pages/ghost/secrets/A", map[string]any{"value": "b"}},
		{http.MethodDelete, "/v1/pages/ghost/secrets/A", nil},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			rec := doJSON(t, s, c.method, c.target, c.body)
			requireStatus(t, rec, http.StatusNotFound)
			if code := errorCode(t, rec); code != codeNotFound {
				t.Fatalf("code = %q", code)
			}
		})
	}
}

// TestSecretRoutesWithoutAdmin pins the split: listing needs only the read
// provider, the three mutating routes need the AdminProvider.
func TestSecretRoutesWithoutAdmin(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f})

	cases := []struct {
		method, target string
		body           any
	}{
		{http.MethodPut, "/v1/pages/blog/secrets", map[string]any{"secrets": map[string]string{"A": "b"}}},
		{http.MethodPut, "/v1/pages/blog/secrets/A", map[string]any{"value": "b"}},
		{http.MethodDelete, "/v1/pages/blog/secrets/A", nil},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			rec := doJSON(t, s, c.method, c.target, c.body)
			requireStatus(t, rec, http.StatusNotImplemented)
			if code := errorCode(t, rec); code != codeNotImplemented {
				t.Fatalf("code = %q", code)
			}
		})
	}

	// Listing keys still works: it only reads.
	rec := do(t, s, http.MethodGet, "/v1/pages/blog/secrets", nil, "")
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, "s3cr3t", "hunter2")
	var got secretKeysResponse
	decodeBody(t, rec, &got)
	if strings.Join(got.SecretKeys, ",") != "API_KEY,DB_PASS" {
		t.Fatalf("secretKeys = %v", got.SecretKeys)
	}
}

func TestSecretBodyValidation(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	tests := []struct {
		name, method, target string
		body                 any
	}{
		{"put unknown field", http.MethodPut, "/v1/pages/blog/secrets/A", `{"value":"v","nope":1}`},
		{"put broken json", http.MethodPut, "/v1/pages/blog/secrets/A", `{`},
		{"put wrong type", http.MethodPut, "/v1/pages/blog/secrets/A", `{"value":42}`},
		{"bulk unknown field", http.MethodPut, "/v1/pages/blog/secrets", `{"secrets":{},"nope":1}`},
		{"bulk broken json", http.MethodPut, "/v1/pages/blog/secrets", `{`},
		{"bulk wrong type", http.MethodPut, "/v1/pages/blog/secrets", `{"secrets":["A"]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, tc.target, tc.body)
			requireStatus(t, rec, http.StatusBadRequest)
			if code := errorCode(t, rec); code != codeInvalidRequest {
				t.Fatalf("code = %q", code)
			}
		})
	}
}

// TestSecretValuesNeverEchoed sweeps every secrets route plus the page routes
// with one distinctive value and asserts the raw bytes never carry it.
func TestSecretValuesNeverEchoed(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	const value = "swordfish-9f2b"
	rec := doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/TOKEN", map[string]any{"value": value})
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, value)

	for _, target := range []string{
		"/v1/pages/blog",
		"/v1/pages",
		"/v1/pages/blog/secrets",
		"/v1/tenants/acme/pages",
	} {
		rec = do(t, s, http.MethodGet, target, nil, "")
		requireStatus(t, rec, http.StatusOK)
		requireNoSecretValues(t, rec, value)
	}

	// Even the bulk replace, which receives the values, hands none back.
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets",
		map[string]any{"secrets": map[string]string{"TOKEN": value}})
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, value)

	rec = do(t, s, http.MethodDelete, "/v1/pages/blog/secrets/TOKEN", nil, "")
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, value)

	// The value really was stored: the absence above is redaction, not a
	// silently dropped write.
	rec = doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/TOKEN", map[string]any{"value": value})
	requireStatus(t, rec, http.StatusOK)
	stored, _ := f.page("blog")
	if stored.Config.Secret["TOKEN"] != value {
		t.Fatalf("stored secrets = %+v", stored.Config.Secret)
	}
}

// TestSecretUpdateDoesNotMutateProviderMap guards the read-modify-write against
// aliasing: the map GetPage returned must not be edited in place.
func TestSecretUpdateDoesNotMutateProviderMap(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	before, err := f.GetPage(t.Context(), "blog")
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	requireStatus(t, doJSON(t, s, http.MethodPut, "/v1/pages/blog/secrets/NEW", map[string]any{"value": "v"}),
		http.StatusOK)
	if _, ok := before.Config.Secret["NEW"]; ok {
		t.Fatalf("handler mutated a previously returned secret map: %+v", before.Config.Secret)
	}
}

func TestSecretRouteMethods(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	// The collection has no DELETE and the item has no GET: a client must not
	// be able to guess a route that reads a value.
	rec := do(t, s, http.MethodDelete, "/v1/pages/blog/secrets", nil, "")
	requireStatus(t, rec, http.StatusMethodNotAllowed)
	rec = do(t, s, http.MethodGet, "/v1/pages/blog/secrets/API_KEY", nil, "")
	requireStatus(t, rec, http.StatusMethodNotAllowed)
	if code := errorCode(t, rec); code != codeMethodNotAllowed {
		t.Fatalf("code = %q", code)
	}
}

// TestPatchSecretsUpserts pins the difference from PUT: the named secrets are
// created or updated and every unmentioned one survives, which is what
// `wrangler secret bulk` does.
func TestPatchSecretsUpserts(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f) // API_KEY=s3cr3t, DB_PASS=hunter2
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPatch, "/v1/pages/blog/secrets", map[string]any{
		"secrets": map[string]any{"API_KEY": "rotated", "NEW_ONE": "n"},
	})
	requireStatus(t, rec, http.StatusOK)
	requireNoSecretValues(t, rec, "rotated", `"n"`, "s3cr3t", "hunter2")

	stored, _ := f.page("blog")
	if stored.Config.Secret["API_KEY"] != "rotated" {
		t.Fatalf("API_KEY not updated: %+v", stored.Config.Secret)
	}
	if stored.Config.Secret["NEW_ONE"] != "n" {
		t.Fatalf("NEW_ONE not added: %+v", stored.Config.Secret)
	}
	// The whole point of PATCH: an unmentioned secret is preserved.
	if stored.Config.Secret["DB_PASS"] != "hunter2" {
		t.Fatalf("PATCH dropped an unmentioned secret: %+v", stored.Config.Secret)
	}
	// And the rest of the page is untouched.
	if stored.ActiveDeploymentID != "dep-1" || stored.Config.Env["STAGE"] != "prod" ||
		len(stored.CustomDomains) != 1 {
		t.Fatalf("PATCH disturbed the page: %+v", stored)
	}
}

// TestPatchSecretsNullDeletes covers wrangler's convention that a null value in
// a bulk upload removes that secret, and that removing an absent key is fine.
func TestPatchSecretsNullDeletes(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	rec := doJSON(t, s, http.MethodPatch, "/v1/pages/blog/secrets",
		`{"secrets":{"DB_PASS":null,"NEVER_SET":null,"KEPT":"k"}}`)
	requireStatus(t, rec, http.StatusOK)

	stored, _ := f.page("blog")
	if _, ok := stored.Config.Secret["DB_PASS"]; ok {
		t.Fatalf("null did not delete DB_PASS: %+v", stored.Config.Secret)
	}
	if stored.Config.Secret["API_KEY"] != "s3cr3t" {
		t.Fatalf("unmentioned secret lost: %+v", stored.Config.Secret)
	}
	if stored.Config.Secret["KEPT"] != "k" {
		t.Fatalf("KEPT not stored: %+v", stored.Config.Secret)
	}
}

// TestPatchSecretsValidation mirrors the PUT checks: the field is mandatory,
// names are binding identifiers and the entry count is capped.
func TestPatchSecretsValidation(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f, Admin: f})

	// A missing object is refused rather than treated as "change nothing".
	rec := doJSON(t, s, http.MethodPatch, "/v1/pages/blog/secrets", map[string]any{})
	requireStatus(t, rec, http.StatusBadRequest)
	if code := errorCode(t, rec); code != codeInvalidRequest {
		t.Fatalf("code = %q", code)
	}

	rec = doJSON(t, s, http.MethodPatch, "/v1/pages/blog/secrets", map[string]any{
		"secrets": map[string]any{"bad-name": "v"},
	})
	requireStatus(t, rec, http.StatusBadRequest)

	over := make(map[string]any, maxBulkSecrets+1)
	for i := 0; i <= maxBulkSecrets; i++ {
		over[fmt.Sprintf("K%d", i)] = "v"
	}
	rec = doJSON(t, s, http.MethodPatch, "/v1/pages/blog/secrets", map[string]any{"secrets": over})
	requireStatus(t, rec, http.StatusBadRequest)

	// Nothing was written by any of the rejected requests.
	stored, _ := f.page("blog")
	if len(stored.Config.Secret) != 2 {
		t.Fatalf("a rejected PATCH still wrote: %+v", stored.Config.Secret)
	}
}

// TestPatchSecretsRequiresAdmin keeps the write side behind AdminProvider.
func TestPatchSecretsRequiresAdmin(t *testing.T) {
	f := newFakeProvider()
	seedSecretPage(t, f)
	s := newTestServer(t, Options{Provider: f}) // no Admin

	rec := doJSON(t, s, http.MethodPatch, "/v1/pages/blog/secrets", map[string]any{
		"secrets": map[string]any{"A": "1"},
	})
	requireStatus(t, rec, http.StatusNotImplemented)

	rec = doJSON(t, s, http.MethodPatch, "/v1/pages/nope/secrets", map[string]any{
		"secrets": map[string]any{"A": "1"},
	})
	requireStatus(t, rec, http.StatusNotImplemented)
}
