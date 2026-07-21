// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package assets

import "testing"

func TestDefaultHeaders(t *testing.T) {
	want := map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Referrer-Policy":             "strict-origin-when-cross-origin",
		"X-Content-Type-Options":      "nosniff",
		"Cache-Control":               "public, max-age=0, must-revalidate",
	}
	got := DefaultHeaders()
	if len(got) != len(want) {
		t.Fatalf("DefaultHeaders() has %d entries, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("DefaultHeaders()[%q] = %q, want %q", k, got[k], v)
		}
	}

	// A fresh map must be returned each call so callers may mutate safely.
	got["X-Extra"] = "1"
	if _, ok := DefaultHeaders()["X-Extra"]; ok {
		t.Error("DefaultHeaders() returned a shared map; mutation leaked")
	}
}
