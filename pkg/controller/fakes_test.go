// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"context"
	"sync"
	"time"

	"github.com/durupages/durupages/pkg/scaler"
)

// fakePodManager records Create/Delete calls and returns a scripted List. It is
// safe for concurrent use.
type fakePodManager struct {
	mu        sync.Mutex
	created   []PodSpec
	deleted   []string
	listResp  []ExistingPod
	createErr error
}

func newFakePodManager() *fakePodManager { return &fakePodManager{} }

func (f *fakePodManager) Create(ctx context.Context, spec PodSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, spec)
	return nil
}

func (f *fakePodManager) Delete(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakePodManager) List(ctx context.Context) ([]ExistingPod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ExistingPod, len(f.listResp))
	copy(out, f.listResp)
	return out, nil
}

func (f *fakePodManager) createdSpecs() []PodSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PodSpec, len(f.created))
	copy(out, f.created)
	return out
}

func (f *fakePodManager) createdNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.created))
	for _, s := range f.created {
		out = append(out, s.Name)
	}
	return out
}

func (f *fakePodManager) deletedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleted))
	copy(out, f.deleted)
	return out
}

func (f *fakePodManager) setList(pods []ExistingPod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listResp = pods
}

// fakeClock is a manually-advanced clock for deterministic time control.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// scriptedScaler lets a test control DesiredPods and SelectScaleDown directly.
type scriptedScaler struct {
	desired     func(scaler.TenantScaleState) int
	selectDrain func(scaler.TenantScaleState) []scaler.PodRef
}

func (s *scriptedScaler) DesiredPods(ctx context.Context, st scaler.TenantScaleState) int {
	if s.desired != nil {
		return s.desired(st)
	}
	return 1
}

func (s *scriptedScaler) SelectScaleDown(ctx context.Context, st scaler.TenantScaleState) []scaler.PodRef {
	if s.selectDrain != nil {
		return s.selectDrain(st)
	}
	return nil
}
