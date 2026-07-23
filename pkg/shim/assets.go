// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/assets"
)

// serveAssets is the :8081 env.ASSETS service. The runtime's per-page external
// service injects X-DuruPages-Page, which selects the loaded deployment whose
// static tree and manifest drive Cloudflare Pages asset resolution (pkg/assets).
func (s *Shim) serveAssets(w http.ResponseWriter, r *http.Request) {
	requestID := requestIDOf(r)
	pageID := r.Header.Get(api.HeaderPage)
	if pageID == "" {
		s.httpError(w, r, http.StatusBadRequest, "missing page header", logMsgAssetsFailed,
			attrIf([]slog.Attr{slog.String("reason", "no "+api.HeaderPage+" header")},
				"requestId", requestID)...)
		return
	}

	s.mu.Lock()
	depID := s.active[pageID]
	dep := s.deployments[depID]
	s.mu.Unlock()
	if dep == nil || dep.manifest == nil {
		// The runtime asked for a page the shim is not serving: a stale workerd
		// config, or a swap that dropped the page from the load set.
		http.NotFound(w, r)
		s.logResponseErr(r, http.StatusNotFound, logMsgAssetsFailed,
			attrIf([]slog.Attr{
				slog.String("pageId", pageID),
				slog.String("deploymentId", depID),
				slog.String("reason", "page not in load set"),
			}, "requestId", requestID)...)
		return
	}

	res := assets.Resolve(dep.manifest.Static, r.URL.Path)
	switch res.Kind {
	case assets.Redirect:
		http.Redirect(w, r, res.Location, res.Status)
	case assets.ServeFile:
		s.serveAssetFile(w, r, dep, res)
	default:
		// A plain miss in the static manifest: normal for worker-handled paths,
		// so this stays at warn like every other 4xx and carries no error.
		http.NotFound(w, r)
		s.logResponseErr(r, http.StatusNotFound, logMsgAssetsFailed,
			attrIf([]slog.Attr{
				slog.String("pageId", pageID),
				slog.String("deploymentId", dep.deploymentID),
				slog.String("reason", "no manifest entry"),
			}, "requestId", requestID)...)
	}
}

// serveAssetFile writes a resolved static file with the Cloudflare default
// headers, ETag and 304 handling.
func (s *Shim) serveAssetFile(w http.ResponseWriter, r *http.Request, dep *deployment, res assets.Result) {
	for k, v := range assets.DefaultHeaders() {
		w.Header().Set(k, v)
	}
	if res.Entry.ContentType != "" {
		w.Header().Set("Content-Type", res.Entry.ContentType)
	}
	etag := assets.ETag(res.Entry)
	w.Header().Set("ETag", etag)

	if assets.NotModified(r.Header.Get("If-None-Match"), res.Entry) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	f, err := os.Open(filepath.Join(dep.staticDir, res.Entry.Hash))
	if err != nil {
		// The manifest promised a blob the unpacked bundle does not have: a
		// truncated download or a bundle/manifest mismatch, worth the detail.
		http.NotFound(w, r)
		s.logResponseErr(r, http.StatusNotFound, logMsgAssetsFailed,
			attrIf([]slog.Attr{
				slog.String("pageId", dep.pageID),
				slog.String("deploymentId", dep.deploymentID),
				slog.String("reason", "static blob missing on disk"),
				slog.String("error", err.Error()),
			}, "requestId", requestIDOf(r))...)
		return
	}
	defer f.Close()

	w.WriteHeader(res.Status)
	_, _ = io.Copy(w, f)
}
