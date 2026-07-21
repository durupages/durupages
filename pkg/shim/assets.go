// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"io"
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
	pageID := r.Header.Get(api.HeaderPage)
	if pageID == "" {
		http.Error(w, "missing page header", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	depID := s.active[pageID]
	dep := s.deployments[depID]
	s.mu.Unlock()
	if dep == nil || dep.manifest == nil {
		http.NotFound(w, r)
		return
	}

	res := assets.Resolve(dep.manifest.Static, r.URL.Path)
	switch res.Kind {
	case assets.Redirect:
		http.Redirect(w, r, res.Location, res.Status)
	case assets.ServeFile:
		s.serveAssetFile(w, r, dep, res)
	default:
		http.NotFound(w, r)
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
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.WriteHeader(res.Status)
	_, _ = io.Copy(w, f)
}
