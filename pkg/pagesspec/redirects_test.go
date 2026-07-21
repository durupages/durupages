// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package pagesspec

import (
	"strings"
	"testing"

	"github.com/durupages/durupages/pkg/manifest"
)

func TestParseRedirectsBasic(t *testing.T) {
	in := `
# comment line
/old            /new                  301
/home           /                     # inline comment
/blog/*         /posts/:splat         301
/team/:name     /people/:name
/legacy         /modern
/rewrite        /index.html           200
`
	rules, err := ParseRedirects([]byte(in))
	if err != nil {
		t.Fatalf("ParseRedirects: %v", err)
	}
	if len(rules) != 6 {
		t.Fatalf("got %d rules, want 6: %+v", len(rules), rules)
	}
	// default status 302 for /team/:name and /legacy
	if rules[3].Status != 302 {
		t.Errorf("default status = %d, want 302", rules[3].Status)
	}
	if rules[5].Status != 200 {
		t.Errorf("rewrite status = %d, want 200", rules[5].Status)
	}
}

func TestParseRedirectsErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"one field", "/only"},
		{"too many fields", "/a /b 301 extra"},
		{"bad status text", "/a /b foo"},
		{"unsupported status", "/a /b 404"},
		{"200 absolute dest", "/a https://x.com/ 200"},
		{"200 non-slash dest", "/a rel 200"},
		{"two splats", "/a/*/b/* /c 301"},
		{"line too long", "/a /" + strings.Repeat("x", 1000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRedirects([]byte(tc.in)); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestParseRedirectsLimits(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2001; i++ {
		b.WriteString("/a /b 301\n")
	}
	if _, err := ParseRedirects([]byte(b.String())); err == nil {
		t.Fatal("expected error for >2000 static rules")
	}

	b.Reset()
	for i := 0; i < 101; i++ {
		b.WriteString("/blog/* /posts/:splat 301\n")
	}
	if _, err := ParseRedirects([]byte(b.String())); err == nil {
		t.Fatal("expected error for >100 dynamic rules")
	}
}

func TestEvalRedirects(t *testing.T) {
	rules, err := ParseRedirects([]byte(`
/first          /a                    301
/first          /b                    302
/blog/*         /posts/:splat         301
/team/:name     /people/:name         302
/proxy/*        /assets/:splat        200
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		path      string
		wantOK    bool
		location  string
		status    int
		isRewrite bool
	}{
		{"/first", true, "/a", 301, false}, // first-match wins over /b
		{"/blog/2024/hello", true, "/posts/2024/hello", 301, false},
		{"/team/alice", true, "/people/alice", 302, false},
		{"/proxy/img/logo.png", true, "/assets/img/logo.png", 200, true},
		{"/nomatch", false, "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			res, ok := EvalRedirects(rules, tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if res.Location != tc.location || res.Status != tc.status || res.IsRewrite != tc.isRewrite {
				t.Errorf("got %+v, want loc=%q status=%d rewrite=%v", res, tc.location, tc.status, tc.isRewrite)
			}
		})
	}
}

func TestParseRedirectsCommentsAndBlanks(t *testing.T) {
	rules, err := ParseRedirects([]byte("\n\n# only comments\n   \n/a /b\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rules) != 1 || rules[0] != (manifest.Redirect{Source: "/a", Destination: "/b", Status: 302}) {
		t.Fatalf("unexpected: %+v", rules)
	}
}
