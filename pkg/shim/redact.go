// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"

	"github.com/durupages/durupages/pkg/usage"
)

// minSecretLen is the shortest Secret value eligible for value redaction. Very
// short values cause too many false positives (docs 9.4).
const minSecretLen = 6

// redactHeaderNames are always redacted by name in recorded event headers.
var redactHeaderNames = map[string]bool{
	"authorization": true,
	"cookie":        true,
	"set-cookie":    true,
	"x-api-key":     true,
}

// secretNeedle is one search/replace pair for value redaction.
type secretNeedle struct {
	needle      string
	replacement string
}

// redactor performs header-name and Secret-value redaction on usage events
// before they leave the pod. It is immutable; a new one is built on each swap.
type redactor struct {
	needles []secretNeedle // sorted longest-first so overlaps redact greedily
}

// newRedactor builds a redactor from the merged Secret map (key -> value) of
// the loaded pages. Each eligible value is matched raw and in its common
// encodings (URL-escaped, standard base64 with and without padding).
func newRedactor(secrets map[string]string) *redactor {
	var needles []secretNeedle
	seen := map[string]bool{}
	add := func(needle, key string) {
		if needle == "" || seen[needle] {
			return
		}
		seen[needle] = true
		needles = append(needles, secretNeedle{needle: needle, replacement: "[REDACTED:" + key + "]"})
	}
	for key, val := range secrets {
		if len(val) < minSecretLen {
			continue
		}
		add(val, key)
		add(url.QueryEscape(val), key)
		add(base64.StdEncoding.EncodeToString([]byte(val)), key)
		add(base64.RawStdEncoding.EncodeToString([]byte(val)), key)
	}
	sort.Slice(needles, func(i, j int) bool {
		return len(needles[i].needle) > len(needles[j].needle)
	})
	return &redactor{needles: needles}
}

// redactString replaces every known Secret-value occurrence in s.
func (r *redactor) redactString(s string) string {
	for _, n := range r.needles {
		if strings.Contains(s, n.needle) {
			s = strings.ReplaceAll(s, n.needle, n.replacement)
		}
	}
	return s
}

// redactHeaders redacts sensitive header names and Secret values in-place.
func (r *redactor) redactHeaders(h map[string]string) {
	for name, val := range h {
		if redactHeaderNames[strings.ToLower(name)] {
			h[name] = "[REDACTED]"
			continue
		}
		h[name] = r.redactString(val)
	}
}

// apply redacts an entire RequestUsage: header values, log messages and
// exception message/stack.
func (r *redactor) apply(u *usage.RequestUsage) {
	r.redactHeaders(u.Event.Request.Headers)
	for i := range u.Logs {
		u.Logs[i].Message = r.redactString(u.Logs[i].Message)
	}
	for i := range u.Exceptions {
		u.Exceptions[i].Message = r.redactString(u.Exceptions[i].Message)
		u.Exceptions[i].Stack = r.redactString(u.Exceptions[i].Stack)
	}
}

// rebuildRedactorLocked rebuilds the Secret-value redactor from the currently
// active load set. Callers hold s.mu.
func (s *Shim) rebuildRedactorLocked() {
	merged := map[string]string{}
	for _, depID := range s.active {
		dep := s.deployments[depID]
		if dep == nil {
			continue
		}
		for k, v := range dep.secret {
			merged[k] = v
		}
	}
	s.redactor.Store(newRedactor(merged))
}
