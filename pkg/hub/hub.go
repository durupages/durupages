// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package hub implements durupages-hub as a library: the data-plane service
// that streams deployment bundles to worker pods (authenticated with the
// worker JWT) and ingests usage/log events from shims and the router. It is
// assembled by a thin main; alternative log sinks and storage backends plug in
// through the exported interfaces.
//
// See docs/ARCHITECTURE.md sections 7 and 9.
package hub

import (
	"context"
	"crypto/ed25519"
	"errors"

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
}

// Hub is the assembled hub. It is safe for concurrent use.
type Hub struct {
	storage               storage.Storage
	jwtPub                ed25519.PublicKey
	cache                 *diskCache // nil when caching is disabled
	sink                  LogSink
	maxLogsPerRequest     int
	maxLogBytesPerRequest int
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
