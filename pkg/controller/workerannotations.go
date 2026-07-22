// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// LoadWorkerAnnotationsFile reads the cluster-wide annotations that the
// operator wants stamped on every worker pod, from a YAML file holding a flat
// string-to-string map:
//
//	prometheus.io/scrape: "true"
//	prometheus.io/port: "9090"
//	example.com/cost-center: platform
//
// The file is read once, at startup: it arrives as a mounted ConfigMap, and
// changing that ConfigMap restarts the controller, so there is nothing for a
// watch to catch that a restart does not already deliver.
//
// Every failure mode is fatal to the caller by design. An unreadable path, YAML
// that is not a string map, or a key Kubernetes would reject all mean the
// operator asked for annotations that will not appear on any pod; reporting
// that at startup beats leaving them to wonder, days later, why their scrape
// config matches nothing. An empty file and an empty map are legitimate and
// return a nil map.
func LoadWorkerAnnotationsFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("controller: read worker annotations file %q: %w", path, err)
	}
	return parseWorkerAnnotations(path, data)
}

// parseWorkerAnnotations decodes and validates the file contents.
func parseWorkerAnnotations(path string, data []byte) (map[string]string, error) {
	// Decoded through any rather than straight into map[string]string so that a
	// non-string value can be blamed on the key that carries it. `scrape: true`
	// (unquoted, hence a YAML bool) is the mistake operators actually make, and
	// "cannot unmarshal bool into Go value of type string" does not say where.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("controller: parse worker annotations file %q: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(raw))
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		v := raw[k]
		s, ok := v.(string)
		if !ok {
			if v == nil {
				return nil, fmt.Errorf("controller: worker annotation %q in %q has no value; "+
					"write an empty annotation as %s: \"\"", k, path, k)
			}
			return nil, fmt.Errorf("controller: worker annotation %q in %q must be a string, got %T; "+
				"quote the value", k, path, v)
		}
		out[k] = s
	}
	if err := validateWorkerAnnotations(out); err != nil {
		return nil, fmt.Errorf("%w (in %q)", err, path)
	}
	return out, nil
}

// validateWorkerAnnotations checks operator-supplied annotations against the
// Kubernetes annotation key rules and the reserved system prefixes.
//
// The key rules (optional DNS-subdomain prefix, then a <=63 character name
// starting and ending alphanumeric) come from validation.IsQualifiedName rather
// than a regexp of our own, so they track whatever the API server actually
// enforces; a key rejected here would otherwise be rejected by the API server on
// every single pod creation, turning a config typo into a tenant that never
// scales up. Keys are checked in sorted order so a file with several mistakes
// reports the same one on every restart.
//
// Annotation values are deliberately unchecked: Kubernetes puts no character
// restrictions on them.
func validateWorkerAnnotations(m map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		// Case is insignificant in an annotation key, so compare lowercased, as
		// apimachinery's own ValidateAnnotations does.
		if msgs := validation.IsQualifiedName(strings.ToLower(k)); len(msgs) > 0 {
			return fmt.Errorf("controller: invalid worker annotation key %q: %s", k, strings.Join(msgs, "; "))
		}
	}
	// The same reservation tenants get: durupages.io/* and app.kubernetes.io/*
	// are how reconcile identifies and adopts pods, and an operator overwriting
	// them cluster-wide would break every worker at once rather than one tenant.
	return validateNoSystemKeys(m)
}
