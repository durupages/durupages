// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package pagesspec

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/durupages/durupages/pkg/manifest"
)

// Route limits, validated at upload time (see ARCHITECTURE.md 3.2).
const (
	maxRouteRules  = 100 // include + exclude combined
	maxRuleLength  = 100 // per rule, in characters
	routesVersion  = 1
	minIncludeRule = 1
)

// ParseRoutes parses and validates a _routes.json document. It enforces
// version == 1, at least one include rule, at most 100 include+exclude rules
// combined, each rule at most 100 characters and starting with "/".
func ParseRoutes(data []byte) (*manifest.Routes, error) {
	var r manifest.Routes
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("_routes.json: decode: %w", err)
	}

	if r.Version != routesVersion {
		return nil, fmt.Errorf("_routes.json: unsupported version %d (want %d)", r.Version, routesVersion)
	}
	if len(r.Include) < minIncludeRule {
		return nil, fmt.Errorf("_routes.json: include must have at least %d rule", minIncludeRule)
	}
	if total := len(r.Include) + len(r.Exclude); total > maxRouteRules {
		return nil, fmt.Errorf("_routes.json: %d rules exceed the maximum of %d", total, maxRouteRules)
	}
	for _, group := range [...]struct {
		name  string
		rules []string
	}{{"include", r.Include}, {"exclude", r.Exclude}} {
		for _, rule := range group.rules {
			if len(rule) > maxRuleLength {
				return nil, fmt.Errorf("_routes.json: %s rule %q exceeds %d characters", group.name, rule, maxRuleLength)
			}
			if len(rule) == 0 || rule[0] != '/' {
				return nil, fmt.Errorf("_routes.json: %s rule %q must start with '/'", group.name, rule)
			}
		}
	}
	return &r, nil
}

// MatchRoutes reports whether the worker should be invoked for path. Exclude
// wins over include. A nil r means "no _routes.json"; callers only invoke this
// when a worker exists, so a missing file is treated as include ["/*"] and
// every path matches (Cloudflare-compatible).
func MatchRoutes(r *manifest.Routes, path string) bool {
	if r == nil {
		return true
	}
	included := false
	for _, rule := range r.Include {
		if matchGlob(rule, path) {
			included = true
			break
		}
	}
	if !included {
		return false
	}
	for _, rule := range r.Exclude {
		if matchGlob(rule, path) {
			return false // exclude wins over include
		}
	}
	return true
}
