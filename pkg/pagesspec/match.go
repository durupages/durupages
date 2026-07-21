// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package pagesspec parses, validates and evaluates Cloudflare Pages special
// files: _routes.json (worker invocation routing), _redirects (redirect and
// rewrite rules) and _headers (custom response headers). Parsers run at upload
// time to reject malformed or oversized input; the matchers (MatchRoutes,
// EvalRedirects, EvalHeaders) run at request time inside router and worker shim
// and share the pattern language implemented here.
//
// Pattern language (used by _redirects and _headers): a pattern is a literal
// URL path with two kinds of dynamic tokens:
//
//   - splat "*": at most one per pattern, greedy, matches any characters
//     including "/"; referenced in the destination/value as :splat.
//   - placeholder ":name": matches one or more characters except "/" and "."; a
//     single URL path segment component, referenced as :name.
//
// A pattern beginning with "https://" (or "http://") additionally matches the
// request host: placeholders inside host labels are supported (e.g.
// https://:project.pages.dev/* captures :project). _routes.json uses a simpler
// glob where "*" matches any characters including "/" and there is no capture.
package pagesspec

import (
	"fmt"
	"regexp"
	"strings"
)

// pattern is a compiled _redirects/_headers URL pattern.
type pattern struct {
	re    *regexp.Regexp
	names []string // capture names in match order; "splat" for the splat token
	host  bool     // true when the pattern carries a scheme+host (absolute URL)
	raw   string
}

// isNameByte reports whether b may appear in a :placeholder name.
func isNameByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// compilePattern turns a raw pattern into a compiled matcher. A pattern with a
// scheme prefix (https:// or http://) matches host+path; otherwise it matches
// the path only. At most one splat "*" is permitted.
func compilePattern(raw string) (*pattern, error) {
	work := raw
	host := false
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(work, scheme) {
			work = work[len(scheme):]
			host = true
			break
		}
	}

	var b strings.Builder
	b.WriteByte('^')
	var names []string
	splats := 0
	for i := 0; i < len(work); {
		c := work[i]
		switch {
		case c == '*':
			splats++
			names = append(names, "splat")
			b.WriteString("(.*)")
			i++
		case c == ':' && i+1 < len(work) && isNameByte(work[i+1]):
			j := i + 1
			for j < len(work) && isNameByte(work[j]) {
				j++
			}
			names = append(names, work[i+1:j])
			b.WriteString("([^/.]+)")
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteByte('$')

	if splats > 1 {
		return nil, fmt.Errorf("pattern %q: at most one splat '*' is allowed", raw)
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", raw, err)
	}
	return &pattern{re: re, names: names, host: host, raw: raw}, nil
}

// match tests subject against the compiled pattern and returns the captured
// tokens keyed by name (splat under "splat").
func (p *pattern) match(subject string) (map[string]string, bool) {
	m := p.re.FindStringSubmatch(subject)
	if m == nil {
		return nil, false
	}
	caps := make(map[string]string, len(p.names))
	for i, name := range p.names {
		caps[name] = m[i+1]
	}
	return caps, true
}

// substitute expands :splat and :name tokens in tmpl using caps. Unknown tokens
// are left untouched.
func substitute(tmpl string, caps map[string]string) string {
	if !strings.ContainsRune(tmpl, ':') {
		return tmpl
	}
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] == ':' {
			j := i + 1
			for j < len(tmpl) && isNameByte(tmpl[j]) {
				j++
			}
			if j > i+1 {
				if v, ok := caps[tmpl[i+1:j]]; ok {
					b.WriteString(v)
					i = j
					continue
				}
			}
		}
		b.WriteByte(tmpl[i])
		i++
	}
	return b.String()
}

// matchGlob reports whether s matches a _routes.json glob, where "*" matches any
// characters including "/" and all other characters are literal.
func matchGlob(glob, s string) bool {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(glob); i++ {
		if glob[i] == '*' {
			b.WriteString(".*")
		} else {
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	b.WriteByte('$')
	return regexp.MustCompile(b.String()).MatchString(s)
}
