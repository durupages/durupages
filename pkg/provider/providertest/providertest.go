// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package providertest is a reusable conformance suite for PageProvider
// implementations. Any implementation (memprovider, postgres, or a
// third-party provider) can be checked against the same behavioural contract
// by supplying a Harness and calling RunConformance.
package providertest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// PagesDomain is the pages domain the suite assumes the provider under test is
// configured with. A Harness factory MUST construct its provider with this
// value so that ResolvePage("{pageID}." + PagesDomain) resolves by subdomain.
const PagesDomain = "pages.test"

// Harness adapts a concrete PageProvider implementation to the conformance
// suite. Provider returns the implementation under test; SeedTenant and
// SeedPage install fixtures directly (bypassing any routing logic) so the
// suite can then exercise the read paths.
type Harness interface {
	// Provider returns the implementation under test. It must be configured
	// with PagesDomain as its pages domain.
	Provider() provider.PageProvider
	// SeedTenant stores (inserts or replaces) a tenant.
	SeedTenant(provider.Tenant)
	// SeedPage stores (inserts or replaces) a page, including its custom
	// domain mappings.
	SeedPage(provider.Page)
}

// RunConformance runs the full behavioural contract against the provider
// produced by factory. factory is called once per sub-test with a fresh,
// empty backing store so sub-tests do not interfere with one another.
func RunConformance(t *testing.T, factory func(t *testing.T) Harness) {
	t.Helper()

	t.Run("ResolveBySubdomain", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		p, err := h.Provider().ResolvePage(context.Background(), "blog."+PagesDomain)
		if err != nil {
			t.Fatalf("ResolvePage subdomain: %v", err)
		}
		if p.ID != "blog" || p.TenantID != "acme" {
			t.Fatalf("resolved wrong page: %+v", p)
		}
	})

	t.Run("ResolveBySubdomainCaseInsensitive", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		p, err := h.Provider().ResolvePage(context.Background(), "BLOG."+PagesDomain)
		if err != nil {
			t.Fatalf("ResolvePage uppercase subdomain: %v", err)
		}
		if p.ID != "blog" {
			t.Fatalf("resolved wrong page: %+v", p)
		}
	})

	t.Run("ResolveByCustomDomain", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		p, err := h.Provider().ResolvePage(context.Background(), "www.example.com")
		if err != nil {
			t.Fatalf("ResolvePage custom domain: %v", err)
		}
		if p.ID != "blog" {
			t.Fatalf("resolved wrong page: %+v", p)
		}
	})

	t.Run("ResolveByCustomDomainCaseInsensitive", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		p, err := h.Provider().ResolvePage(context.Background(), "WWW.Example.COM")
		if err != nil {
			t.Fatalf("ResolvePage custom domain mixed case: %v", err)
		}
		if p.ID != "blog" {
			t.Fatalf("resolved wrong page: %+v", p)
		}
	})

	t.Run("ResolveUnknownHost", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		_, err := h.Provider().ResolvePage(context.Background(), "nope.example.org")
		if !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("ResolveUnknownSubdomain", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		_, err := h.Provider().ResolvePage(context.Background(), "ghost."+PagesDomain)
		if !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("GetPageRoundtrip", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		p, err := h.Provider().GetPage(context.Background(), "blog")
		if err != nil {
			t.Fatalf("GetPage: %v", err)
		}
		if p.ID != "blog" || p.TenantID != "acme" || p.ActiveDeploymentID != "dep_1" {
			t.Fatalf("GetPage wrong result: %+v", p)
		}
	})

	t.Run("GetPageNotFound", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		_, err := h.Provider().GetPage(context.Background(), "missing")
		if !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("GetTenantRoundtrip", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		got, err := h.Provider().GetTenant(context.Background(), "acme")
		if err != nil {
			t.Fatalf("GetTenant: %v", err)
		}
		want := basicTenant()
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("GetTenant mismatch:\n got %+v\nwant %+v", *got, want)
		}
	})

	t.Run("GetTenantNotFound", func(t *testing.T) {
		h := factory(t)
		seedBasic(h)
		_, err := h.Provider().GetTenant(context.Background(), "missing")
		if !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("PageConfigPreserved", func(t *testing.T) {
		h := factory(t)
		h.SeedTenant(basicTenant())
		enabled := true
		want := provider.Page{
			ID:                 "cfg",
			TenantID:           "acme",
			ActiveDeploymentID: "dep_9",
			CustomDomains:      []string{"cfg.example.com"},
			Config: provider.PageConfig{
				QueueTimeout:   30 * time.Second,
				RequestTimeout: 90 * time.Second,
				Env:            map[string]string{"API_URL": "https://api", "FLAG": "on"},
				Secret:         map[string]string{"TOKEN": "s3cr3t"},
				LogsEnabled:    &enabled,
			},
		}
		h.SeedPage(want)
		got, err := h.Provider().GetPage(context.Background(), "cfg")
		if err != nil {
			t.Fatalf("GetPage: %v", err)
		}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("page config mismatch:\n got %+v\nwant %+v", *got, want)
		}
		if got.Config.LogsEnabled == nil || *got.Config.LogsEnabled != true {
			t.Fatalf("LogsEnabled not preserved: %v", got.Config.LogsEnabled)
		}
	})

	t.Run("PageConfigLogsEnabledNil", func(t *testing.T) {
		h := factory(t)
		h.SeedTenant(basicTenant())
		want := provider.Page{
			ID:       "nolog",
			TenantID: "acme",
			Config: provider.PageConfig{
				RequestTimeout: time.Hour,
			},
		}
		h.SeedPage(want)
		got, err := h.Provider().GetPage(context.Background(), "nolog")
		if err != nil {
			t.Fatalf("GetPage: %v", err)
		}
		if got.Config.LogsEnabled != nil {
			t.Fatalf("LogsEnabled should stay nil, got %v", *got.Config.LogsEnabled)
		}
		if got.Config.RequestTimeout != time.Hour {
			t.Fatalf("RequestTimeout not preserved: %v", got.Config.RequestTimeout)
		}
	})

	t.Run("TenantConfigPreserved", func(t *testing.T) {
		h := factory(t)
		want := provider.Tenant{
			ID: "full",
			Config: provider.TenantConfig{
				MaxConcurrency: 5,
				IdleTTL:        15 * time.Minute,
				WorkerCPULimit: "1",
				WorkerMemLimit: "512Mi",
				PodLabels:      map[string]string{"team": "web"},
				PodAnnotations: map[string]string{"cost": "eng"},
			},
		}
		h.SeedTenant(want)
		got, err := h.Provider().GetTenant(context.Background(), "full")
		if err != nil {
			t.Fatalf("GetTenant: %v", err)
		}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("tenant config mismatch:\n got %+v\nwant %+v", *got, want)
		}
	})
}

// basicTenant is the shared tenant fixture used by seedBasic.
func basicTenant() provider.Tenant {
	return provider.Tenant{
		ID: "acme",
		Config: provider.TenantConfig{
			MaxConcurrency: 3,
			IdleTTL:        10 * time.Minute,
			WorkerCPULimit: "1",
			WorkerMemLimit: "256Mi",
			PodLabels:      map[string]string{"env": "prod"},
		},
	}
}

// seedBasic installs the tenant "acme" and page "blog" (with custom domain
// www.example.com) used by the resolution and roundtrip sub-tests.
func seedBasic(h Harness) {
	h.SeedTenant(basicTenant())
	h.SeedPage(provider.Page{
		ID:                 "blog",
		TenantID:           "acme",
		ActiveDeploymentID: "dep_1",
		CustomDomains:      []string{"www.example.com"},
		Config: provider.PageConfig{
			QueueTimeout:   5 * time.Second,
			RequestTimeout: 30 * time.Second,
		},
	})
}
