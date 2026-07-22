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
package adminapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
	// LogWriter, when non-nil, receives one JSON object per request
	// (time, method, path, status, bytes, durationMs).
	LogWriter io.Writer
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
	now          func() time.Time

	logMu sync.Mutex
	logw  io.Writer

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
		now:          opts.Now,
		logw:         opts.LogWriter,
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

// logEntry is one structured request log line.
type logEntry struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	Bytes      int64  `json:"bytes"`
	DurationMs int64  `json:"durationMs"`
}

// logRequest writes one JSON line to the configured writer; it is a no-op when
// no writer was configured.
func (s *Server) logRequest(r *http.Request, rec *responseRecorder, d time.Duration) {
	if s.logw == nil {
		return
	}
	b, err := json.Marshal(logEntry{
		Time:       s.now().UTC().Format(time.RFC3339Nano),
		Method:     r.Method,
		Path:       r.URL.Path,
		Status:     rec.status,
		Bytes:      rec.bytes,
		DurationMs: d.Milliseconds(),
	})
	if err != nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = s.logw.Write(append(b, '\n'))
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
