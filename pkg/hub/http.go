// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/durupages/durupages/pkg/api"
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

// bundleLogMessage labels every bundle request log line, so a handler that
// mixes sources can still filter this package's traffic.
const bundleLogMessage = "hub bundle request"

// Rejection reasons attached to a failed bundle request as "reason". They are
// stable, greppable tokens: an operator alerting on a worker that cannot load
// its bundle filters on these rather than on free-form error text.
const (
	reasonInvalidPathParam   = "invalid_path_parameter"
	reasonMissingBearerToken = "missing_bearer_token"
	reasonTokenExpired       = "token_expired"
	reasonTokenNotValidYet   = "token_not_valid_yet"
	reasonTokenMalformed     = "token_malformed"
	reasonSignatureInvalid   = "signature_invalid"
	reasonTokenUnverifiable  = "token_unverifiable"
	reasonMissingClaim       = "required_claim_missing"
	reasonInvalidClaims      = "invalid_claims"
	reasonTokenRejected      = "token_rejected"
	reasonTenantMismatch     = "tenant_mismatch"
	reasonBundleNotFound     = "bundle_not_found"
	reasonStorageError       = "storage_error"
	reasonCacheEvicted       = "cache_entry_evicted"
	reasonStreamTruncated    = "response_stream_truncated"
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

// bundleReq is the per-request state shared by the handler and its log line.
//
// It doubles as the http.ResponseWriter the handler writes through, so status
// and byte counts are observed rather than guessed.
type bundleReq struct {
	http.ResponseWriter

	tenantID     string
	pageID       string
	deploymentID string
	requestID    string

	// key is the storage key looked up, recorded as soon as it is known. On a
	// 404 it is the whole diagnosis: a deployment that uploaded successfully but
	// is not found here means the key the hub derives and the key the writer
	// used disagree, and nothing else in the system reports that.
	key string

	status int
	bytes  int64
	wrote  bool

	// reason and err describe why the response is not a success. Response
	// bodies stay opaque (the wire contract is a fixed short string), so this
	// is the only place the cause is recorded.
	reason string
	err    error
	// extra carries case-specific attributes (e.g. the token's tenant claim).
	extra []slog.Attr
}

func (b *bundleReq) WriteHeader(code int) {
	if b.wrote {
		return
	}
	b.wrote = true
	b.status = code
	b.ResponseWriter.WriteHeader(code)
}

func (b *bundleReq) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	n, err := b.ResponseWriter.Write(p)
	b.bytes += int64(n)
	return n, err
}

// fail writes the opaque error body and records the cause for the request log.
// body must stay byte-identical to what clients already expect; every added
// detail goes to the log instead.
func (b *bundleReq) fail(status int, body, reason string, err error, extra ...slog.Attr) {
	b.reason = reason
	b.err = err
	b.extra = append(b.extra, extra...)
	http.Error(b, body, status)
}

// note records a non-fatal cause (the response is already a success, or is
// about to be retried) so it still shows up on the request line.
func (b *bundleReq) note(reason string, err error) {
	if b.reason == "" {
		b.reason = reason
		b.err = err
	}
}

func (h *Hub) serveBundle(w http.ResponseWriter, r *http.Request, f bundleFile) {
	start := time.Now()
	b := &bundleReq{
		ResponseWriter: w,
		tenantID:       r.PathValue("tenantId"),
		pageID:         r.PathValue("pageId"),
		deploymentID:   r.PathValue("deploymentId"),
		requestID:      r.Header.Get(api.HeaderRequestID),
		status:         http.StatusOK,
	}
	h.handleBundle(b, r, f)
	h.logBundleRequest(r, b, time.Since(start))
}

func (h *Hub) handleBundle(b *bundleReq, r *http.Request, f bundleFile) {
	if bad, ok := invalidParam(b.tenantID, b.pageID, b.deploymentID); !ok {
		b.fail(http.StatusBadRequest, "invalid path parameter", reasonInvalidPathParam,
			fmt.Errorf("path parameter %s is not a valid identifier", bad))
		return
	}

	claims, err := h.authenticate(r)
	if err != nil {
		b.fail(http.StatusUnauthorized, "unauthorized", authFailureReason(err), err)
		return
	}
	if claims.Tenant != b.tenantID {
		// Logged with the token's tenant claim (an identifier, not a
		// credential) because a systematic mismatch means the controller issued
		// the worker a token for the wrong tenant, which is invisible otherwise.
		b.fail(http.StatusForbidden, "forbidden", reasonTenantMismatch,
			fmt.Errorf("token tenant %q does not match path tenant %q", claims.Tenant, b.tenantID),
			slog.String("tokenTenant", claims.Tenant), slog.String("pod", claims.Pod))
		return
	}

	// Deployments are immutable, so the deployment ID is a complete validator
	// for the response body: an If-None-Match hit can be answered 304 without
	// touching Storage.
	etag := `"` + b.deploymentID + `"`
	b.Header().Set("ETag", etag)
	if ifNoneMatchMatches(r.Header.Get("If-None-Match"), etag) {
		b.WriteHeader(http.StatusNotModified)
		return
	}

	b.key = fmt.Sprintf(f.keyFmt, b.tenantID, b.pageID, b.deploymentID)
	if h.cache != nil {
		h.serveCached(b, r, f)
		return
	}
	h.serveDirect(b, r, f)
}

// serveCached fetches through the on-disk cache and serves the local file.
func (h *Hub) serveCached(b *bundleReq, r *http.Request, f bundleFile) {
	cacheKey := b.deploymentID + "\x00" + f.contentType
	path, err := h.cache.getOrLoad(cacheKey, func(dst io.Writer) error {
		return h.copyFromStorage(r.Context(), b.key, dst)
	})
	if err != nil {
		writeStorageError(b, err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		// The entry was likely evicted between load and open; fall back to a
		// direct stream so the request still succeeds. Recorded anyway: a
		// sustained rate of this means the cache budget is too small for the
		// working set.
		b.note(reasonCacheEvicted, err)
		h.serveDirect(b, r, f)
		return
	}
	defer file.Close()
	b.Header().Set("Content-Type", f.contentType)
	// ServeContent honors Range and re-checks conditional headers; the ETag is
	// already set on the response.
	http.ServeContent(b, r, "", time.Time{}, file)
}

// serveDirect streams straight from Storage without caching.
func (h *Hub) serveDirect(b *bundleReq, r *http.Request, f bundleFile) {
	rc, info, err := h.storage.Get(r.Context(), b.key)
	if err != nil {
		writeStorageError(b, err)
		return
	}
	defer rc.Close()
	b.Header().Set("Content-Type", f.contentType)
	if info.Size >= 0 {
		b.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	b.WriteHeader(http.StatusOK)
	if _, err := io.Copy(b, rc); err != nil {
		// The status line is already committed, so this cannot become a 5xx.
		// Recording it keeps a truncated bundle (a worker that then reports
		// "load failed") from looking like a clean 200 in the log.
		b.note(reasonStreamTruncated, err)
	}
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

// errMissingBearerToken is returned when the Authorization header is absent or
// is not a Bearer credential. It is a sentinel so the request log can name that
// case without matching on error text.
var errMissingBearerToken = errors.New("missing bearer token")

// authenticate extracts and verifies the Bearer worker token.
func (h *Hub) authenticate(r *http.Request) (*workerauth.Claims, error) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return nil, errMissingBearerToken
	}
	return workerauth.Verify(h.jwtPub, auth[len(prefix):])
}

// authFailureReason classifies a worker-JWT rejection into a stable token, so
// "the worker's token expired" (clock skew, a TTL shorter than the pod's cold
// start) is distinguishable from "the hub holds the wrong public key" without
// parsing error strings. The token itself is never logged, only why it lost.
func authFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errMissingBearerToken):
		return reasonMissingBearerToken
	case errors.Is(err, jwt.ErrTokenExpired):
		return reasonTokenExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return reasonTokenNotValidYet
	case errors.Is(err, jwt.ErrTokenMalformed):
		return reasonTokenMalformed
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		// Also covers a rejected alg (an HS256 alg-confusion attempt): the
		// parser refuses it before the signature is even checked.
		return reasonSignatureInvalid
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return reasonTokenUnverifiable
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		// The hub requires exp; a token issued without one lands here.
		return reasonMissingClaim
	case errors.Is(err, jwt.ErrTokenInvalidClaims):
		return reasonInvalidClaims
	default:
		return reasonTokenRejected
	}
}

func writeStorageError(b *bundleReq, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		b.fail(http.StatusNotFound, "not found", reasonBundleNotFound, err)
		return
	}
	b.fail(http.StatusInternalServerError, "internal error", reasonStorageError, err)
}

// logBundleRequest emits exactly one structured line per bundle request.
//
// The level follows the outcome (2xx/3xx info, 4xx warn, 5xx error). The route
// is low-traffic by construction — a worker hits it on cold start only — so a
// line per request is cheap and is the only record of why a worker failed to
// load its code. The URL query is deliberately not logged, and neither is the
// Authorization header nor any token content.
func (h *Hub) logBundleRequest(r *http.Request, b *bundleReq, d time.Duration) {
	level := slog.LevelInfo
	switch {
	case b.status >= http.StatusInternalServerError:
		level = slog.LevelError
	case b.status >= http.StatusBadRequest:
		level = slog.LevelWarn
	}
	logger := h.log()
	if !logger.Enabled(r.Context(), level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", b.status),
		slog.Int64("bytes", b.bytes),
		slog.Int64("durationMs", d.Milliseconds()),
		slog.String("tenantId", b.tenantID),
		slog.String("pageId", b.pageID),
		slog.String("deploymentId", b.deploymentID),
	}
	if b.requestID != "" {
		attrs = append(attrs, slog.String("requestId", b.requestID))
	}
	if b.key != "" {
		attrs = append(attrs, slog.String("key", b.key))
	}
	if b.reason != "" {
		attrs = append(attrs, slog.String("reason", b.reason))
	}
	if b.err != nil {
		attrs = append(attrs, slog.String("error", b.err.Error()))
	}
	attrs = append(attrs, b.extra...)
	logger.LogAttrs(r.Context(), level, bundleLogMessage, attrs...)
}

// invalidParam returns the name of the first path parameter that is not a valid
// identifier. Naming it turns "invalid path parameter" into something an
// operator can act on.
func invalidParam(tenantID, pageID, deploymentID string) (string, bool) {
	switch {
	case !validParam(tenantID):
		return "tenantId", false
	case !validParam(pageID):
		return "pageId", false
	case !validParam(deploymentID):
		return "deploymentId", false
	}
	return "", true
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
