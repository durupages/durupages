// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// diskCache is a size-bounded LRU cache of deployment files on local disk.
//
// Deployment bundles are immutable (a deployment ID names a fixed set of
// bytes), so cache entries never need invalidation — only eviction to stay
// within the byte budget. Concurrent misses for the same key are collapsed
// with a small built-in singleflight so a bundle is fetched from Storage at
// most once even under a thundering herd.
type diskCache struct {
	dir      string
	maxBytes int64

	mu       sync.Mutex
	entries  map[string]*cacheEntry
	lru      *list.List // *cacheEntry, front = most recently used
	curBytes int64
	inflight map[string]*loadCall
}

type cacheEntry struct {
	key  string
	size int64
	elem *list.Element
}

// loadCall is a single in-flight fetch shared by all goroutines waiting on the
// same key (singleflight).
type loadCall struct {
	wg   sync.WaitGroup
	path string
	err  error
}

// newDiskCache creates the cache directory and returns a ready cache.
func newDiskCache(dir string, maxBytes int64) (*diskCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &diskCache{
		dir:      dir,
		maxBytes: maxBytes,
		entries:  make(map[string]*cacheEntry),
		lru:      list.New(),
		inflight: make(map[string]*loadCall),
	}, nil
}

// pathFor returns the on-disk path for key. The key is hashed so arbitrary key
// bytes map to a safe, fixed-length filename.
func (c *diskCache) pathFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:]))
}

// getOrLoad returns the on-disk path of the cached file for key, fetching it
// via load (which must write the full contents to the provided writer) on a
// miss. load is invoked at most once per key across concurrent callers; its
// error (e.g. storage.ErrNotFound) is propagated to every waiter.
func (c *diskCache) getOrLoad(key string, load func(w io.Writer) error) (string, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		c.lru.MoveToFront(e.elem)
		c.mu.Unlock()
		return c.pathFor(key), nil
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		call.wg.Wait()
		return call.path, call.err
	}
	call := &loadCall{}
	call.wg.Add(1)
	c.inflight[key] = call
	c.mu.Unlock()

	path, size, err := c.fetch(key, load)

	c.mu.Lock()
	delete(c.inflight, key)
	if err == nil {
		c.insertLocked(key, size)
	}
	call.path = path
	call.err = err
	c.mu.Unlock()
	call.wg.Done()

	return path, err
}

// fetch writes the loaded contents to a temp file and atomically renames it
// into place, so a reader never observes a partially written cache file.
func (c *diskCache) fetch(key string, load func(w io.Writer) error) (string, int64, error) {
	tmp, err := os.CreateTemp(c.dir, "load-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := load(tmp); err != nil {
		cleanup()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	info, err := os.Stat(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	path := c.pathFor(key)
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	return path, info.Size(), nil
}

// insertLocked records a freshly fetched entry and evicts LRU victims until the
// cache is within budget. c.mu must be held.
func (c *diskCache) insertLocked(key string, size int64) {
	// A concurrent loader may have already inserted this key; if so, drop the
	// duplicate accounting (the file content is identical).
	if _, ok := c.entries[key]; ok {
		return
	}
	e := &cacheEntry{key: key, size: size}
	e.elem = c.lru.PushFront(e)
	c.entries[key] = e
	c.curBytes += size
	c.evictLocked()
}

// evictLocked removes least-recently-used entries until within budget. c.mu
// must be held.
func (c *diskCache) evictLocked() {
	for c.curBytes > c.maxBytes {
		back := c.lru.Back()
		if back == nil {
			return
		}
		e := back.Value.(*cacheEntry)
		c.lru.Remove(back)
		delete(c.entries, e.key)
		c.curBytes -= e.size
		// Unlink the file; any handler currently serving it keeps its open fd
		// valid on POSIX until it closes.
		os.Remove(c.pathFor(e.key))
	}
}
