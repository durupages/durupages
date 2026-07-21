// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package storage defines the Storage extension point. The default
// implementation is S3 (package s3). Only controller, router and hub access
// Storage; worker pods go through the hub.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("storage: not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key         string
	Size        int64
	ETag        string
	ContentType string
}

// Storage is a minimal blob store abstraction.
type Storage interface {
	// Get returns a reader for the object at key. The caller must close it.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	// Put stores r (of the given size, -1 if unknown) at key.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Delete(ctx context.Context, key string) error
	// List returns objects whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// Keys of the canonical storage layout (see docs/ARCHITECTURE.md 2.3).
const (
	// ManifestKeyFmt is tenants/{tenantId}/pages/{pageId}/deployments/{deploymentId}/manifest.json
	ManifestKeyFmt = "tenants/%s/pages/%s/deployments/%s/manifest.json"
	// StaticKeyFmt is tenants/{tenantId}/pages/{pageId}/deployments/{deploymentId}/static/{sha256}
	StaticKeyFmt = "tenants/%s/pages/%s/deployments/%s/static/%s"
	// WorkerBundleKeyFmt is tenants/{tenantId}/pages/{pageId}/deployments/{deploymentId}/worker.tar
	WorkerBundleKeyFmt = "tenants/%s/pages/%s/deployments/%s/worker.tar"
)
