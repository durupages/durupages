// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package provider

import "context"

// AdminProvider is an optional PageProvider extension that adds the write and
// listing operations the controller's admin API needs (tenant/page/deployment
// management). Providers that only serve traffic may skip it; the admin API
// then reports 501 Not Implemented.
//
// The default PostgreSQL provider and the in-memory provider both implement it.
//
// Implementation contract:
//   - Upsert* create the record when absent and update it otherwise.
//   - Delete* are idempotent: deleting something that does not exist is not an
//     error. Deleting a tenant deletes its pages; deleting a page deletes its
//     deployments and custom domains.
//   - SetCustomDomains replaces the page's whole domain set atomically.
//   - SetActiveDeployment returns ErrNotFound when the page does not exist.
type AdminProvider interface {
	UpsertTenant(ctx context.Context, t Tenant) error
	// ListTenants returns every tenant, ordered by ID.
	ListTenants(ctx context.Context) ([]Tenant, error)
	DeleteTenant(ctx context.Context, tenantID string) error

	UpsertPage(ctx context.Context, p Page) error
	// ListPages returns the tenant's pages ordered by ID; an empty tenantID
	// lists every page.
	ListPages(ctx context.Context, tenantID string) ([]Page, error)
	DeletePage(ctx context.Context, pageID string) error
	// SetCustomDomains atomically replaces the page's custom domain set.
	SetCustomDomains(ctx context.Context, pageID string, domains []string) error

	CreateDeployment(ctx context.Context, d Deployment) error
	// ListDeployments returns the page's deployments, newest first.
	ListDeployments(ctx context.Context, pageID string) ([]Deployment, error)
	SetActiveDeployment(ctx context.Context, pageID, deploymentID string) error
}
