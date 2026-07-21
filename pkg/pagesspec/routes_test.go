// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package pagesspec

import (
	"strings"
	"testing"

	"github.com/durupages/durupages/pkg/manifest"
)

func TestParseRoutesValid(t *testing.T) {
	data := []byte(`{"version":1,"include":["/*"],"exclude":["/static/*","/favicon.ico"]}`)
	r, err := ParseRoutes(data)
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	if r.Version != 1 || len(r.Include) != 1 || len(r.Exclude) != 2 {
		t.Fatalf("unexpected routes: %+v", r)
	}
}

func TestParseRoutesErrors(t *testing.T) {
	longRule := "/" + strings.Repeat("a", 100) // 101 chars
	tests := []struct {
		name string
		data string
	}{
		{"bad version", `{"version":2,"include":["/*"]}`},
		{"empty include", `{"version":1,"include":[]}`},
		{"missing include", `{"version":1,"exclude":["/x"]}`},
		{"rule not slash", `{"version":1,"include":["api/*"]}`},
		{"rule too long", `{"version":1,"include":["` + longRule + `"]}`},
		{"invalid json", `{"version":1,`},
		{"unknown field", `{"version":1,"include":["/*"],"foo":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRoutes([]byte(tc.data)); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestParseRoutesTooManyRules(t *testing.T) {
	var inc []string
	for i := 0; i < 101; i++ {
		inc = append(inc, "/p")
	}
	data := `{"version":1,"include":[` + strings.Repeat(`"/p",`, 100) + `"/p"]}`
	if _, err := ParseRoutes([]byte(data)); err == nil {
		t.Fatal("expected error for >100 rules")
	}
	_ = inc
}

func TestMatchRoutes(t *testing.T) {
	r := &manifest.Routes{
		Version: 1,
		Include: []string{"/*"},
		Exclude: []string{"/static/*", "/favicon.ico"},
	}
	tests := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/api/users", true},
		{"/static/app.js", false},    // excluded
		{"/static/", false},          // excluded (wildcard across slash)
		{"/favicon.ico", false},      // excluded exact
		{"/other/favicon.ico", true}, // not excluded
	}
	for _, tc := range tests {
		if got := MatchRoutes(r, tc.path); got != tc.want {
			t.Errorf("MatchRoutes(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMatchRoutesExcludeWins(t *testing.T) {
	r := &manifest.Routes{
		Version: 1,
		Include: []string{"/app/*"},
		Exclude: []string{"/app/public/*"},
	}
	if MatchRoutes(r, "/app/public/x") {
		t.Error("exclude should win over include")
	}
	if !MatchRoutes(r, "/app/private/x") {
		t.Error("included path should match")
	}
}

func TestMatchRoutesNotIncluded(t *testing.T) {
	r := &manifest.Routes{Version: 1, Include: []string{"/api/*"}}
	if MatchRoutes(r, "/home") {
		t.Error("path outside include must not match")
	}
}

func TestMatchRoutesNil(t *testing.T) {
	// nil means no _routes.json: worker handles everything.
	if !MatchRoutes(nil, "/anything") {
		t.Error("nil routes must match all paths")
	}
}
