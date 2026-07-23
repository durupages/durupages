// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/durupages/durupages/pkg/api"
)

// This file holds the router's OPERATIONAL log: the log/slog stream describing
// what the router itself is doing and why a request failed.
//
// It is deliberately separate from log.go, which implements the usage log (the
// StaticAccess events shipped to the hub's LogService, or written as pod-log
// JSON lines to Options.LogWriter). The two never mix:
//
//	Options.Logger              -> this file: operator-facing diagnostics.
//	Options.LogClient/LogWriter -> log.go: per-request usage/billing records.

// Log messages. They are fixed strings so that an operator can grep or alert on
// one specific failure mode; the varying detail lives in the attributes.
const (
	accessLogMessage    = "router request"
	msgUnknownHost      = "router: unknown host"
	msgResolveFailed    = "router: host resolution failed"
	msgManifestMissing  = "router: manifest not found"
	msgManifestFailed   = "router: manifest load failed"
	msgAcquireFailed    = "router: worker slot acquire failed"
	msgAcquireStream    = "router: worker slot stream failed"
	msgQueueTimeout     = "router: worker slot queue timeout"
	msgBadEndpoint      = "router: worker endpoint invalid"
	msgProxyFailed      = "router: worker proxy failed"
	msgUpstream5xx      = "router: worker returned server error"
	msgReleaseFailed    = "router: worker slot release failed"
	msgStaticFetch      = "router: static asset fetch failed"
	msgStaticNotFound   = "router: static asset not found"
	msgMethodNotAllowed = "router: method not allowed on static route"
)

// Route labels for the access log's "route" attribute.
const (
	routeNone    = "none"
	routeStatic  = "static"
	routeDynamic = "dynamic"
)

// reqLog carries the per-request context every operational log line of this
// package is decorated with. It is filled in as the pipeline learns more: host
// resolution supplies the page attribution, and the dynamic route supplies the
// lease request ID once a slot has been granted.
//
// A single request is handled by a single goroutine, so no locking is needed.
type reqLog struct {
	host      string
	route     string
	page      *api.PageInfo
	requestID string
	start     time.Time
}

// log returns the logger to use: the configured one, or the process default
// resolved late so that a slog.SetDefault after New is still honoured. This
// matches the convention of pkg/adminapi.
func (rt *Router) log() *slog.Logger {
	if rt.logger != nil {
		return rt.logger
	}
	return slog.Default()
}

// logEvent emits one operational line, prefixing the request context (method,
// host, path, request ID and page attribution) to the caller's own attributes.
//
// The URL query is deliberately never logged: a page may well accept a token as
// a query parameter, and a router log is the last place such a token should
// surface. Lease signatures, page secrets and the Authorization header are
// likewise never logged.
func (rt *Router) logEvent(r *http.Request, rl *reqLog, level slog.Level, msg string, extra ...slog.Attr) {
	logger := rt.log()
	if !logger.Enabled(r.Context(), level) {
		return
	}
	attrs := make([]slog.Attr, 0, 8+len(extra))
	attrs = append(attrs,
		slog.String("method", r.Method),
		slog.String("host", rl.host),
		slog.String("path", r.URL.Path),
	)
	if rl.requestID != "" {
		attrs = append(attrs, slog.String("requestId", rl.requestID))
	}
	attrs = append(attrs, pageAttrs(rl.page)...)
	attrs = append(attrs, extra...)
	logger.LogAttrs(r.Context(), level, msg, attrs...)
}

// unknownHostLogInterval bounds how often the unknown-host warning is emitted.
const unknownHostLogInterval = 10 * time.Second

// logUnknownHost emits the unknown-host warning at most once per
// unknownHostLogInterval, carrying the count suppressed since the last line.
//
// Unlike every other operational line, this one's trigger is fully
// client-controlled: the request Host. Logging one synchronous slog line per
// request would let a trivial flood of bogus Host headers (or hits on the bare
// node address, which the comment at the call site calls out as background
// noise) amplify into unbounded log volume, and because the line is written
// synchronously to stderr, couple log-sink backpressure straight to request
// latency. Rate-limiting caps both regardless of request rate while still
// surfacing the misconfiguration it is meant to catch.
func (rt *Router) logUnknownHost(r *http.Request, rl *reqLog) {
	logger := rt.log()
	if !logger.Enabled(r.Context(), slog.LevelWarn) {
		return
	}
	now := rt.now().UnixNano()
	last := rt.unknownHostLast.Load()
	if now-last < int64(unknownHostLogInterval) || !rt.unknownHostLast.CompareAndSwap(last, now) {
		// Within the window, or lost the race to another goroutine that is
		// emitting this tick: record the drop and stay silent.
		rt.unknownHostSuppressed.Add(1)
		return
	}
	extra := []slog.Attr{slog.Int("status", http.StatusNotFound)}
	if n := rt.unknownHostSuppressed.Swap(0); n > 0 {
		extra = append(extra, slog.Int64("suppressedSince", n))
	}
	rt.logEvent(r, rl, slog.LevelWarn, msgUnknownHost, extra...)
}

// pageAttrs is the page attribution shared by every line once the host has been
// resolved. It returns nil before that.
func pageAttrs(page *api.PageInfo) []slog.Attr {
	if page == nil {
		return nil
	}
	return []slog.Attr{
		slog.String("tenantId", page.GetTenantId()),
		slog.String("pageId", page.GetPageId()),
		slog.String("deploymentId", page.GetActiveDeploymentId()),
	}
}

// logAccess emits the one-line-per-request access log.
//
// It is logged at DEBUG on purpose. The router is the data plane: every asset
// of every page passes through here, so an always-on info-level access log
// would dominate the operator's log budget while adding nothing that the
// failure lines above do not already carry. Failures are logged at warn/error
// with the full request context, so the default info-level handler still shows
// exactly one line per failed request and nothing per successful one; an
// operator who wants full access logging lowers the handler level to debug
// (durupages-router: --log-level=debug) with no code change or restart-time
// feature flag. The Enabled check below keeps the disabled path close to free.
func (rt *Router) logAccess(r *http.Request, rec *respRecorder, rl *reqLog) {
	logger := rt.log()
	if !logger.Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	route := rl.route
	if route == "" {
		route = routeNone
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("host", rl.host),
		slog.String("path", r.URL.Path),
		slog.Int("status", rec.status),
		slog.Int64("bytes", rec.bytes),
		slog.Int64("durationMs", rt.now().Sub(rl.start).Milliseconds()),
		slog.String("route", route),
	}
	if rl.requestID != "" {
		attrs = append(attrs, slog.String("requestId", rl.requestID))
	}
	attrs = append(attrs, pageAttrs(rl.page)...)
	logger.LogAttrs(r.Context(), slog.LevelDebug, accessLogMessage, attrs...)
}

// respRecorder captures the response status and size for the access log.
//
// It implements Unwrap so that http.ResponseController — which is what
// httputil.ReverseProxy uses for flushing and for protocol switches
// (WebSocket/101 upgrades) — reaches the underlying ResponseWriter through it.
type respRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *respRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respRecorder) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *respRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ReadFrom keeps static serving's zero-copy (sendfile) path. Static assets are
// served with io.Copy(w, *os.File); io.Copy uses the destination's
// io.ReaderFrom when it has one, and *http.response implements ReadFrom with a
// sendfile fast path. Wrapping the writer would hide that ReaderFrom (io.Copy
// does not look through Unwrap), forcing a userspace 32 KiB copy on every asset,
// so respRecorder re-exposes it, delegating to the underlying writer's ReadFrom.
func (w *respRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	rf, ok := w.ResponseWriter.(io.ReaderFrom)
	if !ok {
		// No fast path on the underlying writer (e.g. a test recorder); a plain
		// copy still works and still counts bytes for the access log.
		n, err := io.Copy(w.ResponseWriter, src)
		w.bytes += n
		return n, err
	}
	n, err := rf.ReadFrom(src)
	w.bytes += n
	return n, err
}
