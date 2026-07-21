// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package assets implements Cloudflare Pages "Serving Pages" static asset
// resolution (docs/ARCHITECTURE.md 3.4). It maps a decoded request path onto
// the deployment's content-addressed static entries, deciding whether to serve
// a file, issue a pretty-URL redirect, or fall through to 404/SPA handling.
//
// The package is deliberately free of I/O and HTTP plumbing: it operates purely
// on a manifest static map so that BOTH the router's static pipeline and the
// worker shim's env.ASSETS endpoint can share one deterministic implementation.
package assets

import (
	"net/http"
	"strings"

	"github.com/durupages/durupages/pkg/manifest"
)

// Kind classifies a resolution outcome.
type Kind int

const (
	// NotFound means nothing could be served (no matching asset, no 404 page
	// and no SPA index.html fallback). It is the zero value.
	NotFound Kind = iota
	// ServeFile means FilePath/Entry should be served with Status.
	ServeFile
	// Redirect means the client should be redirected to Location with Status.
	Redirect
)

func (k Kind) String() string {
	switch k {
	case ServeFile:
		return "ServeFile"
	case Redirect:
		return "Redirect"
	default:
		return "NotFound"
	}
}

// Result is the outcome of Resolve.
//
//   - ServeFile: FilePath is the key into the static map, Entry its metadata and
//     Status is 200 (normal) or 404 (a custom 404.html served as an error page).
//   - Redirect: Location is the (path-only) target and Status is 308.
//   - NotFound: all fields are zero.
type Result struct {
	Kind Kind

	// ServeFile fields.
	FilePath string
	Entry    manifest.StaticEntry
	Status   int

	// Redirect fields.
	Location string
}

// permanentRedirectStatus is the status Cloudflare Pages uses for pretty-URL
// and trailing-slash normalizations: 308 Permanent Redirect (which, unlike 301,
// forbids changing the request method).
const permanentRedirectStatus = http.StatusPermanentRedirect

// Resolve maps path onto static following Cloudflare Pages semantics. path is
// assumed to be already URL-decoded and to start with "/" (query-string
// stripping and decoding are the caller's responsibility); ".."/"." segments
// are normalized and any attempt to escape the root is treated as not found.
//
// Resolution priority (the first matching rule wins):
//
//  1. Path normalization. Collapse "."/"" segments, resolve "..", reject escapes.
//  2. Directory request (path ends in "/", including "/"):
//     a. serve "<dir>index.html" (200) if present;
//     b. else, if the extensionless sibling "<dir-without-slash>.html" exists,
//     redirect to the extensionless URL (trailing-slash normalization);
//     c. else fall through to miss handling.
//  3. Explicit ".html" request: if that exact asset exists, redirect to its
//     pretty URL ("/foo.html"->"/foo", "/dir/index.html"->"/dir/",
//     "/index.html"->"/"); otherwise fall through to miss handling.
//  4. Exact asset match: serve path (200).
//  5. Extensionless page: serve "<path>.html" (200) if present.
//  6. Directory index without a slash: if "<path>/index.html" exists, redirect
//     to "<path>/".
//  7. Miss handling: walk UP from the request directory to the nearest
//     "404.html" and serve it with status 404; if none is found (which implies
//     no root "/404.html" exists) treat the deployment as an SPA and serve
//     "/index.html" with status 200; if that too is absent, return NotFound.
func Resolve(static map[string]manifest.StaticEntry, path string) Result {
	p, ok := normalizePath(path)
	if !ok {
		// Escaped or malformed path: behave as a miss rooted at "/".
		return miss(static, "/")
	}

	// Directory request (ends with "/", which also covers the root "/").
	if strings.HasSuffix(p, "/") {
		index := p + "index.html"
		if e, found := static[index]; found {
			return serveFile(index, e, http.StatusOK)
		}
		// Trailing-slash normalization: "/foo/" with only "foo.html" present
		// redirects to the extensionless "/foo".
		if p != "/" {
			noSlash := strings.TrimSuffix(p, "/")
			if _, found := static[noSlash+".html"]; found {
				return redirect(noSlash)
			}
		}
		return miss(static, p)
	}

	// Explicit ".html" request: normalize to the pretty URL when the asset
	// actually exists; the .html form is never served directly.
	if strings.HasSuffix(p, ".html") {
		if _, found := static[p]; found {
			return redirect(prettyURL(p))
		}
		return miss(static, dirOf(p))
	}

	// Exact asset match (e.g. "/style.css", "/image.png").
	if e, found := static[p]; found {
		return serveFile(p, e, http.StatusOK)
	}

	// Extensionless page: "/foo" serves "foo.html".
	if e, found := static[p+".html"]; found {
		return serveFile(p+".html", e, http.StatusOK)
	}

	// Directory index requested without a trailing slash: "/foo" with
	// "foo/index.html" present redirects to "/foo/".
	if _, found := static[p+"/index.html"]; found {
		return redirect(p + "/")
	}

	return miss(static, dirOf(p))
}

// miss implements 404 and SPA-fallback handling for a request whose directory
// is dir (dir starts and ends with "/").
func miss(static map[string]manifest.StaticEntry, dir string) Result {
	if key, found := nearest404(static, dir); found {
		return serveFile(key, static[key], http.StatusNotFound)
	}
	// No 404.html anywhere up to the root implies there is no root
	// "/404.html", so treat the deployment as an SPA and serve index.html.
	if e, found := static["/index.html"]; found {
		return serveFile("/index.html", e, http.StatusOK)
	}
	return Result{Kind: NotFound}
}

// nearest404 walks up the directory tree from dir looking for the closest
// "404.html" ("/a/b/404.html", "/a/404.html", "/404.html").
func nearest404(static map[string]manifest.StaticEntry, dir string) (string, bool) {
	for {
		key := dir + "404.html"
		if _, found := static[key]; found {
			return key, true
		}
		if dir == "/" {
			return "", false
		}
		dir = parentDir(dir)
	}
}

// prettyURL returns the pretty-URL redirect target for an existing ".html"
// asset key: "/foo.html"->"/foo", "/dir/index.html"->"/dir/",
// "/index.html"->"/".
func prettyURL(htmlKey string) string {
	if strings.HasSuffix(htmlKey, "/index.html") {
		// Keep the trailing slash: "/dir/index.html" -> "/dir/",
		// "/index.html" -> "/".
		return strings.TrimSuffix(htmlKey, "index.html")
	}
	return strings.TrimSuffix(htmlKey, ".html")
}

// dirOf returns the directory portion of p (always starting and ending with
// "/"). For "/a/b/c" it is "/a/b/"; for "/a/b/" it is "/a/b/".
func dirOf(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p[:strings.LastIndex(p, "/")+1]
}

// parentDir returns the parent of dir (dir starts and ends with "/" and is not
// the root). For "/a/b/" it returns "/a/"; for "/a/" it returns "/".
func parentDir(dir string) string {
	trimmed := strings.TrimSuffix(dir, "/")
	return trimmed[:strings.LastIndex(trimmed, "/")+1]
}

// normalizePath cleans an absolute request path: it collapses empty and "."
// segments, resolves ".." against the accumulated path, and preserves a
// trailing slash. It returns ok=false when the path is not absolute or when a
// ".." segment would escape above the root.
func normalizePath(p string) (string, bool) {
	if p == "" || p[0] != '/' {
		return "", false
	}
	trailing := len(p) > 1 && strings.HasSuffix(p, "/")

	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			// Skip empty segments (leading slash, doubled slashes) and the
			// current-directory marker.
		case "..":
			if len(out) == 0 {
				return "", false // escape above root
			}
			out = out[:len(out)-1]
		default:
			out = append(out, seg)
		}
	}

	res := "/" + strings.Join(out, "/")
	if trailing && res != "/" {
		res += "/"
	}
	return res, true
}

func serveFile(key string, entry manifest.StaticEntry, status int) Result {
	return Result{Kind: ServeFile, FilePath: key, Entry: entry, Status: status}
}

func redirect(location string) Result {
	return Result{Kind: Redirect, Location: location, Status: permanentRedirectStatus}
}
