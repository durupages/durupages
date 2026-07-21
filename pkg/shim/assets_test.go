// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/durupages/durupages/pkg/api"
)

// TestAssetsService verifies the env.ASSETS endpoint resolves and serves a
// loaded deployment's static files with ETag/304.
func TestAssetsService(t *testing.T) {
	h, hub := newHarness(t, harnessOpts{})
	hub.set("acme", "blog", "dep1", buildBundleTar(t, bundleSpec{
		tenant: "acme", page: "blog", dep: "dep1",
		static: map[string]string{"/index.html": "<html>hi</html>"},
	}))
	// Load the page so the shim holds its static tree.
	h.proxyRequest("blog", "dep1", "r1")

	get := func(path, inm string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, "http://"+h.shim.AssetsAddr()+path, nil)
		req.Header.Set(api.HeaderPage, "blog")
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// "/" resolves to /index.html.
	resp := get("/", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "hi") {
		t.Errorf("body = %q", body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing default CF headers")
	}

	// Conditional request returns 304.
	resp2 := get("/", etag)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional status = %d, want 304", resp2.StatusCode)
	}

	// Missing page header -> 400.
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.shim.AssetsAddr()+"/", nil)
	resp3, _ := http.DefaultClient.Do(req)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("no page header status = %d, want 400", resp3.StatusCode)
	}
}
