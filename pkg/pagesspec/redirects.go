// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package pagesspec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/durupages/durupages/pkg/manifest"
)

// Redirect limits, validated at upload time (see ARCHITECTURE.md 3.3).
const (
	maxStaticRedirects  = 2000
	maxDynamicRedirects = 100
	maxRedirectLine     = 1000 // characters per line
	defaultRedirectCode = 302
)

// validRedirectStatus lists the status codes accepted in _redirects. 200 is an
// internal rewrite (the destination asset is served without changing the URL).
var validRedirectStatus = map[int]bool{
	200: true, 301: true, 302: true, 303: true, 307: true, 308: true,
}

// RedirectResult is the outcome of a matched _redirects rule.
type RedirectResult struct {
	// Location is the destination with :splat/:name tokens substituted.
	Location string
	// Status is the HTTP status code (200 for a rewrite).
	Status int
	// IsRewrite is true when Status == 200 (serve destination asset in place).
	IsRewrite bool
}

// ParseRedirects parses and validates a _redirects file. Each non-empty,
// non-comment line has the form "source destination [status]" separated by
// whitespace; status defaults to 302. Lines beginning with '#' (after leading
// whitespace) are comments; text after '#' is ignored. Sources may contain at
// most one splat '*' (referenced as :splat) and any number of :name
// placeholders. A rule is dynamic when its source contains a splat or
// placeholder. Limits: 2000 static + 100 dynamic rules, 1000 characters/line.
func ParseRedirects(data []byte) ([]manifest.Redirect, error) {
	var rules []manifest.Redirect
	staticN, dynamicN := 0, 0

	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if len(line) > maxRedirectLine {
			return nil, fmt.Errorf("_redirects: line %d exceeds %d characters", lineNo+1, maxRedirectLine)
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("_redirects: line %d: expected 'source destination [status]'", lineNo+1)
		}
		if len(fields) > 3 {
			return nil, fmt.Errorf("_redirects: line %d: too many fields", lineNo+1)
		}
		source, destination := fields[0], fields[1]

		status := defaultRedirectCode
		if len(fields) == 3 {
			code, err := strconv.Atoi(fields[2])
			if err != nil {
				return nil, fmt.Errorf("_redirects: line %d: invalid status %q", lineNo+1, fields[2])
			}
			status = code
		}
		if !validRedirectStatus[status] {
			return nil, fmt.Errorf("_redirects: line %d: unsupported status %d", lineNo+1, status)
		}
		if status == 200 && (destination == "" || destination[0] != '/') {
			return nil, fmt.Errorf("_redirects: line %d: status 200 rewrite destination %q must be a relative path starting with '/'", lineNo+1, destination)
		}

		// Validate the source pattern (catches multiple splats).
		if _, err := compilePattern(source); err != nil {
			return nil, fmt.Errorf("_redirects: line %d: %w", lineNo+1, err)
		}

		if isDynamicSource(source) {
			dynamicN++
			if dynamicN > maxDynamicRedirects {
				return nil, fmt.Errorf("_redirects: more than %d dynamic rules", maxDynamicRedirects)
			}
		} else {
			staticN++
			if staticN > maxStaticRedirects {
				return nil, fmt.Errorf("_redirects: more than %d static rules", maxStaticRedirects)
			}
		}

		rules = append(rules, manifest.Redirect{
			Source:      source,
			Destination: destination,
			Status:      status,
		})
	}
	return rules, nil
}

// isDynamicSource reports whether a source contains a splat or placeholder.
func isDynamicSource(source string) bool {
	if strings.ContainsRune(source, '*') {
		return true
	}
	for i := 0; i+1 < len(source); i++ {
		if source[i] == ':' && isNameByte(source[i+1]) {
			return true
		}
	}
	return false
}

// EvalRedirects returns the first rule (in file order) whose source matches
// path, with :splat/:name substituted into the destination. The bool is false
// when no rule matches.
func EvalRedirects(rules []manifest.Redirect, path string) (RedirectResult, bool) {
	for _, rule := range rules {
		p, err := compilePattern(rule.Source)
		if err != nil {
			continue // already validated at parse time; skip defensively
		}
		caps, ok := p.match(path)
		if !ok {
			continue
		}
		return RedirectResult{
			Location:  substitute(rule.Destination, caps),
			Status:    rule.Status,
			IsRewrite: rule.Status == 200,
		}, true
	}
	return RedirectResult{}, false
}
