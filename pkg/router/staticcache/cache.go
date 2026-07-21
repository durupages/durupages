// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package staticcache is a bounded, on-disk LRU cache for content-addressed
// static files served by the router (docs/ARCHITECTURE.md 5.2, "LRU 캐시 설계").
//
// Files are named by their content hash, so the cache automatically
// deduplicates identical assets across deployments. The in-memory index holds
// (hash -> size, recency); the bytes live on disk under a single directory. On
// startup the directory is scanned to rebuild the index (unreadable entries are
// ignored, and a scan failure simply starts from an empty cache). Concurrent
// Puts of the same hash are collapsed into a single write via singleflight.
//
// A Cache is safe for concurrent use by multiple goroutines.
package staticcache

import (
	"container/list"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// tempPrefix is used for the temporary files written before an atomic rename.
// It intentionally contains a character that never appears in a content hash so
// that leftover temporaries are never mistaken for cache entries during a scan.
const tempPrefix = "tmp-"

// entry is one cached file: its hash, on-disk size and its element in the
// recency list (front = most recently used).
type entry struct {
	hash string
	size int64
	elem *list.Element
}

// Cache is a disk-backed LRU cache keyed by content hash.
type Cache struct {
	dir      string
	maxBytes int64

	mu       sync.Mutex
	entries  map[string]*entry
	ll       *list.List // *entry, front = most recently used
	curBytes int64

	sf singleflight // dedups concurrent Puts of the same hash
}

// New creates (or reopens) a cache rooted at dir with a soft ceiling of
// maxBytes. The directory is created if missing and then scanned to rebuild the
// index; any error during the scan is non-fatal and yields an empty cache.
func New(dir string, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &Cache{
		dir:      dir,
		maxBytes: maxBytes,
		entries:  make(map[string]*entry),
		ll:       list.New(),
	}
	c.scan()
	return c, nil
}

// scan rebuilds the index from the cache directory. Entries are added oldest
// first (by modification time) so the least recently modified files sit at the
// back of the recency list and are evicted first. Any failure is swallowed:
// the cache simply starts with whatever could be read.
func (c *Cache) scan() {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type scanned struct {
		name    string
		size    int64
		modUnix int64
	}
	var files []scanned
	for _, de := range ents {
		if de.IsDir() || !isHashName(de.Name()) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue // unreadable entry: ignore
		}
		files = append(files, scanned{de.Name(), info.Size(), info.ModTime().UnixNano()})
	}
	// Oldest first so the newest end up at the front of the recency list.
	sort.Slice(files, func(i, j int) bool { return files[i].modUnix < files[j].modUnix })
	for _, f := range files {
		c.addLocked(f.name, f.size)
	}
	c.evictLocked("")
}

// Get returns the on-disk path for hash and marks it most recently used. ok is
// false when the hash is not cached.
func (c *Cache) Get(hash string) (path string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[hash]
	if !found {
		return "", false
	}
	c.ll.MoveToFront(e.elem)
	return c.pathFor(hash), true
}

// Put stores the bytes read from r under hash and returns the on-disk path.
// Data is streamed to a temporary file and atomically renamed into place.
// Concurrent Puts of the same hash are merged: only one performs the write and
// all callers receive the same path. If the hash is already cached, r is not
// consumed and the existing path is returned.
func (c *Cache) Put(hash string, r io.Reader) (path string, err error) {
	c.mu.Lock()
	if e, ok := c.entries[hash]; ok {
		c.ll.MoveToFront(e.elem)
		c.mu.Unlock()
		return c.pathFor(hash), nil
	}
	c.mu.Unlock()

	return c.sf.Do(hash, func() (string, error) {
		// Re-check under the flight: a previous flight for the same hash may
		// have completed while we were queued.
		c.mu.Lock()
		if e, ok := c.entries[hash]; ok {
			c.ll.MoveToFront(e.elem)
			c.mu.Unlock()
			return c.pathFor(hash), nil
		}
		c.mu.Unlock()
		return c.putSlow(hash, r)
	})
}

// putSlow performs the actual copy-to-temp-then-rename and index update.
func (c *Cache) putSlow(hash string, r io.Reader) (string, error) {
	tmp, err := os.CreateTemp(c.dir, tempPrefix+"*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	n, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmpName)
		return "", copyErr
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return "", closeErr
	}
	final := c.pathFor(hash)
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", err
	}

	c.mu.Lock()
	c.addLocked(hash, n)
	c.evictLocked(hash)
	c.mu.Unlock()
	return final, nil
}

// addLocked inserts (or refreshes) an index entry at the front. c.mu must be
// held.
func (c *Cache) addLocked(hash string, size int64) {
	if e, ok := c.entries[hash]; ok {
		c.curBytes += size - e.size
		e.size = size
		c.ll.MoveToFront(e.elem)
		return
	}
	e := &entry{hash: hash, size: size}
	e.elem = c.ll.PushFront(e)
	c.entries[hash] = e
	c.curBytes += size
}

// evictLocked removes least-recently-used entries until curBytes fits within
// maxBytes. The entry named protect (the just-written file, at the front) is
// never evicted, so Put always returns a live path even under a tiny budget.
// c.mu must be held.
func (c *Cache) evictLocked(protect string) {
	for c.curBytes > c.maxBytes {
		el := c.ll.Back()
		if el == nil {
			return
		}
		e := el.Value.(*entry)
		if e.hash == protect {
			return
		}
		c.removeLocked(e)
	}
}

// removeLocked drops e from the index and deletes its file (best effort). c.mu
// must be held.
func (c *Cache) removeLocked(e *entry) {
	c.ll.Remove(e.elem)
	delete(c.entries, e.hash)
	c.curBytes -= e.size
	os.Remove(c.pathFor(e.hash))
}

func (c *Cache) pathFor(hash string) string {
	return filepath.Join(c.dir, hash)
}

// isHashName reports whether name looks like a content-hash cache file (a
// non-empty lowercase hex string). This excludes temporary files and any other
// stray directory entries.
func isHashName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}
