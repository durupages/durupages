// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/durupages/durupages/pkg/api"
)

// This file holds the shim's *operational* logging: structured log/slog lines
// describing what the shim itself is doing and, above all, why it refused or
// failed a request.
//
// Do not confuse it with Options.LogWriter / the emitter: those carry the
// tenant-facing pod log (usage events and the worker's own console output).
// Operational logs go to Options.Logger and never contain worker output.

// logMsg values are the stable messages of the shim's operational log lines.
// They are intentionally coarse: the detail lives in the attributes, so an
// operator can grep for a message and then filter by requestId or pageId.
const (
	logMsgProxyRejected = "shim: proxy request rejected"
	logMsgProxyFailed   = "shim: proxy request failed"
	logMsgLoadFailed    = "shim: bundle load failed"
	logMsgLoaded        = "shim: bundle loaded"
	logMsgAssetsFailed  = "shim: assets request failed"
	logMsgTailRejected  = "shim: tail collector request rejected"
)

// log returns the logger to use: the one configured on Options, or the process
// default resolved late so that a slog.SetDefault after New is still honoured.
// slog loggers are safe for concurrent use.
func (s *Shim) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// httpError writes an http.Error response and records its cause on the
// operational logger: 5xx at error level, everything else at warn level.
//
// The body string is part of the wire contract and deliberately stays terse;
// all of the diagnostic detail goes to the log line only. Every call site
// should pass a requestId attribute when one is available, so that the
// X-DuruPages-Request-Id a client saw on a failed response is enough to find
// the reason in the pod's stderr.
func (s *Shim) httpError(w http.ResponseWriter, r *http.Request, status int, body, msg string, attrs ...slog.Attr) {
	http.Error(w, body, status)
	s.logResponseErr(r, status, msg, attrs...)
}

// logResponseErr logs the cause of an already-written error response. Use it
// where the response is produced by something other than http.Error (for
// example http.NotFound).
func (s *Shim) logResponseErr(r *http.Request, status int, msg string, attrs ...slog.Attr) {
	level := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	logger := s.log()
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	if !logger.Enabled(ctx, level) {
		return
	}
	head := []slog.Attr{slog.Int("status", status)}
	if r != nil {
		head = append(head, slog.String("path", r.URL.Path))
	}
	logger.LogAttrs(ctx, level, msg, append(head, attrs...)...)
}

// requestIDOf returns the best request id available for a request that failed
// before (or without) lease verification: the router-supplied correlation
// header. It returns "" when the header is absent, in which case callers omit
// the attribute rather than logging an empty one.
func requestIDOf(r *http.Request) string {
	return r.Header.Get(api.HeaderRequestID)
}

// attrIf returns attrs plus slog.String(key, val) when val is non-empty, so
// that log lines never carry empty placeholder attributes.
func attrIf(attrs []slog.Attr, key, val string) []slog.Attr {
	if val == "" {
		return attrs
	}
	return append(attrs, slog.String(key, val))
}

// requestIDKey carries the lease request id down into the load path so that
// bundle load logs correlate with the proxy response the client saw.
type requestIDKey struct{}

// withRequestID returns ctx tagged with the lease request id.
func withRequestID(ctx context.Context, requestID string) context.Context {
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// requestIDFromContext returns the request id tagged by withRequestID, or "".
func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
