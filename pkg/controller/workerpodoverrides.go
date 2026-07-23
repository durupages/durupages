// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// WorkerPodOverrides is cluster-wide worker pod configuration applied on top
// of what buildPodSpec derives per tenant/request (see kubepods.go's Create).
//
// The field set is a deliberate allowlist, not a raw corev1.PodSpec: it covers
// placement and scheduling (where a pod may run, how it tolerates node
// conditions, its priority and DNS) plus metadata, and nothing that would let
// an operator's YAML override what makes a worker pod what it is -- the
// container, its image and env, the bundles volume, the non-root security
// context, the service account, RestartPolicyNever. Those stay exclusively
// buildPodSpec's and kubepods.Create's to set, so every worker pod remains
// fungible and isolated regardless of what this carries.
//
// Field names and YAML shapes match the corresponding corev1.PodSpec fields
// exactly (same types, same json tags), so anything valid in a normal
// Kubernetes pod spec under these keys is valid here.
type WorkerPodOverrides struct {
	// Annotations / Labels are stamped on every worker pod. On a key collision
	// with a tenant's own PodAnnotations/PodLabels, these win: they are a
	// platform policy -- scrape targets, mesh injection, chargeback -- declared
	// once for the whole cluster, and a policy a tenant can opt out of by
	// naming the same key is not a policy. durupages.io/* and
	// app.kubernetes.io/* remain reserved regardless.
	//
	// A value written unquoted (`scrape: true`, `port: 9090`) is coerced to its
	// string form rather than rejected: sigs.k8s.io/yaml does this for any
	// map[string]string target, and it is what an operator who left it
	// unquoted meant -- annotation and label values are always strings by API
	// contract, so there is no type this could ambiguously mean instead.
	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`

	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	Tolerations               []corev1.Toleration               `json:"tolerations,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	DNSPolicy                 corev1.DNSPolicy                  `json:"dnsPolicy,omitempty"`
	DNSConfig                 *corev1.PodDNSConfig              `json:"dnsConfig,omitempty"`
	PriorityClassName         string                            `json:"priorityClassName,omitempty"`
	RuntimeClassName          *string                           `json:"runtimeClassName,omitempty"`
}

// LoadWorkerPodOverridesFile reads the cluster-wide worker pod configuration
// the operator wants applied to every worker pod, from a YAML file shaped like
// WorkerPodOverrides:
//
//	nodeSelector:
//	  workload: durupages-worker
//	tolerations:
//	  - key: durupages.io/dedicated
//	    operator: Equal
//	    value: "true"
//	    effect: NoSchedule
//	dnsConfig:
//	  nameservers: ["10.0.0.10"]
//	annotations:
//	  prometheus.io/scrape: "true"
//
// The file is read once, at startup: it arrives as a mounted ConfigMap, and
// changing that ConfigMap restarts the controller (its checksum is on the
// controller's own pod template), so there is nothing for a watch to catch
// that a restart does not already deliver.
//
// Every failure mode is fatal to the caller by design: an unreadable path, YAML
// that does not match the shape above, or a value Kubernetes would reject all
// mean the operator's configuration will not appear on any pod, and reporting
// that at startup beats leaving them to wonder, days later, why a node
// selector no worker pod carries didn't do anything. An empty file is
// legitimate and returns a zero WorkerPodOverrides.
func LoadWorkerPodOverridesFile(path string) (WorkerPodOverrides, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkerPodOverrides{}, fmt.Errorf("controller: read worker pod overrides file %q: %w", path, err)
	}
	return parseWorkerPodOverrides(path, data)
}

// parseWorkerPodOverrides decodes and validates the file contents.
func parseWorkerPodOverrides(path string, data []byte) (WorkerPodOverrides, error) {
	// Strict: an unknown key (a typo, or an attempt to set a field outside the
	// allowlist such as `containers:`) is exactly the kind of mistake that
	// should fail loudly here rather than be silently dropped and rediscovered
	// as "why doesn't this do anything" much later.
	var out WorkerPodOverrides
	if err := yaml.UnmarshalStrict(data, &out); err != nil {
		return WorkerPodOverrides{}, fmt.Errorf("controller: parse worker pod overrides file %q: %w", path, err)
	}
	if err := validateWorkerPodOverrides(out); err != nil {
		return WorkerPodOverrides{}, fmt.Errorf("%w (in %q)", err, path)
	}
	return out, nil
}

// validateWorkerPodOverrides checks the metadata fields against the
// Kubernetes key/value rules and the reserved system prefixes.
//
// Tolerations, Affinity, DNSConfig and the rest are left to the API server:
// duplicating its full validation here would drift, and an invalid value
// there fails a single pod Create (logged -- see maybeScaleUp) rather than
// startup, which is an acceptable difference for fields that do not risk
// silently applying to nothing the way a bad annotation key would.
func validateWorkerPodOverrides(o WorkerPodOverrides) error {
	if err := validateMetadataMap("annotation", o.Annotations, false); err != nil {
		return err
	}
	if err := validateMetadataMap("label", o.Labels, true); err != nil {
		return err
	}
	if err := validateMetadataMap("nodeSelector", o.NodeSelector, true); err != nil {
		return err
	}
	if err := validateNoSystemKeys(o.Annotations); err != nil {
		return err
	}
	return validateNoSystemKeys(o.Labels)
}

// validateMetadataMap checks keys against the Kubernetes qualified-name rule
// and, when checkValues is true (labels and node selectors, unlike
// annotations, constrain their values), values against the label-value rule.
func validateMetadataMap(kind string, m map[string]string, checkValues bool) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		// Case is insignificant in a key, so compare lowercased, as
		// apimachinery's own validation does.
		if msgs := validation.IsQualifiedName(strings.ToLower(k)); len(msgs) > 0 {
			return fmt.Errorf("controller: invalid worker pod %s key %q: %s", kind, k, strings.Join(msgs, "; "))
		}
		if checkValues {
			if msgs := validation.IsValidLabelValue(m[k]); len(msgs) > 0 {
				return fmt.Errorf("controller: invalid worker pod %s value %q for key %q: %s",
					kind, m[k], k, strings.Join(msgs, "; "))
			}
		}
	}
	return nil
}
