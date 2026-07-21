// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"sync"
	"time"

	"github.com/durupages/durupages/pkg/api"
)

// resolveCache is an in-memory host -> PageInfo cache with per-entry expiry.
// Host resolution is on the request hot path, so hits avoid a controller RPC.
type resolveCache struct {
	mu sync.Mutex
	m  map[string]resolveEntry
}

type resolveEntry struct {
	page    *api.PageInfo
	expires time.Time
}

func newResolveCache() *resolveCache {
	return &resolveCache{m: make(map[string]resolveEntry)}
}

// get returns the cached page for host if present and not expired at now.
func (c *resolveCache) get(host string, now time.Time) (*api.PageInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[host]
	if !ok || !now.Before(e.expires) {
		return nil, false
	}
	return e.page, true
}

// put stores page for host until expires.
func (c *resolveCache) put(host string, page *api.PageInfo, expires time.Time) {
	c.mu.Lock()
	c.m[host] = resolveEntry{page: page, expires: expires}
	c.mu.Unlock()
}
