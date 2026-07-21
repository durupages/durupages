// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/assets"
	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/pagesspec"
	"github.com/durupages/durupages/pkg/storage"
)

// serveStatic runs the static pipeline of docs/ARCHITECTURE.md 3.1/5.2:
// _redirects, asset resolution, _headers + default headers, ETag/304, and the
// 404/SPA fallback. Only GET and HEAD are accepted here.
func (rt *Router) serveStatic(w http.ResponseWriter, r *http.Request, host string, page *api.PageInfo, m *manifest.Manifest) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
		rt.logStatic(r, host, page, http.StatusMethodNotAllowed, 0)
		return
	}

	// The path used for asset resolution may be rewritten by a 200 _redirects
	// rule; the original path is kept for _headers matching and logging.
	origPath := r.URL.Path
	resolvePath := origPath

	// (3) _redirects: a non-rewrite match responds immediately; a 200 rewrite
	// continues resolution against the rewritten path.
	if res, ok := pagesspec.EvalRedirects(m.Redirects, origPath); ok {
		if res.IsRewrite {
			resolvePath = res.Location
		} else {
			rt.writeRedirect(w, r, res.Location, res.Status)
			rt.logStatic(r, host, page, res.Status, 0)
			return
		}
	}

	// (4) Asset resolution.
	result := assets.Resolve(m.Static, resolvePath)
	switch result.Kind {
	case assets.Redirect:
		rt.writeRedirect(w, r, result.Location, result.Status)
		rt.logStatic(r, host, page, result.Status, 0)
	case assets.ServeFile:
		rt.serveFile(w, r, host, page, m, result)
	default: // assets.NotFound: nothing to serve.
		body := "404 page not found\n"
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		var n int
		if r.Method != http.MethodHead {
			n, _ = io.WriteString(w, body)
		}
		rt.logStatic(r, host, page, http.StatusNotFound, int64(n))
	}
}

// writeRedirect writes a redirect response, preserving the inbound query string
// when the target has none of its own.
func (rt *Router) writeRedirect(w http.ResponseWriter, r *http.Request, location string, status int) {
	if r.URL.RawQuery != "" && !strings.Contains(location, "?") {
		location += "?" + r.URL.RawQuery
	}
	w.Header().Set("Location", location)
	w.WriteHeader(status)
}

// serveFile serves a resolved static asset (result.Status is 200, or 404 for a
// custom 404.html served as an error page). It applies default and _headers
// rules, honors If-None-Match, and streams the body from the LRU cache (filling
// it from Storage on a miss).
func (rt *Router) serveFile(w http.ResponseWriter, r *http.Request, host string, page *api.PageInfo, m *manifest.Manifest, result assets.Result) {
	entry := result.Entry
	header := w.Header()

	// Default headers first, then Content-Type and the strong ETag.
	for k, v := range assets.DefaultHeaders() {
		header.Set(k, v)
	}
	if entry.ContentType != "" {
		header.Set("Content-Type", entry.ContentType)
	}
	header.Set("ETag", assets.ETag(entry))

	// _headers: `set` replaces any existing value(s) for that name (including a
	// default) then appends each configured value in order; `unset` removes the
	// header entirely, defaults included.
	set, unset := pagesspec.EvalHeaders(m.Headers, host, r.URL.Path)
	for name, values := range set {
		header.Del(name)
		for _, v := range values {
			header.Add(name, v)
		}
	}
	for _, name := range unset {
		header.Del(name)
	}

	// Conditional request: 304 short-circuits the body.
	if assets.NotModified(r.Header.Get("If-None-Match"), entry) {
		w.WriteHeader(http.StatusNotModified)
		rt.logStatic(r, host, page, http.StatusNotModified, 0)
		return
	}

	body, err := rt.openBody(r.Context(), page, entry.Hash)
	if err != nil {
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}
	defer body.Close()

	header.Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	w.WriteHeader(result.Status)
	var sent int64
	if r.Method != http.MethodHead {
		sent, _ = io.Copy(w, body)
	}
	rt.logStatic(r, host, page, result.Status, sent)
}

// openBody returns a reader for the content-addressed asset hash. It serves
// from the LRU cache when present, otherwise fetches static/{hash} from Storage
// into the cache and then opens it.
func (rt *Router) openBody(ctx context.Context, page *api.PageInfo, hash string) (io.ReadCloser, error) {
	if path, ok := rt.cache.Get(hash); ok {
		if f, err := os.Open(path); err == nil {
			return f, nil
		}
		// The file vanished (e.g. evicted between Get and Open); fall through
		// to a Storage refill.
	}
	key := fmt.Sprintf(storage.StaticKeyFmt, page.GetTenantId(), page.GetPageId(), page.GetActiveDeploymentId(), hash)
	rc, _, err := rt.storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	path, err := rt.cache.Put(hash, rc)
	rc.Close()
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}
