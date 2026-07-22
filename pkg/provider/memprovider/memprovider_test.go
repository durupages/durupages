// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package memprovider_test

import (
	"context"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/provider/memprovider"
	"github.com/durupages/durupages/pkg/provider/providertest"
)

// harness adapts a memprovider.Provider to the conformance suite.
type harness struct {
	p *memprovider.Provider
}

func (h harness) Provider() provider.PageProvider { return h.p }
func (h harness) SeedTenant(t provider.Tenant)    { h.p.PutTenant(t) }
func (h harness) SeedPage(pg provider.Page)       { h.p.PutPage(pg) }

func TestConformance(t *testing.T) {
	providertest.RunConformance(t, func(t *testing.T) providertest.Harness {
		return harness{p: memprovider.New(memprovider.Options{PagesDomain: providertest.PagesDomain})}
	})
}

func TestAdminConformance(t *testing.T) {
	providertest.RunAdminConformance(t, func(t *testing.T) provider.AdminProvider {
		return memprovider.New(memprovider.Options{PagesDomain: providertest.PagesDomain})
	})
}

func TestResolveStripsPort(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	p.PutPage(provider.Page{ID: "blog", TenantID: "acme"})

	got, err := p.ResolvePage(context.Background(), "blog.pages.test:8443")
	if err != nil {
		t.Fatalf("ResolvePage with port: %v", err)
	}
	if got.ID != "blog" {
		t.Fatalf("wrong page: %+v", got)
	}
}

func TestResolveMultiLabelSubdomainNotFound(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	p.PutPage(provider.Page{ID: "blog", TenantID: "acme"})

	// "a.blog.pages.test" is a two-label subdomain and must not resolve.
	if _, err := p.ResolvePage(context.Background(), "a.blog.pages.test"); err != provider.ErrNotFound {
		t.Fatalf("want ErrNotFound for multi-label subdomain, got %v", err)
	}
}

func TestCustomDomainRemappedOnPut(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	p.PutPage(provider.Page{ID: "blog", TenantID: "acme", CustomDomains: []string{"old.example.com"}})
	// Replace with a different domain; the old mapping must be dropped.
	p.PutPage(provider.Page{ID: "blog", TenantID: "acme", CustomDomains: []string{"new.example.com"}})

	if _, err := p.ResolvePage(context.Background(), "old.example.com"); err != provider.ErrNotFound {
		t.Fatalf("old domain should be gone, got %v", err)
	}
	if _, err := p.ResolvePage(context.Background(), "new.example.com"); err != nil {
		t.Fatalf("new domain should resolve: %v", err)
	}
}

func TestDeletePageDropsDomain(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	p.PutPage(provider.Page{ID: "blog", TenantID: "acme", CustomDomains: []string{"www.example.com"}})
	p.RemovePage("blog")

	if _, err := p.ResolvePage(context.Background(), "www.example.com"); err != provider.ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if _, err := p.GetPage(context.Background(), "blog"); err != provider.ErrNotFound {
		t.Fatalf("GetPage after delete: %v", err)
	}
}

func TestReturnedPageIsCopy(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	p.PutPage(provider.Page{
		ID: "blog", TenantID: "acme",
		CustomDomains: []string{"www.example.com"},
		Config:        provider.PageConfig{Env: map[string]string{"A": "1"}},
	})
	got, _ := p.GetPage(context.Background(), "blog")
	got.Config.Env["A"] = "mutated"
	got.CustomDomains[0] = "evil.example.com"

	again, _ := p.GetPage(context.Background(), "blog")
	if again.Config.Env["A"] != "1" || again.CustomDomains[0] != "www.example.com" {
		t.Fatalf("stored state was mutated through returned copy: %+v", again)
	}
}

func TestWatchReceivesEvents(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	p.PutTenant(provider.Tenant{ID: "acme"})
	p.PutPage(provider.Page{ID: "blog", TenantID: "acme"})
	p.PutActiveDeployment("blog", "dep_2")
	p.RemovePage("blog")
	p.RemoveTenant("acme")

	want := []provider.PageEventType{
		provider.PageEventTenantChanged,
		provider.PageEventPageChanged,
		provider.PageEventPageChanged,
		provider.PageEventPageDeleted,
		provider.PageEventTenantDeleted,
	}
	for _, wt := range want {
		select {
		case ev := <-ch:
			if ev.Type != wt {
				t.Fatalf("want event %s, got %s", wt, ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %s", wt)
		}
	}
}

func TestWatchClosesOnContextCancel(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := p.Watch(ctx)
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			// Could be a buffered event; drain until closed.
			for range ch {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after context cancel")
	}
}

func TestWatchDropsWhenSlow(t *testing.T) {
	p := memprovider.New(memprovider.Options{PagesDomain: "pages.test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := p.Watch(ctx); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Never drain; overflow the buffer. Must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			p.PutTenant(provider.Tenant{ID: "acme"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked on a slow watcher")
	}
}
