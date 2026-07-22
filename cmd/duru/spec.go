// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The *Spec types mirror the admin API's JSON documents field for field, with
// every optional field behind a pointer so that "absent" and "explicitly empty"
// stay distinguishable. That is what makes `duru tenant get acme > t.json`,
// edit, `duru tenant set acme -f t.json` a faithful round-trip, and what lets a
// `--file` overlay touch only the fields the file actually carries.

// tenantSpec is the wire document of GET/POST /v1/tenants[/{id}].
type tenantSpec struct {
	ID     string        `json:"id"`
	Config tenantCfgSpec `json:"config"`
}

// tenantCfgSpec is the "config" object of a tenant.
type tenantCfgSpec struct {
	MaxConcurrency *int               `json:"maxConcurrency,omitempty"`
	IdleTTL        *string            `json:"idleTTL,omitempty"`
	WorkerCPULimit *string            `json:"workerCPULimit,omitempty"`
	WorkerMemLimit *string            `json:"workerMemLimit,omitempty"`
	PodLabels      *map[string]string `json:"podLabels,omitempty"`
	PodAnnotations *map[string]string `json:"podAnnotations,omitempty"`
}

// overlay copies every field src actually carries over t, leaving the rest
// alone. Maps are replaced wholesale: a map present in a file is that map.
func (t *tenantSpec) overlay(src tenantSpec) {
	if src.ID != "" {
		t.ID = src.ID
	}
	c, s := &t.Config, src.Config
	if s.MaxConcurrency != nil {
		c.MaxConcurrency = s.MaxConcurrency
	}
	if s.IdleTTL != nil {
		c.IdleTTL = s.IdleTTL
	}
	if s.WorkerCPULimit != nil {
		c.WorkerCPULimit = s.WorkerCPULimit
	}
	if s.WorkerMemLimit != nil {
		c.WorkerMemLimit = s.WorkerMemLimit
	}
	if s.PodLabels != nil {
		c.PodLabels = s.PodLabels
	}
	if s.PodAnnotations != nil {
		c.PodAnnotations = s.PodAnnotations
	}
}

// pageSpec is the wire document of GET/POST /v1/pages[/{id}].
type pageSpec struct {
	ID                 string      `json:"id"`
	TenantID           string      `json:"tenantId"`
	ActiveDeploymentID string      `json:"activeDeploymentId,omitempty"`
	CustomDomains      *[]string   `json:"customDomains,omitempty"`
	Config             pageCfgSpec `json:"config"`
}

// pageCfgSpec is the "config" object of a page.
//
// SecretKeys is read-only: the API reports it on reads and the CLI accepts it
// in a file (so a get/edit/set round-trip works) but never sends it back.
type pageCfgSpec struct {
	QueueTimeout   *string            `json:"queueTimeout,omitempty"`
	RequestTimeout *string            `json:"requestTimeout,omitempty"`
	Env            *map[string]string `json:"env,omitempty"`
	Secret         *map[string]string `json:"secret,omitempty"`
	SecretKeys     []string           `json:"secretKeys,omitempty"`
	LogsEnabled    *bool              `json:"logsEnabled,omitempty"`
}

// overlay copies every field src actually carries over p, leaving the rest
// alone. Maps and the domain list are replaced wholesale.
func (p *pageSpec) overlay(src pageSpec) {
	if src.ID != "" {
		p.ID = src.ID
	}
	if src.TenantID != "" {
		p.TenantID = src.TenantID
	}
	if src.ActiveDeploymentID != "" {
		p.ActiveDeploymentID = src.ActiveDeploymentID
	}
	if src.CustomDomains != nil {
		p.CustomDomains = src.CustomDomains
	}
	c, s := &p.Config, src.Config
	if s.QueueTimeout != nil {
		c.QueueTimeout = s.QueueTimeout
	}
	if s.RequestTimeout != nil {
		c.RequestTimeout = s.RequestTimeout
	}
	if s.Env != nil {
		c.Env = s.Env
	}
	if s.Secret != nil {
		c.Secret = s.Secret
	}
	if s.LogsEnabled != nil {
		c.LogsEnabled = s.LogsEnabled
	}
}

// forRequest returns the document to POST: the read-only secretKeys field is
// dropped so the request carries only writable state.
func (p pageSpec) forRequest() pageSpec {
	p.Config.SecretKeys = nil
	return p
}

// mergeMap applies the repeatable k=v flag pairs on top of cur (the map merged
// from server state and --file, nil when neither carried one), honouring a
// --clear-* that drops it first. With neither a clear nor a pair it returns cur
// untouched, so a map nobody mentioned is left exactly as it was.
func mergeMap(cur *map[string]string, clear bool, pairs []kvPair) *map[string]string {
	if !clear && len(pairs) == 0 {
		return cur
	}
	out := map[string]string{}
	if !clear && cur != nil {
		for k, v := range *cur {
			out[k] = v
		}
	}
	for _, p := range pairs {
		out[p.key] = p.value
	}
	return &out
}

// kvPair is one parsed "key=value" flag occurrence.
type kvPair struct{ key, value string }

// kvFlag collects repeatable "key=value" flags in the order they were given.
type kvFlag struct{ pairs []kvPair }

var _ flagValue = (*kvFlag)(nil)

// String renders the collected pairs, sorted, for the flag package.
func (f *kvFlag) String() string {
	if f == nil || len(f.pairs) == 0 {
		return ""
	}
	out := make([]string, 0, len(f.pairs))
	for _, p := range f.pairs {
		out = append(out, p.key+"="+p.value)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// Set parses one "key=value" occurrence. A missing '=' is a usage error.
func (f *kvFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("%q is not in key=value form", s)
	}
	if k == "" {
		return fmt.Errorf("%q has an empty key", s)
	}
	f.pairs = append(f.pairs, kvPair{key: k, value: v})
	return nil
}

// listFlag collects a repeatable plain-string flag. Unlike a map flag it
// replaces the stored list rather than merging into it.
type listFlag struct{ values []string }

var _ flagValue = (*listFlag)(nil)

func (f *listFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(f.values, ",")
}

func (f *listFlag) Set(s string) error {
	if s == "" {
		return fmt.Errorf("empty value")
	}
	f.values = append(f.values, s)
	return nil
}

// headerFlag collects repeatable `--header "Name: value"` flags.
type headerFlag struct{ header http.Header }

var _ flagValue = (*headerFlag)(nil)

func (f *headerFlag) String() string {
	if f == nil || len(f.header) == 0 {
		return ""
	}
	out := make([]string, 0, len(f.header))
	for name, values := range f.header {
		for _, v := range values {
			out = append(out, name+": "+v)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// Set parses one "Name: value" header. A missing ':' is a usage error.
func (f *headerFlag) Set(s string) error {
	name, value, ok := strings.Cut(s, ":")
	if !ok {
		return fmt.Errorf("%q is not in \"Name: value\" form", s)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%q has an empty header name", s)
	}
	if f.header == nil {
		f.header = http.Header{}
	}
	f.header.Add(name, strings.TrimSpace(value))
	return nil
}

// boolFlag is a tri-state boolean: it stays unset until the flag is given, so
// "unset" and "false" remain distinguishable (the API field is a *bool). It
// always takes an argument, so both `--logs-enabled true` and
// `--logs-enabled=true` work.
type boolFlag struct{ value *bool }

var _ flagValue = (*boolFlag)(nil)

func (f *boolFlag) String() string {
	if f == nil || f.value == nil {
		return ""
	}
	return strconv.FormatBool(*f.value)
}

func (f *boolFlag) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("%q is not a boolean (want true or false)", s)
	}
	f.value = &v
	return nil
}

// flagValue is the subset of flag.Value the custom flag types implement; it
// exists only for the compile-time assertions above.
type flagValue interface {
	String() string
	Set(string) error
}

// decodeSpec decodes an admin API document into v, rejecting unknown fields so
// that a typo in a hand-written file fails loudly instead of being ignored.
func decodeSpec(data []byte, what string, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}
