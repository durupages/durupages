// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package manifest defines the deployment manifest schema: derived metadata
// generated at upload time from a wrangler build output directory, shared by
// router (static pipeline) and worker shim (ASSETS binding).
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
)

// Version is the current manifest schema version.
const Version = 1

// Manifest is stored as manifest.json next to each deployment.
type Manifest struct {
	Version      int    `json:"version"`
	TenantID     string `json:"tenantId"`
	PageID       string `json:"pageId"`
	DeploymentID string `json:"deploymentId"`
	HasWorker    bool   `json:"hasWorker"`
	// Static maps request paths ("/index.html") to content-addressed entries.
	Static map[string]StaticEntry `json:"static"`
	// Routes is the parsed _routes.json (nil when absent; with a worker
	// present that means include=["/*"], Cloudflare-compatible).
	Routes *Routes `json:"routes,omitempty"`
	// Redirects are parsed _redirects rules in file order.
	Redirects []Redirect `json:"redirects,omitempty"`
	// Headers are parsed _headers rules in file order.
	Headers []HeaderRule `json:"headers,omitempty"`
	// Compat carries wrangler compatibility settings for the worker.
	Compat Compat `json:"compat,omitzero"`
}

type StaticEntry struct {
	// Hash is the hex sha256 of the file content (also the storage key leaf).
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

// Routes mirrors Cloudflare's _routes.json. Exclude wins over include.
type Routes struct {
	Version int      `json:"version"`
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// Redirect is one parsed _redirects rule. Status 200 means an internal
// rewrite (proxy) to Destination.
type Redirect struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Status      int    `json:"status"`
}

// HeaderRule is one parsed _headers rule: a URL pattern with headers to set
// and (leading-!) headers to remove.
type HeaderRule struct {
	Pattern string            `json:"pattern"`
	Set     map[string]string `json:"set,omitempty"`
	Unset   []string          `json:"unset,omitempty"`
}

// Compat carries the worker's compatibility settings from wrangler.toml.
type Compat struct {
	Date  string   `json:"date,omitempty"`
	Flags []string `json:"flags,omitempty"`
}

// Decode reads and validates a manifest.
func Decode(r io.Reader) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(r)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: decode: %w", err)
	}
	if m.Version != Version {
		return nil, fmt.Errorf("manifest: unsupported version %d", m.Version)
	}
	return &m, nil
}

// Encode writes the manifest as JSON.
func (m *Manifest) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}
