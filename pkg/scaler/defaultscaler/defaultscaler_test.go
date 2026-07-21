// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package defaultscaler_test

import (
	"context"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/scaler"
	"github.com/durupages/durupages/pkg/scaler/defaultscaler"
)

func tenant(maxConc int, idle time.Duration) *provider.Tenant {
	return &provider.Tenant{
		ID: "t1",
		Config: provider.TenantConfig{
			MaxConcurrency: maxConc,
			IdleTTL:        idle,
		},
	}
}

func defaults() scaler.ScaleDefaults {
	return scaler.ScaleDefaults{
		TargetConcurrencyPerPod: 32,
		MaxConcurrencyPerPod:    256,
		DefaultMaxConcurrency:   10,
		DefaultIdleTTL:          5 * time.Minute,
	}
}

func TestDesiredPods(t *testing.T) {
	tests := []struct {
		name     string
		inFlight int
		queue    int
		tenant   *provider.Tenant
		defs     scaler.ScaleDefaults
		want     int
	}{
		{
			name: "idle tenant needs one pod",
			defs: defaults(),
			want: 1, // demand=1, ceil(1/32)=1
		},
		{
			name:     "at target keeps one pod",
			inFlight: 31,
			defs:     defaults(),
			want:     1, // demand=32, ceil(32/32)=1
		},
		{
			name:     "one over target adds a pod",
			inFlight: 32,
			defs:     defaults(),
			want:     2, // demand=33, ceil(33/32)=2
		},
		{
			name:     "queue depth counts toward demand",
			inFlight: 20,
			queue:    50,
			defs:     defaults(),
			want:     3, // demand=71, ceil(71/32)=3
		},
		{
			name:     "burst demand scales up",
			inFlight: 200,
			queue:    100,
			tenant:   tenant(0, 0),
			defs:     defaults(),
			want:     10, // demand=301, ceil/32=10, clamp to default max 10
		},
		{
			name:     "clamp at tenant max",
			inFlight: 1000,
			tenant:   tenant(4, 0),
			defs:     defaults(),
			want:     4,
		},
		{
			name:     "tenant max overrides higher default",
			inFlight: 1000,
			tenant:   tenant(3, 0),
			defs:     defaults(),
			want:     3,
		},
		{
			name:     "default max used when tenant unset",
			inFlight: 1000,
			tenant:   tenant(0, 0),
			defs:     defaults(),
			want:     10,
		},
		{
			name:     "nil tenant falls back to default max",
			inFlight: 1000,
			tenant:   nil,
			defs:     defaults(),
			want:     10,
		},
		{
			name:     "zero target guarded (no divide by zero)",
			inFlight: 5,
			tenant:   tenant(0, 0),
			defs:     scaler.ScaleDefaults{TargetConcurrencyPerPod: 0, DefaultMaxConcurrency: 100},
			want:     6, // target->1, demand=6, ceil(6/1)=6
		},
		{
			name:   "zero default max floored to one",
			tenant: nil,
			defs:   scaler.ScaleDefaults{TargetConcurrencyPerPod: 32, DefaultMaxConcurrency: 0},
			want:   1,
		},
	}

	s := defaultscaler.New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := scaler.TenantScaleState{
				Tenant:     tt.tenant,
				InFlight:   tt.inFlight,
				QueueDepth: tt.queue,
				Defaults:   tt.defs,
			}
			if got := s.DesiredPods(context.Background(), st); got != tt.want {
				t.Fatalf("DesiredPods = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestDesiredPodsIgnoresCreatingPods asserts DesiredPods is a pure target: it
// does not subtract creating pods (the controller compares against ready+creating).
func TestDesiredPodsIgnoresCreatingPods(t *testing.T) {
	s := defaultscaler.New()
	base := scaler.TenantScaleState{
		Tenant:   tenant(0, 0),
		InFlight: 100,
		Defaults: defaults(),
	}
	withCreating := base
	withCreating.CreatingPods = []scaler.PodRef{{Name: "c1"}, {Name: "c2"}, {Name: "c3"}}
	withReady := base
	withReady.ReadyPods = []scaler.PodState{{Ref: scaler.PodRef{Name: "r1"}}}

	got := s.DesiredPods(context.Background(), base)
	if g := s.DesiredPods(context.Background(), withCreating); g != got {
		t.Fatalf("creating pods affected DesiredPods: %d vs %d", g, got)
	}
	if g := s.DesiredPods(context.Background(), withReady); g != got {
		t.Fatalf("ready pods affected DesiredPods: %d vs %d", g, got)
	}
}

func podRefs(refs []scaler.PodRef) map[string]bool {
	m := make(map[string]bool, len(refs))
	for _, r := range refs {
		m[r.Name] = true
	}
	return m
}

func TestSelectScaleDown(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	ttl := 5 * time.Minute

	idlePod := func(name string, idleAgo time.Duration, inFlight int) scaler.PodState {
		p := scaler.PodState{Ref: scaler.PodRef{Name: name}, InFlight: inFlight}
		if idleAgo >= 0 {
			p.IdleSince = now.Add(-idleAgo)
		}
		return p
	}

	tests := []struct {
		name     string
		inFlight int
		queue    int
		tenant   *provider.Tenant
		defs     scaler.ScaleDefaults
		pods     []scaler.PodState
		want     []string
	}{
		{
			name:   "idle past ttl is drained",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", 10*time.Minute, 0)},
			want:   []string{"a"},
		},
		{
			name:   "idle within ttl is kept",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", 1*time.Minute, 0)},
			want:   nil,
		},
		{
			name:   "exactly at ttl boundary is kept (strictly older required)",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", ttl, 0)},
			want:   nil,
		},
		{
			name:   "one nanosecond past ttl is drained",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", ttl+time.Nanosecond, 0)},
			want:   []string{"a"},
		},
		{
			name:   "zero IdleSince never drained",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", -1, 0)},
			want:   nil,
		},
		{
			name:   "busy pod never drained even if IdleSince stale",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", 10*time.Minute, 3)},
			want:   nil,
		},
		{
			name:     "with demand keep at least one pod",
			inFlight: 5,
			tenant:   tenant(0, 0),
			defs:     defaults(),
			pods: []scaler.PodState{
				idlePod("a", 10*time.Minute, 0),
				idlePod("b", 10*time.Minute, 0),
			},
			want: []string{"a"}, // one of the two kept
		},
		{
			name:   "zero demand allows full scale down",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods: []scaler.PodState{
				idlePod("a", 10*time.Minute, 0),
				idlePod("b", 10*time.Minute, 0),
			},
			want: []string{"a", "b"},
		},
		{
			name:   "demand from queue keeps one pod",
			queue:  3,
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods: []scaler.PodState{
				idlePod("a", 10*time.Minute, 0),
				idlePod("b", 10*time.Minute, 0),
			},
			want: []string{"a"},
		},
		{
			name:     "busy pod satisfies min-one so all idle drained",
			inFlight: 4,
			tenant:   tenant(0, 0),
			defs:     defaults(),
			pods: []scaler.PodState{
				idlePod("busy", -1, 4),
				idlePod("a", 10*time.Minute, 0),
				idlePod("b", 10*time.Minute, 0),
			},
			want: []string{"a", "b"},
		},
		{
			name:   "tenant idle ttl override (shorter) drains sooner",
			tenant: tenant(0, 1*time.Minute),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", 2*time.Minute, 0)},
			want:   []string{"a"},
		},
		{
			name:   "tenant idle ttl override (longer) keeps pod",
			tenant: tenant(0, 20*time.Minute),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", 10*time.Minute, 0)},
			want:   nil,
		},
		{
			name:   "default idle ttl used when tenant unset",
			tenant: tenant(0, 0),
			defs:   defaults(),
			pods:   []scaler.PodState{idlePod("a", 6*time.Minute, 0)},
			want:   []string{"a"},
		},
	}

	s := defaultscaler.New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := scaler.TenantScaleState{
				Tenant:     tt.tenant,
				InFlight:   tt.inFlight,
				QueueDepth: tt.queue,
				ReadyPods:  tt.pods,
				Defaults:   tt.defs,
				Now:        now,
			}
			got := s.SelectScaleDown(context.Background(), st)
			if len(got) != len(tt.want) {
				t.Fatalf("SelectScaleDown returned %v, want %v (count mismatch)", refNames(got), tt.want)
			}
			// Every returned pod must be an idle, non-busy candidate.
			gotSet := podRefs(got)
			for _, r := range got {
				if !idleCandidate(st, r) {
					t.Fatalf("SelectScaleDown returned non-idle-candidate %q", r.Name)
				}
			}
			// When demand>0 and all ready pods are idle candidates, which single
			// pod is retained is implementation-defined, so only the count is
			// asserted. Otherwise the exact set must match.
			if !ambiguousKeep(st) {
				for _, w := range tt.want {
					if !gotSet[w] {
						t.Fatalf("SelectScaleDown = %v, want to include %q", refNames(got), w)
					}
				}
			}
		})
	}
}

// idleCandidate reports whether ref names an idle, non-busy, TTL-expired pod in st.
func idleCandidate(st scaler.TenantScaleState, ref scaler.PodRef) bool {
	ttl := st.Defaults.DefaultIdleTTL
	if st.Tenant != nil && st.Tenant.Config.IdleTTL > 0 {
		ttl = st.Tenant.Config.IdleTTL
	}
	for _, p := range st.ReadyPods {
		if p.Ref != ref {
			continue
		}
		return p.InFlight == 0 && !p.IdleSince.IsZero() && st.Now.Sub(p.IdleSince) > ttl
	}
	return false
}

// ambiguousKeep reports whether demand>0 and every ready pod is an idle
// candidate, so which single pod is retained is implementation-defined.
func ambiguousKeep(st scaler.TenantScaleState) bool {
	if st.InFlight+st.QueueDepth == 0 {
		return false
	}
	for _, p := range st.ReadyPods {
		if p.InFlight != 0 {
			return false // a busy pod satisfies the min-one, so the set is exact
		}
	}
	return true
}

func refNames(refs []scaler.PodRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}
