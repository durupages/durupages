// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package assets

import (
	"strings"

	"github.com/durupages/durupages/pkg/manifest"
)

// ETag returns the entity tag for entry: the content hash wrapped in double
// quotes, as required by RFC 9110 (e.g. `"a1b2c3"`). It is a strong validator
// because the hash uniquely identifies the byte content.
func ETag(entry manifest.StaticEntry) string {
	return `"` + entry.Hash + `"`
}

// NotModified reports whether a request carrying the given If-None-Match header
// value should receive a 304 Not Modified instead of entry's body.
//
// The comparison follows RFC 9110 If-None-Match semantics: "*" matches any
// current representation, otherwise the header is a comma-separated list of
// entity tags compared with the weak comparison function (the "W/" weak
// validator prefix is ignored on both sides). A match on any list member means
// the client's cached copy is current.
func NotModified(ifNoneMatch string, entry manifest.StaticEntry) bool {
	inm := strings.TrimSpace(ifNoneMatch)
	if inm == "" {
		return false
	}
	if inm == "*" {
		return true
	}

	want := stripWeak(ETag(entry))
	for _, tok := range strings.Split(inm, ",") {
		if stripWeak(strings.TrimSpace(tok)) == want {
			return true
		}
	}
	return false
}

// stripWeak removes the leading "W/" weak-validator prefix so that strong and
// weak tags of the same opaque value compare equal (weak comparison).
func stripWeak(tag string) string {
	return strings.TrimPrefix(tag, "W/")
}
