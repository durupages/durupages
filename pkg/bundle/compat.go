// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package bundle

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/durupages/durupages/pkg/manifest"
)

// parseCompat extracts the worker compatibility settings from the deployment's
// wrangler configuration. wrangler.toml takes precedence, then wrangler.json,
// then wrangler.jsonc. A missing configuration yields the zero Compat.
func parseCompat(dir string) (manifest.Compat, error) {
	if data, ok, err := readOptional(filepath.Join(dir, "wrangler.toml")); err != nil {
		return manifest.Compat{}, err
	} else if ok {
		return parseWranglerTOML(data), nil
	}
	if data, ok, err := readOptional(filepath.Join(dir, "wrangler.json")); err != nil {
		return manifest.Compat{}, err
	} else if ok {
		return parseWranglerJSON(data)
	}
	if data, ok, err := readOptional(filepath.Join(dir, "wrangler.jsonc")); err != nil {
		return manifest.Compat{}, err
	} else if ok {
		return parseWranglerJSON(data)
	}
	return manifest.Compat{}, nil
}

// parseWranglerTOML is a minimal, purpose-built parser (stdlib only) that
// extracts the compatibility_date string and compatibility_flags array of
// strings from a wrangler.toml. It handles quoted strings, single-line arrays
// of quoted strings and '#' comments, and ignores everything else, including
// table headers.
func parseWranglerTOML(data []byte) manifest.Compat {
	var c manifest.Compat
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripTOMLComment(raw))
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "compatibility_date":
			if q := extractQuoted(val); len(q) > 0 {
				c.Date = q[0]
			}
		case "compatibility_flags":
			if q := extractQuoted(val); len(q) > 0 {
				c.Flags = q
			}
		}
	}
	return c
}

// stripTOMLComment removes a trailing '#' comment while respecting double- and
// single-quoted strings.
func stripTOMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '"' || ch == '\'':
			quote = ch
		case ch == '#':
			return line[:i]
		}
	}
	return line
}

// extractQuoted returns every double-quoted substring in s, in order. Escapes
// are not interpreted (compatibility values do not contain them).
func extractQuoted(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			return out
		}
		s = s[i+1:]
		j := strings.IndexByte(s, '"')
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+1:]
	}
}

// wranglerJSON captures the compatibility fields of a wrangler.json/.jsonc.
type wranglerJSON struct {
	CompatibilityDate  string   `json:"compatibility_date"`
	CompatibilityFlags []string `json:"compatibility_flags"`
}

// parseWranglerJSON parses a wrangler.json or .jsonc after naively stripping
// // line and /* */ block comments.
func parseWranglerJSON(data []byte) (manifest.Compat, error) {
	var w wranglerJSON
	if err := json.Unmarshal(stripJSONComments(data), &w); err != nil {
		return manifest.Compat{}, fmt.Errorf("bundle: parse wrangler json: %w", err)
	}
	return manifest.Compat{Date: w.CompatibilityDate, Flags: w.CompatibilityFlags}, nil
}

// stripJSONComments removes // line comments and /* */ block comments. The
// stripping is naive (not string-aware), as documented for JSONC support.
func stripJSONComments(data []byte) []byte {
	s := string(data)
	// Block comments first.
	for {
		start := strings.Index(s, "/*")
		if start < 0 {
			break
		}
		end := strings.Index(s[start+2:], "*/")
		if end < 0 {
			s = s[:start]
			break
		}
		s = s[:start] + s[start+2+end+2:]
	}
	// Line comments.
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}
