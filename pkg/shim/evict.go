// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"context"
	"os"
	"sort"
	"time"
)

// runSweep runs the periodic LRU eviction sweep.
func (s *Shim) runSweep(ctx context.Context) {
	ticker := time.NewTicker(s.sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep evicts deployments per the LRU + min-idle policy (docs 6.3):
//
//   - Superseded deployments (not in the active load set) that have been idle
//     for at least MinIdle are deleted immediately — no swap is needed since
//     they are not serving.
//   - When total bundle bytes exceed CacheMaxBytes, active deployments that are
//     idle for at least MinIdle are additionally evicted oldest-first until the
//     budget is met; because that shrinks the load set it triggers ONE swap.
//
// A deployment idle for less than MinIdle is never evicted, even under disk
// pressure.
func (s *Shim) sweep(ctx context.Context) {
	now := s.now()

	s.mu.Lock()
	// 1. Delete superseded, sufficiently-idle deployments (no swap).
	var deleted []*deployment
	for id, d := range s.deployments {
		if s.active[d.pageID] == id {
			continue // still the active deployment for its page
		}
		if now.Sub(d.lastUsed) >= s.minIdle {
			delete(s.deployments, id)
			deleted = append(deleted, d)
		}
	}

	// 2. Over budget: evict active idle deployments LRU-first.
	needSwap := false
	total := s.totalBytesLocked()
	if total > s.cacheMax {
		cands := make([]*deployment, 0, len(s.active))
		for _, id := range s.active {
			if d := s.deployments[id]; d != nil && now.Sub(d.lastUsed) >= s.minIdle {
				cands = append(cands, d)
			}
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].lastUsed.Before(cands[j].lastUsed) })
		for _, d := range cands {
			if total <= s.cacheMax {
				break
			}
			delete(s.active, d.pageID)
			delete(s.deployments, d.deploymentID)
			deleted = append(deleted, d)
			total -= d.sizeBytes
			needSwap = true
		}
	}
	s.mu.Unlock()

	for _, d := range deleted {
		_ = os.RemoveAll(d.dir)
	}
	if needSwap {
		// Relaunch with the shrunk load set. The mutate function keeps whatever
		// the load set is now (the active map was already trimmed above).
		_ = s.swap(ctx, func(cur map[string]string) map[string]string { return cur })
	}
}

// totalBytesLocked sums the on-disk size of all loaded deployments. Callers
// hold s.mu.
func (s *Shim) totalBytesLocked() int64 {
	var total int64
	for _, d := range s.deployments {
		total += d.sizeBytes
	}
	return total
}
