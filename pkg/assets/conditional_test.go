// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package assets

import (
	"testing"

	"github.com/durupages/durupages/pkg/manifest"
)

func TestETag(t *testing.T) {
	got := ETag(manifest.StaticEntry{Hash: "deadbeef"})
	if want := `"deadbeef"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
}

func TestNotModified(t *testing.T) {
	entry := manifest.StaticEntry{Hash: "abc123"}
	tests := []struct {
		name        string
		ifNoneMatch string
		want        bool
	}{
		{"empty header", "", false},
		{"whitespace only", "   ", false},
		{"wildcard", "*", true},
		{"exact strong match", `"abc123"`, true},
		{"quoted-but-different", `"xyz"`, false},
		{"weak validator match", `W/"abc123"`, true},
		{"weak validator no match", `W/"nope"`, false},
		{"match within list", `"one", "abc123", "two"`, true},
		{"weak match within list", `W/"one", W/"abc123"`, true},
		{"no match in list", `"one", "two"`, false},
		{"padded whitespace", `   "abc123"   `, true},
		{"hash without quotes does not match", "abc123", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NotModified(tt.ifNoneMatch, entry); got != tt.want {
				t.Errorf("NotModified(%q) = %v, want %v", tt.ifNoneMatch, got, tt.want)
			}
		})
	}
}
