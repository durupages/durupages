// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// writeOverridesFile writes body to a temp file and returns its path.
func writeOverridesFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pod-overrides.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write overrides file: %v", err)
	}
	return path
}

func TestLoadWorkerPodOverridesFile(t *testing.T) {
	path := writeOverridesFile(t, `
nodeSelector:
  workload: durupages-worker
tolerations:
  - key: durupages.io/dedicated
    operator: Equal
    value: "true"
    effect: NoSchedule
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: kubernetes.io/arch
              operator: In
              values: ["amd64"]
dnsPolicy: None
dnsConfig:
  nameservers: ["10.0.0.10"]
priorityClassName: high-priority
runtimeClassName: gvisor
annotations:
  prometheus.io/scrape: "true"
labels:
  team: platform
`)
	got, err := LoadWorkerPodOverridesFile(path)
	if err != nil {
		t.Fatalf("LoadWorkerPodOverridesFile: %v", err)
	}
	if got.NodeSelector["workload"] != "durupages-worker" {
		t.Errorf("nodeSelector = %v", got.NodeSelector)
	}
	if len(got.Tolerations) != 1 || got.Tolerations[0].Key != "durupages.io/dedicated" ||
		got.Tolerations[0].Operator != corev1.TolerationOpEqual || got.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("tolerations = %+v", got.Tolerations)
	}
	if got.Affinity == nil || got.Affinity.NodeAffinity == nil {
		t.Errorf("affinity = %+v", got.Affinity)
	}
	if got.DNSPolicy != corev1.DNSNone {
		t.Errorf("dnsPolicy = %v", got.DNSPolicy)
	}
	if got.DNSConfig == nil || len(got.DNSConfig.Nameservers) != 1 || got.DNSConfig.Nameservers[0] != "10.0.0.10" {
		t.Errorf("dnsConfig = %+v", got.DNSConfig)
	}
	if got.PriorityClassName != "high-priority" {
		t.Errorf("priorityClassName = %q", got.PriorityClassName)
	}
	if got.RuntimeClassName == nil || *got.RuntimeClassName != "gvisor" {
		t.Errorf("runtimeClassName = %v", got.RuntimeClassName)
	}
	if got.Annotations["prometheus.io/scrape"] != "true" {
		t.Errorf("annotations = %v", got.Annotations)
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("labels = %v", got.Labels)
	}
}

// TestLoadWorkerPodOverridesFileEmpty covers the two shapes of "nothing
// configured": a ConfigMap key with no content, and an explicitly empty map.
func TestLoadWorkerPodOverridesFileEmpty(t *testing.T) {
	for _, body := range []string{"", "\n", "# only a comment\n", "{}\n"} {
		got, err := LoadWorkerPodOverridesFile(writeOverridesFile(t, body))
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if !reflect.DeepEqual(got, WorkerPodOverrides{}) {
			t.Fatalf("body %q: got %+v, want the zero value", body, got)
		}
	}
}

// TestLoadWorkerPodOverridesFileCoercesScalarValues documents a
// sigs.k8s.io/yaml behavior this loader relies on rather than works around:
// unmarshaling into a typed map[string]string coerces a YAML scalar (bool,
// number) to its string form instead of erroring. Kubernetes annotation and
// label values are always strings by API contract, so `scrape: true` giving
// the annotation value "true" is what an operator who left it unquoted meant,
// not a type mismatch to reject.
func TestLoadWorkerPodOverridesFileCoercesScalarValues(t *testing.T) {
	got, err := LoadWorkerPodOverridesFile(writeOverridesFile(t, `
annotations:
  prometheus.io/scrape: true
  prometheus.io/port: 9090
`))
	if err != nil {
		t.Fatalf("LoadWorkerPodOverridesFile: %v", err)
	}
	if got.Annotations["prometheus.io/scrape"] != "true" || got.Annotations["prometheus.io/port"] != "9090" {
		t.Fatalf("annotations = %v", got.Annotations)
	}
}

// TestLoadWorkerPodOverridesFileMissing checks that a configured path that
// cannot be read fails startup rather than silently disabling the feature.
func TestLoadWorkerPodOverridesFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := LoadWorkerPodOverridesFile(path)
	if err == nil {
		t.Fatal("missing file accepted, want an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the file: %v", err)
	}
}

func TestLoadWorkerPodOverridesFileRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring the message must carry
	}{
		{
			name: "malformed YAML",
			body: "nodeSelector: \"true\n  bad: [",
			want: "parse worker pod overrides file",
		},
		{
			name: "not a map",
			body: "- nodeSelector\n- tolerations\n",
			want: "parse worker pod overrides file",
		},
		{
			name: "unknown field: typo",
			body: "toleration:\n  - key: x\n",
			want: "parse worker pod overrides file",
		},
		{
			name: "unknown field: outside the allowlist",
			body: "containers:\n  - name: evil\n    image: attacker/payload\n",
			want: "parse worker pod overrides file",
		},
		{
			name: "invalid annotation key",
			body: "annotations:\n  \"bad key!\": x\n",
			want: "invalid worker pod annotation key",
		},
		{
			name: "invalid label value",
			body: "labels:\n  team: \"not a valid label value!!\"\n",
			want: "invalid worker pod label value",
		},
		{
			name: "invalid nodeSelector value",
			body: "nodeSelector:\n  workload: \"not a valid label value!!\"\n",
			want: "invalid worker pod nodeSelector value",
		},
		{
			name: "system annotation key: durupages.io",
			body: "annotations:\n  durupages.io/tenant-id: evil\n",
			want: "reserved system prefix",
		},
		{
			name: "system label key: app.kubernetes.io",
			body: "labels:\n  app.kubernetes.io/name: spoof\n",
			want: "reserved system prefix",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeOverridesFile(t, tc.body)
			got, err := LoadWorkerPodOverridesFile(path)
			if err == nil {
				t.Fatalf("accepted %q, want an error (got %+v)", tc.body, got)
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

// TestValidateWorkerPodOverridesZero checks the disabled case: no file, zero
// value, no error.
func TestValidateWorkerPodOverridesZero(t *testing.T) {
	if err := validateWorkerPodOverrides(WorkerPodOverrides{}); err != nil {
		t.Fatalf("zero-value overrides rejected: %v", err)
	}
}
