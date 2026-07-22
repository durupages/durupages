// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package memprovider

import (
	"context"
	"sort"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// compile-time interface check: the in-memory provider is a full admin backend.
var _ provider.AdminProvider = (*Provider)(nil)

// UpsertTenant inserts or replaces a tenant and emits a tenant-changed event.
func (p *Provider) UpsertTenant(ctx context.Context, t provider.Tenant) error {
	p.PutTenant(t)
	return nil
}

// ListTenants returns every tenant, ordered by ID. An empty store yields a nil
// slice.
func (p *Provider) ListTenants(ctx context.Context) ([]provider.Tenant, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ids := make([]string, 0, len(p.tenants))
	for id := range p.tenants {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var tenants []provider.Tenant
	for _, id := range ids {
		tenants = append(tenants, *cloneTenant(p.tenants[id]))
	}
	return tenants, nil
}

// DeleteTenant removes a tenant and its pages (and, with them, their
// deployments and custom domains), emitting a page-deleted event per cascaded
// page followed by a tenant-deleted event. It is idempotent.
func (p *Provider) DeleteTenant(ctx context.Context, tenantID string) error {
	p.mu.Lock()
	_, existed := p.tenants[tenantID]
	delete(p.tenants, tenantID)

	var doomed []string
	for id, pg := range p.pages {
		if pg.TenantID == tenantID {
			doomed = append(doomed, id)
		}
	}
	sort.Strings(doomed)
	for _, id := range doomed {
		p.deletePageLocked(id)
	}
	p.mu.Unlock()

	for _, id := range doomed {
		p.emit(provider.PageEvent{Type: provider.PageEventPageDeleted, TenantID: tenantID, PageID: id})
	}
	if existed {
		p.emit(provider.PageEvent{Type: provider.PageEventTenantDeleted, TenantID: tenantID})
	}
	return nil
}

// UpsertPage inserts or replaces a page and emits a page-changed event.
//
// Like the PostgreSQL provider, it does not touch the page's custom domains:
// pg.CustomDomains is ignored and an existing domain set survives the upsert.
// Domains are managed with SetCustomDomains; PutPage installs a page and its
// domains together.
func (p *Provider) UpsertPage(ctx context.Context, pg provider.Page) error {
	p.mu.Lock()
	stored := *clonePage(pg)
	stored.CustomDomains = nil
	if old, ok := p.pages[pg.ID]; ok {
		stored.CustomDomains = append([]string(nil), old.CustomDomains...)
	}
	p.pages[pg.ID] = stored
	p.mu.Unlock()

	p.emit(provider.PageEvent{Type: provider.PageEventPageChanged, TenantID: pg.TenantID, PageID: pg.ID})
	return nil
}

// ListPages returns pages ordered by ID. An empty tenantID lists every page;
// otherwise only that tenant's pages are returned. An empty result yields a nil
// slice.
func (p *Provider) ListPages(ctx context.Context, tenantID string) ([]provider.Page, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ids := make([]string, 0, len(p.pages))
	for id, pg := range p.pages {
		if tenantID != "" && pg.TenantID != tenantID {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var pages []provider.Page
	for _, id := range ids {
		pages = append(pages, *clonePage(p.pages[id]))
	}
	return pages, nil
}

// DeletePage removes a page along with its custom domains and deployments,
// emitting a page-deleted event. It is idempotent.
func (p *Provider) DeletePage(ctx context.Context, pageID string) error {
	p.mu.Lock()
	pg, existed := p.deletePageLocked(pageID)
	p.mu.Unlock()

	if existed {
		p.emit(provider.PageEvent{Type: provider.PageEventPageDeleted, TenantID: pg.TenantID, PageID: pageID})
	}
	return nil
}

// SetCustomDomains atomically replaces the page's whole custom domain set.
// Domains are lowercased, trimmed, de-duplicated and stored sorted. It returns
// provider.ErrNotFound when the page does not exist, and emits a page-changed
// event otherwise.
func (p *Provider) SetCustomDomains(ctx context.Context, pageID string, domains []string) error {
	p.mu.Lock()
	pg, ok := p.pages[pageID]
	if !ok {
		p.mu.Unlock()
		return provider.ErrNotFound
	}
	p.removeDomainsLocked(pageID)
	pg.CustomDomains = normalizeDomains(domains)
	for _, d := range pg.CustomDomains {
		p.byDomain[d] = pageID
	}
	p.pages[pageID] = pg
	p.mu.Unlock()

	p.emit(provider.PageEvent{Type: provider.PageEventPageChanged, TenantID: pg.TenantID, PageID: pageID})
	return nil
}

// CreateDeployment records an immutable deployment snapshot for a page. A zero
// CreatedAt is filled in with the current time, mirroring the PostgreSQL
// provider's server-side default. It returns provider.ErrNotFound when the page
// does not exist.
func (p *Provider) CreateDeployment(ctx context.Context, d provider.Deployment) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pages[d.PageID]; !ok {
		return provider.ErrNotFound
	}
	p.deployments[d.PageID] = append(p.deployments[d.PageID], d)
	return nil
}

// ListDeployments returns the page's deployments newest first (CreatedAt
// descending, ties broken by descending ID). An unknown page yields a nil slice
// rather than an error.
func (p *Provider) ListDeployments(ctx context.Context, pageID string) ([]provider.Deployment, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stored := p.deployments[pageID]
	if len(stored) == 0 {
		return nil, nil
	}
	out := append([]provider.Deployment(nil), stored...)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// SetActiveDeployment points a page at a new active deployment and emits a
// page-changed event. An empty deploymentID clears it. It returns
// provider.ErrNotFound when the page does not exist.
func (p *Provider) SetActiveDeployment(ctx context.Context, pageID, deploymentID string) error {
	p.mu.Lock()
	pg, ok := p.pages[pageID]
	if !ok {
		p.mu.Unlock()
		return provider.ErrNotFound
	}
	pg.ActiveDeploymentID = deploymentID
	p.pages[pageID] = pg
	p.mu.Unlock()

	p.emit(provider.PageEvent{Type: provider.PageEventPageChanged, TenantID: pg.TenantID, PageID: pageID})
	return nil
}
