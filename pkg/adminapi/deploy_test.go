// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/storage/memstorage"
)

// tarEntry describes one entry of a test archive.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

// buildTar renders the entries as an uncompressed tar stream.
func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: flag,
			Mode:     0o644,
			Linkname: e.linkname,
			ModTime:  fixedNow,
		}
		switch flag {
		case tar.TypeReg:
			hdr.Size = int64(len(e.body))
		case tar.TypeDir:
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", e.name, err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// gzipBytes compresses b with gzip.
func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// site is the archive content shared by the upload tests.
var site = []tarEntry{
	{name: "index.html", body: "<h1>hello</h1>"},
	{name: "assets/app.css", body: "body{color:red}"},
	{name: "_worker.js", body: "export default { fetch() {} }"},
}

// withPrefix re-roots the entries under dir/ and prepends the directory entry.
func withPrefix(dir string, entries []tarEntry) []tarEntry {
	out := []tarEntry{{name: dir + "/", typeflag: tar.TypeDir}}
	for _, e := range entries {
		e.name = dir + "/" + e.name
		out = append(out, e)
	}
	return out
}

// deployFixture wires a server with a seeded page and in-memory storage.
type deployFixture struct {
	f  *fakeProvider
	st *memstorage.Store
	s  *Server
}

func newDeployFixture(t *testing.T, opts ...func(*Options)) *deployFixture {
	t.Helper()
	f := newFakeProvider()
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme"})
	st := memstorage.New()
	o := Options{Provider: f, Admin: f, Storage: st}
	for _, fn := range opts {
		fn(&o)
	}
	return &deployFixture{f: f, st: st, s: newTestServer(t, o)}
}

// upload POSTs an archive to the deployment route.
func (d *deployFixture) upload(t *testing.T, target string, body []byte, contentType string) *bytes.Buffer {
	t.Helper()
	rec := do(t, d.s, http.MethodPost, target, bytes.NewReader(body), contentType)
	requireStatus(t, rec, http.StatusCreated)
	return rec.Body
}

// mustGet returns the stored object body, failing when the key is absent.
func mustGet(t *testing.T, st *memstorage.Store, key string) []byte {
	t.Helper()
	rc, _, err := st.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("storage get %q: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("storage read %q: %v", key, err)
	}
	return b
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestUploadDeploymentEndToEnd(t *testing.T) {
	d := newDeployFixture(t)
	body := d.upload(t, "/v1/pages/blog/deployments", buildTar(t, site...), "application/x-tar")

	var resp deployResponse
	if err := json.Unmarshal(body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", body.String(), err)
	}
	wantID := fmt.Sprintf("dep-%d", fixedNow.UnixNano())
	if resp.DeploymentID != wantID {
		t.Fatalf("deploymentId = %q, want %q", resp.DeploymentID, wantID)
	}
	if resp.PageID != "blog" || resp.TenantID != "acme" || !resp.Activated {
		t.Fatalf("response = %+v", resp)
	}
	if !resp.Manifest.HasWorker || resp.Manifest.StaticFileCount != 2 {
		t.Fatalf("manifest summary = %+v", resp.Manifest)
	}

	// Objects landed under the canonical key layout.
	mKey := fmt.Sprintf(storage.ManifestKeyFmt, "acme", "blog", wantID)
	m, err := manifest.Decode(bytes.NewReader(mustGet(t, d.st, mKey)))
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.TenantID != "acme" || m.PageID != "blog" || m.DeploymentID != wantID || !m.HasWorker {
		t.Fatalf("manifest = %+v", m)
	}
	for path, content := range map[string]string{
		"/index.html":     "<h1>hello</h1>",
		"/assets/app.css": "body{color:red}",
	} {
		entry, ok := m.Static[path]
		if !ok {
			t.Fatalf("manifest has no static entry for %q: %+v", path, m.Static)
		}
		if entry.Hash != sha256Hex(content) {
			t.Fatalf("%s hash = %q", path, entry.Hash)
		}
		key := fmt.Sprintf(storage.StaticKeyFmt, "acme", "blog", wantID, entry.Hash)
		if got := string(mustGet(t, d.st, key)); got != content {
			t.Fatalf("stored %s = %q", key, got)
		}
	}
	workerTar := mustGet(t, d.st, fmt.Sprintf(storage.WorkerBundleKeyFmt, "acme", "blog", wantID))
	if !bytes.Contains(workerTar, []byte("export default")) {
		t.Fatal("worker.tar does not carry the worker source")
	}

	// The deployment was registered and activated.
	if len(d.f.created) != 1 || d.f.created[0].ID != wantID || d.f.created[0].PageID != "blog" {
		t.Fatalf("CreateDeployment calls = %+v", d.f.created)
	}
	if d.f.created[0].CreatedAt.IsZero() {
		t.Fatal("CreateDeployment got a zero CreatedAt")
	}
	if len(d.f.activate) != 1 || d.f.activate[0] != [2]string{"blog", wantID} {
		t.Fatalf("SetActiveDeployment calls = %v", d.f.activate)
	}
	page, _ := d.f.page("blog")
	if page.ActiveDeploymentID != wantID {
		t.Fatalf("page active = %q", page.ActiveDeploymentID)
	}
}

func TestUploadGzipAutoDetected(t *testing.T) {
	d := newDeployFixture(t)
	// Deliberately mislabelled: detection uses the gzip magic bytes.
	d.upload(t, "/v1/pages/blog/deployments?deploymentId=dep-gz",
		gzipBytes(t, buildTar(t, site...)), "application/octet-stream")

	mustGet(t, d.st, fmt.Sprintf(storage.ManifestKeyFmt, "acme", "blog", "dep-gz"))
	if len(d.f.created) != 1 || d.f.created[0].ID != "dep-gz" {
		t.Fatalf("CreateDeployment calls = %+v", d.f.created)
	}
}

func TestUploadSingleLeadingDirectory(t *testing.T) {
	d := newDeployFixture(t)
	d.upload(t, "/v1/pages/blog/deployments?deploymentId=dep-nested",
		buildTar(t, withPrefix("dist", site)...), "application/x-tar")

	key := fmt.Sprintf(storage.ManifestKeyFmt, "acme", "blog", "dep-nested")
	m, err := manifest.Decode(bytes.NewReader(mustGet(t, d.st, key)))
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if _, ok := m.Static["/index.html"]; !ok {
		t.Fatalf("leading directory not stripped: %+v", m.Static)
	}
	if !m.HasWorker {
		t.Fatal("worker not found under the leading directory")
	}
}

func TestUploadActivateFalse(t *testing.T) {
	d := newDeployFixture(t)
	d.f.seedDeployment(provider.Deployment{ID: "dep-old", PageID: "blog"})
	page, _ := d.f.page("blog")
	page.ActiveDeploymentID = "dep-old"
	d.f.seedPage(page)

	body := d.upload(t, "/v1/pages/blog/deployments?deploymentId=dep-new&activate=false",
		buildTar(t, site...), "application/x-tar")
	if !strings.Contains(body.String(), `"activated":false`) {
		t.Fatalf("response = %s", body.String())
	}
	if len(d.f.activate) != 0 {
		t.Fatalf("SetActiveDeployment was called: %v", d.f.activate)
	}
	got, _ := d.f.page("blog")
	if got.ActiveDeploymentID != "dep-old" {
		t.Fatalf("active deployment changed to %q", got.ActiveDeploymentID)
	}
	// The deployment is registered nonetheless.
	if len(d.f.created) != 1 || d.f.created[0].ID != "dep-new" {
		t.Fatalf("CreateDeployment calls = %+v", d.f.created)
	}
}

func TestUploadRejectsUnsafeArchives(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
		status  int
		code    string
	}{
		{
			name:    "path traversal",
			entries: []tarEntry{{name: "index.html", body: "x"}, {name: "../evil.txt", body: "pwned"}},
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
		},
		{
			name:    "nested traversal",
			entries: []tarEntry{{name: "a/../../evil.txt", body: "pwned"}},
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
		},
		{
			name:    "absolute path",
			entries: []tarEntry{{name: "/etc/evil.txt", body: "pwned"}},
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
		},
		{
			name:    "symlink",
			entries: []tarEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "../../../etc/passwd"}},
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
		},
		{
			name:    "hard link",
			entries: []tarEntry{{name: "link", typeflag: tar.TypeLink, linkname: "index.html"}},
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
		},
		{
			name:    "no files",
			entries: []tarEntry{{name: "empty/", typeflag: tar.TypeDir}},
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
		},
		{
			name:    "not an archive",
			entries: nil,
			status:  http.StatusBadRequest,
			code:    codeInvalidRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeployFixture(t)
			body := []byte("this is not a tar file at all, not even close")
			if tc.entries != nil {
				body = buildTar(t, tc.entries...)
			}
			rec := do(t, d.s, http.MethodPost, "/v1/pages/blog/deployments",
				bytes.NewReader(body), "application/x-tar")
			requireStatus(t, rec, tc.status)
			if code := errorCode(t, rec); code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
			if len(d.f.created) != 0 {
				t.Fatalf("a deployment was registered: %+v", d.f.created)
			}
			objs, err := d.st.List(t.Context(), "")
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(objs) != 0 {
				t.Fatalf("objects were uploaded: %+v", objs)
			}
		})
	}
}

func TestUploadBodyTooLarge(t *testing.T) {
	d := newDeployFixture(t, func(o *Options) { o.MaxUploadBytes = 64 })
	rec := do(t, d.s, http.MethodPost, "/v1/pages/blog/deployments",
		bytes.NewReader(buildTar(t, site...)), "application/x-tar")
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
	if code := errorCode(t, rec); code != codeTooLarge {
		t.Fatalf("code = %q", code)
	}
}

func TestUploadExtractedTooLarge(t *testing.T) {
	d := newDeployFixture(t, func(o *Options) {
		o.MaxUploadBytes = 1 << 20
		o.MaxExtractedBytes = 8
	})
	rec := do(t, d.s, http.MethodPost, "/v1/pages/blog/deployments",
		bytes.NewReader(buildTar(t, site...)), "application/x-tar")
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
}

func TestUploadRequiresStorageAndPage(t *testing.T) {
	f := newFakeProvider()
	f.seedPage(provider.Page{ID: "blog", TenantID: "acme"})

	// No storage -> 501.
	s := newTestServer(t, Options{Provider: f, Admin: f})
	rec := do(t, s, http.MethodPost, "/v1/pages/blog/deployments",
		bytes.NewReader(buildTar(t, site...)), "application/x-tar")
	requireStatus(t, rec, http.StatusNotImplemented)
	if code := errorCode(t, rec); code != codeNotImplemented {
		t.Fatalf("code = %q", code)
	}

	// Unknown page -> 404, and nothing is stored.
	d := newDeployFixture(t)
	rec = do(t, d.s, http.MethodPost, "/v1/pages/ghost/deployments",
		bytes.NewReader(buildTar(t, site...)), "application/x-tar")
	requireStatus(t, rec, http.StatusNotFound)
	objs, err := d.st.List(t.Context(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("objects were uploaded: %+v", objs)
	}
}

func TestUploadParameterValidation(t *testing.T) {
	d := newDeployFixture(t)
	for _, target := range []string{
		"/v1/pages/blog/deployments?deploymentId=../evil",
		"/v1/pages/blog/deployments?deploymentId=" + strings.Repeat("x", maxIDLen+1),
		"/v1/pages/blog/deployments?activate=maybe",
	} {
		rec := do(t, d.s, http.MethodPost, target, bytes.NewReader(buildTar(t, site...)), "application/x-tar")
		requireStatus(t, rec, http.StatusBadRequest)
		if code := errorCode(t, rec); code != codeInvalidRequest {
			t.Fatalf("%s: code = %q", target, code)
		}
	}
}

func TestUploadRejectsBundleWithoutCompiledWorker(t *testing.T) {
	d := newDeployFixture(t)
	rec := do(t, d.s, http.MethodPost, "/v1/pages/blog/deployments", bytes.NewReader(buildTar(t,
		tarEntry{name: "index.html", body: "x"},
		tarEntry{name: "functions/api.js", body: "export function onRequest() {}"},
	)), "application/x-tar")
	requireStatus(t, rec, http.StatusBadRequest)
	if code := errorCode(t, rec); code != codeInvalidBundle {
		t.Fatalf("code = %q", code)
	}
}

func TestSanitizeTarPath(t *testing.T) {
	ok := map[string]string{
		"index.html":      "index.html",
		"./index.html":    "index.html",
		"a/b/c.txt":       "a/b/c.txt",
		"a/./b.txt":       "a/b.txt",
		"dist/":           "dist",
		"./":              "",
		".":               "",
		"a/../b.txt":      "b.txt",
		"_worker.js/x.js": "_worker.js/x.js",
	}
	for in, want := range ok {
		got, err := sanitizeTarPath(in)
		if err != nil {
			t.Fatalf("sanitizeTarPath(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("sanitizeTarPath(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"../x", "a/../../x", "/x", "/", "..", "a\\b", "x\x00y"} {
		if got, err := sanitizeTarPath(in); err == nil {
			t.Fatalf("sanitizeTarPath(%q) = %q, want an error", in, got)
		}
	}
}

// TestUploadUsesConfiguredTempDir pins that extraction happens under
// Options.TempDir — the whole point of the option on a read-only root
// filesystem — and that the upload directory is removed afterwards.
func TestUploadUsesConfiguredTempDir(t *testing.T) {
	tmp := t.TempDir()
	d := newDeployFixture(t, func(o *Options) { o.TempDir = tmp })

	d.upload(t, "/v1/pages/blog/deployments?deploymentId=dep-tmp",
		buildTar(t, site...), "application/x-tar")

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload directory left behind in %s: %v", tmp, entries)
	}
}

// TestUploadTempDirFailureIsLoggedAndReported covers the production incident:
// when the upload directory cannot be created the client gets a 500 AND the
// cause reaches the server log.
func TestUploadTempDirFailureIsLoggedAndReported(t *testing.T) {
	logs := newLogCapture()
	// Point the server at a directory that is removed after New validated it,
	// which is the closest portable stand-in for a volume that stops being
	// writable (a read-only root filesystem fails the same way).
	tmp := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	d := newDeployFixture(t, func(o *Options) {
		o.TempDir = tmp
		o.Logger = logs.logger
	})
	if err := os.Remove(tmp); err != nil {
		t.Fatalf("remove: %v", err)
	}

	rec := do(t, d.s, http.MethodPost, "/v1/pages/blog/deployments",
		bytes.NewReader(buildTar(t, site...)), "application/x-tar")
	requireStatus(t, rec, http.StatusInternalServerError)

	entry := logs.only(t)
	if !strings.Contains(entry.Error, "create temp dir") {
		t.Fatalf("temp dir failure not logged server-side: %+v", entry)
	}
	if entry.Level != slog.LevelError.String() {
		t.Fatalf("level = %q, want ERROR", entry.Level)
	}
}
