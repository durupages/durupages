// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package storagetest provides a reusable conformance suite that any
// storage.Storage implementation can run against to verify it satisfies the
// contract expected by the rest of DuruPages. Point it at your implementation
// with RunConformance.
package storagetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/durupages/durupages/pkg/storage"
)

// RunConformance runs the full conformance suite against the Storage returned
// by factory. factory is called once per subtest so that implementations may
// return a fresh, isolated store each time (or reuse one that is cleaned up via
// t.Cleanup). Run the suite with -race to exercise the concurrency subtest.
func RunConformance(t *testing.T, factory func(t *testing.T) storage.Storage) {
	t.Helper()

	t.Run("PutGetRoundtrip", func(t *testing.T) { testPutGetRoundtrip(t, factory(t)) })
	t.Run("Overwrite", func(t *testing.T) { testOverwrite(t, factory(t)) })
	t.Run("GetMissing", func(t *testing.T) { testGetMissing(t, factory(t)) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, factory(t)) })
	t.Run("ListByPrefix", func(t *testing.T) { testListByPrefix(t, factory(t)) })
	t.Run("LayoutKeys", func(t *testing.T) { testLayoutKeys(t, factory(t)) })
	t.Run("LargeObject", func(t *testing.T) { testLargeObject(t, factory(t)) })
	t.Run("Concurrent", func(t *testing.T) { testConcurrent(t, factory(t)) })
}

// mustPut stores content and fails the test on error.
func mustPut(t *testing.T, s storage.Storage, key, contentType string, content []byte) {
	t.Helper()
	if err := s.Put(context.Background(), key, bytes.NewReader(content), int64(len(content)), contentType); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

// readObject fetches key and returns its content and info, failing on error.
func readObject(t *testing.T, s storage.Storage, key string) ([]byte, storage.ObjectInfo) {
	t.Helper()
	rc, info, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body of %q: %v", key, err)
	}
	return data, info
}

func testPutGetRoundtrip(t *testing.T, s storage.Storage) {
	key := "tenants/t1/pages/p1/hello.txt"
	content := []byte("hello, durupages")
	contentType := "text/plain"
	mustPut(t, s, key, contentType, content)

	data, info := readObject(t, s, key)
	if !bytes.Equal(data, content) {
		t.Fatalf("content mismatch: got %q want %q", data, content)
	}
	if info.Key != key {
		t.Errorf("info.Key = %q want %q", info.Key, key)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("info.Size = %d want %d", info.Size, len(content))
	}
	if info.ContentType != contentType {
		t.Errorf("info.ContentType = %q want %q", info.ContentType, contentType)
	}
	if info.ETag == "" {
		t.Error("info.ETag is empty")
	}

	// ETag must be stable across repeated Gets of the same content.
	_, info2 := readObject(t, s, key)
	if info.ETag != info2.ETag {
		t.Errorf("ETag not stable: %q != %q", info.ETag, info2.ETag)
	}
}

func testOverwrite(t *testing.T, s storage.Storage) {
	key := "tenants/t1/pages/p1/data.bin"
	mustPut(t, s, key, "application/octet-stream", []byte("first"))
	_, first := readObject(t, s, key)

	mustPut(t, s, key, "text/plain", []byte("second-longer"))
	data, second := readObject(t, s, key)

	if string(data) != "second-longer" {
		t.Fatalf("overwrite content = %q want %q", data, "second-longer")
	}
	if second.Size != int64(len("second-longer")) {
		t.Errorf("overwrite Size = %d want %d", second.Size, len("second-longer"))
	}
	if second.ContentType != "text/plain" {
		t.Errorf("overwrite ContentType = %q want %q", second.ContentType, "text/plain")
	}
	if first.ETag == second.ETag {
		t.Errorf("ETag unchanged after overwrite with different content: %q", first.ETag)
	}
}

func testGetMissing(t *testing.T, s storage.Storage) {
	rc, _, err := s.Get(context.Background(), "tenants/t1/pages/p1/does-not-exist")
	if rc != nil {
		rc.Close()
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get missing: err = %v, want storage.ErrNotFound", err)
	}
}

func testDelete(t *testing.T, s storage.Storage) {
	key := "tenants/t1/pages/p1/tmp"
	mustPut(t, s, key, "text/plain", []byte("bye"))
	if err := s.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete existing: %v", err)
	}
	if _, _, err := s.Get(context.Background(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get after Delete: err = %v, want storage.ErrNotFound", err)
	}

	// Deleting a missing key is idempotent: it must not panic and must not
	// resurrect the object.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Delete of missing key panicked: %v", r)
			}
		}()
		if err := s.Delete(context.Background(), key); err != nil {
			t.Fatalf("idempotent Delete: %v", err)
		}
	}()
	if _, _, err := s.Get(context.Background(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get after idempotent Delete: err = %v, want storage.ErrNotFound", err)
	}
}

func testListByPrefix(t *testing.T, s storage.Storage) {
	// Insert in a deliberately unsorted order.
	keys := []string{
		"tenants/t1/pages/p1/b",
		"tenants/t1/pages/p1/a",
		"tenants/t1/pages/p2/a",
		"tenants/t2/pages/p1/a",
	}
	for _, k := range keys {
		mustPut(t, s, k, "text/plain", []byte(k))
	}

	// Prefix match with ordering.
	got := listKeys(t, s, "tenants/t1/pages/p1/")
	want := []string{"tenants/t1/pages/p1/a", "tenants/t1/pages/p1/b"}
	assertKeys(t, got, want)

	// Empty prefix returns everything, sorted.
	all := listKeys(t, s, "")
	assertKeys(t, all, []string{
		"tenants/t1/pages/p1/a",
		"tenants/t1/pages/p1/b",
		"tenants/t1/pages/p2/a",
		"tenants/t2/pages/p1/a",
	})

	// No matches returns an empty (non-nil) slice.
	none, err := s.List(context.Background(), "tenants/nope/")
	if err != nil {
		t.Fatalf("List no-match: %v", err)
	}
	if none == nil {
		t.Error("List returned nil slice for no matches, want empty non-nil slice")
	}
	if len(none) != 0 {
		t.Errorf("List no-match returned %d entries, want 0", len(none))
	}
}

func testLayoutKeys(t *testing.T, s storage.Storage) {
	tenant, page, deployment, sha := "t1", "p1", "d1", "deadbeef"
	manifest := fmt.Sprintf(storage.ManifestKeyFmt, tenant, page, deployment)
	static := fmt.Sprintf(storage.StaticKeyFmt, tenant, page, deployment, sha)
	bundle := fmt.Sprintf(storage.WorkerBundleKeyFmt, tenant, page, deployment)

	mustPut(t, s, manifest, "application/json", []byte(`{"v":1}`))
	mustPut(t, s, static, "text/html", []byte("<html></html>"))
	mustPut(t, s, bundle, "application/x-tar", []byte("tarbytes"))

	// Each slash-bearing key roundtrips independently.
	for _, k := range []string{manifest, static, bundle} {
		if _, info := readObject(t, s, k); info.Key != k {
			t.Errorf("layout key roundtrip: info.Key = %q want %q", info.Key, k)
		}
	}

	// Deployment-scoped prefix lists exactly the three keys, in lexicographic
	// order:
	//   .../deployments/d1/manifest.json
	//   .../deployments/d1/static/deadbeef
	//   .../deployments/d1/worker.tar
	prefix := fmt.Sprintf("tenants/%s/pages/%s/deployments/%s/", tenant, page, deployment)
	got := listKeys(t, s, prefix)
	assertKeys(t, got, []string{manifest, static, bundle})
}

func testLargeObject(t *testing.T, s storage.Storage) {
	const size = 5 << 20 // 5 MiB
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand: %v", err)
	}
	key := "tenants/t1/pages/p1/deployments/d1/static/large"
	mustPut(t, s, key, "application/octet-stream", content)

	rc, info, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get large: %v", err)
	}
	defer rc.Close()
	if info.Size != size {
		t.Errorf("large Size = %d want %d", info.Size, size)
	}
	// Stream and hash-compare without holding a second full copy longer than
	// necessary.
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read large: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("large object content mismatch (len got=%d want=%d)", len(got), size)
	}
}

func testConcurrent(t *testing.T, s storage.Storage) {
	const workers = 16
	const perWorker = 25
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := fmt.Sprintf("tenants/t1/pages/p1/deployments/d1/static/w%d-i%d", w, i)
				content := []byte(key)
				if err := s.Put(context.Background(), key, bytes.NewReader(content), int64(len(content)), "text/plain"); err != nil {
					t.Errorf("concurrent Put(%q): %v", key, err)
					return
				}
				rc, _, err := s.Get(context.Background(), key)
				if err != nil {
					t.Errorf("concurrent Get(%q): %v", key, err)
					return
				}
				got, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Errorf("concurrent read(%q): %v", key, err)
					return
				}
				if !bytes.Equal(got, content) {
					t.Errorf("concurrent content mismatch for %q", key)
					return
				}
			}
			// Concurrent List alongside writers must not race or panic.
			if _, err := s.List(context.Background(), "tenants/t1/"); err != nil {
				t.Errorf("concurrent List: %v", err)
			}
		}(w)
	}
	wg.Wait()
}

// listKeys returns the keys of List(prefix) in the order returned.
func listKeys(t *testing.T, s storage.Storage, prefix string) []string {
	t.Helper()
	infos, err := s.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%q): %v", prefix, err)
	}
	keys := make([]string, len(infos))
	for i, info := range infos {
		keys[i] = info.Key
	}
	return keys
}

// assertKeys fails unless got equals want element-for-element (order matters).
func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("key count = %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys not equal/ordered:\n got  %v\n want %v", got, want)
		}
	}
}
