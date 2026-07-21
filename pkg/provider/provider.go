// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package provider defines the PageProvider extension point: the source of
// truth for tenants, pages and deployments. The default implementation is
// PostgreSQL (package postgres); operators may supply their own.
package provider

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a host, page or tenant does not exist.
var ErrNotFound = errors.New("provider: not found")

// Tenant is the unit of worker execution, billing and isolation.
type Tenant struct {
	ID     string
	Config TenantConfig
}

// TenantConfig controls worker pod scaling for a tenant. Zero values mean
// "use the controller default".
type TenantConfig struct {
	// MaxConcurrency is the maximum number of worker pods for this tenant.
	MaxConcurrency int
	// IdleTTL is how long an idle worker pod is kept before scale-down.
	IdleTTL time.Duration
	// WorkerCPULimit / WorkerMemLimit are pod resource limits (k8s quantity
	// strings, e.g. "1", "512Mi").
	WorkerCPULimit string
	WorkerMemLimit string
	// PodLabels / PodAnnotations are merged into worker pod metadata.
	// System labels (durupages.io/*, app.kubernetes.io/*) always win.
	PodLabels      map[string]string
	PodAnnotations map[string]string
}

// Page is a single site owned by a tenant.
type Page struct {
	ID                 string
	TenantID           string
	ActiveDeploymentID string
	CustomDomains      []string
	Config             PageConfig
}

// PageConfig holds per-page settings. Env and Secret are both exposed to the
// page worker as bindings; Secret values are additionally redacted from logs.
type PageConfig struct {
	QueueTimeout   time.Duration
	RequestTimeout time.Duration
	Env            map[string]string
	Secret         map[string]string
	// LogsEnabled overrides the global log-ingest setting; nil follows it.
	LogsEnabled *bool
}

// Deployment is an immutable deployment snapshot of a page.
type Deployment struct {
	ID        string
	PageID    string
	CreatedAt time.Time
}

// PageProvider is the routing/configuration source of truth.
type PageProvider interface {
	// ResolvePage maps a request host ({pageId}.{pagesDomain} or a custom
	// CNAME domain) to its page. Returns ErrNotFound for unknown hosts.
	ResolvePage(ctx context.Context, host string) (*Page, error)
	GetPage(ctx context.Context, pageID string) (*Page, error)
	GetTenant(ctx context.Context, tenantID string) (*Tenant, error)
}

// PageEventType enumerates change notifications from PageWatcher.
type PageEventType string

const (
	PageEventPageChanged   PageEventType = "page-changed"
	PageEventPageDeleted   PageEventType = "page-deleted"
	PageEventTenantChanged PageEventType = "tenant-changed"
	PageEventTenantDeleted PageEventType = "tenant-deleted"
)

// PageEvent describes a tenant/page change.
type PageEvent struct {
	Type     PageEventType
	TenantID string
	PageID   string
}

// PageWatcher is an optional extension: providers that implement it push
// change events so the controller can invalidate caches without polling.
type PageWatcher interface {
	Watch(ctx context.Context) (<-chan PageEvent, error)
}
