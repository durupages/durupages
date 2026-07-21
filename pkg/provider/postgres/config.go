// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package postgres

import (
	"encoding/json"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// duration is a local DTO type that JSON-encodes a time.Duration as a Go
// duration string ("30s", "1h") instead of an integer nanosecond count, so the
// JSONB stored in Postgres is human-readable and stable across Go versions.
type duration time.Duration

// MarshalJSON encodes the duration as a Go duration string.
func (d duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON parses a Go duration string back into a duration.
func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = duration(parsed)
	return nil
}

// tenantConfigDTO is the JSONB representation of provider.TenantConfig.
type tenantConfigDTO struct {
	MaxConcurrency int               `json:"maxConcurrency,omitempty"`
	IdleTTL        duration          `json:"idleTTL,omitempty"`
	WorkerCPULimit string            `json:"workerCPULimit,omitempty"`
	WorkerMemLimit string            `json:"workerMemLimit,omitempty"`
	PodLabels      map[string]string `json:"podLabels,omitempty"`
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
}

// pageConfigDTO is the JSONB representation of provider.PageConfig.
//
// Env and Secret are both persisted. Secret is stored in the same JSONB
// document as plaintext; at-rest encryption (e.g. pgcrypto, disk/volume
// encryption, or an external KMS) is the operator's choice and is intentionally
// out of scope for this reference implementation.
type pageConfigDTO struct {
	QueueTimeout   duration          `json:"queueTimeout,omitempty"`
	RequestTimeout duration          `json:"requestTimeout,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Secret         map[string]string `json:"secret,omitempty"`
	LogsEnabled    *bool             `json:"logsEnabled,omitempty"`
}

// encodeTenantConfig marshals a provider.TenantConfig to JSONB bytes.
func encodeTenantConfig(c provider.TenantConfig) ([]byte, error) {
	return json.Marshal(tenantConfigDTO{
		MaxConcurrency: c.MaxConcurrency,
		IdleTTL:        duration(c.IdleTTL),
		WorkerCPULimit: c.WorkerCPULimit,
		WorkerMemLimit: c.WorkerMemLimit,
		PodLabels:      c.PodLabels,
		PodAnnotations: c.PodAnnotations,
	})
}

// decodeTenantConfig unmarshals JSONB bytes into a provider.TenantConfig.
func decodeTenantConfig(b []byte) (provider.TenantConfig, error) {
	var dto tenantConfigDTO
	if len(b) > 0 {
		if err := json.Unmarshal(b, &dto); err != nil {
			return provider.TenantConfig{}, err
		}
	}
	return provider.TenantConfig{
		MaxConcurrency: dto.MaxConcurrency,
		IdleTTL:        time.Duration(dto.IdleTTL),
		WorkerCPULimit: dto.WorkerCPULimit,
		WorkerMemLimit: dto.WorkerMemLimit,
		PodLabels:      dto.PodLabels,
		PodAnnotations: dto.PodAnnotations,
	}, nil
}

// encodePageConfig marshals a provider.PageConfig to JSONB bytes.
func encodePageConfig(c provider.PageConfig) ([]byte, error) {
	return json.Marshal(pageConfigDTO{
		QueueTimeout:   duration(c.QueueTimeout),
		RequestTimeout: duration(c.RequestTimeout),
		Env:            c.Env,
		Secret:         c.Secret,
		LogsEnabled:    c.LogsEnabled,
	})
}

// decodePageConfig unmarshals JSONB bytes into a provider.PageConfig.
func decodePageConfig(b []byte) (provider.PageConfig, error) {
	var dto pageConfigDTO
	if len(b) > 0 {
		if err := json.Unmarshal(b, &dto); err != nil {
			return provider.PageConfig{}, err
		}
	}
	return provider.PageConfig{
		QueueTimeout:   time.Duration(dto.QueueTimeout),
		RequestTimeout: time.Duration(dto.RequestTimeout),
		Env:            dto.Env,
		Secret:         dto.Secret,
		LogsEnabled:    dto.LogsEnabled,
	}, nil
}
