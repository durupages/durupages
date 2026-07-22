// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAnnotationsFile writes body to a temp file and returns its path.
func writeAnnotationsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "annotations.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write annotations file: %v", err)
	}
	return path
}

func TestLoadWorkerAnnotationsFile(t *testing.T) {
	path := writeAnnotationsFile(t, `
prometheus.io/scrape: "true"
prometheus.io/port: "9090"
example.com/cost-center: platform
sidecar.istio.io/inject: "false"
`)
	got, err := LoadWorkerAnnotationsFile(path)
	if err != nil {
		t.Fatalf("LoadWorkerAnnotationsFile: %v", err)
	}
	want := map[string]string{
		"prometheus.io/scrape":    "true",
		"prometheus.io/port":      "9090",
		"example.com/cost-center": "platform",
		"sidecar.istio.io/inject": "false",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d annotations, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("annotation %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestLoadWorkerAnnotationsFileEmpty covers the two shapes of "nothing
// configured": a ConfigMap key with no content, and an explicitly empty map.
func TestLoadWorkerAnnotationsFileEmpty(t *testing.T) {
	for _, body := range []string{"", "\n", "# only a comment\n", "{}\n"} {
		got, err := LoadWorkerAnnotationsFile(writeAnnotationsFile(t, body))
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if len(got) != 0 {
			t.Fatalf("body %q: got %v, want no annotations", body, got)
		}
	}
}

// TestLoadWorkerAnnotationsFileMissing checks that a configured path that
// cannot be read fails startup rather than silently disabling the feature.
func TestLoadWorkerAnnotationsFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := LoadWorkerAnnotationsFile(path)
	if err == nil {
		t.Fatal("missing file accepted, want an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the file: %v", err)
	}
}

func TestLoadWorkerAnnotationsFileRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring the message must carry
	}{
		{
			name: "malformed YAML",
			body: "prometheus.io/scrape: \"true\n  bad: [",
			want: "parse worker annotations file",
		},
		{
			name: "not a map",
			body: "- prometheus.io/scrape\n- prometheus.io/port\n",
			want: "parse worker annotations file",
		},
		{
			name: "unquoted boolean value",
			body: "prometheus.io/scrape: true\n",
			want: `"prometheus.io/scrape"`,
		},
		{
			name: "unquoted numeric value",
			body: "prometheus.io/port: 9090\n",
			want: "must be a string",
		},
		{
			name: "nested map value",
			body: "example.com/policy:\n  tier: gold\n",
			want: "must be a string",
		},
		{
			name: "key with no value",
			body: "example.com/note:\n",
			want: "has no value",
		},
		{
			name: "invalid key: empty name",
			body: "\"example.com/\": x\n",
			want: "invalid worker annotation key",
		},
		{
			name: "invalid key: illegal character",
			body: "\"bad key!\": x\n",
			want: "invalid worker annotation key",
		},
		{
			name: "invalid key: two slashes",
			body: "\"a/b/c\": x\n",
			want: "invalid worker annotation key",
		},
		{
			name: "invalid key: name over 63 characters",
			body: "\"" + strings.Repeat("a", 64) + "\": x\n",
			want: "invalid worker annotation key",
		},
		{
			name: "system key: durupages.io",
			body: "durupages.io/tenant-id: evil\n",
			want: "reserved system prefix",
		},
		{
			name: "system key: app.kubernetes.io",
			body: "app.kubernetes.io/name: spoof\n",
			want: "reserved system prefix",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeAnnotationsFile(t, tc.body)
			got, err := LoadWorkerAnnotationsFile(path)
			if err == nil {
				t.Fatalf("accepted %q, want an error (got %v)", tc.body, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

// TestLoadWorkerAnnotationsFileAcceptsBareName checks that a prefix-less key --
// the other half of the Kubernetes rule -- is allowed.
func TestLoadWorkerAnnotationsFileAcceptsBareName(t *testing.T) {
	got, err := LoadWorkerAnnotationsFile(writeAnnotationsFile(t, "kubectl.kubernetes-io_note: hi\n"))
	if err != nil {
		t.Fatalf("bare name rejected: %v", err)
	}
	if got["kubectl.kubernetes-io_note"] != "hi" {
		t.Fatalf("got %v", got)
	}
}

// TestValidateWorkerAnnotationsNil checks the disabled case: no file, no map,
// no error.
func TestValidateWorkerAnnotationsNil(t *testing.T) {
	if err := validateWorkerAnnotations(nil); err != nil {
		t.Fatalf("nil annotations rejected: %v", err)
	}
}
