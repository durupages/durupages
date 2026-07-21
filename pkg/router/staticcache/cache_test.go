// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package staticcache

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestPutGetRoundTrip(t *testing.T) {
	c, err := New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Put("aa", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "hello" {
		t.Fatalf("content = %q, want hello", got)
	}
	gp, ok := c.Get("aa")
	if !ok {
		t.Fatal("Get miss after Put")
	}
	if gp != p {
		t.Fatalf("Get path %q != Put path %q", gp, p)
	}
	if _, ok := c.Get("bb"); ok {
		t.Fatal("Get hit for absent hash")
	}
}

func TestPutAlreadyPresentDoesNotConsume(t *testing.T) {
	c, _ := New(t.TempDir(), 1<<20)
	if _, err := c.Put("aa", strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	// Second Put of the same hash must not overwrite and must not read r.
	r := &trackingReader{data: []byte("second")}
	p, err := c.Put("aa", r)
	if err != nil {
		t.Fatal(err)
	}
	if r.reads.Load() != 0 {
		t.Fatalf("reader was consumed on already-present Put")
	}
	if got := read(t, p); got != "first" {
		t.Fatalf("content = %q, want first (unchanged)", got)
	}
}

func TestLRUEvictionOrder(t *testing.T) {
	// Budget holds exactly two 4-byte entries.
	c, _ := New(t.TempDir(), 8)
	mustPut(t, c, "a1", "aaaa")
	mustPut(t, c, "b2", "bbbb")
	// Touch a1 so b2 becomes least recently used.
	if _, ok := c.Get("a1"); !ok {
		t.Fatal("a1 should be present")
	}
	mustPut(t, c, "c3", "cccc") // triggers eviction of b2
	if _, ok := c.Get("b2"); ok {
		t.Fatal("b2 should have been evicted (LRU)")
	}
	if _, ok := c.Get("a1"); !ok {
		t.Fatal("a1 should survive (recently used)")
	}
	if _, ok := c.Get("c3"); !ok {
		t.Fatal("c3 should be present")
	}
	// Evicted file must be gone from disk too.
	if _, err := os.Stat(c.pathFor("b2")); !os.IsNotExist(err) {
		t.Fatalf("b2 file still on disk: %v", err)
	}
}

func TestPutLargerThanBudgetIsKept(t *testing.T) {
	c, _ := New(t.TempDir(), 4)
	// A single entry larger than the budget must not evict itself.
	p, err := c.Put("aa", strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "0123456789" {
		t.Fatalf("content = %q", got)
	}
	if _, ok := c.Get("aa"); !ok {
		t.Fatal("oversized entry should still be retrievable")
	}
}

func TestConcurrentPutSameHash(t *testing.T) {
	c, _ := New(t.TempDir(), 1<<20)
	const n = 50
	var wg sync.WaitGroup
	var writes atomic.Int64
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := &countingReader{r: strings.NewReader("payload"), writes: &writes}
			p, err := c.Put("dup", r)
			if err != nil {
				t.Errorf("put: %v", err)
				return
			}
			paths[i] = p
		}(i)
	}
	wg.Wait()
	// All callers see the same path and content is intact.
	for i := 0; i < n; i++ {
		if paths[i] != paths[0] {
			t.Fatalf("path %d = %q != %q", i, paths[i], paths[0])
		}
	}
	if got := read(t, paths[0]); got != "payload" {
		t.Fatalf("content = %q", got)
	}
	// singleflight must collapse to at most one actual read of a fresh reader.
	if writes.Load() > 1 {
		t.Fatalf("readers consumed %d times, want <= 1 (singleflight)", writes.Load())
	}
}

func TestScanRebuildsIndex(t *testing.T) {
	dir := t.TempDir()
	c1, _ := New(dir, 1<<20)
	mustPut(t, c1, "aa", "one")
	mustPut(t, c1, "bb", "two")

	// Reopen: the index must be rebuilt from disk.
	c2, err := New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c2.Get("aa"); !ok || read(t, p) != "one" {
		t.Fatalf("aa not rebuilt")
	}
	if p, ok := c2.Get("bb"); !ok || read(t, p) != "two" {
		t.Fatalf("bb not rebuilt")
	}
	if c2.curBytes != 6 {
		t.Fatalf("curBytes = %d, want 6", c2.curBytes)
	}
}

func TestScanEvictsBeyondBudget(t *testing.T) {
	dir := t.TempDir()
	c1, _ := New(dir, 1<<20)
	for i := 0; i < 5; i++ {
		mustPut(t, c1, fmt.Sprintf("%02x", i), "xxxx") // 4 bytes each
	}
	// Reopen with a budget that fits only two entries; scan must evict down.
	c2, _ := New(dir, 8)
	if c2.curBytes > 8 {
		t.Fatalf("curBytes after scan = %d, want <= 8", c2.curBytes)
	}
}

func TestScanIgnoresTempAndBadNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/"+tempPrefix+"leftover", []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/NotAHash.txt", []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := New(dir, 1<<20)
	if c.curBytes != 0 {
		t.Fatalf("curBytes = %d, want 0 (non-hash files ignored)", c.curBytes)
	}
}

func mustPut(t *testing.T, c *Cache, hash, data string) {
	t.Helper()
	if _, err := c.Put(hash, strings.NewReader(data)); err != nil {
		t.Fatalf("put %s: %v", hash, err)
	}
}

type trackingReader struct {
	data  []byte
	off   int
	reads atomic.Int64
}

func (r *trackingReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// countingReader increments writes on the first Read of a fresh reader so tests
// can assert how many distinct readers were actually consumed.
type countingReader struct {
	r      io.Reader
	writes *atomic.Int64
	once   sync.Once
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { r.writes.Add(1) })
	return r.r.Read(p)
}
