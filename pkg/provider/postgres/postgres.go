// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package postgres is the default PageProvider implementation, backed by
// PostgreSQL via pgx/v5. It stores tenant and page configuration as JSONB and
// resolves request hosts either through the custom_domains table or by parsing
// "{pageID}.{PagesDomain}" subdomains. The schema mirrors ARCHITECTURE 2.4 and
// is applied by Migrate.
package postgres

import (
	"context"
	_ "embed"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/durupages/durupages/pkg/provider"
)

//go:embed schema.sql
var schemaSQL string

// Options configures a Provider.
type Options struct {
	// DSN is the PostgreSQL connection string (libpq or URL form).
	DSN string
	// PagesDomain is the apex domain for subdomain routing, e.g.
	// "pages.example.com". A host of "{pageID}.{PagesDomain}" resolves to the
	// page with that ID.
	PagesDomain string
}

// Provider is a PostgreSQL-backed PageProvider.
type Provider struct {
	pool        *pgxpool.Pool
	pagesDomain string
}

// compile-time interface check.
var _ provider.PageProvider = (*Provider)(nil)

// New opens a pgxpool connection pool from opts.DSN and returns a Provider. The
// caller must call Close when done. It does not create the schema; call Migrate
// for that.
func New(ctx context.Context, opts Options) (*Provider, error) {
	pool, err := pgxpool.New(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	return &Provider{
		pool:        pool,
		pagesDomain: strings.ToLower(opts.PagesDomain),
	}, nil
}

// Close releases the underlying connection pool.
func (p *Provider) Close() {
	p.pool.Close()
}

// Migrate applies the embedded schema (schema.sql). It uses CREATE TABLE IF NOT
// EXISTS and is safe to run repeatedly.
func (p *Provider) Migrate(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, schemaSQL)
	return err
}

// ResolvePage maps a request host to its page. It first tries an exact,
// case-insensitive match against custom_domains; failing that it parses the
// host as "{pageID}.{PagesDomain}" (single label only). A :port suffix is
// stripped defensively. Unknown hosts yield provider.ErrNotFound.
func (p *Provider) ResolvePage(ctx context.Context, host string) (*provider.Page, error) {
	host = normalizeHost(host)

	var pageID string
	err := p.pool.QueryRow(ctx, `SELECT page_id FROM custom_domains WHERE domain = $1`, host).Scan(&pageID)
	switch {
	case err == nil:
		return p.GetPage(ctx, pageID)
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to subdomain parsing
	default:
		return nil, err
	}

	label, ok := subdomainLabel(host, p.pagesDomain)
	if !ok {
		return nil, provider.ErrNotFound
	}
	return p.GetPage(ctx, label)
}

// pageColumns is the column list every pages query selects, in the order
// scanPage expects.
const pageColumns = `id, tenant_id, active_deployment_id, config`

// rowScanner is the scanning behaviour shared by pgx.Row and pgx.Rows, so one
// helper can decode both single-row and multi-row queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanPage decodes a single pages row selected with pageColumns. CustomDomains
// is left nil; callers fill it in from the custom_domains table.
func scanPage(s rowScanner) (provider.Page, error) {
	var (
		id         string
		tenantID   string
		activeDep  *string
		configJSON []byte
	)
	if err := s.Scan(&id, &tenantID, &activeDep, &configJSON); err != nil {
		return provider.Page{}, err
	}
	cfg, err := decodePageConfig(configJSON)
	if err != nil {
		return provider.Page{}, err
	}
	pg := provider.Page{ID: id, TenantID: tenantID, Config: cfg}
	if activeDep != nil {
		pg.ActiveDeploymentID = *activeDep
	}
	return pg, nil
}

// GetPage returns the page with the given ID, or provider.ErrNotFound.
func (p *Provider) GetPage(ctx context.Context, pageID string) (*provider.Page, error) {
	pg, err := scanPage(p.pool.QueryRow(ctx,
		`SELECT `+pageColumns+` FROM pages WHERE id = $1`, pageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, provider.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	domains, err := p.pageDomains(ctx, pageID)
	if err != nil {
		return nil, err
	}
	pg.CustomDomains = domains
	return &pg, nil
}

// GetTenant returns the tenant with the given ID, or provider.ErrNotFound.
func (p *Provider) GetTenant(ctx context.Context, tenantID string) (*provider.Tenant, error) {
	var configJSON []byte
	err := p.pool.QueryRow(ctx, `SELECT config FROM tenants WHERE id = $1`, tenantID).Scan(&configJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, provider.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	cfg, err := decodeTenantConfig(configJSON)
	if err != nil {
		return nil, err
	}
	return &provider.Tenant{ID: tenantID, Config: cfg}, nil
}

// pageDomains returns the custom domains bound to pageID, sorted for
// determinism. An empty result returns a nil slice.
func (p *Provider) pageDomains(ctx context.Context, pageID string) ([]string, error) {
	byPage, err := p.domainsByPage(ctx, []string{pageID})
	if err != nil {
		return nil, err
	}
	return byPage[pageID], nil
}

// domainsByPage returns the custom domains of every requested page, keyed by
// page ID and sorted for determinism. Pages without domains are absent from the
// map (a lookup then yields a nil slice, matching GetPage).
func (p *Provider) domainsByPage(ctx context.Context, pageIDs []string) (map[string][]string, error) {
	byPage := make(map[string][]string, len(pageIDs))
	if len(pageIDs) == 0 {
		return byPage, nil
	}
	rows, err := p.pool.Query(ctx,
		`SELECT page_id, domain FROM custom_domains WHERE page_id = ANY($1) ORDER BY domain`, pageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var pageID, domain string
		if err := rows.Scan(&pageID, &domain); err != nil {
			return nil, err
		}
		byPage[pageID] = append(byPage[pageID], domain)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return byPage, nil
}

// UpsertTenant inserts or replaces a tenant's configuration.
func (p *Provider) UpsertTenant(ctx context.Context, t provider.Tenant) error {
	cfg, err := encodeTenantConfig(t.Config)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`INSERT INTO tenants (id, config) VALUES ($1, $2)
		 ON CONFLICT (id) DO UPDATE SET config = EXCLUDED.config, updated_at = now()`,
		t.ID, cfg)
	return err
}

// UpsertPage inserts or replaces a page row (tenant, active deployment and
// config). Custom domains are managed separately via SetCustomDomains.
func (p *Provider) UpsertPage(ctx context.Context, pg provider.Page) error {
	cfg, err := encodePageConfig(pg.Config)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`INSERT INTO pages (id, tenant_id, active_deployment_id, config) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (id) DO UPDATE SET
		   tenant_id = EXCLUDED.tenant_id,
		   active_deployment_id = EXCLUDED.active_deployment_id,
		   config = EXCLUDED.config,
		   updated_at = now()`,
		pg.ID, pg.TenantID, nullString(pg.ActiveDeploymentID), cfg)
	return err
}

// CreateDeployment records an immutable deployment snapshot for a page.
func (p *Provider) CreateDeployment(ctx context.Context, d provider.Deployment) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO deployments (id, page_id) VALUES ($1, $2)`, d.ID, d.PageID)
	return err
}

// SetActiveDeployment points a page at a new active deployment. An empty
// deploymentID clears it. It returns provider.ErrNotFound when the page does
// not exist.
func (p *Provider) SetActiveDeployment(ctx context.Context, pageID, deploymentID string) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE pages SET active_deployment_id = $2, updated_at = now() WHERE id = $1`,
		pageID, nullString(deploymentID))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return provider.ErrNotFound
	}
	return nil
}

// SetCustomDomains transactionally replaces the full set of custom domains for
// a page: existing mappings are deleted and the given domains inserted. Domains
// are stored lowercased. Duplicate domains within the input are de-duplicated.
func (p *Provider) SetCustomDomains(ctx context.Context, pageID string, domains []string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM custom_domains WHERE page_id = $1`, pageID); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		if _, err := tx.Exec(ctx,
			`INSERT INTO custom_domains (domain, page_id) VALUES ($1, $2)
			 ON CONFLICT (domain) DO UPDATE SET page_id = EXCLUDED.page_id`,
			d, pageID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// nullString maps "" to a nil *string so the DB stores NULL rather than an
// empty string for optional columns.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// normalizeHost lowercases host and strips any :port suffix.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		if !strings.ContainsAny(host[i+1:], ".") {
			host = host[:i]
		}
	}
	return host
}

// subdomainLabel returns the single-label subdomain of host under pagesDomain.
// It reports false when pagesDomain is empty, when host is not a direct
// subdomain, or when the label itself contains a dot (multi-label).
func subdomainLabel(host, pagesDomain string) (string, bool) {
	if pagesDomain == "" {
		return "", false
	}
	suffix := "." + pagesDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := host[:len(host)-len(suffix)]
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}
