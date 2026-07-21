// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/provider/providertest"
)

// newTestProvider opens a provider against DURUPAGES_TEST_PG_DSN, migrating the
// schema. It skips the test when the env var is unset.
func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	dsn := os.Getenv("DURUPAGES_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DURUPAGES_TEST_PG_DSN not set; skipping PostgreSQL integration tests")
	}
	ctx := context.Background()
	p, err := New(ctx, Options{DSN: dsn, PagesDomain: providertest.PagesDomain})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return p
}

// truncate empties all tables so each conformance sub-test starts clean.
func truncate(t *testing.T, p *Provider) {
	t.Helper()
	_, err := p.pool.Exec(context.Background(),
		`TRUNCATE tenants, pages, deployments, custom_domains CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// pgHarness adapts a Provider to the conformance suite.
type pgHarness struct {
	t *testing.T
	p *Provider
}

func (h pgHarness) Provider() provider.PageProvider { return h.p }

func (h pgHarness) SeedTenant(tn provider.Tenant) {
	if err := h.p.UpsertTenant(context.Background(), tn); err != nil {
		h.t.Fatalf("UpsertTenant: %v", err)
	}
}

func (h pgHarness) SeedPage(pg provider.Page) {
	ctx := context.Background()
	if err := h.p.UpsertPage(ctx, pg); err != nil {
		h.t.Fatalf("UpsertPage: %v", err)
	}
	if err := h.p.SetCustomDomains(ctx, pg.ID, pg.CustomDomains); err != nil {
		h.t.Fatalf("SetCustomDomains: %v", err)
	}
}

func TestPostgresConformance(t *testing.T) {
	// Establish (and skip on absence) once, then reuse the pool per sub-test.
	p := newTestProvider(t)
	providertest.RunConformance(t, func(t *testing.T) providertest.Harness {
		truncate(t, p)
		return pgHarness{t: t, p: p}
	})
}

func TestPostgresWriteHelpers(t *testing.T) {
	p := newTestProvider(t)
	truncate(t, p)
	ctx := context.Background()

	if err := p.UpsertTenant(ctx, provider.Tenant{ID: "acme"}); err != nil {
		t.Fatalf("UpsertTenant: %v", err)
	}
	if err := p.UpsertPage(ctx, provider.Page{ID: "blog", TenantID: "acme"}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}

	// CreateDeployment then SetActiveDeployment.
	if err := p.CreateDeployment(ctx, provider.Deployment{ID: "dep_1", PageID: "blog", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if err := p.SetActiveDeployment(ctx, "blog", "dep_1"); err != nil {
		t.Fatalf("SetActiveDeployment: %v", err)
	}
	got, err := p.GetPage(ctx, "blog")
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if got.ActiveDeploymentID != "dep_1" {
		t.Fatalf("active deployment not set: %q", got.ActiveDeploymentID)
	}

	// SetCustomDomains is a transactional replace; lowercases and de-duplicates.
	if err := p.SetCustomDomains(ctx, "blog", []string{"A.example.com", "b.example.com", "a.example.com"}); err != nil {
		t.Fatalf("SetCustomDomains: %v", err)
	}
	got, err = p.GetPage(ctx, "blog")
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if len(got.CustomDomains) != 2 {
		t.Fatalf("want 2 domains, got %v", got.CustomDomains)
	}
	if _, err := p.ResolvePage(ctx, "b.example.com"); err != nil {
		t.Fatalf("ResolvePage custom domain: %v", err)
	}

	// Replace with a smaller set; old domains must disappear.
	if err := p.SetCustomDomains(ctx, "blog", []string{"only.example.com"}); err != nil {
		t.Fatalf("SetCustomDomains replace: %v", err)
	}
	if _, err := p.ResolvePage(ctx, "a.example.com"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("old domain should be gone, got %v", err)
	}

	// Clearing the active deployment stores NULL and reads back as "".
	if err := p.SetActiveDeployment(ctx, "blog", ""); err != nil {
		t.Fatalf("SetActiveDeployment clear: %v", err)
	}
	got, err = p.GetPage(ctx, "blog")
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if got.ActiveDeploymentID != "" {
		t.Fatalf("active deployment should be empty, got %q", got.ActiveDeploymentID)
	}
}
