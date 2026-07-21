// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/storage/memstorage"
)

// writeTree writes the given path->content map under root, creating parent
// directories as needed. A path ending in "/" (with empty content) creates an
// empty directory.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", p, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

func hexHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var testOpts = ScanOptions{TenantID: "acme", PageID: "blog", DeploymentID: "dep_1"}

func TestScanPureStatic(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":    "<h1>hi</h1>",
		"assets/app.js": "console.log(1)",
		"assets/a.css":  "body{}",
	})

	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	m := s.Manifest
	if m.HasWorker {
		t.Error("HasWorker should be false")
	}
	if len(s.workers) != 0 {
		t.Errorf("expected no worker modules, got %d", len(s.workers))
	}
	if len(m.Static) != 3 {
		t.Fatalf("expected 3 static entries, got %d: %v", len(m.Static), m.Static)
	}
	e, ok := m.Static["/index.html"]
	if !ok {
		t.Fatal("missing /index.html")
	}
	if e.Hash != hexHash("<h1>hi</h1>") {
		t.Errorf("bad hash: %s", e.Hash)
	}
	if e.Size != int64(len("<h1>hi</h1>")) {
		t.Errorf("bad size: %d", e.Size)
	}
	if e.ContentType != "text/html; charset=utf-8" {
		t.Errorf("bad content type: %s", e.ContentType)
	}
	if ct := m.Static["/assets/app.js"].ContentType; ct != "text/javascript; charset=utf-8" {
		t.Errorf("bad js content type: %s", ct)
	}
	if ct := m.Static["/assets/a.css"].ContentType; ct != "text/css; charset=utf-8" {
		t.Errorf("bad css content type: %s", ct)
	}
}

func TestScanSpecialFilesNotAssets(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":   "x",
		"_headers":     "/assets/*\n  Cache-Control: max-age=31536000\n  ! X-Robots-Tag\n",
		"_redirects":   "/old/* /new/:splat 301\n",
		"_routes.json": `{"version":1,"include":["/*"],"exclude":["/assets/*"]}`,
		"_worker.js":   "export default { async fetch() { return new Response('hi') } }",
	})

	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	m := s.Manifest
	if len(m.Static) != 1 {
		t.Fatalf("special files must not be assets; got %d static: %v", len(m.Static), m.Static)
	}
	if _, ok := m.Static["/index.html"]; !ok {
		t.Error("index.html should be the only static asset")
	}
	// Routes parsed.
	if m.Routes == nil || len(m.Routes.Exclude) != 1 || m.Routes.Exclude[0] != "/assets/*" {
		t.Errorf("routes not parsed: %+v", m.Routes)
	}
	// Redirect parsed.
	if len(m.Redirects) != 1 || m.Redirects[0].Status != 301 || m.Redirects[0].Destination != "/new/:splat" {
		t.Errorf("redirects not parsed: %+v", m.Redirects)
	}
	// Headers parsed (spot-check set + unset).
	if len(m.Headers) != 1 {
		t.Fatalf("headers not parsed: %+v", m.Headers)
	}
	h := m.Headers[0]
	if h.Pattern != "/assets/*" || h.Set["Cache-Control"] != "max-age=31536000" {
		t.Errorf("header set wrong: %+v", h)
	}
	if len(h.Unset) != 1 || h.Unset[0] != "X-Robots-Tag" {
		t.Errorf("header unset wrong: %+v", h.Unset)
	}
	if !m.HasWorker {
		t.Error("HasWorker should be true")
	}
}

func TestScanSingleWorker(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html": "x",
		"_worker.js": "export default {}",
	})
	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !s.Manifest.HasWorker {
		t.Error("HasWorker should be true")
	}
	if len(s.workers) != 1 || s.workers[0].tarRel != "index.js" {
		t.Fatalf("expected single worker module index.js, got %+v", s.workers)
	}
}

func TestScanMultiModuleWorker(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":             "x",
		"_worker.js/index.js":    "import './lib/util.js'; export default {}",
		"_worker.js/lib/util.js": "export const x = 1",
	})
	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !s.Manifest.HasWorker {
		t.Error("HasWorker should be true")
	}
	got := map[string]bool{}
	for _, w := range s.workers {
		got[w.tarRel] = true
	}
	if !got["index.js"] || !got["lib/util.js"] || len(got) != 2 {
		t.Fatalf("unexpected worker modules: %+v", s.workers)
	}
	// The worker directory must not appear among static assets.
	if len(s.Manifest.Static) != 1 {
		t.Errorf("worker dir leaked into static: %v", s.Manifest.Static)
	}
}

func TestScanMultiModuleMissingEntry(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"_worker.js/main.js": "export default {}",
	})
	if _, err := Scan(dir, testOpts); err == nil {
		t.Fatal("expected error for missing index.js entry")
	}
}

func TestScanFunctionsOnlyError(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":             "x",
		"functions/api/hello.js": "export function onRequest() {}",
	})
	_, err := Scan(dir, testOpts)
	if err == nil {
		t.Fatal("expected error for functions/ without compiled worker")
	}
	if !strings.Contains(err.Error(), "wrangler pages functions build") {
		t.Errorf("error should mention precompilation: %v", err)
	}
}

func TestScanWorkerIgnoresFunctions(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"_worker.js":         "export default {}",
		"functions/api/x.js": "export function onRequest() {}",
	})
	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !s.Manifest.HasWorker {
		t.Error("HasWorker should be true")
	}
	// functions/ is ignored and must not appear as static assets.
	if len(s.Manifest.Static) != 0 {
		t.Errorf("functions/ leaked into static: %v", s.Manifest.Static)
	}
}

func TestScanBadSpecialFileAborts(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"_routes.json": `{"version":2,"include":["/*"]}`, // unsupported version
	})
	_, err := Scan(dir, testOpts)
	if err == nil {
		t.Fatal("expected error for bad _routes.json")
	}
	if !strings.Contains(err.Error(), "_routes.json") {
		t.Errorf("error should name the file: %v", err)
	}
}

func TestScanCompatTOML(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html": "x",
		"wrangler.toml": `
name = "my-app"   # a comment
compatibility_date = "2024-05-01" # date
compatibility_flags = ["nodejs_compat", "streams_enable_constructors"] # flags

[vars]
FOO = "bar"
`,
	})
	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	c := s.Manifest.Compat
	if c.Date != "2024-05-01" {
		t.Errorf("compat date: %q", c.Date)
	}
	if len(c.Flags) != 2 || c.Flags[0] != "nodejs_compat" || c.Flags[1] != "streams_enable_constructors" {
		t.Errorf("compat flags: %v", c.Flags)
	}
	// wrangler.toml must not be a static asset.
	if _, ok := s.Manifest.Static["/wrangler.toml"]; ok {
		t.Error("wrangler.toml leaked into static assets")
	}
}

func TestScanCompatJSONC(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html": "x",
		"wrangler.jsonc": `{
  // config
  "name": "my-app",
  "compatibility_date": "2023-10-30",
  /* block */
  "compatibility_flags": ["nodejs_compat"]
}`,
	})
	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	c := s.Manifest.Compat
	if c.Date != "2023-10-30" || len(c.Flags) != 1 || c.Flags[0] != "nodejs_compat" {
		t.Errorf("jsonc compat: %+v", c)
	}
}

// readTar returns the tar's entries as name->body (dirs have nil body) and the
// ordered list of names.
func readTar(t *testing.T, data []byte) (map[string][]byte, []string, map[string]*tar.Header) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	bodies := map[string][]byte{}
	hdrs := map[string]*tar.Header{}
	var order []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		order = append(order, h.Name)
		hdrs[h.Name] = h
		if h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("tar read: %v", err)
			}
			bodies[h.Name] = b
		}
	}
	return bodies, order, hdrs
}

func TestTarLayout(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":          "<h1>hi</h1>",
		"assets/app.js":       "console.log(1)",
		"_worker.js/index.js": "export default {}",
		"_worker.js/lib/u.js": "export const x=1",
	})
	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteWorkerTar(&buf, s); err != nil {
		t.Fatalf("WriteWorkerTar: %v", err)
	}
	bodies, order, hdrs := readTar(t, buf.Bytes())

	// Expected files present with correct content.
	if string(bodies["manifest.json"]) == "" {
		t.Error("manifest.json missing or empty")
	}
	if string(bodies["worker/index.js"]) != "export default {}" {
		t.Errorf("worker/index.js body wrong: %q", bodies["worker/index.js"])
	}
	if string(bodies["worker/lib/u.js"]) != "export const x=1" {
		t.Errorf("worker/lib/u.js body wrong: %q", bodies["worker/lib/u.js"])
	}
	// Static assets are content-addressed by the manifest hash (what the
	// shim's env.ASSETS service opens).
	for reqPath, want := range map[string]string{"/index.html": "<h1>hi</h1>", "/assets/app.js": "console.log(1)"} {
		entry, ok := s.Manifest.Static[reqPath]
		if !ok {
			t.Fatalf("manifest missing %s", reqPath)
		}
		if string(bodies["static/"+entry.Hash]) != want {
			t.Errorf("static/%s (%s) body wrong: %q", entry.Hash, reqPath, bodies["static/"+entry.Hash])
		}
	}

	// Sorted order.
	if !sort.StringsAreSorted(order) {
		t.Errorf("tar entries not sorted: %v", order)
	}

	// Modes and mtimes.
	for name, h := range hdrs {
		if !h.ModTime.Equal(epoch) {
			t.Errorf("%s: mtime not epoch: %v", name, h.ModTime)
		}
		if h.Typeflag == tar.TypeDir {
			if !strings.HasSuffix(h.Name, "/") {
				t.Errorf("dir %s missing trailing slash", h.Name)
			}
			if h.Mode != 0o755 {
				t.Errorf("dir %s mode %o", h.Name, h.Mode)
			}
		} else if h.Mode != 0o644 {
			t.Errorf("file %s mode %o", h.Name, h.Mode)
		}
	}

	// Directory entries present.
	if _, ok := hdrs["worker/"]; !ok {
		t.Error("missing worker/ dir entry")
	}
	if _, ok := hdrs["worker/lib/"]; !ok {
		t.Error("missing worker/lib/ dir entry")
	}
	if _, ok := hdrs["static/"]; !ok {
		t.Error("missing static/ dir entry")
	}
}

func TestTarDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":          "a",
		"b/c.js":              "b",
		"_worker.js/index.js": "w",
		"wrangler.toml":       `compatibility_date = "2024-01-01"`,
	})
	s, err := Scan(dir, testOpts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var a, b bytes.Buffer
	if err := WriteWorkerTar(&a, s); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkerTar(&b, s); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("tar output is not byte-identical across runs")
	}
}

func TestUploadKeys(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":    "<h1>hi</h1>",
		"assets/app.js": "console.log(1)",
		"_worker.js":    "export default {}",
	})
	st := memstorage.New()
	m, err := Deploy(context.Background(), st, dir, testOpts)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	prefix := "tenants/acme/pages/blog/deployments/dep_1/"
	objs, _ := st.List(context.Background(), prefix)
	got := map[string]storage.ObjectInfo{}
	for _, o := range objs {
		got[o.Key] = o
	}

	// manifest.json
	if o, ok := got[prefix+"manifest.json"]; !ok || o.ContentType != "application/json" {
		t.Errorf("manifest.json missing or wrong content type: %+v", o)
	}
	// worker.tar
	if o, ok := got[prefix+"worker.tar"]; !ok || o.ContentType != "application/x-tar" {
		t.Errorf("worker.tar missing or wrong content type: %+v", o)
	}
	// static objects, content-addressed.
	for _, e := range m.Static {
		key := prefix + "static/" + e.Hash
		if _, ok := got[key]; !ok {
			t.Errorf("static object %s missing", key)
		}
	}
	// Total: manifest + worker.tar + 2 static.
	if len(objs) != 4 {
		t.Errorf("expected 4 objects, got %d: %v", len(objs), keysOf(objs))
	}
}

func TestUploadNoWorkerNoTar(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "x"})
	st := memstorage.New()
	if _, err := Deploy(context.Background(), st, dir, testOpts); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	prefix := "tenants/acme/pages/blog/deployments/dep_1/"
	if _, _, err := st.Get(context.Background(), prefix+"worker.tar"); err == nil {
		t.Error("worker.tar must not be uploaded when there is no worker")
	}
}

// countingStore wraps a Storage and counts Put calls per key.
type countingStore struct {
	storage.Storage
	mu   sync.Mutex
	puts map[string]int
}

func (c *countingStore) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	c.mu.Lock()
	c.puts[key]++
	c.mu.Unlock()
	return c.Storage.Put(ctx, key, r, size, ct)
}

func TestUploadContentAddressedDedup(t *testing.T) {
	dir := t.TempDir()
	// Same content under two different request paths => one static object.
	writeTree(t, dir, map[string]string{
		"a.txt":     "identical",
		"sub/b.txt": "identical",
	})
	st := &countingStore{Storage: memstorage.New(), puts: map[string]int{}}
	m, err := Deploy(context.Background(), st, dir, testOpts)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	hash := hexHash("identical")
	key := fmt.Sprintf(storage.StaticKeyFmt, "acme", "blog", "dep_1", hash)
	if st.puts[key] != 1 {
		t.Errorf("expected exactly 1 Put for shared content, got %d", st.puts[key])
	}
	// Manifest still maps both paths to the same hash.
	if m.Static["/a.txt"].Hash != hash || m.Static["/sub/b.txt"].Hash != hash {
		t.Errorf("both paths should share the hash: %+v", m.Static)
	}

	// Re-running the deployment is idempotent: existing objects are skipped.
	before := st.puts[key]
	s, _ := Scan(dir, testOpts)
	if err := Upload(context.Background(), st, s); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if st.puts[key] != before {
		t.Errorf("existing static object should not be re-uploaded: %d -> %d", before, st.puts[key])
	}
}

func keysOf(objs []storage.ObjectInfo) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}
