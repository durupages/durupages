// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/workerauth"
)

// bundleFile describes one of the two downloadable deployment files and how it
// maps onto the storage layout.
type bundleFile struct {
	keyFmt      string // storage key format string: tenant, page, deployment
	contentType string
}

var (
	fileBundle   = bundleFile{keyFmt: storage.WorkerBundleKeyFmt, contentType: "application/x-tar"}
	fileManifest = bundleFile{keyFmt: storage.ManifestKeyFmt, contentType: "application/json"}
)

// HTTPHandler returns the bundle download API (section 7). Both routes require
// a valid worker JWT whose tenant claim matches the path tenant.
func (h *Hub) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tenants/{tenantId}/pages/{pageId}/deployments/{deploymentId}/bundle.tar",
		func(w http.ResponseWriter, r *http.Request) { h.serveBundle(w, r, fileBundle) })
	mux.HandleFunc("GET /v1/tenants/{tenantId}/pages/{pageId}/deployments/{deploymentId}/manifest.json",
		func(w http.ResponseWriter, r *http.Request) { h.serveBundle(w, r, fileManifest) })
	return mux
}

func (h *Hub) serveBundle(w http.ResponseWriter, r *http.Request, f bundleFile) {
	tenantID := r.PathValue("tenantId")
	pageID := r.PathValue("pageId")
	deploymentID := r.PathValue("deploymentId")

	if !validParam(tenantID) || !validParam(pageID) || !validParam(deploymentID) {
		http.Error(w, "invalid path parameter", http.StatusBadRequest)
		return
	}

	claims, err := h.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.Tenant != tenantID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Deployments are immutable, so the deployment ID is a complete validator
	// for the response body: an If-None-Match hit can be answered 304 without
	// touching Storage.
	etag := `"` + deploymentID + `"`
	w.Header().Set("ETag", etag)
	if ifNoneMatchMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	key := fmt.Sprintf(f.keyFmt, tenantID, pageID, deploymentID)
	if h.cache != nil {
		h.serveCached(w, r, key, deploymentID, f)
		return
	}
	h.serveDirect(w, r, key, f)
}

// serveCached fetches through the on-disk cache and serves the local file.
func (h *Hub) serveCached(w http.ResponseWriter, r *http.Request, key, deploymentID string, f bundleFile) {
	cacheKey := deploymentID + "\x00" + f.contentType
	path, err := h.cache.getOrLoad(cacheKey, func(dst io.Writer) error {
		return h.copyFromStorage(r.Context(), key, dst)
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		// The entry was likely evicted between load and open; fall back to a
		// direct stream so the request still succeeds.
		h.serveDirect(w, r, key, f)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", f.contentType)
	// ServeContent honors Range and re-checks conditional headers; the ETag is
	// already set on the response.
	http.ServeContent(w, r, "", time.Time{}, file)
}

// serveDirect streams straight from Storage without caching.
func (h *Hub) serveDirect(w http.ResponseWriter, r *http.Request, key string, f bundleFile) {
	rc, info, err := h.storage.Get(r.Context(), key)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", f.contentType)
	if info.Size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// copyFromStorage streams the object at key into dst.
func (h *Hub) copyFromStorage(ctx context.Context, key string, dst io.Writer) error {
	rc, _, err := h.storage.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(dst, rc)
	return err
}

// authenticate extracts and verifies the Bearer worker token.
func (h *Hub) authenticate(r *http.Request) (*workerauth.Claims, error) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return nil, errors.New("missing bearer token")
	}
	return workerauth.Verify(h.jwtPub, auth[len(prefix):])
}

func writeStorageError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// validParam rejects empty, slash-bearing and dot-traversal path parameters.
func validParam(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// ifNoneMatchMatches reports whether the If-None-Match header matches etag. It
// supports "*" and a comma-separated list of entity tags.
func ifNoneMatchMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/") // weak validator prefix
		if part == etag {
			return true
		}
	}
	return false
}
