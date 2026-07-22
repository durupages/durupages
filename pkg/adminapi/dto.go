// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// duration is a local DTO type that JSON-encodes a time.Duration as a Go
// duration string ("30s", "1h") instead of an integer nanosecond count. It
// matches the representation the PostgreSQL provider persists, so a value
// copied out of the database reads the same over the wire.
type duration time.Duration

// MarshalJSON encodes the duration as a Go duration string.
func (d duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON parses a Go duration string ("30s", "1h30m"). An empty string
// and JSON null both mean zero; a bare number is rejected on purpose so that
// an ambiguous unit can never be silently assumed.
func (d *duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = duration(parsed)
	return nil
}

// tenantJSON is the wire representation of provider.Tenant.
type tenantJSON struct {
	ID     string           `json:"id"`
	Config tenantConfigJSON `json:"config,omitzero"`
}

// tenantConfigJSON is the wire representation of provider.TenantConfig.
type tenantConfigJSON struct {
	MaxConcurrency int               `json:"maxConcurrency,omitempty"`
	IdleTTL        duration          `json:"idleTTL,omitempty"`
	WorkerCPULimit string            `json:"workerCPULimit,omitempty"`
	WorkerMemLimit string            `json:"workerMemLimit,omitempty"`
	PodLabels      map[string]string `json:"podLabels,omitempty"`
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
}

// pageJSON is the wire representation of provider.Page.
//
// CustomDomains is a pointer so that an absent field ("keep what is stored")
// is distinguishable from an explicit empty list ("remove every domain").
type pageJSON struct {
	ID                 string         `json:"id"`
	TenantID           string         `json:"tenantId"`
	ActiveDeploymentID string         `json:"activeDeploymentId,omitempty"`
	CustomDomains      *[]string      `json:"customDomains,omitempty"`
	Config             pageConfigJSON `json:"config,omitzero"`
}

// pageConfigJSON is the wire representation of provider.PageConfig.
//
// Secret is write-only: it is accepted on writes and never populated in a
// response. Responses instead carry SecretKeys, the sorted list of secret
// names, so an operator can see which secrets exist without the values ever
// leaving the controller. Like CustomDomains it is a pointer: absent means
// "keep the stored secrets", an explicit object (including {}) replaces them.
type pageConfigJSON struct {
	QueueTimeout   duration           `json:"queueTimeout,omitempty"`
	RequestTimeout duration           `json:"requestTimeout,omitempty"`
	Env            map[string]string  `json:"env,omitempty"`
	Secret         *map[string]string `json:"secret,omitempty"`
	SecretKeys     []string           `json:"secretKeys,omitempty"`
	LogsEnabled    *bool              `json:"logsEnabled,omitempty"`
}

// deploymentJSON is the wire representation of provider.Deployment.
type deploymentJSON struct {
	ID        string    `json:"id"`
	PageID    string    `json:"pageId"`
	CreatedAt time.Time `json:"createdAt"`
	Active    bool      `json:"active,omitempty"`
}

// List response envelopes.
type (
	tenantsResponse struct {
		Tenants []tenantJSON `json:"tenants"`
	}
	pagesResponse struct {
		Pages []pageJSON `json:"pages"`
	}
	deploymentsResponse struct {
		Deployments []deploymentJSON `json:"deployments"`
	}
)

// customDomainsRequest is the PUT .../custom-domains body.
type customDomainsRequest struct {
	Domains []string `json:"domains"`
}

// toTenantJSON converts a provider tenant to its wire form.
func toTenantJSON(t provider.Tenant) tenantJSON {
	return tenantJSON{
		ID: t.ID,
		Config: tenantConfigJSON{
			MaxConcurrency: t.Config.MaxConcurrency,
			IdleTTL:        duration(t.Config.IdleTTL),
			WorkerCPULimit: t.Config.WorkerCPULimit,
			WorkerMemLimit: t.Config.WorkerMemLimit,
			PodLabels:      t.Config.PodLabels,
			PodAnnotations: t.Config.PodAnnotations,
		},
	}
}

// toProvider converts the wire form back to a provider tenant.
func (t tenantJSON) toProvider() provider.Tenant {
	return provider.Tenant{
		ID: t.ID,
		Config: provider.TenantConfig{
			MaxConcurrency: t.Config.MaxConcurrency,
			IdleTTL:        time.Duration(t.Config.IdleTTL),
			WorkerCPULimit: t.Config.WorkerCPULimit,
			WorkerMemLimit: t.Config.WorkerMemLimit,
			PodLabels:      t.Config.PodLabels,
			PodAnnotations: t.Config.PodAnnotations,
		},
	}
}

// toPageJSON converts a provider page to its wire form. Secret values are
// dropped; only their sorted key list is reported.
func toPageJSON(p provider.Page) pageJSON {
	domains := p.CustomDomains
	if domains == nil {
		domains = []string{}
	}
	return pageJSON{
		ID:                 p.ID,
		TenantID:           p.TenantID,
		ActiveDeploymentID: p.ActiveDeploymentID,
		CustomDomains:      &domains,
		Config: pageConfigJSON{
			QueueTimeout:   duration(p.Config.QueueTimeout),
			RequestTimeout: duration(p.Config.RequestTimeout),
			Env:            p.Config.Env,
			SecretKeys:     secretKeys(p.Config.Secret),
			LogsEnabled:    p.Config.LogsEnabled,
		},
	}
}

// toProvider converts the wire form back to a provider page. Fields whose
// wire representation was absent come back zero; the handler merges them with
// the stored page.
func (p pageJSON) toProvider() provider.Page {
	out := provider.Page{
		ID:                 p.ID,
		TenantID:           p.TenantID,
		ActiveDeploymentID: p.ActiveDeploymentID,
		Config: provider.PageConfig{
			QueueTimeout:   time.Duration(p.Config.QueueTimeout),
			RequestTimeout: time.Duration(p.Config.RequestTimeout),
			Env:            p.Config.Env,
			LogsEnabled:    p.Config.LogsEnabled,
		},
	}
	if p.CustomDomains != nil {
		out.CustomDomains = *p.CustomDomains
	}
	if p.Config.Secret != nil {
		out.Config.Secret = *p.Config.Secret
	}
	return out
}

// secretKeys returns the sorted key list of a secret map, or nil when empty.
func secretKeys(secret map[string]string) []string {
	if len(secret) == 0 {
		return nil
	}
	keys := make([]string, 0, len(secret))
	for k := range secret {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// toDeploymentJSON converts a provider deployment to its wire form.
func toDeploymentJSON(d provider.Deployment, activeID string) deploymentJSON {
	return deploymentJSON{
		ID:        d.ID,
		PageID:    d.PageID,
		CreatedAt: d.CreatedAt,
		Active:    d.ID != "" && d.ID == activeID,
	}
}
