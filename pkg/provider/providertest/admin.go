// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package providertest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// RunAdminConformance runs the provider.AdminProvider behavioural contract
// against the implementation produced by factory. factory is called once per
// sub-test and MUST return a provider whose backing store is empty, so sub-tests
// do not interfere with one another.
//
// The suite deliberately reads back only through the AdminProvider surface
// (ListTenants/ListPages/ListDeployments), so it applies to implementations
// that are not also a PageProvider.
func RunAdminConformance(t *testing.T, factory func(t *testing.T) provider.AdminProvider) {
	t.Helper()

	t.Run("ListTenantsEmpty", func(t *testing.T) {
		a := factory(t)
		got, err := a.ListTenants(context.Background())
		if err != nil {
			t.Fatalf("ListTenants: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("want no tenants, got %+v", got)
		}
	})

	t.Run("UpsertTenantCreateThenUpdate", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()

		want := provider.Tenant{
			ID: "acme",
			Config: provider.TenantConfig{
				MaxConcurrency: 3,
				IdleTTL:        10 * time.Minute,
				WorkerCPULimit: "1",
				WorkerMemLimit: "256Mi",
				PodLabels:      map[string]string{"env": "prod"},
				PodAnnotations: map[string]string{"cost": "eng"},
			},
		}
		if err := a.UpsertTenant(ctx, want); err != nil {
			t.Fatalf("UpsertTenant: %v", err)
		}
		got := listTenants(t, a)
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("after create:\n got %+v\nwant %+v", got, want)
		}

		// Upserting the same ID must replace, not duplicate. Cleared fields
		// (here the label/annotation maps) must come back empty.
		want.Config = provider.TenantConfig{
			MaxConcurrency: 7,
			IdleTTL:        90 * time.Second,
			WorkerCPULimit: "2",
		}
		if err := a.UpsertTenant(ctx, want); err != nil {
			t.Fatalf("UpsertTenant update: %v", err)
		}
		got = listTenants(t, a)
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("after update:\n got %+v\nwant %+v", got, want)
		}
	})

	t.Run("ListTenantsOrderedByID", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()
		for _, id := range []string{"charlie", "alpha", "bravo"} {
			if err := a.UpsertTenant(ctx, provider.Tenant{ID: id}); err != nil {
				t.Fatalf("UpsertTenant %s: %v", id, err)
			}
		}
		got := listTenants(t, a)
		wantIDs := []string{"alpha", "bravo", "charlie"}
		if ids := tenantIDs(got); !reflect.DeepEqual(ids, wantIDs) {
			t.Fatalf("ListTenants order = %v, want %v", ids, wantIDs)
		}
	})

	t.Run("UpsertPageCreateThenUpdate", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()
		seedTenants(t, a, "acme")

		enabled := true
		want := provider.Page{
			ID:                 "blog",
			TenantID:           "acme",
			ActiveDeploymentID: "dep_1",
			Config: provider.PageConfig{
				QueueTimeout:   5 * time.Second,
				RequestTimeout: 90 * time.Second,
				Env:            map[string]string{"API_URL": "https://api", "FLAG": "on"},
				Secret:         map[string]string{"TOKEN": "s3cr3t"},
				LogsEnabled:    &enabled,
			},
		}
		if err := a.UpsertPage(ctx, want); err != nil {
			t.Fatalf("UpsertPage: %v", err)
		}
		got := listPages(t, a, "acme")
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("after create:\n got %+v\nwant %+v", got, want)
		}

		// Update: durations change, maps are cleared and LogsEnabled goes back
		// to "follow the global setting".
		want.ActiveDeploymentID = ""
		want.Config = provider.PageConfig{RequestTimeout: time.Hour}
		if err := a.UpsertPage(ctx, want); err != nil {
			t.Fatalf("UpsertPage update: %v", err)
		}
		got = listPages(t, a, "acme")
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("after update:\n got %+v\nwant %+v", got, want)
		}
		if got[0].Config.LogsEnabled != nil {
			t.Fatalf("LogsEnabled should be nil again, got %v", *got[0].Config.LogsEnabled)
		}

		// LogsEnabled=false must survive as a non-nil pointer, distinct from nil.
		disabled := false
		want.Config.LogsEnabled = &disabled
		if err := a.UpsertPage(ctx, want); err != nil {
			t.Fatalf("UpsertPage logs disabled: %v", err)
		}
		got = listPages(t, a, "acme")
		if len(got) != 1 || got[0].Config.LogsEnabled == nil || *got[0].Config.LogsEnabled {
			t.Fatalf("LogsEnabled=false not preserved: %+v", got)
		}
	})

	t.Run("ListPagesOrderingAndTenantFilter", func(t *testing.T) {
		a := factory(t)
		seedTenants(t, a, "acme", "globex")
		seedPage(t, a, "shop", "acme")
		seedPage(t, a, "blog", "acme")
		seedPage(t, a, "docs", "globex")

		if ids := pageIDs(listPages(t, a, "acme")); !reflect.DeepEqual(ids, []string{"blog", "shop"}) {
			t.Fatalf("ListPages(acme) = %v, want [blog shop]", ids)
		}
		if ids := pageIDs(listPages(t, a, "globex")); !reflect.DeepEqual(ids, []string{"docs"}) {
			t.Fatalf("ListPages(globex) = %v, want [docs]", ids)
		}
		// An empty tenantID lists every page, still ordered by ID.
		if ids := pageIDs(listPages(t, a, "")); !reflect.DeepEqual(ids, []string{"blog", "docs", "shop"}) {
			t.Fatalf("ListPages(all) = %v, want [blog docs shop]", ids)
		}
		if got := listPages(t, a, "nosuchtenant"); len(got) != 0 {
			t.Fatalf("ListPages(unknown tenant) = %+v, want none", got)
		}
	})

	t.Run("SetCustomDomainsReplacesSet", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()
		seedTenants(t, a, "acme")
		seedPage(t, a, "blog", "acme")

		if err := a.SetCustomDomains(ctx, "blog", []string{"b.example.com", "a.example.com"}); err != nil {
			t.Fatalf("SetCustomDomains: %v", err)
		}
		want := []string{"a.example.com", "b.example.com"}
		if got := listPages(t, a, "acme"); !reflect.DeepEqual(got[0].CustomDomains, want) {
			t.Fatalf("domains = %v, want %v", got[0].CustomDomains, want)
		}

		// Replace, not append.
		if err := a.SetCustomDomains(ctx, "blog", []string{"c.example.com"}); err != nil {
			t.Fatalf("SetCustomDomains replace: %v", err)
		}
		if got := listPages(t, a, "acme"); !reflect.DeepEqual(got[0].CustomDomains, []string{"c.example.com"}) {
			t.Fatalf("domains after replace = %v, want [c.example.com]", got[0].CustomDomains)
		}

		// An upsert manages page fields only; the domain set survives it.
		if err := a.UpsertPage(ctx, provider.Page{ID: "blog", TenantID: "acme"}); err != nil {
			t.Fatalf("UpsertPage: %v", err)
		}
		if got := listPages(t, a, "acme"); !reflect.DeepEqual(got[0].CustomDomains, []string{"c.example.com"}) {
			t.Fatalf("domains after upsert = %v, want [c.example.com]", got[0].CustomDomains)
		}

		// Clearing yields an empty set.
		if err := a.SetCustomDomains(ctx, "blog", nil); err != nil {
			t.Fatalf("SetCustomDomains clear: %v", err)
		}
		if got := listPages(t, a, "acme"); len(got[0].CustomDomains) != 0 {
			t.Fatalf("domains after clear = %v, want none", got[0].CustomDomains)
		}
	})

	t.Run("CreateAndListDeploymentsNewestFirst", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()
		seedTenants(t, a, "acme")
		seedPage(t, a, "blog", "acme")

		base := time.Now().Add(-time.Hour).UTC()
		for i, id := range []string{"dep_1", "dep_2", "dep_3"} {
			d := provider.Deployment{
				ID:        id,
				PageID:    "blog",
				CreatedAt: base.Add(time.Duration(i) * time.Minute),
			}
			if err := a.CreateDeployment(ctx, d); err != nil {
				t.Fatalf("CreateDeployment %s: %v", id, err)
			}
		}
		got, err := a.ListDeployments(ctx, "blog")
		if err != nil {
			t.Fatalf("ListDeployments: %v", err)
		}
		wantIDs := []string{"dep_3", "dep_2", "dep_1"}
		if ids := deploymentIDs(got); !reflect.DeepEqual(ids, wantIDs) {
			t.Fatalf("ListDeployments order = %v, want %v", ids, wantIDs)
		}
		for _, d := range got {
			if d.PageID != "blog" {
				t.Fatalf("deployment %s has PageID %q, want blog", d.ID, d.PageID)
			}
			if d.CreatedAt.IsZero() {
				t.Fatalf("deployment %s has zero CreatedAt", d.ID)
			}
		}

		// An unknown page simply has no deployments.
		other, err := a.ListDeployments(ctx, "nosuchpage")
		if err != nil {
			t.Fatalf("ListDeployments unknown page: %v", err)
		}
		if len(other) != 0 {
			t.Fatalf("want no deployments for unknown page, got %+v", other)
		}
	})

	t.Run("CreateDeploymentUnknownPage", func(t *testing.T) {
		a := factory(t)
		err := a.CreateDeployment(context.Background(),
			provider.Deployment{ID: "dep_1", PageID: "nosuchpage", CreatedAt: time.Now()})
		if err == nil {
			t.Fatal("CreateDeployment for an unknown page must fail")
		}
	})

	t.Run("SetActiveDeployment", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()
		seedTenants(t, a, "acme")
		seedPage(t, a, "blog", "acme")
		if err := a.CreateDeployment(ctx, provider.Deployment{ID: "dep_1", PageID: "blog", CreatedAt: time.Now()}); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}

		if err := a.SetActiveDeployment(ctx, "blog", "dep_1"); err != nil {
			t.Fatalf("SetActiveDeployment: %v", err)
		}
		if got := listPages(t, a, "acme"); got[0].ActiveDeploymentID != "dep_1" {
			t.Fatalf("active deployment = %q, want dep_1", got[0].ActiveDeploymentID)
		}

		// An empty deployment ID clears the pointer.
		if err := a.SetActiveDeployment(ctx, "blog", ""); err != nil {
			t.Fatalf("SetActiveDeployment clear: %v", err)
		}
		if got := listPages(t, a, "acme"); got[0].ActiveDeploymentID != "" {
			t.Fatalf("active deployment = %q, want empty", got[0].ActiveDeploymentID)
		}
	})

	t.Run("SetActiveDeploymentUnknownPage", func(t *testing.T) {
		a := factory(t)
		err := a.SetActiveDeployment(context.Background(), "nosuchpage", "dep_1")
		if !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("DeletePageCascades", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()
		seedTenants(t, a, "acme")
		seedPage(t, a, "blog", "acme")
		seedPage(t, a, "shop", "acme")
		if err := a.SetCustomDomains(ctx, "blog", []string{"www.example.com"}); err != nil {
			t.Fatalf("SetCustomDomains: %v", err)
		}
		if err := a.CreateDeployment(ctx, provider.Deployment{ID: "dep_1", PageID: "blog", CreatedAt: time.Now()}); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}

		if err := a.DeletePage(ctx, "blog"); err != nil {
			t.Fatalf("DeletePage: %v", err)
		}
		if ids := pageIDs(listPages(t, a, "")); !reflect.DeepEqual(ids, []string{"shop"}) {
			t.Fatalf("pages after delete = %v, want [shop]", ids)
		}
		deps, err := a.ListDeployments(ctx, "blog")
		if err != nil {
			t.Fatalf("ListDeployments: %v", err)
		}
		if len(deps) != 0 {
			t.Fatalf("deployments should cascade, got %+v", deps)
		}
		// The domain is free again: rebinding it to another page must work.
		if err := a.SetCustomDomains(ctx, "shop", []string{"www.example.com"}); err != nil {
			t.Fatalf("rebind cascaded domain: %v", err)
		}
	})

	t.Run("DeleteTenantCascades", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()
		seedTenants(t, a, "acme", "globex")
		seedPage(t, a, "blog", "acme")
		seedPage(t, a, "docs", "globex")
		if err := a.CreateDeployment(ctx, provider.Deployment{ID: "dep_1", PageID: "blog", CreatedAt: time.Now()}); err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}

		if err := a.DeleteTenant(ctx, "acme"); err != nil {
			t.Fatalf("DeleteTenant: %v", err)
		}
		if ids := tenantIDs(listTenants(t, a)); !reflect.DeepEqual(ids, []string{"globex"}) {
			t.Fatalf("tenants after delete = %v, want [globex]", ids)
		}
		// The tenant's pages are gone; the other tenant's are untouched.
		if ids := pageIDs(listPages(t, a, "")); !reflect.DeepEqual(ids, []string{"docs"}) {
			t.Fatalf("pages after tenant delete = %v, want [docs]", ids)
		}
		deps, err := a.ListDeployments(ctx, "blog")
		if err != nil {
			t.Fatalf("ListDeployments: %v", err)
		}
		if len(deps) != 0 {
			t.Fatalf("deployments should cascade, got %+v", deps)
		}
	})

	t.Run("DeletesAreIdempotent", func(t *testing.T) {
		a := factory(t)
		ctx := context.Background()

		// Deleting something that never existed is not an error.
		if err := a.DeleteTenant(ctx, "ghost"); err != nil {
			t.Fatalf("DeleteTenant unknown: %v", err)
		}
		if err := a.DeletePage(ctx, "ghost"); err != nil {
			t.Fatalf("DeletePage unknown: %v", err)
		}

		seedTenants(t, a, "acme")
		seedPage(t, a, "blog", "acme")
		for i := 0; i < 2; i++ {
			if err := a.DeletePage(ctx, "blog"); err != nil {
				t.Fatalf("DeletePage call %d: %v", i+1, err)
			}
			if err := a.DeleteTenant(ctx, "acme"); err != nil {
				t.Fatalf("DeleteTenant call %d: %v", i+1, err)
			}
		}
		if got := listTenants(t, a); len(got) != 0 {
			t.Fatalf("want no tenants, got %+v", got)
		}
		if got := listPages(t, a, ""); len(got) != 0 {
			t.Fatalf("want no pages, got %+v", got)
		}
	})
}

// seedTenants upserts bare tenants with the given IDs.
func seedTenants(t *testing.T, a provider.AdminProvider, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := a.UpsertTenant(context.Background(), provider.Tenant{ID: id}); err != nil {
			t.Fatalf("UpsertTenant %s: %v", id, err)
		}
	}
}

// seedPage upserts a bare page owned by tenantID.
func seedPage(t *testing.T, a provider.AdminProvider, pageID, tenantID string) {
	t.Helper()
	if err := a.UpsertPage(context.Background(), provider.Page{ID: pageID, TenantID: tenantID}); err != nil {
		t.Fatalf("UpsertPage %s: %v", pageID, err)
	}
}

// listTenants calls ListTenants and fails the test on error.
func listTenants(t *testing.T, a provider.AdminProvider) []provider.Tenant {
	t.Helper()
	got, err := a.ListTenants(context.Background())
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	return got
}

// listPages calls ListPages and fails the test on error.
func listPages(t *testing.T, a provider.AdminProvider, tenantID string) []provider.Page {
	t.Helper()
	got, err := a.ListPages(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ListPages(%q): %v", tenantID, err)
	}
	return got
}

// tenantIDs projects tenants onto their IDs for order assertions.
func tenantIDs(tenants []provider.Tenant) []string {
	ids := make([]string, 0, len(tenants))
	for _, t := range tenants {
		ids = append(ids, t.ID)
	}
	return ids
}

// pageIDs projects pages onto their IDs for order assertions.
func pageIDs(pages []provider.Page) []string {
	ids := make([]string, 0, len(pages))
	for _, p := range pages {
		ids = append(ids, p.ID)
	}
	return ids
}

// deploymentIDs projects deployments onto their IDs for order assertions.
func deploymentIDs(deployments []provider.Deployment) []string {
	ids := make([]string, 0, len(deployments))
	for _, d := range deployments {
		ids = append(ids, d.ID)
	}
	return ids
}
