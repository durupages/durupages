// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/durupages/durupages/pkg/adminapi"
	"github.com/durupages/durupages/pkg/provider/memprovider"
	"github.com/durupages/durupages/pkg/storage/memstorage"
)

// testAPI is an httptest server running the real admin API on top of the
// in-memory provider and storage, so the CLI is exercised against the same
// handler the controller serves.
type testAPI struct {
	*httptest.Server
	provider *memprovider.Provider
	storage  *memstorage.Store

	mu      sync.Mutex
	headers []http.Header
}

// newTestAPI starts an admin API server for one test, serving the default
// pages domain.
func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	return newTestAPIWithDomain(t, "pages.local")
}

// newTestAPIWithDomain starts an admin API server that serves pagesDomain, so a
// test can tell the controller's domain apart from the CLI's own default.
func newTestAPIWithDomain(t *testing.T, pagesDomain string) *testAPI {
	t.Helper()
	// Keep the ambient environment out of the flag defaults.
	t.Setenv("DURUPAGES_ADMIN_URL", "")
	t.Setenv("DURUPAGES_ADMIN_TOKEN", "")
	t.Setenv("DURUPAGES_TENANT", "")
	t.Setenv("DURUPAGES_PAGE", "")
	t.Setenv("DURUPAGES_PAGES_DOMAIN", "")

	api := &testAPI{
		provider: memprovider.New(memprovider.Options{PagesDomain: pagesDomain}),
		storage:  memstorage.New(),
	}
	h, err := adminapi.New(adminapi.Options{
		Provider:    api.provider,
		Admin:       api.provider,
		Storage:     api.storage,
		PagesDomain: pagesDomain,
		// The admin API logs every request to slog.Default() unless told
		// otherwise; these tests assert on the CLI's output, so keep the
		// server's request log out of the test output.
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Middleware: []func(http.Handler) http.Handler{api.recordHeaders},
	})
	if err != nil {
		t.Fatalf("adminapi.New: %v", err)
	}
	api.Server = httptest.NewServer(h)
	t.Cleanup(api.Close)
	return api
}

// recordHeaders captures every request's headers for the auth assertions.
func (a *testAPI) recordHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.headers = append(a.headers, r.Header.Clone())
		a.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// lastHeaders returns the headers of the most recent request.
func (a *testAPI) lastHeaders(t *testing.T) http.Header {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.headers) == 0 {
		t.Fatal("no request reached the server")
	}
	return a.headers[len(a.headers)-1]
}

// result is one CLI invocation's outcome.
type result struct {
	code   int
	stdout string
	stderr string
}

// object decodes the stdout JSON document.
func (r result) object(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, r.stdout)
	}
	return v
}

// run drives the CLI with the test server's URL appended.
func (a *testAPI) run(t *testing.T, args ...string) result {
	t.Helper()
	return runCLI(t, "", append(args, "--admin-url", a.URL)...)
}

// runIn drives the CLI with the given stdin and the test server's URL.
func (a *testAPI) runIn(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return runCLI(t, stdin, append(args, "--admin-url", a.URL)...)
}

// mustRun drives the CLI and fails the test unless it exits 0.
func (a *testAPI) mustRun(t *testing.T, args ...string) result {
	t.Helper()
	res := a.run(t, args...)
	if res.code != 0 {
		t.Fatalf("duru %s: exit %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return res
}

// runCLI executes one CLI invocation against injected streams.
func runCLI(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	c := &cli{
		args:   args,
		stdin:  strings.NewReader(stdin),
		stdout: &stdout,
		stderr: &stderr,
	}
	code := c.run()
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// dig walks a decoded JSON document along the given keys.
func dig(t *testing.T, v map[string]any, keys ...string) any {
	t.Helper()
	var cur any = v
	for i, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: %q is not an object", keys[:i], keys[i-1])
		}
		cur, ok = m[k]
		if !ok {
			return nil
		}
	}
	return cur
}

// writeBuildOutput creates a minimal wrangler build output directory.
func writeBuildOutput(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.html":        "<!doctype html><title>duru</title>",
		"about/index.html":  "<!doctype html><title>about</title>",
		"assets/app.css":    "body{margin:0}",
		"_redirects":        "/old /new 301",
		"_worker.js":        "export default { fetch(){ return new Response('hi') } }",
		"_routes.json":      `{"version":1,"include":["/api/*"],"exclude":[]}`,
		"a/b/c/deep.txt":    "deep",
		"_headers":          "/*\n  X-Test: 1",
		"compat-check.json": "{}",
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// seed creates a tenant and a page through the CLI.
func seed(t *testing.T, api *testAPI, tenantID, pageID string) {
	t.Helper()
	api.mustRun(t, "--tenant", tenantID, "tenant", "set")
	api.mustRun(t, "--tenant", tenantID, "--page", pageID, "page", "set")
}

func TestHealth(t *testing.T) {
	api := newTestAPI(t)
	res := api.mustRun(t, "health")
	if got := res.object(t)["status"]; got != "ok" {
		t.Fatalf("status = %v, want ok", got)
	}
}

func TestTenantLifecycle(t *testing.T) {
	api := newTestAPI(t)

	res := api.mustRun(t, "--tenant", "acme", "tenant", "set", "--max-concurrency", "4", "--idle-ttl", "5m")
	if !strings.Contains(res.stderr, `tenant "acme" created`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	if got := dig(t, res.object(t), "config", "maxConcurrency"); got != float64(4) {
		t.Fatalf("maxConcurrency = %v", got)
	}

	res = api.mustRun(t, "--tenant", "acme", "tenant", "get")
	obj := res.object(t)
	if got := dig(t, obj, "config", "idleTTL"); got != "5m0s" {
		t.Fatalf("idleTTL = %v", got)
	}
	if got := obj["id"]; got != "acme" {
		t.Fatalf("id = %v", got)
	}

	// A second set reports an update, not a create.
	res = api.mustRun(t, "--tenant", "acme", "tenant", "set", "--cpu-limit", "1")
	if !strings.Contains(res.stderr, `tenant "acme" updated`) {
		t.Fatalf("stderr = %q", res.stderr)
	}

	res = api.mustRun(t, "tenant", "list")
	tenants, _ := res.object(t)["tenants"].([]any)
	if len(tenants) != 1 {
		t.Fatalf("tenants = %v", tenants)
	}

	api.mustRun(t, "--tenant", "acme", "--page", "blog", "page", "set")
	res = api.mustRun(t, "--tenant", "acme", "tenant", "pages")
	pages, _ := res.object(t)["pages"].([]any)
	if len(pages) != 1 {
		t.Fatalf("pages = %v", pages)
	}

	res = api.mustRun(t, "--tenant", "acme", "tenant", "delete")
	if res.stdout != "" {
		t.Fatalf("delete wrote to stdout: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, `tenant "acme" deleted`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	if res := api.run(t, "--tenant", "acme", "tenant", "get"); res.code != 1 {
		t.Fatalf("get after delete: exit %d", res.code)
	}
}

func TestTenantSetReadModifyWrite(t *testing.T) {
	api := newTestAPI(t)
	api.mustRun(t, "--tenant", "acme", "tenant", "set",
		"--max-concurrency", "4", "--idle-ttl", "5m", "--cpu-limit", "2", "--mem-limit", "512Mi")

	// A partial update must not wipe the other fields.
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "--max-concurrency", "9")
	cfg := api.mustRun(t, "--tenant", "acme", "tenant", "get").object(t)
	if got := dig(t, cfg, "config", "maxConcurrency"); got != float64(9) {
		t.Fatalf("maxConcurrency = %v", got)
	}
	if got := dig(t, cfg, "config", "idleTTL"); got != "5m0s" {
		t.Fatalf("idleTTL wiped: %v", got)
	}
	if got := dig(t, cfg, "config", "workerMemLimit"); got != "512Mi" {
		t.Fatalf("workerMemLimit wiped: %v", got)
	}

	// --replace sends only what was given.
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "--replace", "--max-concurrency", "1")
	cfg = api.mustRun(t, "--tenant", "acme", "tenant", "get").object(t)
	if got := dig(t, cfg, "config", "idleTTL"); got != nil {
		t.Fatalf("--replace kept idleTTL: %v", got)
	}
	if got := dig(t, cfg, "config", "maxConcurrency"); got != float64(1) {
		t.Fatalf("maxConcurrency = %v", got)
	}
}

func TestTenantSetFileAndPrecedence(t *testing.T) {
	api := newTestAPI(t)
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "--max-concurrency", "2", "--cpu-limit", "1")

	// A round-trip of `tenant get` output must be accepted verbatim.
	dumped := api.mustRun(t, "--tenant", "acme", "tenant", "get").stdout
	path := filepath.Join(t.TempDir(), "acme.json")
	if err := os.WriteFile(path, []byte(dumped), 0o644); err != nil {
		t.Fatal(err)
	}
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "-f", path)
	cfg := api.mustRun(t, "--tenant", "acme", "tenant", "get").object(t)
	if got := dig(t, cfg, "config", "workerCPULimit"); got != "1" {
		t.Fatalf("round-trip lost workerCPULimit: %v", got)
	}

	// The file beats server state; an explicit flag beats the file.
	file := `{"config":{"maxConcurrency":7,"idleTTL":"9m","workerMemLimit":"1Gi"}}`
	res := api.runIn(t, file, "--tenant", "acme", "tenant", "set", "--file", "-", "--max-concurrency", "11")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	cfg = api.mustRun(t, "--tenant", "acme", "tenant", "get").object(t)
	if got := dig(t, cfg, "config", "maxConcurrency"); got != float64(11) {
		t.Fatalf("flag did not beat file: %v", got)
	}
	if got := dig(t, cfg, "config", "idleTTL"); got != "9m0s" {
		t.Fatalf("file value lost: %v", got)
	}
	if got := dig(t, cfg, "config", "workerCPULimit"); got != "1" {
		t.Fatalf("server value lost: %v", got)
	}
}

func TestTenantPodMapMergeAndClear(t *testing.T) {
	api := newTestAPI(t)
	api.mustRun(t, "--tenant", "acme", "tenant", "set",
		"--pod-label", "team=web", "--pod-label", "tier=front",
		"--pod-annotation", "note=hello")

	// A repeated map flag merges: only the named key changes.
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "--pod-label", "tier=back")
	labels, _ := dig(t, api.mustRun(t, "--tenant", "acme", "tenant", "get").object(t),
		"config", "podLabels").(map[string]any)
	if labels["team"] != "web" || labels["tier"] != "back" {
		t.Fatalf("podLabels = %v", labels)
	}

	// --clear-* wins over the stored map, and the flags after it still apply.
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "--clear-pod-labels", "--pod-label", "only=me")
	obj := api.mustRun(t, "--tenant", "acme", "tenant", "get").object(t)
	labels, _ = dig(t, obj, "config", "podLabels").(map[string]any)
	if len(labels) != 1 || labels["only"] != "me" {
		t.Fatalf("podLabels after clear = %v", labels)
	}
	annots, _ := dig(t, obj, "config", "podAnnotations").(map[string]any)
	if annots["note"] != "hello" {
		t.Fatalf("annotations were touched: %v", annots)
	}

	// A bare --clear-* empties the map.
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "--clear-pod-annotations")
	if got := dig(t, api.mustRun(t, "--tenant", "acme", "tenant", "get").object(t),
		"config", "podAnnotations"); got != nil {
		t.Fatalf("podAnnotations = %v, want absent", got)
	}
}

func TestPageSetReadModifyWrite(t *testing.T) {
	api := newTestAPI(t)
	api.mustRun(t, "--tenant", "acme", "tenant", "set")
	api.mustRun(t, "--page", "blog", "--tenant", "acme", "page", "set",
		"--env", "API=https://api.example", "--queue-timeout", "30s")

	// The regression this design exists for: a partial update must not wipe
	// the env the API replaces on every write.
	api.mustRun(t, "--page", "blog", "page", "set", "--request-timeout", "5s")
	obj := api.mustRun(t, "--page", "blog", "page", "get").object(t)
	env, _ := dig(t, obj, "config", "env").(map[string]any)
	if env["API"] != "https://api.example" {
		t.Fatalf("env wiped by a partial update: %v", env)
	}
	if got := dig(t, obj, "config", "queueTimeout"); got != "30s" {
		t.Fatalf("queueTimeout wiped: %v", got)
	}
	if got := dig(t, obj, "config", "requestTimeout"); got != "5s" {
		t.Fatalf("requestTimeout = %v", got)
	}
	if got := obj["tenantId"]; got != "acme" {
		t.Fatalf("tenantId lost: %v", got)
	}

	// --replace is the documented way to wipe it.
	api.mustRun(t, "--page", "blog", "--tenant", "acme", "page", "set", "--replace", "--request-timeout", "5s")
	obj = api.mustRun(t, "--page", "blog", "page", "get").object(t)
	if got := dig(t, obj, "config", "env"); got != nil {
		t.Fatalf("--replace kept env: %v", got)
	}
	if got := dig(t, obj, "config", "queueTimeout"); got != nil {
		t.Fatalf("--replace kept queueTimeout: %v", got)
	}
}

func TestPageSetFileAndPrecedence(t *testing.T) {
	api := newTestAPI(t)
	api.mustRun(t, "--tenant", "acme", "tenant", "set")
	api.mustRun(t, "--tenant", "acme", "--page", "blog", "page", "set", "--env", "KEEP=1")

	// `page get` output is a valid `page set` input.
	dumped := api.mustRun(t, "--page", "blog", "page", "get").stdout
	path := filepath.Join(t.TempDir(), "blog.json")
	if err := os.WriteFile(path, []byte(dumped), 0o644); err != nil {
		t.Fatal(err)
	}
	api.mustRun(t, "--page", "blog", "page", "set", "-f", path)

	// File beats server state, explicit flag beats the file.
	file := `{"config":{"env":{"FROM":"file"},"requestTimeout":"7s"}}`
	if res := api.runIn(t, file, "--page", "blog", "page", "set", "--file", "-",
		"--request-timeout", "9s"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	obj := api.mustRun(t, "--page", "blog", "page", "get").object(t)
	if got := dig(t, obj, "config", "requestTimeout"); got != "9s" {
		t.Fatalf("flag did not beat file: %v", got)
	}
	env, _ := dig(t, obj, "config", "env").(map[string]any)
	if len(env) != 1 || env["FROM"] != "file" {
		t.Fatalf("file map should replace the stored map: %v", env)
	}
}

func TestPageMapMergeAndListReplace(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")
	api.mustRun(t, "--page", "blog", "page", "set", "--env", "A=1", "--env", "B=2",
		"--domain", "a.example.com", "--domain", "b.example.com")

	// Map flags merge.
	api.mustRun(t, "--page", "blog", "page", "set", "--env", "B=22", "--env", "C=3")
	obj := api.mustRun(t, "--page", "blog", "page", "get").object(t)
	env, _ := dig(t, obj, "config", "env").(map[string]any)
	if env["A"] != "1" || env["B"] != "22" || env["C"] != "3" {
		t.Fatalf("env = %v", env)
	}

	// List flags replace.
	api.mustRun(t, "--page", "blog", "page", "set", "--domain", "c.example.com")
	obj = api.mustRun(t, "--page", "blog", "page", "get").object(t)
	domains, _ := obj["customDomains"].([]any)
	if len(domains) != 1 || domains[0] != "c.example.com" {
		t.Fatalf("customDomains = %v", domains)
	}

	// --clear-env then --env yields exactly the given map.
	api.mustRun(t, "--page", "blog", "page", "set", "--clear-env", "--env", "ONLY=1")
	env, _ = dig(t, api.mustRun(t, "--page", "blog", "page", "get").object(t),
		"config", "env").(map[string]any)
	if len(env) != 1 || env["ONLY"] != "1" {
		t.Fatalf("env after clear = %v", env)
	}

	// --clear-domains empties the list.
	api.mustRun(t, "--page", "blog", "page", "set", "--clear-domains")
	obj = api.mustRun(t, "--page", "blog", "page", "get").object(t)
	if domains, _ := obj["customDomains"].([]any); len(domains) != 0 {
		t.Fatalf("customDomains = %v", domains)
	}
}

func TestPageLogsEnabledTriState(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	// Unset leaves the field null (follow the global setting).
	obj := api.mustRun(t, "--page", "blog", "page", "get").object(t)
	if got := dig(t, obj, "config", "logsEnabled"); got != nil {
		t.Fatalf("logsEnabled = %v, want absent", got)
	}

	api.mustRun(t, "--page", "blog", "page", "set", "--logs-enabled", "false")
	obj = api.mustRun(t, "--page", "blog", "page", "get").object(t)
	if got := dig(t, obj, "config", "logsEnabled"); got != false {
		t.Fatalf("logsEnabled = %v, want false", got)
	}

	// An unrelated update must keep the explicit false.
	api.mustRun(t, "--page", "blog", "page", "set", "--env", "X=1")
	obj = api.mustRun(t, "--page", "blog", "page", "get").object(t)
	if got := dig(t, obj, "config", "logsEnabled"); got != false {
		t.Fatalf("logsEnabled = %v, want false", got)
	}

	api.mustRun(t, "--page", "blog", "page", "set", "--logs-enabled=true")
	obj = api.mustRun(t, "--page", "blog", "page", "get").object(t)
	if got := dig(t, obj, "config", "logsEnabled"); got != true {
		t.Fatalf("logsEnabled = %v, want true", got)
	}

	if res := api.run(t, "--page", "blog", "page", "set", "--logs-enabled", "yolo"); res.code != 2 {
		t.Fatalf("bad boolean: exit %d (%s)", res.code, res.stderr)
	}
}

func TestPageSecretsAreWriteOnly(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	res := api.mustRun(t, "--page", "blog", "page", "set", "--secret", "TOKEN=s3cr3t", "--secret", "OTHER=hunter2")
	if strings.Contains(res.stdout, "s3cr3t") || strings.Contains(res.stdout, "hunter2") {
		t.Fatalf("set echoed a secret value: %s", res.stdout)
	}

	res = api.mustRun(t, "--page", "blog", "page", "get")
	if strings.Contains(res.stdout, "s3cr3t") || strings.Contains(res.stdout, "hunter2") {
		t.Fatalf("get echoed a secret value: %s", res.stdout)
	}
	keys, _ := dig(t, res.object(t), "config", "secretKeys").([]any)
	if len(keys) != 2 || keys[0] != "OTHER" || keys[1] != "TOKEN" {
		t.Fatalf("secretKeys = %v", keys)
	}

	// An unrelated update keeps the stored secrets (the API merges them when
	// the request omits the field).
	api.mustRun(t, "--page", "blog", "page", "set", "--env", "A=1")
	keys, _ = dig(t, api.mustRun(t, "--page", "blog", "page", "get").object(t),
		"config", "secretKeys").([]any)
	if len(keys) != 2 {
		t.Fatalf("secretKeys after unrelated update = %v", keys)
	}

	// A `page get` dump round-trips: secretKeys is accepted and not sent back.
	dumped := api.mustRun(t, "--page", "blog", "page", "get").stdout
	if res := api.runIn(t, dumped, "--page", "blog", "page", "set", "-f", "-"); res.code != 0 {
		t.Fatalf("round-trip with secretKeys: exit %d: %s", res.code, res.stderr)
	}

	// --clear-secrets removes them all.
	api.mustRun(t, "--page", "blog", "page", "set", "--clear-secrets")
	if got := dig(t, api.mustRun(t, "--page", "blog", "page", "get").object(t),
		"config", "secretKeys"); got != nil {
		t.Fatalf("secretKeys = %v, want absent", got)
	}
}

func TestPageDomainsCommand(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	res := api.mustRun(t, "--page", "blog", "page", "domains",
		"--domain", "One.example.com", "--domain", "two.example.com")
	domains, _ := res.object(t)["customDomains"].([]any)
	if len(domains) != 2 || domains[0] != "one.example.com" {
		t.Fatalf("customDomains = %v", domains)
	}
	if !strings.Contains(res.stderr, "custom domains updated (2)") {
		t.Fatalf("stderr = %q", res.stderr)
	}

	res = api.mustRun(t, "--page", "blog", "page", "domains", "--clear")
	if domains, _ := res.object(t)["customDomains"].([]any); len(domains) != 0 {
		t.Fatalf("customDomains = %v", domains)
	}

	// Neither flag is a usage error, so the set is never wiped by accident.
	if res := api.run(t, "--page", "blog", "page", "domains"); res.code != 2 {
		t.Fatalf("bare invocation: exit %d", res.code)
	}
}

func TestPageListAndDelete(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")
	seed(t, api, "other", "shop")

	all, _ := api.mustRun(t, "page", "list").object(t)["pages"].([]any)
	if len(all) != 2 {
		t.Fatalf("pages = %v", all)
	}
	mine, _ := api.mustRun(t, "--tenant", "acme", "page", "list").object(t)["pages"].([]any)
	if len(mine) != 1 {
		t.Fatalf("pages of acme = %v", mine)
	}
	// The flag reads the same after the command word.
	mine, _ = api.mustRun(t, "page", "list", "--tenant", "acme").object(t)["pages"].([]any)
	if len(mine) != 1 {
		t.Fatalf("pages of acme = %v", mine)
	}

	res := api.mustRun(t, "--page", "blog", "page", "delete")
	if res.stdout != "" {
		t.Fatalf("delete wrote to stdout: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, `page "blog" deleted`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	if res := api.run(t, "--page", "blog", "page", "get"); res.code != 1 {
		t.Fatalf("get after delete: exit %d", res.code)
	}
}

func TestPageSetRequiresTenantWhenCreating(t *testing.T) {
	api := newTestAPI(t)
	api.mustRun(t, "--tenant", "acme", "tenant", "set")

	res := api.run(t, "--page", "fresh", "page", "set")
	if res.code != 2 {
		t.Fatalf("exit %d, want 2 (%s)", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "--tenant") {
		t.Fatalf("stderr = %q", res.stderr)
	}

	// Once the page exists its tenant is carried over automatically.
	api.mustRun(t, "--page", "fresh", "--tenant", "acme", "page", "set")
	api.mustRun(t, "--page", "fresh", "page", "set", "--request-timeout", "3s")

	// DURUPAGES_TENANT is an equivalent source when creating.
	t.Setenv("DURUPAGES_TENANT", "acme")
	api.mustRun(t, "--page", "from-env", "page", "set")
	if got := api.mustRun(t, "--page", "from-env", "page", "get").object(t)["tenantId"]; got != "acme" {
		t.Fatalf("tenantId = %v", got)
	}
}

func TestDeploymentUploadAndActivate(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")
	dir := writeBuildOutput(t)

	// Upload without activating: the page keeps no active deployment.
	res := api.mustRun(t, "--page", "blog", "deployment", "upload", "--dir", dir,
		"--deployment", "dep-one", "--no-activate")
	obj := res.object(t)
	if obj["deploymentId"] != "dep-one" || obj["activated"] != false {
		t.Fatalf("upload response = %v", obj)
	}
	if got := dig(t, obj, "manifest", "hasWorker"); got != true {
		t.Fatalf("hasWorker = %v", got)
	}
	if got := dig(t, obj, "manifest", "staticFileCount"); got == float64(0) {
		t.Fatalf("staticFileCount = %v", got)
	}
	if !strings.Contains(res.stderr, "deployment dep-one uploaded to page \"blog\" (not activated)") {
		t.Fatalf("stderr = %q", res.stderr)
	}
	if got := api.mustRun(t, "--page", "blog", "page", "get").object(t)["activeDeploymentId"]; got != nil {
		t.Fatalf("activeDeploymentId = %v, want absent", got)
	}

	// The deployment is listed but not active.
	deps, _ := api.mustRun(t, "--page", "blog", "deployment", "list").object(t)["deployments"].([]any)
	if len(deps) != 1 {
		t.Fatalf("deployments = %v", deps)
	}
	if first, _ := deps[0].(map[string]any); first["active"] != nil {
		t.Fatalf("deployment is active: %v", first)
	}

	// Activating it moves the page's pointer.
	res = api.mustRun(t, "--page", "blog", "deployment", "activate", "dep-one")
	if got := res.object(t)["activeDeploymentId"]; got != "dep-one" {
		t.Fatalf("activeDeploymentId = %v", got)
	}
	if !strings.Contains(res.stderr, "deployment dep-one activated") {
		t.Fatalf("stderr = %q", res.stderr)
	}

	// A second upload activates by default.
	res = api.mustRun(t, "--page", "blog", "deployment", "upload", "--dir", dir, "--deployment", "dep-two")
	if got := res.object(t)["activated"]; got != true {
		t.Fatalf("activated = %v", got)
	}
	if got := api.mustRun(t, "--page", "blog", "page", "get").object(t)["activeDeploymentId"]; got != "dep-two" {
		t.Fatalf("activeDeploymentId = %v", got)
	}
	deps, _ = api.mustRun(t, "--page", "blog", "deployment", "list").object(t)["deployments"].([]any)
	if len(deps) != 2 {
		t.Fatalf("deployments = %v", deps)
	}

	// The uploaded objects really reached storage.
	objects, err := api.storage.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) == 0 {
		t.Fatal("nothing was uploaded to storage")
	}

	// Activating an unknown deployment is a 404.
	if res := api.run(t, "--page", "blog", "deployment", "activate", "nope"); res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
}

// A controller serving a domain other than the CLI's built-in default must
// still have its own domain reported. Before the admin API returned
// pagesDomain, changing the controller's domain (for example through the Helm
// chart) left `duru deploy` printing "pages.local" regardless.
func TestDeployReportsControllerPagesDomain(t *testing.T) {
	api := newTestAPIWithDomain(t, "pages.example.com")
	dir := writeBuildOutput(t)

	res := runCLI(t, "", "deploy", "--dir", dir, "--tenant", "acme", "--page", "blog",
		"--admin-url", api.URL, "--deployment", "dep-one")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if want := "url: https://blog.pages.example.com/\n"; !strings.Contains(res.stdout, want) {
		t.Fatalf("stdout = %q, want it to contain %q", res.stdout, want)
	}
	if strings.Contains(res.stdout, "pages.local") {
		t.Fatalf("stdout still reports the CLI default: %q", res.stdout)
	}
}

// An explicitly chosen --pages-domain is the operator's call and outranks what
// the controller reports.
func TestDeployExplicitPagesDomainWins(t *testing.T) {
	api := newTestAPIWithDomain(t, "pages.example.com")
	dir := writeBuildOutput(t)

	res := runCLI(t, "", "deploy", "--dir", dir, "--tenant", "acme", "--page", "blog",
		"--admin-url", api.URL, "--deployment", "dep-one", "--pages-domain", "vanity.test")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if want := "url: https://blog.vanity.test/\n"; !strings.Contains(res.stdout, want) {
		t.Fatalf("stdout = %q, want it to contain %q", res.stdout, want)
	}
}

func TestDeploymentUploadMissingDir(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")
	res := api.run(t, "--page", "blog", "deployment", "upload", "--dir", filepath.Join(t.TempDir(), "nope"))
	if res.code != 1 {
		t.Fatalf("exit %d", res.code)
	}
	if !strings.Contains(res.stderr, "duru deployment upload:") {
		t.Fatalf("stderr = %q", res.stderr)
	}
}

func TestAuthTokenAndHeaders(t *testing.T) {
	api := newTestAPI(t)

	api.mustRun(t, "tenant", "list", "--token", "s3cret")
	if got := api.lastHeaders(t).Get("Authorization"); got != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", got)
	}

	api.mustRun(t, "tenant", "list", "--header", "X-Trace: abc", "--header", "X-Other: 1")
	h := api.lastHeaders(t)
	if h.Get("X-Trace") != "abc" || h.Get("X-Other") != "1" {
		t.Fatalf("headers = %v", h)
	}
	if h.Get("Authorization") != "" {
		t.Fatalf("unexpected Authorization: %q", h.Get("Authorization"))
	}

	// An explicit --header wins over --token for the same name.
	api.mustRun(t, "tenant", "list", "--token", "s3cret", "--header", "Authorization: Basic zzz")
	if got := api.lastHeaders(t).Get("Authorization"); got != "Basic zzz" {
		t.Fatalf("Authorization = %q", got)
	}

	// The token also reaches an upload, whose body is streamed.
	seed(t, api, "acme", "blog")
	api.mustRun(t, "--page", "blog", "deployment", "upload", "--dir", writeBuildOutput(t), "--token", "up")
	if got := api.lastHeaders(t).Get("Authorization"); got != "Bearer up" {
		t.Fatalf("upload Authorization = %q", got)
	}

	// The env var is an equivalent source.
	t.Setenv("DURUPAGES_ADMIN_TOKEN", "from-env")
	api.mustRun(t, "tenant", "list")
	if got := api.lastHeaders(t).Get("Authorization"); got != "Bearer from-env" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestIdentifierEnvFallback drives the commands with DURUPAGES_TENANT and
// DURUPAGES_PAGE instead of the flags: "export DURUPAGES_PAGE=blog" must make
// every page command work bare.
func TestIdentifierEnvFallback(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	t.Setenv("DURUPAGES_TENANT", "acme")
	if got := api.mustRun(t, "tenant", "get").object(t)["id"]; got != "acme" {
		t.Fatalf("id = %v", got)
	}
	pages, _ := api.mustRun(t, "tenant", "pages").object(t)["pages"].([]any)
	if len(pages) != 1 {
		t.Fatalf("pages = %v", pages)
	}
	// page list treats it as a filter.
	pages, _ = api.mustRun(t, "page", "list").object(t)["pages"].([]any)
	if len(pages) != 1 {
		t.Fatalf("filtered pages = %v", pages)
	}

	t.Setenv("DURUPAGES_PAGE", "blog")
	if got := api.mustRun(t, "page", "get").object(t)["id"]; got != "blog" {
		t.Fatalf("id = %v", got)
	}
	if res := api.runIn(t, "s3cr3t", "secret", "put", "API_KEY"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog")["API_KEY"]; got != "s3cr3t" {
		t.Fatalf("stored %q", got)
	}
	keys, _ := api.mustRun(t, "secret", "list").object(t)["secretKeys"].([]any)
	if len(keys) != 1 || keys[0] != "API_KEY" {
		t.Fatalf("secretKeys = %v", keys)
	}
	if deps, _ := api.mustRun(t, "deployment", "list").object(t)["deployments"].([]any); len(deps) != 0 {
		t.Fatalf("deployments = %v", deps)
	}

	// An explicit flag still wins over the environment.
	seed(t, api, "acme", "shop")
	if got := api.mustRun(t, "--page", "shop", "page", "get").object(t)["id"]; got != "shop" {
		t.Fatalf("flag did not beat the env var: %v", got)
	}
}

// TestMissingIdentifierUsageErrors covers the error a command reports when
// neither the flag nor its environment variable named the object.
func TestMissingIdentifierUsageErrors(t *testing.T) {
	api := newTestAPI(t)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"tenant", "get"}, "--tenant is required (or set DURUPAGES_TENANT)"},
		{[]string{"tenant", "set"}, "--tenant is required (or set DURUPAGES_TENANT)"},
		{[]string{"tenant", "delete"}, "--tenant is required (or set DURUPAGES_TENANT)"},
		{[]string{"tenant", "pages"}, "--tenant is required (or set DURUPAGES_TENANT)"},
		{[]string{"page", "get"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"page", "set"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"page", "delete"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"page", "domains", "--clear"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"secret", "list"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"secret", "put", "API_KEY"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"secret", "delete", "API_KEY"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"secret", "bulk", "--file", "-"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"deployment", "list"}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"deployment", "upload", "--dir", "."}, "--page is required (or set DURUPAGES_PAGE)"},
		{[]string{"deployment", "activate", "dep-one"}, "--page is required (or set DURUPAGES_PAGE)"},
	} {
		res := api.run(t, tc.args...)
		if res.code != 2 {
			t.Fatalf("duru %s: exit %d (%s)", strings.Join(tc.args, " "), res.code, res.stderr)
		}
		if !strings.Contains(res.stderr, tc.want) {
			t.Fatalf("duru %s: stderr = %q, want %q", strings.Join(tc.args, " "), res.stderr, tc.want)
		}
	}
}

// TestOldPositionalFormHint covers the migration message: the identifier used
// to be the first positional argument, and an invocation shaped like the old
// one is answered with the new command line.
func TestOldPositionalFormHint(t *testing.T) {
	api := newTestAPI(t)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{
			[]string{"secret", "put", "blog", "API_KEY"},
			`duru secret put: unexpected argument "API_KEY"; the page is now a flag: duru --page blog secret put API_KEY`,
		},
		{
			[]string{"secret", "delete", "blog", "API_KEY"},
			`duru secret delete: unexpected argument "API_KEY"; the page is now a flag: duru --page blog secret delete API_KEY`,
		},
		{
			[]string{"secret", "list", "blog"},
			`duru secret list: unexpected argument "blog"; the page is now a flag: duru --page blog secret list`,
		},
		{
			[]string{"page", "get", "blog"},
			`duru page get: unexpected argument "blog"; the page is now a flag: duru --page blog page get`,
		},
		{
			[]string{"page", "domains", "blog"},
			`duru page domains: unexpected argument "blog"; the page is now a flag: duru --page blog page domains`,
		},
		{
			[]string{"deployment", "activate", "blog", "dep-one"},
			`duru deployment activate: unexpected argument "dep-one"; the page is now a flag: duru --page blog deployment activate dep-one`,
		},
		{
			[]string{"deployment", "upload", "blog"},
			`duru deployment upload: unexpected argument "blog"; the page is now a flag: duru --page blog deployment upload`,
		},
		{
			[]string{"tenant", "get", "acme"},
			`duru tenant get: unexpected argument "acme"; the tenant is now a flag: duru --tenant acme tenant get`,
		},
		{
			[]string{"tenant", "pages", "acme"},
			`duru tenant pages: unexpected argument "acme"; the tenant is now a flag: duru --tenant acme tenant pages`,
		},
	} {
		res := api.run(t, tc.args...)
		if res.code != 2 {
			t.Fatalf("duru %s: exit %d (%s)", strings.Join(tc.args, " "), res.code, res.stderr)
		}
		if !strings.Contains(res.stderr, tc.want) {
			t.Fatalf("duru %s:\n got %q\nwant %q", strings.Join(tc.args, " "), res.stderr, tc.want)
		}
	}

	// The hint is only offered when the flag is missing: with --page given, an
	// extra argument is just an extra argument.
	res := api.run(t, "--page", "blog", "secret", "put", "blog", "API_KEY")
	if res.code != 2 {
		t.Fatalf("exit %d", res.code)
	}
	if strings.Contains(res.stderr, "is now a flag") {
		t.Fatalf("stderr = %q, want the plain usage line", res.stderr)
	}

	// Two extra arguments are not an old-style invocation either.
	res = api.run(t, "secret", "put", "blog", "API_KEY", "extra")
	if res.code != 2 || strings.Contains(res.stderr, "is now a flag") {
		t.Fatalf("exit %d, stderr = %q", res.code, res.stderr)
	}
}

func TestErrorMapping(t *testing.T) {
	api := newTestAPI(t)

	// 404 with the API's own message.
	res := api.run(t, "--page", "ghost", "page", "get")
	if res.code != 1 {
		t.Fatalf("exit %d", res.code)
	}
	if want := `duru page get: page "ghost" does not exist (404 Not Found)`; !strings.Contains(res.stderr, want) {
		t.Fatalf("stderr = %q, want %q", res.stderr, want)
	}
	if res.stdout != "" {
		t.Fatalf("stdout = %q", res.stdout)
	}

	// A missing --admin-url is a usage error.
	res = runCLI(t, "", "tenant", "list")
	if res.code != 2 {
		t.Fatalf("exit %d", res.code)
	}
	if !strings.Contains(res.stderr, "--admin-url is required") {
		t.Fatalf("stderr = %q", res.stderr)
	}

	// The env var satisfies it.
	t.Setenv("DURUPAGES_ADMIN_URL", api.URL)
	if res := runCLI(t, "", "tenant", "list"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	t.Setenv("DURUPAGES_ADMIN_URL", "")

	// k=v without '=' is a usage error.
	seed(t, api, "acme", "blog")
	res = api.run(t, "--page", "blog", "page", "set", "--env", "NOEQUALS")
	if res.code != 2 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "key=value") {
		t.Fatalf("stderr = %q", res.stderr)
	}
	res = api.run(t, "--tenant", "acme", "tenant", "set", "--pod-label", "NOEQUALS")
	if res.code != 2 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}

	// A malformed --header is a usage error too.
	if res := api.run(t, "tenant", "list", "--header", "nocolon"); res.code != 2 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}

	// Unknown flags, missing arguments and extra arguments are usage errors.
	for _, args := range [][]string{
		{"tenant", "get"},
		{"tenant", "get", "a", "b"},
		{"tenant", "bogus"},
		{"tenant"},
		{"--tenant", "acme", "tenant"},
		{"page", "get", "--nope", "x"},
		{"--page", "blog", "deployment", "activate"},
		{"--page", "blog", "deployment", "activate", "a", "b"},
	} {
		if res := api.run(t, args...); res.code != 2 {
			t.Fatalf("duru %s: exit %d (%s)", strings.Join(args, " "), res.code, res.stderr)
		}
	}

	// A file that does not exist, and one with a typo in a field name.
	if res := api.run(t, "--tenant", "acme", "tenant", "set", "-f",
		filepath.Join(t.TempDir(), "no.json")); res.code != 1 {
		t.Fatalf("exit %d", res.code)
	}
	if res := api.runIn(t, `{"config":{"maxConcurency":2}}`,
		"--tenant", "acme", "tenant", "set", "-f", "-"); res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
}

func TestOverviewAndHelp(t *testing.T) {
	// No arguments: the overview on stderr, exit 2.
	res := runCLI(t, "")
	if res.code != 2 {
		t.Fatalf("exit %d", res.code)
	}
	for _, want := range []string{
		"tenant set", "page domains", "deployment upload", "health",
		"--admin-url", "--tenant ID", "--page ID",
	} {
		if !strings.Contains(res.stderr, want) {
			t.Fatalf("overview is missing %q:\n%s", want, res.stderr)
		}
	}

	// -h: the same overview on stdout, exit 0.
	res = runCLI(t, "", "-h")
	if res.code != 0 || !strings.Contains(res.stdout, "deployment activate") {
		t.Fatalf("exit %d, stdout %q", res.code, res.stdout)
	}
	if !strings.Contains(res.stdout, "duru --page blog secret put API_KEY") {
		t.Fatalf("overview does not show the new syntax:\n%s", res.stdout)
	}

	// Shared flags with no command at all.
	res = runCLI(t, "", "--page", "blog")
	if res.code != 2 || !strings.Contains(res.stderr, "missing command") {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}

	// An unknown command.
	res = runCLI(t, "", "bogus")
	if res.code != 2 || !strings.Contains(res.stderr, `unknown command "bogus"`) {
		t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
	}

	// Every subcommand prints its own flags on -h, and exits 0.
	for _, args := range [][]string{
		{"health", "-h"},
		{"tenant", "-h"},
		{"tenant", "set", "-h"},
		{"page", "-h"},
		{"page", "set", "-h"},
		{"page", "domains", "-h"},
		{"secret", "-h"},
		{"deployment", "-h"},
		{"deployment", "upload", "-h"},
	} {
		res := runCLI(t, "", args...)
		if res.code != 0 {
			t.Fatalf("duru %s: exit %d", strings.Join(args, " "), res.code)
		}
		if !strings.Contains(res.stdout, "usage: duru") {
			t.Fatalf("duru %s: stdout = %q", strings.Join(args, " "), res.stdout)
		}
	}

	// Leaf help lists the command's own flags and the shared ones.
	res = runCLI(t, "", "page", "set", "-h")
	for _, want := range []string{"-admin-url", "-clear-env", "-logs-enabled", "-replace", "-secret", "-tenant", "-page"} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("page set help is missing %q:\n%s", want, res.stdout)
		}
	}
	// Group help shows the flag-first syntax.
	res = runCLI(t, "", "secret", "-h")
	if !strings.Contains(res.stdout, "duru --page <id> secret <subcommand>") {
		t.Fatalf("secret help is missing the new syntax:\n%s", res.stdout)
	}
}

// TestSharedFlagsPosition asserts that the identifiers and the connection flags
// work before the command word, between the command and its subcommand, and
// after the positional arguments.
func TestSharedFlagsPosition(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	// Documented form: everything before the command.
	if res := runCLI(t, "v", "--admin-url", api.URL, "--page", "blog",
		"secret", "put", "API_KEY"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	// Flags after the positional argument.
	if res := runCLI(t, "v2", "secret", "put", "API_KEY",
		"--page", "blog", "--admin-url", api.URL); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog")["API_KEY"]; got != "v2" {
		t.Fatalf("stored %q", got)
	}
	// Between the group and the subcommand, and in "--flag=value" form.
	if res := runCLI(t, "", "secret", "--page=blog", "list", "--admin-url", api.URL); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if res := runCLI(t, "", "tenant", "--tenant", "acme", "get", "--admin-url", api.URL); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	// The single-dash spelling the flag package also accepts.
	if res := runCLI(t, "", "-page", "blog", "page", "get", "--admin-url", api.URL); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	// A flag value that looks like a command word is still a value.
	if res := runCLI(t, "", "--page", "list", "page", "get", "--admin-url", api.URL); res.code != 1 {
		t.Fatalf("exit %d (want a 404 for page \"list\"): %s", res.code, res.stderr)
	}
}

func TestOutputIsIndentedJSON(t *testing.T) {
	api := newTestAPI(t)
	api.mustRun(t, "--tenant", "acme", "tenant", "set", "--max-concurrency", "2")
	out := api.mustRun(t, "--tenant", "acme", "tenant", "get").stdout
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("output does not end in a newline: %q", out)
	}
	if !strings.Contains(out, "\n  \"config\": {") {
		t.Fatalf("output is not indented with two spaces:\n%s", out)
	}
}

// storedSecrets returns the secret map the provider actually holds for a page.
// The admin API never returns secret values, so this is the only way to assert
// what a write really stored.
func (a *testAPI) storedSecrets(t *testing.T, pageID string) map[string]string {
	t.Helper()
	page, err := a.provider.GetPage(t.Context(), pageID)
	if err != nil {
		t.Fatalf("GetPage(%q): %v", pageID, err)
	}
	return page.Config.Secret
}

// secretKeysOf decodes config.secretKeys out of a page document on stdout.
func secretKeysOf(t *testing.T, res result) []string {
	t.Helper()
	raw, _ := dig(t, res.object(t), "config", "secretKeys").([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("secretKeys holds a non-string: %v", raw)
		}
		out = append(out, s)
	}
	return out
}

// writeFile writes content to a fresh temporary file and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSecretPutFromPipedStdin(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	// A single trailing newline is stripped, the way `echo v | ...` implies.
	res := api.runIn(t, "s3cr3t\n", "--page", "blog", "secret", "put", "API_KEY")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog")["API_KEY"]; got != "s3cr3t" {
		t.Fatalf("stored %q, want %q", got, "s3cr3t")
	}
	if !strings.Contains(res.stderr, `secret "API_KEY" set on page "blog"`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	if keys := secretKeysOf(t, res); len(keys) != 1 || keys[0] != "API_KEY" {
		t.Fatalf("secretKeys = %v", keys)
	}

	// Only ONE trailing newline goes: a value that ends in a blank line keeps it.
	api.runIn(t, "line\n\n", "--page", "blog", "secret", "put", "MULTI")
	if got := api.storedSecrets(t, "blog")["MULTI"]; got != "line\n" {
		t.Fatalf("stored %q, want %q", got, "line\n")
	}

	// Spaces, '=' and non-ASCII survive verbatim; nothing is trimmed or split.
	awkward := "  a=b c  d  =\tパスワード ✓  "
	if res := api.runIn(t, awkward, "--page", "blog", "secret", "put", "AWKWARD"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog")["AWKWARD"]; got != awkward {
		t.Fatalf("stored %q, want %q", got, awkward)
	}

	// Overwriting one key leaves the others alone (the controller does the
	// read-modify-write, which is the whole point of this command).
	res = api.runIn(t, "rotated", "--page", "blog", "secret", "put", "API_KEY")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	stored := api.storedSecrets(t, "blog")
	if stored["API_KEY"] != "rotated" {
		t.Fatalf("API_KEY = %q", stored["API_KEY"])
	}
	if stored["AWKWARD"] != awkward || stored["MULTI"] != "line\n" {
		t.Fatalf("put clobbered the other secrets: %v", stored)
	}
	if keys := secretKeysOf(t, res); len(keys) != 3 {
		t.Fatalf("secretKeys = %v", keys)
	}
}

func TestSecretPutFromRedirectedFile(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	// `duru --page blog secret put API_KEY < file` is the same code path as a
	// pipe.
	f, err := os.Open(writeFile(t, "value.txt", "from-a-file\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var stdout, stderr bytes.Buffer
	c := &cli{
		args:   []string{"--page", "blog", "secret", "put", "API_KEY", "--admin-url", api.URL},
		stdin:  f,
		stdout: &stdout,
		stderr: &stderr,
	}
	if code := c.run(); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := api.storedSecrets(t, "blog")["API_KEY"]; got != "from-a-file" {
		t.Fatalf("stored %q", got)
	}
}

func TestSecretListAndDelete(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")
	api.runIn(t, "one", "--page", "blog", "secret", "put", "A")
	api.runIn(t, "two", "--page", "blog", "secret", "put", "B")

	res := api.mustRun(t, "--page", "blog", "secret", "list")
	keys, _ := res.object(t)["secretKeys"].([]any)
	if len(keys) != 2 || keys[0] != "A" || keys[1] != "B" {
		t.Fatalf("secretKeys = %v", keys)
	}
	if strings.Contains(res.stdout, "one") || strings.Contains(res.stdout, "two") {
		t.Fatalf("list leaked a value: %s", res.stdout)
	}

	// Deleting one key leaves the rest.
	res = api.mustRun(t, "--page", "blog", "secret", "delete", "A")
	if !strings.Contains(res.stderr, `secret "A" deleted from page "blog"`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	if keys := secretKeysOf(t, res); len(keys) != 1 || keys[0] != "B" {
		t.Fatalf("secretKeys = %v", keys)
	}
	stored := api.storedSecrets(t, "blog")
	if _, ok := stored["A"]; ok {
		t.Fatalf("A survived the delete: %v", stored)
	}
	if stored["B"] != "two" {
		t.Fatalf("B was touched: %v", stored)
	}

	// Deleting a name that is not there is idempotent: the API answers 200 with
	// the unchanged page.
	res = api.mustRun(t, "--page", "blog", "secret", "delete", "GHOST")
	if keys := secretKeysOf(t, res); len(keys) != 1 || keys[0] != "B" {
		t.Fatalf("secretKeys = %v", keys)
	}
	if got := api.storedSecrets(t, "blog"); len(got) != 1 || got["B"] != "two" {
		t.Fatalf("secrets after deleting a missing key: %v", got)
	}

	// A page that does not exist is a 404, not a usage error.
	if res := api.run(t, "--page", "ghost-page", "secret", "delete", "B"); res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if res := api.run(t, "--page", "ghost-page", "secret", "list"); res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}

	// A page with no secrets at all lists an empty set.
	api.mustRun(t, "--page", "blog", "secret", "delete", "B")
	res = api.mustRun(t, "--page", "blog", "secret", "list")
	if keys, _ := res.object(t)["secretKeys"].([]any); len(keys) != 0 {
		t.Fatalf("secretKeys = %v", keys)
	}
}

func TestSecretBulkJSON(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")
	api.runIn(t, "gone", "--page", "blog", "secret", "put", "OLD")

	path := writeFile(t, "secrets.json", `{"API_KEY":"s3cr3t","DB_PASSWORD":"hunter2"}`)
	res := api.mustRun(t, "--page", "blog", "secret", "bulk", "--file", path)
	if !strings.Contains(res.stderr, `page "blog" secrets updated (2 set)`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	stored := api.storedSecrets(t, "blog")
	if stored["API_KEY"] != "s3cr3t" || stored["DB_PASSWORD"] != "hunter2" {
		t.Fatalf("stored = %v", stored)
	}
	// Default bulk upserts, like `wrangler secret bulk`: a secret the file does
	// not mention survives.
	if stored["OLD"] != "gone" {
		t.Fatalf("bulk dropped an unmentioned secret: %v", stored)
	}
	if keys := secretKeysOf(t, res); len(keys) != 3 {
		t.Fatalf("secretKeys = %v", keys)
	}

	// --replace makes it a whole-map write: unmentioned secrets go away.
	res = api.mustRun(t, "--page", "blog", "secret", "bulk", "--replace", "--file", path)
	if !strings.Contains(res.stderr, `page "blog" secrets replaced (2)`) {
		t.Fatalf("stderr = %q", res.stderr)
	}
	stored = api.storedSecrets(t, "blog")
	if len(stored) != 2 {
		t.Fatalf("--replace kept extra secrets: %v", stored)
	}

	// An explicit empty object removes every secret, but only as a replacement:
	// upserting nothing changes nothing.
	if res := api.runIn(t, "{}", "--page", "blog", "secret", "bulk", "-f", "-"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog"); len(got) != 2 {
		t.Fatalf("an upsert of {} changed the map: %v", got)
	}
	if res := api.runIn(t, "{}", "--page", "blog", "secret", "bulk", "--replace", "-f", "-"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog"); len(got) != 0 {
		t.Fatalf("secrets after --replace {} = %v", got)
	}
}

// TestSecretBulkNullDeletes covers wrangler's convention that a null value in a
// JSON bulk file removes that one secret, and that a null is refused where the
// whole map is being replaced (there it cannot mean anything).
func TestSecretBulkNullDeletes(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")
	api.runIn(t, "a", "--page", "blog", "secret", "put", "KEEP")
	api.runIn(t, "b", "--page", "blog", "secret", "put", "DROP")

	path := writeFile(t, "patch.json", `{"DROP":null,"ADDED":"new"}`)
	res := api.mustRun(t, "--page", "blog", "secret", "bulk", "--file", path)
	if !strings.Contains(res.stderr, "1 set, 1 removed") {
		t.Fatalf("stderr = %q", res.stderr)
	}
	stored := api.storedSecrets(t, "blog")
	if _, ok := stored["DROP"]; ok {
		t.Fatalf("null did not delete: %v", stored)
	}
	if stored["KEEP"] != "a" || stored["ADDED"] != "new" {
		t.Fatalf("stored = %v", stored)
	}

	// The same file with --replace is refused rather than silently dropping
	// the key, because "delete this one" contradicts "these are all of them".
	res = api.run(t, "--page", "blog", "secret", "bulk", "--replace", "--file", path)
	if res.code != 1 || !strings.Contains(res.stderr, "null") {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	// And deploy --secrets-file, which also replaces, refuses it too.
	if got := api.storedSecrets(t, "blog"); got["KEEP"] != "a" {
		t.Fatalf("a rejected --replace still wrote: %v", got)
	}
}

func TestSecretBulkDotenv(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	const env = "" +
		"# a comment\n" +
		"\n" +
		"API_KEY=s3cr3t\n" +
		"  DB_PASSWORD = \"hunter2\"  \n" +
		"QUOTED='  spaced  '\n" +
		"EQUALS=a=b=c\n" +
		"EMPTY=\n" +
		"   # indented comment\n" +
		"UNICODE=パスワード\n"

	path := writeFile(t, ".env", env)
	res := api.mustRun(t, "--page", "blog", "secret", "bulk", "--file", path)
	if !strings.Contains(res.stderr, "secrets updated (6 set)") {
		t.Fatalf("stderr = %q", res.stderr)
	}
	stored := api.storedSecrets(t, "blog")
	want := map[string]string{
		"API_KEY":     "s3cr3t",
		"DB_PASSWORD": "hunter2",
		"QUOTED":      "  spaced  ",
		"EQUALS":      "a=b=c",
		"EMPTY":       "",
		"UNICODE":     "パスワード",
	}
	if len(stored) != len(want) {
		t.Fatalf("stored = %v", stored)
	}
	for k, v := range want {
		if stored[k] != v {
			t.Fatalf("stored[%q] = %q, want %q", k, stored[k], v)
		}
	}

	// The same document over stdin with --file -.
	seed(t, api, "acme", "other")
	if res := api.runIn(t, env, "--page", "other", "secret", "bulk", "--file", "-"); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "other"); len(got) != 6 || got["QUOTED"] != "  spaced  " {
		t.Fatalf("stored = %v", got)
	}
}

func TestSecretFileErrors(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	// A line without '=' names its line number.
	res := api.runIn(t, "A=1\n\nOOPS\n", "--page", "blog", "secret", "bulk", "--file", "-")
	if res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	for _, want := range []string{"<stdin>:3", "OOPS", "KEY=value"} {
		if !strings.Contains(res.stderr, want) {
			t.Fatalf("stderr = %q, want %q", res.stderr, want)
		}
	}

	// An invalid binding name, in either format, is rejected client-side.
	if res := api.runIn(t, "not-a-name=1\n", "--page", "blog", "secret", "bulk", "-f", "-"); res.code != 1 ||
		!strings.Contains(res.stderr, "invalid secret name") {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if res := api.runIn(t, `{"1BAD":"x"}`, "--page", "blog", "secret", "bulk", "-f", "-"); res.code != 1 ||
		!strings.Contains(res.stderr, "invalid secret name") {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}

	// Malformed JSON, and a JSON object with a non-string value.
	if res := api.runIn(t, `{"A":`, "--page", "blog", "secret", "bulk", "-f", "-"); res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if res := api.runIn(t, `{"A":3}`, "--page", "blog", "secret", "bulk", "-f", "-"); res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}

	// A blank file never silently wipes the secret set.
	if res := api.runIn(t, "\n  \n", "--page", "blog", "secret", "bulk", "-f", "-"); res.code != 1 ||
		!strings.Contains(res.stderr, "no secrets in this file") {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if res := api.runIn(t, "# only a comment\n", "--page", "blog", "secret", "bulk", "-f", "-"); res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}

	// A file that does not exist.
	if res := api.run(t, "--page", "blog", "secret", "bulk", "--file",
		filepath.Join(t.TempDir(), "nope.env")); res.code != 1 {
		t.Fatalf("exit %d", res.code)
	}

	// The entry cap is the server's, and its message is what the user sees.
	var big strings.Builder
	for i := 0; i < 101; i++ {
		fmt.Fprintf(&big, "K%d=v\n", i)
	}
	res = api.runIn(t, big.String(), "--page", "blog", "secret", "bulk", "-f", "-")
	if res.code != 1 || !strings.Contains(res.stderr, "too many secrets") {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
}

func TestSecretUsageErrors(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	// An empty value is a usage error, not an empty secret.
	res := api.runIn(t, "", "--page", "blog", "secret", "put", "API_KEY")
	if res.code != 2 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "is empty") {
		t.Fatalf("stderr = %q", res.stderr)
	}
	// A lone newline is an empty value too.
	if res := api.runIn(t, "\n", "--page", "blog", "secret", "put", "API_KEY"); res.code != 2 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog"); len(got) != 0 {
		t.Fatalf("an empty value was stored: %v", got)
	}

	// An invalid name is rejected before the value is even read.
	for _, name := range []string{"not-a-name", "1LEADING", "with space", ""} {
		if res := api.runIn(t, "v", "--page", "blog", "secret", "put", name); res.code != 2 {
			t.Fatalf("put %q: exit %d (%s)", name, res.code, res.stderr)
		}
		if res := api.run(t, "--page", "blog", "secret", "delete", name); res.code != 2 {
			t.Fatalf("delete %q: exit %d (%s)", name, res.code, res.stderr)
		}
	}

	// bulk without a file cannot guess an empty map.
	res = api.run(t, "--page", "blog", "secret", "bulk")
	if res.code != 2 || !strings.Contains(res.stderr, "--file") {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}

	// Missing, extra and unknown arguments.
	for _, args := range [][]string{
		{"secret"},
		{"secret", "bogus"},
		{"--page", "blog", "secret", "list", "extra"},
		{"--page", "blog", "secret", "put"},
		{"--page", "blog", "secret", "put", "A", "extra"},
		{"--page", "blog", "secret", "delete"},
		{"--page", "blog", "secret", "bulk", "--nope"},
	} {
		if res := api.run(t, args...); res.code != 2 {
			t.Fatalf("duru %s: exit %d (%s)", strings.Join(args, " "), res.code, res.stderr)
		}
	}

	// A missing --admin-url is a usage error here too.
	if res := runCLI(t, "v", "--page", "blog", "secret", "put", "API_KEY"); res.code != 2 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
}

func TestSecretHelp(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	// The token reaches the server whichever side of the command it is on.
	api.mustRun(t, "--page", "blog", "secret", "list", "--token", "s3cret")
	if got := api.lastHeaders(t).Get("Authorization"); got != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", got)
	}

	// Help for the group and every subcommand.
	for _, args := range [][]string{
		{"secret", "-h"},
		{"secret", "list", "-h"},
		{"secret", "put", "-h"},
		{"secret", "delete", "-h"},
		{"secret", "bulk", "-h"},
	} {
		res := runCLI(t, "", args...)
		if res.code != 0 {
			t.Fatalf("duru %s: exit %d", strings.Join(args, " "), res.code)
		}
		if !strings.Contains(res.stdout, "usage: duru") || !strings.Contains(res.stdout, "secret") {
			t.Fatalf("duru %s: stdout = %q", strings.Join(args, " "), res.stdout)
		}
	}
	if res := runCLI(t, "", "secret", "bulk", "-h"); !strings.Contains(res.stdout, "-file") {
		t.Fatalf("bulk help is missing --file:\n%s", res.stdout)
	}
	if res := runCLI(t, "", "-h"); !strings.Contains(res.stdout, "secret bulk") {
		t.Fatalf("overview is missing the secret group:\n%s", res.stdout)
	}
	if res := runCLI(t, ""); !strings.Contains(res.stderr, "secret put") {
		t.Fatalf("overview is missing the secret group:\n%s", res.stderr)
	}
}

func TestDeploySecretsFileAdminMode(t *testing.T) {
	api := newTestAPI(t)
	dir := writeBuildOutput(t)
	path := writeFile(t, ".env.production", "API_KEY=s3cr3t\nDB_PASSWORD=hunter2\n")

	// deploy creates the tenant and the page, then replaces the secret set.
	// This is the command line the README, the e2e scripts and CI use, and it
	// is unchanged by the move of the identifiers into the shared flags.
	res := runCLI(t, "", "deploy", "--dir", dir, "--tenant", "acme", "--page", "blog",
		"--admin-url", api.URL, "--deployment", "dep-one", "--secrets-file", path)
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	stored := api.storedSecrets(t, "blog")
	if len(stored) != 2 || stored["API_KEY"] != "s3cr3t" || stored["DB_PASSWORD"] != "hunter2" {
		t.Fatalf("stored = %v", stored)
	}
	page, err := api.provider.GetPage(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if page.ActiveDeploymentID != "dep-one" {
		t.Fatalf("activeDeploymentId = %q", page.ActiveDeploymentID)
	}

	// A redeploy with a different file replaces the whole map, and keeps the
	// rest of the page configuration.
	api.mustRun(t, "--page", "blog", "page", "set", "--env", "STAGE=prod")
	path = writeFile(t, ".env.next", `{"ONLY":"one"}`)
	res = runCLI(t, "", "deploy", "--dir", dir, "--tenant", "acme", "--page", "blog",
		"--admin-url", api.URL, "--deployment", "dep-two", "--secrets-file", path)
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	stored = api.storedSecrets(t, "blog")
	if len(stored) != 1 || stored["ONLY"] != "one" {
		t.Fatalf("stored = %v", stored)
	}
	page, err = api.provider.GetPage(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if page.Config.Env["STAGE"] != "prod" {
		t.Fatalf("deploy wiped the env: %v", page.Config.Env)
	}
	if page.ActiveDeploymentID != "dep-two" {
		t.Fatalf("activeDeploymentId = %q", page.ActiveDeploymentID)
	}

	// A deploy without the flag leaves the secrets untouched.
	res = runCLI(t, "", "deploy", "--dir", dir, "--tenant", "acme", "--page", "blog",
		"--admin-url", api.URL, "--deployment", "dep-three")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if got := api.storedSecrets(t, "blog"); len(got) != 1 || got["ONLY"] != "one" {
		t.Fatalf("stored = %v", got)
	}

	// The identifiers may also lead the command line, like every other command.
	res = runCLI(t, "", "--tenant", "acme", "--page", "blog", "deploy",
		"--dir", dir, "--admin-url", api.URL, "--deployment", "dep-four")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	page, err = api.provider.GetPage(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if page.ActiveDeploymentID != "dep-four" {
		t.Fatalf("activeDeploymentId = %q", page.ActiveDeploymentID)
	}
}

// TestSecretValuesNeverPrinted drives every command that touches a secret and
// asserts that no value ever reaches stdout or stderr.
func TestSecretValuesNeverPrinted(t *testing.T) {
	api := newTestAPI(t)
	seed(t, api, "acme", "blog")

	const (
		putValue  = "put-value-9f3a"
		bulkValue = "bulk-value-7c1d"
		envValue  = "deploy-value-4b2e"
	)
	bulkFile := writeFile(t, "secrets.json", `{"BULK":"`+bulkValue+`"}`)
	envFile := writeFile(t, ".env", "DEPLOYED="+envValue+"\n")
	dir := writeBuildOutput(t)

	var out []string
	record := func(res result) { out = append(out, res.stdout, res.stderr) }

	record(api.runIn(t, putValue, "--page", "blog", "secret", "put", "PUT"))
	record(api.mustRun(t, "--page", "blog", "secret", "list"))
	record(api.mustRun(t, "--page", "blog", "page", "get"))
	record(api.mustRun(t, "--page", "blog", "secret", "bulk", "--file", bulkFile))
	record(api.mustRun(t, "--page", "blog", "secret", "list"))
	record(api.mustRun(t, "--page", "blog", "secret", "delete", "BULK"))
	record(api.mustRun(t, "page", "list"))
	record(runCLI(t, "", "deploy", "--dir", dir, "--tenant", "acme", "--page", "blog",
		"--admin-url", api.URL, "--secrets-file", envFile))
	record(api.mustRun(t, "--page", "blog", "secret", "list"))
	record(api.mustRun(t, "--page", "blog", "page", "get"))

	for i, s := range out {
		for _, secret := range []string{putValue, bulkValue, envValue} {
			if strings.Contains(s, secret) {
				t.Fatalf("stream %d leaked %q:\n%s", i, secret, s)
			}
		}
	}
	// The values really did make it to the store, so the assertion above is
	// about redaction and not about nothing having happened.
	if got := api.storedSecrets(t, "blog")["DEPLOYED"]; got != envValue {
		t.Fatalf("stored = %q", got)
	}
}

func TestSecretFileFormats(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		got, err := parseSecretFile([]byte("  \n {\"A\":\"1\",\"B\":\"\"} \n"), "f.json")
		if err != nil {
			t.Fatal(err)
		}
		if v := derefSecrets(got); len(v) != 2 || v["A"] != "1" || v["B"] != "" {
			t.Fatalf("got = %v", v)
		}
	})
	t.Run("dotenv", func(t *testing.T) {
		got, err := parseSecretFile([]byte("\xef\xbb\xbfA=1\r\nB='x y'\r\n#c\r\n_C=\"{not json}\"\n"), ".env")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"A": "1", "B": "x y", "_C": "{not json}"}
		have := derefSecrets(got)
		if len(have) != len(want) {
			t.Fatalf("got = %v", have)
		}
		for k, v := range want {
			if have[k] != v {
				t.Fatalf("got[%q] = %q, want %q", k, have[k], v)
			}
		}
	})
	t.Run("unbalanced quotes are kept", func(t *testing.T) {
		got, err := parseSecretFile([]byte(`A="x`+"\nB=y\"\nC=\"\n"), ".env")
		if err != nil {
			t.Fatal(err)
		}
		if v := derefSecrets(got); v["A"] != `"x` || v["B"] != `y"` || v["C"] != `"` {
			t.Fatalf("got = %v", v)
		}
	})
	t.Run("last duplicate wins", func(t *testing.T) {
		got, err := parseSecretFile([]byte("A=1\nA=2\n"), ".env")
		if err != nil {
			t.Fatal(err)
		}
		if v := derefSecrets(got); len(v) != 1 || v["A"] != "2" {
			t.Fatalf("got = %v", v)
		}
	})
	t.Run("line number", func(t *testing.T) {
		_, err := parseSecretFile([]byte("A=1\n# c\n\nBROKEN\n"), "x.env")
		if err == nil || !strings.Contains(err.Error(), "x.env:4") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		cmd  string
		rest []string
	}{
		{[]string{"health"}, "health", []string{}},
		{[]string{"--page", "blog", "secret", "put", "A"}, "secret", []string{"--page", "blog", "put", "A"}},
		{[]string{"--page=blog", "page", "get"}, "page", []string{"--page=blog", "get"}},
		{[]string{"-page", "blog", "page", "get"}, "page", []string{"-page", "blog", "get"}},
		{[]string{"secret", "put", "A", "--page", "blog"}, "secret", []string{"put", "A", "--page", "blog"}},
		{[]string{"--tenant", "acme", "--page", "blog", "deploy", "--dir", "."}, "deploy",
			[]string{"--tenant", "acme", "--page", "blog", "--dir", "."}},
		{[]string{"--page", "blog"}, "", []string{"--page", "blog"}},
		{[]string{"--page"}, "", []string{"--page"}},
		{[]string{"-h"}, "-h", []string{}},
		{[]string{"--nope", "secret"}, "--nope", []string{"secret"}},
		{nil, "", []string{}},
	} {
		cmd, rest := splitCommand(tc.in)
		if cmd != tc.cmd || strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Fatalf("splitCommand(%v) = %q, %v; want %q, %v", tc.in, cmd, rest, tc.cmd, tc.rest)
		}
	}
}

func TestTrimOneNewline(t *testing.T) {
	for in, want := range map[string]string{
		"v":        "v",
		"v\n":      "v",
		"v\r\n":    "v",
		"v\n\n":    "v\n",
		"":         "",
		"\n":       "",
		"v\r":      "v\r",
		" v ":      " v ",
		"a\nb\n":   "a\nb",
		"\r\n\r\n": "\r\n",
	} {
		if got := trimOneNewline(in); got != want {
			t.Fatalf("trimOneNewline(%q) = %q, want %q", in, got, want)
		}
	}
}

// derefSecrets flattens a parsed secret file for comparison, rendering a null
// (a deletion in `secret bulk`) as the literal "<null>" so a test can tell it
// apart from an empty value.
func derefSecrets(in map[string]*string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = "<null>"
			continue
		}
		out[k] = *v
	}
	return out
}
