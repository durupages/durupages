// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package memprovider is a thread-safe, in-memory PageProvider intended for
// tests and local development. It implements provider.PageProvider, the
// optional provider.PageWatcher extension and provider.AdminProvider (see
// admin.go), plus non-context mutation helpers used to drive fixtures. It is
// not durable: all state lives in memory.
package memprovider

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/durupages/durupages/pkg/provider"
)

// watchBuffer is the per-watcher channel buffer. Sends are non-blocking; when a
// watcher falls behind, events are dropped rather than blocking mutators.
const watchBuffer = 256

// Options configures a Provider.
type Options struct {
	// PagesDomain is the apex domain for subdomain routing, e.g.
	// "pages.example.com". A host of "{pageID}.{PagesDomain}" resolves to the
	// page with that ID.
	PagesDomain string
}

// Provider is an in-memory PageProvider.
type Provider struct {
	pagesDomain string

	mu          sync.RWMutex
	tenants     map[string]provider.Tenant
	pages       map[string]provider.Page
	byDomain    map[string]string                // lowercased custom domain -> pageID
	deployments map[string][]provider.Deployment // pageID -> deployments
	watchers    []chan provider.PageEvent
}

// compile-time interface checks.
var (
	_ provider.PageProvider = (*Provider)(nil)
	_ provider.PageWatcher  = (*Provider)(nil)
)

// New returns an empty Provider configured with the given options.
func New(opts Options) *Provider {
	return &Provider{
		pagesDomain: strings.ToLower(opts.PagesDomain),
		tenants:     make(map[string]provider.Tenant),
		pages:       make(map[string]provider.Page),
		byDomain:    make(map[string]string),
		deployments: make(map[string][]provider.Deployment),
	}
}

// ResolvePage maps a request host to its page. A host of the form
// "{label}.{PagesDomain}" (single label only) resolves by page ID; any other
// host is matched case-insensitively against custom domains. A :port suffix is
// stripped defensively. Unknown hosts yield provider.ErrNotFound.
func (p *Provider) ResolvePage(ctx context.Context, host string) (*provider.Page, error) {
	host = normalizeHost(host)

	p.mu.RLock()
	defer p.mu.RUnlock()

	if label, ok := subdomainLabel(host, p.pagesDomain); ok {
		if pg, found := p.pages[label]; found {
			return clonePage(pg), nil
		}
		return nil, provider.ErrNotFound
	}

	if pageID, ok := p.byDomain[host]; ok {
		if pg, found := p.pages[pageID]; found {
			return clonePage(pg), nil
		}
	}
	return nil, provider.ErrNotFound
}

// GetPage returns the page with the given ID, or provider.ErrNotFound.
func (p *Provider) GetPage(ctx context.Context, pageID string) (*provider.Page, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if pg, ok := p.pages[pageID]; ok {
		return clonePage(pg), nil
	}
	return nil, provider.ErrNotFound
}

// GetTenant returns the tenant with the given ID, or provider.ErrNotFound.
func (p *Provider) GetTenant(ctx context.Context, tenantID string) (*provider.Tenant, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if t, ok := p.tenants[tenantID]; ok {
		return cloneTenant(t), nil
	}
	return nil, provider.ErrNotFound
}

// Watch returns a channel that receives change events until ctx is cancelled.
// Sends are non-blocking (buffered, dropped when full) so a slow consumer never
// blocks mutators.
func (p *Provider) Watch(ctx context.Context) (<-chan provider.PageEvent, error) {
	ch := make(chan provider.PageEvent, watchBuffer)

	p.mu.Lock()
	p.watchers = append(p.watchers, ch)
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		p.mu.Lock()
		for i, w := range p.watchers {
			if w == ch {
				p.watchers = append(p.watchers[:i], p.watchers[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		close(ch)
	}()

	return ch, nil
}

// PutTenant inserts or replaces a tenant and emits a tenant-changed event.
func (p *Provider) PutTenant(t provider.Tenant) {
	stored := cloneTenant(t)
	p.mu.Lock()
	p.tenants[t.ID] = *stored
	p.mu.Unlock()
	p.emit(provider.PageEvent{Type: provider.PageEventTenantChanged, TenantID: t.ID})
}

// RemoveTenant is the non-context spelling of DeleteTenant, kept for fixture
// code. Like DeleteTenant it also removes the tenant's pages.
func (p *Provider) RemoveTenant(tenantID string) {
	_ = p.DeleteTenant(context.Background(), tenantID)
}

// PutPage inserts or replaces a page together with its custom domains: the
// domain index is rebuilt from the page's CustomDomains, and a page-changed
// event is emitted. This differs from UpsertPage, which leaves the domain set
// alone (see admin.go).
func (p *Provider) PutPage(pg provider.Page) {
	p.mu.Lock()
	p.removeDomainsLocked(pg.ID)
	stored := *clonePage(pg)
	stored.CustomDomains = normalizeDomains(pg.CustomDomains)
	p.pages[pg.ID] = stored
	for _, d := range stored.CustomDomains {
		p.byDomain[d] = pg.ID
	}
	p.mu.Unlock()
	p.emit(provider.PageEvent{Type: provider.PageEventPageChanged, TenantID: pg.TenantID, PageID: pg.ID})
}

// RemovePage is the non-context spelling of DeletePage, kept for fixture code.
func (p *Provider) RemovePage(pageID string) {
	_ = p.DeletePage(context.Background(), pageID)
}

// PutActiveDeployment is the non-context spelling of SetActiveDeployment, kept
// for fixture code. It is a no-op if the page does not exist.
func (p *Provider) PutActiveDeployment(pageID, deploymentID string) {
	_ = p.SetActiveDeployment(context.Background(), pageID, deploymentID)
}

// removeDomainsLocked drops all custom-domain mappings that point at pageID.
// The caller must hold p.mu for writing.
func (p *Provider) removeDomainsLocked(pageID string) {
	for d, id := range p.byDomain {
		if id == pageID {
			delete(p.byDomain, d)
		}
	}
}

// deletePageLocked removes a page along with its custom domains and
// deployments, reporting whether it existed. The caller must hold p.mu for
// writing and is responsible for emitting the page-deleted event.
func (p *Provider) deletePageLocked(pageID string) (provider.Page, bool) {
	pg, existed := p.pages[pageID]
	p.removeDomainsLocked(pageID)
	delete(p.pages, pageID)
	delete(p.deployments, pageID)
	return pg, existed
}

// normalizeDomains lowercases and trims domains, drops empties and duplicates
// and sorts the result, mirroring how the PostgreSQL provider stores and
// returns custom domains. An empty input yields nil.
func normalizeDomains(domains []string) []string {
	var out []string
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
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// emit broadcasts an event to every watcher without blocking.
func (p *Provider) emit(ev provider.PageEvent) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, ch := range p.watchers {
		select {
		case ch <- ev:
		default:
			// Watcher is slow; drop the event.
		}
	}
}

// normalizeHost lowercases host and strips any :port suffix.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		// Only strip when the remainder looks like a port (no IPv6 brackets
		// expected here since hosts are DNS names).
		if !strings.ContainsAny(host[i+1:], ".") {
			host = host[:i]
		}
	}
	return strings.TrimPrefix(host, "[")
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

// clonePage returns a deep copy of pg so callers cannot mutate stored state.
func clonePage(pg provider.Page) *provider.Page {
	out := pg
	if pg.CustomDomains != nil {
		out.CustomDomains = append([]string(nil), pg.CustomDomains...)
	}
	out.Config.Env = cloneMap(pg.Config.Env)
	out.Config.Secret = cloneMap(pg.Config.Secret)
	if pg.Config.LogsEnabled != nil {
		v := *pg.Config.LogsEnabled
		out.Config.LogsEnabled = &v
	}
	return &out
}

// cloneTenant returns a deep copy of t.
func cloneTenant(t provider.Tenant) *provider.Tenant {
	out := t
	out.Config.PodLabels = cloneMap(t.Config.PodLabels)
	out.Config.PodAnnotations = cloneMap(t.Config.PodAnnotations)
	return &out
}

// cloneMap copies a string map, preserving nil.
func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
