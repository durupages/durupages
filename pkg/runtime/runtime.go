// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package runtime defines the Runtime extension point: the JS worker engine
// running inside a worker pod. The default implementation is durupages-workerd
// (package workerdruntime); the shim performs lazy loading, graceful swap and
// LRU eviction on top of this interface only, so a different runtime can be
// substituted by implementing it.
//
// Runtime-neutral contracts fixed by the platform:
//   - Requests are proxied to Instance.Endpoint() with the X-DuruPages-Page
//     header selecting the target page worker.
//   - Bundles are unpacked per deployment under a directory the shim owns.
//   - Usage/trace events follow the pkg/usage schema.
package runtime

import (
	"context"
	"time"

	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/usage"
)

// PageWorker describes one page to load into an instance.
type PageWorker struct {
	PageID       string
	DeploymentID string
	// BundleDir contains the unpacked worker bundle (_worker.js[/], etc).
	BundleDir string
	// StaticDir contains the deployment's static files (for ASSETS).
	StaticDir string
	Manifest  *manifest.Manifest
	// Env and Secret are exposed identically as bindings; Secret values are
	// additionally redacted from logs by the shim.
	Env    map[string]string
	Secret map[string]string
}

// InstanceSpec is the full load set for one runtime instance. Instances are
// immutable: changing the load set means launching a new instance and
// swapping (blue-green) — the shim orchestrates that.
type InstanceSpec struct {
	Pages []PageWorker
	// AssetsEndpoint is the shim's asset service base URL (loopback) that
	// serves env.ASSETS for each page (page selected via header).
	AssetsEndpoint string
	// TailEndpoint is the shim's trace collector base URL (loopback).
	TailEndpoint string
}

// Runtime launches instances.
type Runtime interface {
	Launch(ctx context.Context, spec InstanceSpec) (Instance, error)
}

// Instance is a running engine process (or equivalent).
type Instance interface {
	// Endpoint is the loopback address requests are proxied to.
	Endpoint() string
	// WaitReady blocks until the instance can serve requests.
	WaitReady(ctx context.Context) error
	// Drain waits for in-flight requests to finish. The shim switches new
	// traffic away before calling Drain.
	Drain(ctx context.Context) error
	// Close terminates the instance and releases resources.
	Close() error
}

// RequestTrace is a per-request trace emitted by the runtime (workerd: via
// the tail stream). The shim correlates it with the in-flight request by
// RequestID and merges it into the RequestUsage event.
type RequestTrace struct {
	RequestID  string
	PageID     string
	Outcome    string
	CPUTime    time.Duration
	WallTime   time.Duration
	Logs       []usage.LogEntry
	Exceptions []usage.Exception
}

// MetricsSource is an optional Runtime/Instance extension: implementations
// that can report per-request traces (logs, exceptions, cpu time) expose
// them as a stream.
type MetricsSource interface {
	Events() <-chan RequestTrace
}
