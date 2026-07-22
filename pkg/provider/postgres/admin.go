// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/durupages/durupages/pkg/provider"
)

// compile-time interface check: the PostgreSQL provider is a full admin backend.
var _ provider.AdminProvider = (*Provider)(nil)

// ListTenants returns every tenant, ordered by ID. An empty store yields a nil
// slice.
func (p *Provider) ListTenants(ctx context.Context) ([]provider.Tenant, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, config FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tenants []provider.Tenant
	for rows.Next() {
		var (
			id         string
			configJSON []byte
		)
		if err := rows.Scan(&id, &configJSON); err != nil {
			return nil, err
		}
		cfg, err := decodeTenantConfig(configJSON)
		if err != nil {
			return nil, err
		}
		tenants = append(tenants, provider.Tenant{ID: id, Config: cfg})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tenants, nil
}

// DeleteTenant removes a tenant. It is idempotent: deleting an unknown tenant
// is not an error. The tenant's pages (and, transitively, their deployments and
// custom domains) are removed by the schema's ON DELETE CASCADE constraints.
func (p *Provider) DeleteTenant(ctx context.Context, tenantID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	return err
}

// ListPages returns pages ordered by ID. An empty tenantID lists every page;
// otherwise only that tenant's pages are returned. Each page carries the same
// CustomDomains and Config that GetPage would report.
func (p *Provider) ListPages(ctx context.Context, tenantID string) ([]provider.Page, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if tenantID == "" {
		rows, err = p.pool.Query(ctx, `SELECT `+pageColumns+` FROM pages ORDER BY id`)
	} else {
		rows, err = p.pool.Query(ctx,
			`SELECT `+pageColumns+` FROM pages WHERE tenant_id = $1 ORDER BY id`, tenantID)
	}
	if err != nil {
		return nil, err
	}

	var (
		pages   []provider.Page
		pageIDs []string
	)
	for rows.Next() {
		pg, err := scanPage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		pages = append(pages, pg)
		pageIDs = append(pageIDs, pg.ID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	byPage, err := p.domainsByPage(ctx, pageIDs)
	if err != nil {
		return nil, err
	}
	for i := range pages {
		pages[i].CustomDomains = byPage[pages[i].ID]
	}
	return pages, nil
}

// DeletePage removes a page. It is idempotent: deleting an unknown page is not
// an error. The page's deployments and custom domains are removed by the
// schema's ON DELETE CASCADE constraints.
func (p *Provider) DeletePage(ctx context.Context, pageID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1`, pageID)
	return err
}

// ListDeployments returns the page's deployments newest first (created_at
// descending, ties broken by descending ID). An unknown page yields a nil
// slice rather than an error.
func (p *Provider) ListDeployments(ctx context.Context, pageID string) ([]provider.Deployment, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, page_id, created_at FROM deployments
		 WHERE page_id = $1 ORDER BY created_at DESC, id DESC`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []provider.Deployment
	for rows.Next() {
		var d provider.Deployment
		if err := rows.Scan(&d.ID, &d.PageID, &d.CreatedAt); err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deployments, nil
}
