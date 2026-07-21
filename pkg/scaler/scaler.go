// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package scaler defines the Scaler extension point: scale up/down policy
// for tenant worker pods. The default implementation (package defaultscaler)
// implements the target/max-concurrency algorithm from the architecture doc.
//
// Call sites are fixed regardless of implementation:
//   - DesiredPods runs synchronously on every AcquireSlot enqueue (hot path;
//     must return in microseconds — use internal caches for external signals).
//   - SelectScaleDown runs on a periodic loop (default 30s).
//
// The controller clamps DesiredPods to the tenant MaxConcurrency; custom
// implementations cannot exceed the safety limit.
package scaler

import (
	"context"
	"time"

	"github.com/durupages/durupages/pkg/provider"
)

// PodRef identifies a worker pod.
type PodRef struct {
	Name string
}

// PodState is the per-pod view given to the scaler.
type PodState struct {
	Ref      PodRef
	InFlight int
	// IdleSince is the time the pod last became idle (zero if serving).
	IdleSince time.Time
	// LoadedPages lists pageIDs currently loaded in the pod.
	LoadedPages []string
}

// ScaleDefaults carries controller-level defaults.
type ScaleDefaults struct {
	TargetConcurrencyPerPod int
	MaxConcurrencyPerPod    int
	DefaultMaxConcurrency   int
	DefaultIdleTTL          time.Duration
}

// TenantScaleState is a consistent snapshot of one tenant's demand and pods.
type TenantScaleState struct {
	Tenant       *provider.Tenant
	InFlight     int
	QueueDepth   int
	ReadyPods    []PodState
	CreatingPods []PodRef
	Defaults     ScaleDefaults
	Now          time.Time
}

// Scaler decides scale up/down.
type Scaler interface {
	// DesiredPods returns the number of pods the tenant should have.
	// If it exceeds ready+creating, the controller creates the difference.
	DesiredPods(ctx context.Context, s TenantScaleState) int
	// SelectScaleDown returns pods that may be drained and terminated now.
	SelectScaleDown(ctx context.Context, s TenantScaleState) []PodRef
}
