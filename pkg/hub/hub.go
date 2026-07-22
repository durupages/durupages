// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package hub implements durupages-hub as a library: the data-plane service
// that streams deployment bundles to worker pods (authenticated with the
// worker JWT) and ingests usage/log events from shims and the router. It is
// assembled by a thin main; alternative log sinks and storage backends plug in
// through the exported interfaces.
//
// # Observability
//
// The bundle API emits exactly one log/slog line per request on Options.Logger
// (slog.Default() when unset), at info level for 2xx/3xx, warn for 4xx and
// error for 5xx. Failures carry the server-side cause as "error" plus a stable
// "reason" (why a worker JWT was rejected, which storage key was missing, what
// the blob store returned), so that a worker stuck on "load failed" is
// diagnosable from the hub's own log. Response bodies stay opaque: detail goes
// to the log, never to the client. Tokens, secret values and the Authorization
// header are never logged.
//
// See docs/ARCHITECTURE.md sections 7 and 9.
package hub

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"

	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/usage"
)

// Default limits.
const (
	// DefaultMaxLogsPerRequest caps the number of log entries kept per event.
	DefaultMaxLogsPerRequest = 256
	// DefaultMaxLogBytesPerRequest caps the total log message bytes per event.
	DefaultMaxLogBytesPerRequest = 128 * 1024
	// DefaultCacheMaxBytes is the default on-disk bundle cache budget.
	DefaultCacheMaxBytes = 4 << 30 // 4 GiB
)

// LogSink receives fully decoded usage events. The default implementation
// (package stdoutsink) writes structured JSON to stdout; production
// deployments swap in Loki/ClickHouse/S3 sinks.
type LogSink interface {
	WriteRequestUsage(ctx context.Context, events []usage.RequestUsage) error
	WriteStaticAccess(ctx context.Context, events []usage.StaticAccess) error
}

// Options configures a Hub. Storage and JWTPublicKey are required.
type Options struct {
	// Storage is the blob store bundles are read from.
	Storage storage.Storage
	// JWTPublicKey verifies worker tokens on the bundle API.
	JWTPublicKey ed25519.PublicKey
	// CacheDir is the directory for the on-disk bundle LRU cache. When empty,
	// the bundle API streams straight from Storage without caching.
	CacheDir string
	// CacheMaxBytes is the disk cache budget; <= 0 selects DefaultCacheMaxBytes.
	CacheMaxBytes int64
	// Sink receives ingested log events. Required for the LogService server.
	Sink LogSink
	// MaxLogsPerRequest caps log entries per event; <= 0 selects the default.
	MaxLogsPerRequest int
	// MaxLogBytesPerRequest caps log message bytes per event; <= 0 selects the
	// default.
	MaxLogBytesPerRequest int
	// Logger receives the hub's own operational log: one structured line per
	// bundle request (method, path, status, bytes, durationMs, tenantId,
	// pageId, deploymentId, requestId) carrying, on a failure, the storage key
	// that was looked up and the server-side cause as "error" plus a stable
	// "reason". Ingest-side faults (malformed events, sink errors, broken
	// streams) land here too.
	//
	// This is NOT the worker log pipeline: log events shipped by shims and the
	// router go to Sink (see LogSink), never here. Logger is about the hub
	// itself, so that a worker failing to load a bundle is diagnosable from the
	// hub's own log instead of only from the shim's "load failed".
	//
	// When nil the slog.Default() logger is used, resolved at the time of
	// logging so that a later slog.SetDefault is still honoured. Defaulting to
	// the process logger rather than to "logging disabled" is deliberate: an
	// embedder that wires nothing still gets its 4xx/5xx causes recorded, which
	// is exactly the operational gap this package used to have. Tests that must
	// stay quiet pass an explicit logger over io.Discard.
	Logger *slog.Logger
}

// Hub is the assembled hub. It is safe for concurrent use.
type Hub struct {
	storage               storage.Storage
	jwtPub                ed25519.PublicKey
	cache                 *diskCache // nil when caching is disabled
	sink                  LogSink
	maxLogsPerRequest     int
	maxLogBytesPerRequest int

	// logger may be nil, meaning "resolve slog.Default() per log call" so that a
	// later slog.SetDefault still takes effect. slog loggers are safe for
	// concurrent use, so no mutex is needed here.
	logger *slog.Logger
}

// log returns the logger to use: the configured one, or the process default
// resolved late so that a slog.SetDefault after New is still honoured.
func (h *Hub) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}

// New validates opts and returns a Hub.
func New(opts Options) (*Hub, error) {
	if opts.Storage == nil {
		return nil, errors.New("hub: Storage is required")
	}
	if len(opts.JWTPublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("hub: JWTPublicKey is required")
	}
	h := &Hub{
		storage:               opts.Storage,
		jwtPub:                opts.JWTPublicKey,
		sink:                  opts.Sink,
		maxLogsPerRequest:     opts.MaxLogsPerRequest,
		maxLogBytesPerRequest: opts.MaxLogBytesPerRequest,
		logger:                opts.Logger,
	}
	if h.maxLogsPerRequest <= 0 {
		h.maxLogsPerRequest = DefaultMaxLogsPerRequest
	}
	if h.maxLogBytesPerRequest <= 0 {
		h.maxLogBytesPerRequest = DefaultMaxLogBytesPerRequest
	}
	if opts.CacheDir != "" {
		max := opts.CacheMaxBytes
		if max <= 0 {
			max = DefaultCacheMaxBytes
		}
		c, err := newDiskCache(opts.CacheDir, max)
		if err != nil {
			return nil, err
		}
		h.cache = c
	}
	return h, nil
}
