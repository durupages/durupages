// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package pagesspec

import (
	"fmt"
	"net/textproto"
	"strings"

	"github.com/durupages/durupages/pkg/manifest"
)

// Header limits, validated at upload time (see ARCHITECTURE.md 3.5).
const (
	maxHeaderRules = 100
	maxHeaderLine  = 2000 // characters per line
)

// ParseHeaders parses and validates a _headers file. An unindented line is a
// URL pattern; the indented lines that follow belong to it. An indented
// "Header-Name: value" line sets a header; an indented "! Header-Name" line
// removes it (recorded in Unset). Lines whose first non-whitespace character is
// '#' are comments; blank lines are ignored. Patterns use the same splat and
// placeholder language as _redirects, and may be absolute (https://...) to also
// match the request host. Header values may reference :splat/:name. When a rule
// sets the same header more than once the values are joined with ", ". Limits:
// 100 rules, 2000 characters/line.
func ParseHeaders(data []byte) ([]manifest.HeaderRule, error) {
	var rules []manifest.HeaderRule
	current := -1 // index into rules of the rule currently being filled

	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if len(line) > maxHeaderLine {
			return nil, fmt.Errorf("_headers: line %d exceeds %d characters", lineNo+1, maxHeaderLine)
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		indented := line[0] == ' ' || line[0] == '\t'
		if !indented {
			// New URL pattern line.
			if _, err := compilePattern(trimmed); err != nil {
				return nil, fmt.Errorf("_headers: line %d: %w", lineNo+1, err)
			}
			rules = append(rules, manifest.HeaderRule{Pattern: trimmed})
			current = len(rules) - 1
			if len(rules) > maxHeaderRules {
				return nil, fmt.Errorf("_headers: more than %d rules", maxHeaderRules)
			}
			continue
		}

		// Indented header line: belongs to the current pattern.
		if current < 0 {
			return nil, fmt.Errorf("_headers: line %d: header %q has no preceding URL pattern", lineNo+1, trimmed)
		}
		rule := &rules[current]

		if strings.HasPrefix(trimmed, "!") {
			name := strings.TrimSpace(trimmed[1:])
			if name == "" {
				return nil, fmt.Errorf("_headers: line %d: '!' requires a header name", lineNo+1)
			}
			rule.Unset = append(rule.Unset, textproto.CanonicalMIMEHeaderKey(name))
			continue
		}

		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			return nil, fmt.Errorf("_headers: line %d: expected 'Header-Name: value'", lineNo+1)
		}
		name := strings.TrimSpace(trimmed[:colon])
		value := strings.TrimSpace(trimmed[colon+1:])
		if name == "" {
			return nil, fmt.Errorf("_headers: line %d: empty header name", lineNo+1)
		}
		key := textproto.CanonicalMIMEHeaderKey(name)
		if rule.Set == nil {
			rule.Set = make(map[string]string)
		}
		if existing, ok := rule.Set[key]; ok {
			rule.Set[key] = existing + ", " + value
		} else {
			rule.Set[key] = value
		}
	}
	return rules, nil
}

// EvalHeaders accumulates headers from every rule whose pattern matches. Values
// from multiple matching rules are returned as an ordered list per canonical
// header name (in rule/file order) for the caller to join; header values have
// :splat/:name substituted from the pattern match. Relative patterns match path
// only; absolute (https://) patterns match host+path. unset lists the headers
// removed via "! Header-Name" (canonical, de-duplicated, in order).
func EvalHeaders(rules []manifest.HeaderRule, host, path string) (set map[string][]string, unset []string) {
	set = make(map[string][]string)
	seenUnset := make(map[string]bool)

	for _, rule := range rules {
		p, err := compilePattern(rule.Pattern)
		if err != nil {
			continue // already validated at parse time; skip defensively
		}
		subject := path
		if p.host {
			subject = host + path
		}
		caps, ok := p.match(subject)
		if !ok {
			continue
		}
		for name, value := range rule.Set {
			set[name] = append(set[name], substitute(value, caps))
		}
		for _, name := range rule.Unset {
			if !seenUnset[name] {
				seenUnset[name] = true
				unset = append(unset, name)
			}
		}
	}
	if len(set) == 0 {
		set = nil
	}
	return set, unset
}
