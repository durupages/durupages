// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEvictSupersededDeployment verifies that an old (superseded) deployment
// idle beyond MinIdle is deleted by the sweep without a swap.
func TestEvictSupersededDeployment(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{minIdle: time.Hour})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep1"}))
	hub.set("acme", "blog", "dep2", buildBundleTar(t, bundleSpec{tenant: "acme", page: "blog", dep: "dep2"}))

	h.proxyRequest("blog", "dep1", "r1")
	h.proxyRequest("blog", "dep2", "r2") // dep1 becomes superseded
	launchesBefore := h.rt.launchCount()

	dep1Dir := filepath.Join(h.shim.opts.BundleDir, "dep1")
	if _, err := os.Stat(dep1Dir); err != nil {
		t.Fatalf("dep1 dir missing before sweep: %v", err)
	}

	// Not yet idle enough: nothing evicted.
	h.shim.sweep(h.ctx)
	h.shim.mu.Lock()
	_, stillThere := h.shim.deployments["dep1"]
	h.shim.mu.Unlock()
	if !stillThere {
		t.Fatal("dep1 evicted before MinIdle elapsed")
	}

	// Advance past MinIdle and sweep: dep1 is removed, no swap.
	h.clock.advance(2 * time.Hour)
	h.shim.sweep(h.ctx)

	h.shim.mu.Lock()
	_, gone := h.shim.deployments["dep1"]
	activeDep2 := h.shim.active["blog"]
	h.shim.mu.Unlock()
	if gone {
		t.Error("dep1 not evicted after MinIdle")
	}
	if activeDep2 != "dep2" {
		t.Errorf("active blog = %q, want dep2", activeDep2)
	}
	if _, err := os.Stat(dep1Dir); !os.IsNotExist(err) {
		t.Error("dep1 dir not deleted")
	}
	if h.rt.launchCount() != launchesBefore {
		t.Errorf("swap happened for superseded eviction (launchCount %d -> %d)", launchesBefore, h.rt.launchCount())
	}
}

// TestEvictOverBudget verifies that exceeding CacheMaxBytes evicts the LRU
// active deployment and triggers one swap to shrink the load set.
func TestEvictOverBudget(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{minIdle: time.Hour, cacheMax: 2000})
	hub.set("acme", "blog", "depB", buildBundleTar(t, bundleSpec{
		tenant: "acme", page: "blog", dep: "depB",
		static: map[string]string{"/big.txt": string(make([]byte, 3000))},
	}))
	hub.set("acme", "shop", "depS", buildBundleTar(t, bundleSpec{
		tenant: "acme", page: "shop", dep: "depS",
		static: map[string]string{"/s.txt": "small"},
	}))

	h.proxyRequest("blog", "depB", "r1") // blog is older
	h.clock.advance(time.Minute)
	h.proxyRequest("shop", "depS", "r2")
	launchesBefore := h.rt.launchCount()

	// Both idle beyond MinIdle; total bytes exceed the budget.
	h.clock.advance(2 * time.Hour)
	h.shim.sweep(h.ctx)

	h.shim.mu.Lock()
	_, blogActive := h.shim.active["blog"]
	_, shopActive := h.shim.active["shop"]
	_, blogDep := h.shim.deployments["depB"]
	h.shim.mu.Unlock()

	if blogActive || blogDep {
		t.Error("LRU (blog) not evicted under budget pressure")
	}
	if !shopActive {
		t.Error("shop should remain active (evicting blog met the budget)")
	}
	if h.rt.launchCount() <= launchesBefore {
		t.Error("over-budget eviction did not trigger a swap")
	}
}
