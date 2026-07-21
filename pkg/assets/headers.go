// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package assets

// DefaultHeaders returns the default HTTP response headers Cloudflare Pages
// applies to every static asset (docs/ARCHITECTURE.md 3.4). A fresh map is
// returned on each call so callers may mutate it freely; individual headers can
// be overridden or removed afterwards by a project's _headers rules (the
// leading-"!" removal syntax).
func DefaultHeaders() map[string]string {
	return map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Referrer-Policy":             "strict-origin-when-cross-origin",
		"X-Content-Type-Options":      "nosniff",
		"Cache-Control":               "public, max-age=0, must-revalidate",
	}
}
