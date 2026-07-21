// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package pagesspec

import "testing"

func TestCompilePatternAndMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		subject string
		want    bool
		caps    map[string]string
	}{
		{"exact match", "/about", "/about", true, map[string]string{}},
		{"exact no match", "/about", "/about/", false, nil},
		{"splat matches across slashes", "/blog/*", "/blog/2024/post", true, map[string]string{"splat": "2024/post"}},
		{"splat matches empty", "/blog/*", "/blog/", true, map[string]string{"splat": ""}},
		{"splat requires prefix slash", "/blog/*", "/blog", false, nil},
		{"placeholder matches segment", "/user/:id", "/user/42", true, map[string]string{"id": "42"}},
		{"placeholder rejects slash", "/user/:id", "/user/42/x", false, nil},
		{"placeholder rejects dot", "/user/:id", "/user/42.json", false, nil},
		{"placeholder requires one char", "/user/:id", "/user/", false, nil},
		{"placeholder plus splat", "/u/:id/*", "/u/7/a/b", true, map[string]string{"id": "7", "splat": "a/b"}},
		{"absolute host placeholder", "https://:project.pages.dev/*", "acme.pages.dev/style.css", true, map[string]string{"project": "acme", "splat": "style.css"}},
		{"absolute host literal", "https://example.com/*", "example.com/foo", true, map[string]string{"splat": "foo"}},
		{"absolute host mismatch", "https://example.com/*", "other.com/foo", false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := compilePattern(tc.pattern)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			caps, ok := p.match(tc.subject)
			if ok != tc.want {
				t.Fatalf("match(%q) = %v, want %v", tc.subject, ok, tc.want)
			}
			if !ok {
				return
			}
			for k, v := range tc.caps {
				if caps[k] != v {
					t.Errorf("cap %q = %q, want %q", k, caps[k], v)
				}
			}
			if len(caps) != len(tc.caps) {
				t.Errorf("caps = %v, want keys %v", caps, tc.caps)
			}
		})
	}
}

func TestCompilePatternMultipleSplats(t *testing.T) {
	if _, err := compilePattern("/a/*/b/*"); err == nil {
		t.Fatal("expected error for two splats")
	}
}

func TestCompilePatternHostDetection(t *testing.T) {
	for _, tc := range []struct {
		pat  string
		host bool
	}{
		{"/foo/*", false},
		{"https://a.com/*", true},
		{"http://a.com/*", true},
	} {
		p, err := compilePattern(tc.pat)
		if err != nil {
			t.Fatalf("compile %q: %v", tc.pat, err)
		}
		if p.host != tc.host {
			t.Errorf("%q host = %v, want %v", tc.pat, p.host, tc.host)
		}
	}
}

func TestSubstitute(t *testing.T) {
	caps := map[string]string{"splat": "a/b", "id": "42"}
	tests := []struct {
		tmpl string
		want string
	}{
		{"/new/:splat", "/new/a/b"},
		{"/user/:id/profile", "/user/42/profile"},
		{"/x/:id/:splat", "/x/42/a/b"},
		{"/literal", "/literal"},
		{"/unknown/:missing", "/unknown/:missing"},
		{"no-colon", "no-colon"},
	}
	for _, tc := range tests {
		if got := substitute(tc.tmpl, caps); got != tc.want {
			t.Errorf("substitute(%q) = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		glob string
		s    string
		want bool
	}{
		{"/*", "/anything/here", true},
		{"/*", "/", true},
		{"/users/*", "/users/", true},
		{"/users/*", "/users/a/b", true},
		{"/users/*", "/other", false},
		{"/", "/", true},
		{"/", "/x", false},
		{"/exact", "/exact", true},
		{"/build/*", "/build/app.js", true},
	}
	for _, tc := range tests {
		if got := matchGlob(tc.glob, tc.s); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.glob, tc.s, got, tc.want)
		}
	}
}
