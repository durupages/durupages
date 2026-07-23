// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/durupages/durupages/pkg/provider"
)

// fakeProvider is a small in-memory PageProvider + AdminProvider used by the
// tests. It deliberately does not depend on pkg/provider/memprovider so the
// admin API tests stay independent of that package's evolution.
type fakeProvider struct {
	mu          sync.Mutex
	tenants     map[string]provider.Tenant
	pages       map[string]provider.Page
	deployments map[string][]provider.Deployment // page id -> newest first

	// Call records, for assertions.
	created  []provider.Deployment
	activate [][2]string // {pageID, deploymentID}

	// listTenantsErr, when set, makes ListTenants fail with it. It is the
	// simplest way to drive a non-ErrNotFound provider failure, i.e. a 500.
	listTenantsErr error
}

var (
	_ provider.PageProvider  = (*fakeProvider)(nil)
	_ provider.AdminProvider = (*fakeProvider)(nil)
)

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		tenants:     map[string]provider.Tenant{},
		pages:       map[string]provider.Page{},
		deployments: map[string][]provider.Deployment{},
	}
}

// seedTenant inserts a tenant directly, bypassing the API.
func (f *fakeProvider) seedTenant(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants[id] = provider.Tenant{ID: id}
}

// seedPage inserts a page directly, bypassing the API.
func (f *fakeProvider) seedPage(p provider.Page) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tenants[p.TenantID]; !ok {
		f.tenants[p.TenantID] = provider.Tenant{ID: p.TenantID}
	}
	f.pages[p.ID] = clonePage(p)
}

// seedDeployment inserts a deployment directly, bypassing the API.
func (f *fakeProvider) seedDeployment(d provider.Deployment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployments[d.PageID] = append([]provider.Deployment{d}, f.deployments[d.PageID]...)
}

// page returns a copy of a stored page for assertions.
func (f *fakeProvider) page(id string) (provider.Page, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pages[id]
	return clonePage(p), ok
}

func clonePage(p provider.Page) provider.Page {
	out := p
	if p.CustomDomains != nil {
		out.CustomDomains = append([]string(nil), p.CustomDomains...)
	}
	out.Config.Env = cloneMap(p.Config.Env)
	out.Config.Secret = cloneMap(p.Config.Secret)
	return out
}

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

// --- PageProvider ---

func (f *fakeProvider) ResolvePage(ctx context.Context, host string) (*provider.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pages {
		for _, d := range p.CustomDomains {
			if d == host {
				c := clonePage(p)
				return &c, nil
			}
		}
	}
	return nil, fmt.Errorf("resolve %q: %w", host, provider.ErrNotFound)
}

func (f *fakeProvider) GetPage(ctx context.Context, pageID string) (*provider.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pages[pageID]
	if !ok {
		return nil, fmt.Errorf("page %q: %w", pageID, provider.ErrNotFound)
	}
	c := clonePage(p)
	return &c, nil
}

func (f *fakeProvider) GetTenant(ctx context.Context, tenantID string) (*provider.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tenants[tenantID]
	if !ok {
		return nil, fmt.Errorf("tenant %q: %w", tenantID, provider.ErrNotFound)
	}
	c := t
	return &c, nil
}

// --- AdminProvider ---

func (f *fakeProvider) UpsertTenant(ctx context.Context, t provider.Tenant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants[t.ID] = t
	return nil
}

func (f *fakeProvider) ListTenants(ctx context.Context) ([]provider.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listTenantsErr != nil {
		return nil, f.listTenantsErr
	}
	out := make([]provider.Tenant, 0, len(f.tenants))
	for _, t := range f.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeProvider) DeleteTenant(ctx context.Context, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tenants, tenantID)
	for id, p := range f.pages {
		if p.TenantID == tenantID {
			delete(f.pages, id)
			delete(f.deployments, id)
		}
	}
	return nil
}

func (f *fakeProvider) UpsertPage(ctx context.Context, p provider.Page) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tenants[p.TenantID]; !ok {
		return fmt.Errorf("tenant %q: %w", p.TenantID, provider.ErrNotFound)
	}
	// Mirror the AdminProvider contract (see pkg/provider/admin.go): UpsertPage
	// does NOT manage custom domains — they are owned by SetCustomDomains — so
	// the stored set is preserved and p.CustomDomains ignored. Storing them here
	// would make this fake more permissive than the real providers and hide
	// handlers that forget the separate SetCustomDomains call.
	stored := clonePage(p)
	if prev, ok := f.pages[p.ID]; ok {
		stored.CustomDomains = append([]string(nil), prev.CustomDomains...)
	} else {
		stored.CustomDomains = nil
	}
	f.pages[p.ID] = stored
	return nil
}

func (f *fakeProvider) ListPages(ctx context.Context, tenantID string) ([]provider.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []provider.Page
	for _, p := range f.pages {
		if tenantID == "" || p.TenantID == tenantID {
			out = append(out, clonePage(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeProvider) DeletePage(ctx context.Context, pageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pages, pageID)
	delete(f.deployments, pageID)
	return nil
}

func (f *fakeProvider) SetCustomDomains(ctx context.Context, pageID string, domains []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pages[pageID]
	if !ok {
		return fmt.Errorf("page %q: %w", pageID, provider.ErrNotFound)
	}
	p.CustomDomains = append([]string(nil), domains...)
	f.pages[pageID] = p
	return nil
}

func (f *fakeProvider) CreateDeployment(ctx context.Context, d provider.Deployment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pages[d.PageID]; !ok {
		return fmt.Errorf("page %q: %w", d.PageID, provider.ErrNotFound)
	}
	f.created = append(f.created, d)
	f.deployments[d.PageID] = append([]provider.Deployment{d}, f.deployments[d.PageID]...)
	return nil
}

func (f *fakeProvider) ListDeployments(ctx context.Context, pageID string) ([]provider.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pages[pageID]; !ok {
		return nil, fmt.Errorf("page %q: %w", pageID, provider.ErrNotFound)
	}
	return append([]provider.Deployment(nil), f.deployments[pageID]...), nil
}

func (f *fakeProvider) SetActiveDeployment(ctx context.Context, pageID, deploymentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pages[pageID]
	if !ok {
		return fmt.Errorf("page %q: %w", pageID, provider.ErrNotFound)
	}
	f.activate = append(f.activate, [2]string{pageID, deploymentID})
	p.ActiveDeploymentID = deploymentID
	f.pages[pageID] = p
	return nil
}
