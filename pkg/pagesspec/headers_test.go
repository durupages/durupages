// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package pagesspec

import (
	"strings"
	"testing"
)

func TestParseHeadersBasic(t *testing.T) {
	in := `# global comment
/*
  X-Frame-Options: DENY
  X-Content-Type-Options: nosniff

/secure/*
  ! Access-Control-Allow-Origin
  Cache-Control: no-store
`
	rules, err := ParseHeaders([]byte(in))
	if err != nil {
		t.Fatalf("ParseHeaders: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	if rules[0].Set["X-Frame-Options"] != "DENY" {
		t.Errorf("X-Frame-Options = %q", rules[0].Set["X-Frame-Options"])
	}
	if rules[0].Set["X-Content-Type-Options"] != "nosniff" {
		t.Errorf("nosniff missing: %v", rules[0].Set)
	}
	if len(rules[1].Unset) != 1 || rules[1].Unset[0] != "Access-Control-Allow-Origin" {
		t.Errorf("unset = %v", rules[1].Unset)
	}
}

func TestParseHeadersRepeatedNameJoined(t *testing.T) {
	in := "/*\n  Set-Cookie: a=1\n  Set-Cookie: b=2\n"
	rules, err := ParseHeaders([]byte(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := rules[0].Set["Set-Cookie"]; got != "a=1, b=2" {
		t.Errorf("joined = %q, want %q", got, "a=1, b=2")
	}
}

func TestParseHeadersErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"header before pattern", "  X-Foo: bar\n"},
		{"missing colon", "/*\n  BrokenHeader\n"},
		{"empty unset", "/*\n  !\n"},
		{"empty name", "/*\n  : value\n"},
		{"line too long", "/*\n  X-Foo: " + strings.Repeat("v", 2000) + "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseHeaders([]byte(tc.in)); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestParseHeadersTooManyRules(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 101; i++ {
		b.WriteString("/p\n  X-A: 1\n")
	}
	if _, err := ParseHeaders([]byte(b.String())); err == nil {
		t.Fatal("expected error for >100 rules")
	}
}

func TestEvalHeadersAccumulate(t *testing.T) {
	rules, err := ParseHeaders([]byte(`/*
  X-A: base
/blog/*
  X-A: blog
  X-B: only
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	set, unset := EvalHeaders(rules, "example.com", "/blog/post")
	if len(unset) != 0 {
		t.Errorf("unset = %v, want none", unset)
	}
	// Both rules match: X-A accumulates in file order.
	if got := set["X-A"]; len(got) != 2 || got[0] != "base" || got[1] != "blog" {
		t.Errorf("X-A = %v, want [base blog]", got)
	}
	if got := set["X-B"]; len(got) != 1 || got[0] != "only" {
		t.Errorf("X-B = %v", got)
	}
}

func TestEvalHeadersUnset(t *testing.T) {
	rules, err := ParseHeaders([]byte("/*\n  ! X-Powered-By\n  ! X-Powered-By\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, unset := EvalHeaders(rules, "h", "/x")
	if len(unset) != 1 || unset[0] != "X-Powered-By" {
		t.Errorf("unset = %v, want [X-Powered-By] deduped", unset)
	}
}

func TestEvalHeadersSubstitution(t *testing.T) {
	rules, err := ParseHeaders([]byte("/files/:name/*\n  X-Name: :name\n  X-Path: :splat\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	set, _ := EvalHeaders(rules, "h", "/files/report/2024/q1.pdf")
	if got := set["X-Name"]; len(got) != 1 || got[0] != "report" {
		t.Errorf("X-Name = %v", got)
	}
	if got := set["X-Path"]; len(got) != 1 || got[0] != "2024/q1.pdf" {
		t.Errorf("X-Path = %v", got)
	}
}

func TestEvalHeadersHostPattern(t *testing.T) {
	rules, err := ParseHeaders([]byte(`https://:project.pages.dev/*
  X-Project: :project
/*
  X-Always: yes
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	set, _ := EvalHeaders(rules, "acme.pages.dev", "/index.html")
	if got := set["X-Project"]; len(got) != 1 || got[0] != "acme" {
		t.Errorf("X-Project = %v, want [acme]", got)
	}
	if got := set["X-Always"]; len(got) != 1 {
		t.Errorf("X-Always = %v", got)
	}

	// Different host: absolute rule must not match, relative one still does.
	set2, _ := EvalHeaders(rules, "other.com", "/index.html")
	if _, ok := set2["X-Project"]; ok {
		t.Error("X-Project should not be set for non-matching host")
	}
	if got := set2["X-Always"]; len(got) != 1 {
		t.Errorf("X-Always = %v", got)
	}
}

func TestEvalHeadersNoMatch(t *testing.T) {
	rules, _ := ParseHeaders([]byte("/admin/*\n  X-A: 1\n"))
	set, unset := EvalHeaders(rules, "h", "/public")
	if set != nil || unset != nil {
		t.Errorf("expected no headers, got set=%v unset=%v", set, unset)
	}
}
