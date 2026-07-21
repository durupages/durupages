// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package staticcache

import "sync"

// singleflight collapses concurrent calls sharing a key into a single
// execution, giving every caller the same result. It is a minimal, dependency
// free equivalent of golang.org/x/sync/singleflight sufficient for deduplicating
// concurrent cache writes.
type singleflight struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	wg  sync.WaitGroup
	val string
	err error
}

// Do executes fn for key, ensuring that only one execution is in flight for a
// given key at a time. Duplicate callers wait for the original to finish and
// receive its result.
func (g *singleflight) Do(key string, fn func() (string, error)) (string, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flight)
	}
	if fl, ok := g.m[key]; ok {
		g.mu.Unlock()
		fl.wg.Wait()
		return fl.val, fl.err
	}
	fl := &flight{}
	fl.wg.Add(1)
	g.m[key] = fl
	g.mu.Unlock()

	fl.val, fl.err = fn()
	fl.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return fl.val, fl.err
}
