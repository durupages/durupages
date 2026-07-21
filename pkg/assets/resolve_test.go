// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package assets

import (
	"net/http"
	"testing"

	"github.com/durupages/durupages/pkg/manifest"
)

// mkStatic builds a static map from a set of keys; entries carry a deterministic
// hash derived from the key so ServeFile results can be checked precisely.
func mkStatic(keys ...string) map[string]manifest.StaticEntry {
	m := make(map[string]manifest.StaticEntry, len(keys))
	for _, k := range keys {
		m[k] = manifest.StaticEntry{Hash: "h" + k, Size: 1, ContentType: "text/html"}
	}
	return m
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name   string
		static map[string]manifest.StaticEntry
		path   string

		wantKind     Kind
		wantFile     string // ServeFile
		wantStatus   int    // ServeFile or Redirect
		wantLocation string // Redirect
	}{
		// ---- Basic index serving ----
		{
			name:       "root serves index.html",
			static:     mkStatic("/index.html"),
			path:       "/",
			wantKind:   ServeFile,
			wantFile:   "/index.html",
			wantStatus: http.StatusOK,
		},
		{
			name:       "exact asset match css",
			static:     mkStatic("/index.html", "/style.css"),
			path:       "/style.css",
			wantKind:   ServeFile,
			wantFile:   "/style.css",
			wantStatus: http.StatusOK,
		},
		{
			name:       "exact asset match nested png",
			static:     mkStatic("/index.html", "/assets/logo.png"),
			path:       "/assets/logo.png",
			wantKind:   ServeFile,
			wantFile:   "/assets/logo.png",
			wantStatus: http.StatusOK,
		},

		// ---- Pretty-URL redirects (308) ----
		{
			name:         "/index.html redirects to /",
			static:       mkStatic("/index.html"),
			path:         "/index.html",
			wantKind:     Redirect,
			wantStatus:   http.StatusPermanentRedirect,
			wantLocation: "/",
		},
		{
			name:         "/foo.html redirects to /foo",
			static:       mkStatic("/foo.html"),
			path:         "/foo.html",
			wantKind:     Redirect,
			wantStatus:   http.StatusPermanentRedirect,
			wantLocation: "/foo",
		},
		{
			name:         "/dir/index.html redirects to /dir/",
			static:       mkStatic("/dir/index.html"),
			path:         "/dir/index.html",
			wantKind:     Redirect,
			wantStatus:   http.StatusPermanentRedirect,
			wantLocation: "/dir/",
		},
		{
			name:         "nested /a/b.html redirects to /a/b",
			static:       mkStatic("/a/b.html"),
			path:         "/a/b.html",
			wantKind:     Redirect,
			wantStatus:   http.StatusPermanentRedirect,
			wantLocation: "/a/b",
		},

		// ---- Extensionless serving ----
		{
			name:       "/foo serves foo.html",
			static:     mkStatic("/foo.html"),
			path:       "/foo",
			wantKind:   ServeFile,
			wantFile:   "/foo.html",
			wantStatus: http.StatusOK,
		},
		{
			name:         "/foo redirects to /foo/ when only foo/index.html",
			static:       mkStatic("/foo/index.html"),
			path:         "/foo",
			wantKind:     Redirect,
			wantStatus:   http.StatusPermanentRedirect,
			wantLocation: "/foo/",
		},
		{
			name:       "extensionless prefers foo.html over foo/index.html",
			static:     mkStatic("/foo.html", "/foo/index.html"),
			path:       "/foo",
			wantKind:   ServeFile,
			wantFile:   "/foo.html",
			wantStatus: http.StatusOK,
		},
		{
			name:       "exact extensionless asset wins over .html",
			static:     mkStatic("/foo", "/foo.html"),
			path:       "/foo",
			wantKind:   ServeFile,
			wantFile:   "/foo",
			wantStatus: http.StatusOK,
		},

		// ---- Directory (trailing slash) serving ----
		{
			name:       "/dir/ serves dir/index.html",
			static:     mkStatic("/dir/index.html"),
			path:       "/dir/",
			wantKind:   ServeFile,
			wantFile:   "/dir/index.html",
			wantStatus: http.StatusOK,
		},

		// ---- Trailing-slash normalization (both directions) ----
		{
			name:         "/foo/ redirects to /foo when only foo.html",
			static:       mkStatic("/foo.html"),
			path:         "/foo/",
			wantKind:     Redirect,
			wantStatus:   http.StatusPermanentRedirect,
			wantLocation: "/foo",
		},

		// ---- 404 handling ----
		{
			name:       "root 404 page served with 404",
			static:     mkStatic("/index.html", "/404.html"),
			path:       "/missing",
			wantKind:   ServeFile,
			wantFile:   "/404.html",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "nearest nested 404 page",
			static:     mkStatic("/index.html", "/404.html", "/blog/404.html"),
			path:       "/blog/missing",
			wantKind:   ServeFile,
			wantFile:   "/blog/404.html",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "deep miss falls back to root 404",
			static:     mkStatic("/index.html", "/404.html", "/blog/404.html"),
			path:       "/other/deep/missing",
			wantKind:   ServeFile,
			wantFile:   "/404.html",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "walk up finds parent 404 not root",
			static:     mkStatic("/404.html", "/a/404.html"),
			path:       "/a/b/c/missing",
			wantKind:   ServeFile,
			wantFile:   "/a/404.html",
			wantStatus: http.StatusNotFound,
		},

		// ---- SPA fallback (no root 404.html) ----
		{
			name:       "SPA fallback serves index.html 200",
			static:     mkStatic("/index.html"),
			path:       "/some/client/route",
			wantKind:   ServeFile,
			wantFile:   "/index.html",
			wantStatus: http.StatusOK,
		},
		{
			name:       "SPA fallback ignores nested-only 404 for unrelated path",
			static:     mkStatic("/index.html", "/blog/404.html"),
			path:       "/dashboard",
			wantKind:   ServeFile,
			wantFile:   "/index.html",
			wantStatus: http.StatusOK,
		},
		{
			name:       "nested 404 still served even without root 404",
			static:     mkStatic("/index.html", "/blog/404.html"),
			path:       "/blog/missing",
			wantKind:   ServeFile,
			wantFile:   "/blog/404.html",
			wantStatus: http.StatusNotFound,
		},

		// ---- Plain NotFound ----
		{
			name:     "empty map is NotFound",
			static:   mkStatic(),
			path:     "/",
			wantKind: NotFound,
		},
		{
			name:     "miss without 404 or index is NotFound",
			static:   mkStatic("/style.css"),
			path:     "/missing",
			wantKind: NotFound,
		},
		{
			name:     "root request without index and without 404 is NotFound",
			static:   mkStatic("/style.css"),
			path:     "/",
			wantKind: NotFound,
		},

		// ---- Path normalization ----
		{
			name:       "dot-dot within bounds normalizes",
			static:     mkStatic("/index.html", "/bar.html"),
			path:       "/foo/../bar",
			wantKind:   ServeFile,
			wantFile:   "/bar.html",
			wantStatus: http.StatusOK,
		},
		{
			name:       "single dot segment normalizes",
			static:     mkStatic("/a/b.html"),
			path:       "/a/./b",
			wantKind:   ServeFile,
			wantFile:   "/a/b.html",
			wantStatus: http.StatusOK,
		},
		{
			name:       "doubled slashes collapse",
			static:     mkStatic("/a/b.html"),
			path:       "/a//b",
			wantKind:   ServeFile,
			wantFile:   "/a/b.html",
			wantStatus: http.StatusOK,
		},

		// ---- Path traversal escapes -> treated as miss ----
		{
			name:       "traversal escape falls back to root 404",
			static:     mkStatic("/index.html", "/404.html"),
			path:       "/../etc/passwd",
			wantKind:   ServeFile,
			wantFile:   "/404.html",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "deep traversal escape falls back to SPA",
			static:     mkStatic("/index.html"),
			path:       "/foo/../../bar",
			wantKind:   ServeFile,
			wantFile:   "/index.html",
			wantStatus: http.StatusOK,
		},
		{
			name:     "non-absolute path is a miss with empty map",
			static:   mkStatic(),
			path:     "relative/path",
			wantKind: NotFound,
		},

		// ---- .html request for a missing asset ----
		{
			name:     "missing .html asset is NotFound",
			static:   mkStatic("/style.css"),
			path:     "/gone.html",
			wantKind: NotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.static, tt.path)
			if got.Kind != tt.wantKind {
				t.Fatalf("Kind = %v, want %v (result %+v)", got.Kind, tt.wantKind, got)
			}
			switch tt.wantKind {
			case ServeFile:
				if got.FilePath != tt.wantFile {
					t.Errorf("FilePath = %q, want %q", got.FilePath, tt.wantFile)
				}
				if got.Status != tt.wantStatus {
					t.Errorf("Status = %d, want %d", got.Status, tt.wantStatus)
				}
				if got.Entry.Hash != tt.static[tt.wantFile].Hash {
					t.Errorf("Entry.Hash = %q, want %q", got.Entry.Hash, tt.static[tt.wantFile].Hash)
				}
			case Redirect:
				if got.Location != tt.wantLocation {
					t.Errorf("Location = %q, want %q", got.Location, tt.wantLocation)
				}
				if got.Status != tt.wantStatus {
					t.Errorf("Status = %d, want %d", got.Status, tt.wantStatus)
				}
			}
		})
	}
}

func TestKindString(t *testing.T) {
	cases := map[Kind]string{
		NotFound:  "NotFound",
		ServeFile: "ServeFile",
		Redirect:  "Redirect",
		Kind(99):  "NotFound",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/", "/", true},
		{"/foo", "/foo", true},
		{"/foo/", "/foo/", true},
		{"/foo/../bar", "/bar", true},
		{"/a/./b", "/a/b", true},
		{"/a//b", "/a/b", true},
		{"/a/b/../", "/a/", true},
		{"/foo/..", "/", true},
		{"/../etc", "", false},
		{"/foo/../../x", "", false},
		{"", "", false},
		{"relative", "", false},
	}
	for _, tt := range tests {
		got, ok := normalizePath(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("normalizePath(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
