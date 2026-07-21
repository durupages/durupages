// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package postgres

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"Blog.Pages.Test":      "blog.pages.test",
		"blog.pages.test:8443": "blog.pages.test",
		"  WWW.Example.COM  ":  "www.example.com",
		"host:80":              "host",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSubdomainLabel(t *testing.T) {
	type tc struct {
		host, domain string
		wantLabel    string
		wantOK       bool
	}
	cases := []tc{
		{"blog.pages.test", "pages.test", "blog", true},
		{"a.blog.pages.test", "pages.test", "", false}, // multi-label
		{"pages.test", "pages.test", "", false},        // no label
		{"other.example.com", "pages.test", "", false},
		{"blog.pages.test", "", "", false}, // empty domain
	}
	for _, c := range cases {
		label, ok := subdomainLabel(c.host, c.domain)
		if label != c.wantLabel || ok != c.wantOK {
			t.Errorf("subdomainLabel(%q,%q) = (%q,%v), want (%q,%v)",
				c.host, c.domain, label, ok, c.wantLabel, c.wantOK)
		}
	}
}

func TestPageConfigJSONRoundtrip(t *testing.T) {
	enabled := true
	cfg := provider.PageConfig{
		QueueTimeout:   30 * time.Second,
		RequestTimeout: time.Hour,
		Env:            map[string]string{"API": "https://api", "N": "1"},
		Secret:         map[string]string{"TOKEN": "abc"},
		LogsEnabled:    &enabled,
	}
	b, err := encodePageConfig(cfg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Durations must be stored as Go duration strings, not nanoseconds.
	if s := string(b); !strings.Contains(s, `"30s"`) || !strings.Contains(s, `"1h0m0s"`) {
		t.Fatalf("durations not stored as strings: %s", s)
	}
	got, err := decodePageConfig(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
}

func TestPageConfigLogsEnabledNil(t *testing.T) {
	cfg := provider.PageConfig{RequestTimeout: time.Minute}
	b, err := encodePageConfig(cfg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(b), "logsEnabled") {
		t.Fatalf("nil LogsEnabled should be omitted: %s", b)
	}
	got, err := decodePageConfig(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LogsEnabled != nil {
		t.Fatalf("LogsEnabled should stay nil, got %v", *got.LogsEnabled)
	}
}

func TestPageConfigLogsEnabledFalse(t *testing.T) {
	f := false
	cfg := provider.PageConfig{LogsEnabled: &f}
	b, err := encodePageConfig(cfg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodePageConfig(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LogsEnabled == nil || *got.LogsEnabled != false {
		t.Fatalf("LogsEnabled=false not preserved: %v", got.LogsEnabled)
	}
}

func TestTenantConfigJSONRoundtrip(t *testing.T) {
	cfg := provider.TenantConfig{
		MaxConcurrency: 4,
		IdleTTL:        15 * time.Minute,
		WorkerCPULimit: "1",
		WorkerMemLimit: "512Mi",
		PodLabels:      map[string]string{"team": "web"},
		PodAnnotations: map[string]string{"cost": "eng"},
	}
	b, err := encodeTenantConfig(cfg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(b), `"15m0s"`) {
		t.Fatalf("IdleTTL not stored as duration string: %s", b)
	}
	got, err := decodeTenantConfig(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
}

func TestDecodeEmptyConfig(t *testing.T) {
	pc, err := decodePageConfig(nil)
	if err != nil {
		t.Fatalf("decode nil page config: %v", err)
	}
	if !reflect.DeepEqual(pc, provider.PageConfig{}) {
		t.Fatalf("want zero PageConfig, got %+v", pc)
	}
	tc, err := decodeTenantConfig([]byte("{}"))
	if err != nil {
		t.Fatalf("decode empty tenant config: %v", err)
	}
	if !reflect.DeepEqual(tc, provider.TenantConfig{}) {
		t.Fatalf("want zero TenantConfig, got %+v", tc)
	}
}

func TestNullString(t *testing.T) {
	if nullString("") != nil {
		t.Fatal("empty string should map to nil")
	}
	if s := nullString("x"); s == nil || *s != "x" {
		t.Fatalf("non-empty string mishandled: %v", s)
	}
}
