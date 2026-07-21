// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"container/list"
	"sync"

	"github.com/durupages/durupages/pkg/manifest"
)

// manifestCache is a small LRU cache of parsed manifests keyed by deployment
// ID. Deployments are immutable, so cached entries never need invalidation; the
// LRU bound only caps memory when many deployments are in play.
type manifestCache struct {
	mu      sync.Mutex
	max     int
	ll      *list.List // *manifestEntry, front = most recently used
	entries map[string]*list.Element
}

type manifestEntry struct {
	deploymentID string
	m            *manifest.Manifest
}

func newManifestCache(max int) *manifestCache {
	if max <= 0 {
		max = 1
	}
	return &manifestCache{
		max:     max,
		ll:      list.New(),
		entries: make(map[string]*list.Element),
	}
}

func (c *manifestCache) get(deploymentID string) (*manifest.Manifest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[deploymentID]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*manifestEntry).m, true
}

func (c *manifestCache) add(deploymentID string, m *manifest.Manifest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[deploymentID]; ok {
		el.Value.(*manifestEntry).m = m
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&manifestEntry{deploymentID: deploymentID, m: m})
	c.entries[deploymentID] = el
	for c.ll.Len() > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.ll.Remove(back)
		delete(c.entries, back.Value.(*manifestEntry).deploymentID)
	}
}
