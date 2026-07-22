// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package adminapi implements the controller's admin HTTP API: tenant, page
// and deployment management, plus deployment upload from a tarball of a
// wrangler build output directory.
//
// It exists so that operators (and the duru CLI) can deploy without holding
// PostgreSQL and object storage credentials themselves: a client POSTs a tar
// (optionally gzipped) of the build output and the controller performs the
// scan, the upload to Storage, the deployment registration and the activation.
//
// # Security
//
// The API is UNAUTHENTICATED by default. Out of the box it carries no
// credentials, no tokens and no authorization checks, and every route is fully
// privileged: anyone able to reach the listener can read every tenant's
// configuration, write page secrets and deploy arbitrary worker code. The
// handler MUST therefore be served on a separate, private listener (loopback,
// a cluster-internal Service, or a network otherwise restricted by
// firewall/NetworkPolicy) and MUST never be exposed on the public data-plane
// port. The controller only enables it when the operator explicitly opts in.
//
// Authentication is an extension point rather than a built-in policy: supply
// Options.Middleware to enforce whatever scheme you use (bearer token, mTLS
// client certificate, OIDC, an internal SSO header, per-tenant authorization,
// audit logging). Middleware wraps every route, so nothing is exempt unless
// you exempt it. See Options.Middleware for an example.
//
// Note that the durupages-controller binary ships no authentication: adding it
// means assembling your own binary around this package, exactly like the
// Storage/PageProvider/Queue/Scaler extension points.
//
// # Conventions
//
// All responses are JSON except GET /healthz. Errors use a single envelope:
//
//	{"error":{"code":"not_found","message":"page \"blog\" does not exist"}}
//
// Durations are encoded as Go duration strings ("30s", "1h30m"), matching how
// the PostgreSQL provider persists them, never as nanosecond integers.
//
// Page secret values are write-only: they are accepted on writes and are never
// echoed back. Responses instead carry the sorted key list in
// config.secretKeys. Because a client therefore never holds the full map, the
// /v1/pages/{pageId}/secrets sub-resource does per-key upsert and delete
// server-side (read the page, change one key, write it back).
//
// # Observability
//
// Every request produces exactly one log/slog line on Options.Logger
// (slog.Default() when unset), at info level for 2xx/3xx, warn for 4xx and
// error for 5xx. A 5xx line also carries the server-side cause as "error", so
// that a fault like an unwritable upload directory is diagnosable from the
// controller's log alone and not only from the client's response. The URL
// query is never logged.
package adminapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/storage"
)

// DefaultMaxUploadBytes is the default cap on a deployment upload request body.
const DefaultMaxUploadBytes = 512 << 20

// HealthPath is the liveness route. It is exported so Options.Middleware can
// exempt it from authentication (probes usually cannot present credentials).
const HealthPath = "/healthz"

// maxJSONBodyBytes caps non-upload request bodies.
const maxJSONBodyBytes = 1 << 20

// extractedSizeFactor multiplies MaxUploadBytes to obtain the default cap on
// the total number of bytes written while extracting an archive. It bounds
// decompression bombs while still allowing the compression ratio a realistic
// site bundle achieves.
const extractedSizeFactor = 4

// Options configures a Server.
type Options struct {
	// Provider is the read side (pages and tenants) and is required.
	Provider provider.PageProvider
	// Admin is the write side. When nil every mutating route answers 501.
	Admin provider.AdminProvider
	// Storage receives uploaded deployments. When nil the deployment upload
	// route answers 501; the other routes are unaffected.
	Storage storage.Storage
	// MaxUploadBytes caps the deployment upload request body. Defaults to
	// DefaultMaxUploadBytes (512 MiB). A body beyond it yields 413.
	MaxUploadBytes int64
	// MaxExtractedBytes caps the total number of bytes written while
	// extracting an uploaded archive. Defaults to four times MaxUploadBytes.
	MaxExtractedBytes int64
	// PagesDomain is the apex domain this controller serves pages on, e.g.
	// "pages.example.com" for a page reachable at "{pageId}.pages.example.com".
	// It is reported back on a deployment upload so that a client does not have
	// to be configured with it separately (and cannot report a stale URL when
	// the controller's domain changes). Optional: when empty the field is
	// omitted from the response.
	PagesDomain string
	// TempDir is the parent directory for the temporary directory a deployment
	// upload is extracted into. An empty value uses the os.TempDir() default.
	//
	// Deployments that run with a read-only root filesystem (for example a
	// Kubernetes Pod with securityContext.readOnlyRootFilesystem: true) must
	// point this at a writable volume, otherwise every upload fails with
	// "create temp dir: ... read-only file system". New validates a non-empty
	// value at startup so such a misconfiguration fails fast instead of on the
	// first upload.
	TempDir string
	// Logger receives one structured line per request (method, path, status,
	// bytes, durationMs, remoteAddr, plus error for 5xx). When nil the
	// slog.Default() logger is used at the time of logging.
	//
	// Defaulting to slog.Default() rather than to "logging disabled" is
	// deliberate: an embedder that wires nothing at all still gets its admin
	// API traffic and its 5xx causes recorded, which is exactly the operational
	// gap this package used to have. Tests that must stay quiet pass an
	// explicit logger over io.Discard.
	Logger *slog.Logger
	// Now overrides the clock (for tests). Defaults to time.Now.
	Now func() time.Time

	// Middleware wraps the routed handler, letting an operator add the
	// authentication and authorization this package deliberately does not
	// implement (see the package Security note).
	//
	// Entries run outermost first: with []Middleware{auth, audit} auth sees
	// the request before audit, and audit can be skipped by auth rejecting it.
	// Middleware runs inside request logging, so rejected requests are logged
	// with the status the middleware wrote.
	//
	// Every route is wrapped, HealthPath included — nothing is implicitly
	// exempt, so an authenticating middleware cannot leave a hole it did not
	// intend. Probes that cannot authenticate are the one case worth exempting
	// explicitly:
	//
	//	func auth(next http.Handler) http.Handler {
	//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	//	        if r.URL.Path == adminapi.HealthPath {
	//	            next.ServeHTTP(w, r) // unauthenticated liveness probe
	//	            return
	//	        }
	//	        if !valid(r.Header.Get("Authorization")) {
	//	            http.Error(w, "unauthorized", http.StatusUnauthorized)
	//	            return
	//	        }
	//	        next.ServeHTTP(w, r)
	//	    })
	//	}
	Middleware []func(http.Handler) http.Handler
}

// Server is the admin API http.Handler. Use New to build one.
type Server struct {
	provider provider.PageProvider
	admin    provider.AdminProvider
	storage  storage.Storage

	maxUpload    int64
	maxExtracted int64
	tempDir      string
	pagesDomain  string
	now          func() time.Time

	// logger may be nil, meaning "resolve slog.Default() per request" so that a
	// later slog.SetDefault still takes effect. slog loggers are safe for
	// concurrent use, so no mutex is needed here.
	logger *slog.Logger

	mux *http.ServeMux
	// handler is mux wrapped in Options.Middleware; ServeHTTP dispatches to it.
	handler http.Handler
}

var _ http.Handler = (*Server)(nil)

// New builds the admin API handler. Options.Provider is required; everything
// else is optional and degrades to 501 responses on the affected routes.
func New(opts Options) (*Server, error) {
	if opts.Provider == nil {
		return nil, fmt.Errorf("adminapi: Provider is required")
	}
	s := &Server{
		provider:     opts.Provider,
		admin:        opts.Admin,
		storage:      opts.Storage,
		maxUpload:    opts.MaxUploadBytes,
		maxExtracted: opts.MaxExtractedBytes,
		tempDir:      opts.TempDir,
		pagesDomain:  strings.TrimSpace(opts.PagesDomain),
		now:          opts.Now,
		logger:       opts.Logger,
	}
	if s.maxUpload <= 0 {
		s.maxUpload = DefaultMaxUploadBytes
	}
	if s.maxExtracted <= 0 {
		s.maxExtracted = s.maxUpload * extractedSizeFactor
	}
	if s.now == nil {
		s.now = time.Now
	}
	if err := checkTempDir(s.tempDir); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+HealthPath, s.handleHealth)

	mux.HandleFunc("GET /v1/tenants", s.handleListTenants)
	mux.HandleFunc("POST /v1/tenants", s.handleUpsertTenant)
	mux.HandleFunc("GET /v1/tenants/{tenantId}", s.handleGetTenant)
	mux.HandleFunc("DELETE /v1/tenants/{tenantId}", s.handleDeleteTenant)
	mux.HandleFunc("GET /v1/tenants/{tenantId}/pages", s.handleListTenantPages)

	mux.HandleFunc("GET /v1/pages", s.handleListPages)
	mux.HandleFunc("POST /v1/pages", s.handleUpsertPage)
	mux.HandleFunc("GET /v1/pages/{pageId}", s.handleGetPage)
	mux.HandleFunc("DELETE /v1/pages/{pageId}", s.handleDeletePage)
	mux.HandleFunc("PUT /v1/pages/{pageId}/custom-domains", s.handleSetCustomDomains)

	mux.HandleFunc("GET /v1/pages/{pageId}/secrets", s.handleListSecrets)
	mux.HandleFunc("PUT /v1/pages/{pageId}/secrets", s.handleReplaceSecrets)
	mux.HandleFunc("PATCH /v1/pages/{pageId}/secrets", s.handlePatchSecrets)
	mux.HandleFunc("PUT /v1/pages/{pageId}/secrets/{name}", s.handlePutSecret)
	mux.HandleFunc("DELETE /v1/pages/{pageId}/secrets/{name}", s.handleDeleteSecret)

	mux.HandleFunc("GET /v1/pages/{pageId}/deployments", s.handleListDeployments)
	mux.HandleFunc("POST /v1/pages/{pageId}/deployments", s.handleCreateDeployment)
	mux.HandleFunc("POST /v1/pages/{pageId}/deployments/{deploymentId}/activate", s.handleActivateDeployment)

	s.mux = mux

	// Wrap in reverse so that Middleware[0] ends up outermost.
	var h http.Handler = mux
	for i := len(opts.Middleware) - 1; i >= 0; i-- {
		mw := opts.Middleware[i]
		if mw == nil {
			return nil, fmt.Errorf("adminapi: Middleware[%d] is nil", i)
		}
		h = mw(h)
		if h == nil {
			return nil, fmt.Errorf("adminapi: Middleware[%d] returned a nil handler", i)
		}
	}
	s.handler = h
	return s, nil
}

// checkTempDir verifies that a configured Options.TempDir is usable, so that a
// read-only or missing directory is reported by New instead of turning the
// first deployment upload into a 500. An empty dir means os.TempDir(), which is
// left to the standard library.
func checkTempDir(dir string) error {
	if dir == "" {
		return nil
	}
	probe, err := os.MkdirTemp(dir, "probe-")
	if err != nil {
		return fmt.Errorf("adminapi: TempDir %q is not usable: %w", dir, err)
	}
	_ = os.Remove(probe)
	return nil
}

// ServeHTTP runs the middleware chain and the router, then logs the outcome.
// Logging is outermost so requests a middleware rejects are logged too.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	s.handler.ServeHTTP(rec, r)
	s.logRequest(r, rec, s.now().Sub(start))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// logMessage labels every request log line, so a handler that mixes sources
// can still filter this package's traffic.
const logMessage = "admin api request"

// log returns the logger to use: the configured one, or the process default
// resolved late so that a slog.SetDefault after New is still honoured.
func (s *Server) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// logRequest emits exactly one structured line per request.
//
// The level follows the outcome (2xx/3xx info, 4xx warn, 5xx error) so that an
// operator can alert on server faults without drowning in normal traffic, and
// the detail behind a 5xx (recorded by writeError) rides along as "error".
//
// The URL query is deliberately NOT logged. This API takes no secret in a
// query parameter (secret values are body-only, and only deploymentId/activate
// are read from the query), but Options.Middleware is an open extension point
// where an operator may well accept a token as a query parameter, and a
// request log is the last place such a token should surface.
func (s *Server) logRequest(r *http.Request, rec *responseRecorder, d time.Duration) {
	level := slog.LevelInfo
	switch {
	case rec.status >= http.StatusInternalServerError:
		level = slog.LevelError
	case rec.status >= http.StatusBadRequest:
		level = slog.LevelWarn
	}
	logger := s.log()
	if !logger.Enabled(r.Context(), level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", rec.status),
		slog.Int64("bytes", rec.bytes),
		slog.Int64("durationMs", d.Milliseconds()),
		slog.String("remoteAddr", r.RemoteAddr),
	}
	if rec.serverErr != "" {
		attrs = append(attrs, slog.String("error", rec.serverErr))
	}
	logger.LogAttrs(r.Context(), level, logMessage, attrs...)
}

// responseRecorder captures the response status and size for logging, and
// rewrites the ServeMux's own plain-text 404/405 replies into the API's JSON
// error envelope so that every response of this handler has the same shape.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	bytes   int64
	wrote   bool
	swallow bool
	// serverErr is the detail behind a 5xx reply, kept for the request log.
	serverErr string
}

// recordServerError stores the cause of a server-side failure so ServeHTTP can
// log it. The first one wins: it is the failure that produced the response,
// while anything after it is a follow-up on an already-committed reply.
func (w *responseRecorder) recordServerError(msg string) {
	if w.serverErr == "" {
		w.serverErr = msg
	}
}

func (w *responseRecorder) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	// http.Error (used by ServeMux for its built-in 404/405) is the only
	// producer of text/plain replies in this handler; every handler of ours
	// sets application/json before writing the header.
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		w.swallow = true
		code, body := muxErrorBody(code)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.ResponseWriter.WriteHeader(code)
		n, _ := w.ResponseWriter.Write(body)
		w.bytes += int64(n)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseRecorder) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		// The replacement envelope has already been written.
		return len(b), nil
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// muxErrorBody renders the envelope for a status produced by the ServeMux.
func muxErrorBody(code int) (int, []byte) {
	env := errorEnvelope{}
	switch code {
	case http.StatusMethodNotAllowed:
		env.Error.Code = codeMethodNotAllowed
		env.Error.Message = "method not allowed for this route"
	default:
		env.Error.Code = codeNotFound
		env.Error.Message = "no such route"
	}
	b, err := json.Marshal(env)
	if err != nil {
		return code, []byte(`{"error":{"code":"internal","message":"encode error"}}`)
	}
	return code, b
}
