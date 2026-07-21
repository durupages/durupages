// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package workerdruntime

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/runtime"
)

var update = flag.Bool("update", false, "update golden files")

// writeBundle lays out a minimal worker bundle under dir and returns dir.
func writeBundle(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// goldenSpec builds a deterministic two-page spec for golden testing.
func goldenSpec(t *testing.T) runtime.InstanceSpec {
	t.Helper()
	blogDir := writeBundle(t, filepath.Join(t.TempDir(), "blog"), map[string]string{
		"worker/index.js":    "export default { async fetch() { return new Response('blog'); } }\n",
		"worker/lib/util.js": "export const x = 1;\n",
	})
	shopDir := writeBundle(t, filepath.Join(t.TempDir(), "shop"), map[string]string{
		"worker/index.js": "export default { async fetch() { return new Response('shop'); } }\n",
	})
	return runtime.InstanceSpec{
		AssetsEndpoint: "127.0.0.1:8081",
		TailEndpoint:   "127.0.0.1:8082",
		Pages: []runtime.PageWorker{
			{
				PageID:       "blog",
				DeploymentID: "dep_blog_1",
				BundleDir:    blogDir,
				Manifest: &manifest.Manifest{
					Version: 1,
					Compat:  manifest.Compat{Date: "2024-11-01", Flags: []string{"nodejs_compat", "streams_enable_constructors"}},
				},
				Env:    map[string]string{"API_URL": "https://api.example.com", "REGION": "eu"},
				Secret: map[string]string{"TOKEN": "s3cr3t-value"},
			},
			{
				PageID:       "shop",
				DeploymentID: "dep_shop_1",
				BundleDir:    shopDir,
				// No manifest -> default compat date, no flags.
				Env: map[string]string{"STORE": "main"},
			},
		},
	}
}

func TestGenerateConfigGolden(t *testing.T) {
	spec := goldenSpec(t)
	gen, err := generateConfig(spec, 8788)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "config.capnp.golden", gen.Capnp)
	checkGolden(t, "entry.js.golden", gen.EntryJS)
	checkGolden(t, "tail.js.golden", gen.TailJS)
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create)", name, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestGenerateConfigDeterministic verifies two renders of the same spec are
// byte-identical (binding/route ordering is stable).
func TestGenerateConfigDeterministic(t *testing.T) {
	spec := goldenSpec(t)
	a, err := generateConfig(spec, 9000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateConfig(spec, 9000)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Capnp) != string(b.Capnp) {
		t.Error("config not deterministic")
	}
}

func TestCapnpString(t *testing.T) {
	cases := map[string]string{
		`plain`:            `"plain"`,
		"quote\"and\\back": `"quote\"and\\back"`,
		"line\nbreak":      `"line\nbreak"`,
		"tab\there":        `"tab\there"`,
		"ctl\x01":          `"ctl\x01"`,
	}
	for in, want := range cases {
		if got := capnpString(in); got != want {
			t.Errorf("capnpString(%q) = %s, want %s", in, got, want)
		}
	}
}
