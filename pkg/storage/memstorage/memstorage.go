// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package memstorage provides a thread-safe, in-memory implementation of the
// storage.Storage interface. It is intended for tests and local development;
// all objects are held in a map guarded by an RWMutex and are lost when the
// process exits.
package memstorage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"sync"

	"github.com/durupages/durupages/pkg/storage"
)

// object is a single stored blob together with its computed metadata.
type object struct {
	data        []byte
	etag        string
	contentType string
}

// Store is an in-memory storage.Storage. The zero value is not usable; call
// New. A Store is safe for concurrent use by multiple goroutines.
type Store struct {
	mu      sync.RWMutex
	objects map[string]object
}

// New returns an empty in-memory Store.
func New() *Store {
	return &Store{objects: make(map[string]object)}
}

// compile-time check that *Store implements storage.Storage.
var _ storage.Storage = (*Store)(nil)

// Put stores r at key, overwriting any existing object. size is advisory and
// may be -1; the whole reader is buffered in memory regardless.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	obj := object{
		data:        data,
		etag:        hex.EncodeToString(sum[:]),
		contentType: contentType,
	}
	s.mu.Lock()
	s.objects[key] = obj
	s.mu.Unlock()
	return nil
}

// Get returns a reader for the object at key. It returns storage.ErrNotFound
// if the key does not exist. The returned reader never needs to touch the
// underlying store again, so concurrent mutations are safe.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.RLock()
	obj, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	info := storage.ObjectInfo{
		Key:         key,
		Size:        int64(len(obj.data)),
		ETag:        obj.etag,
		ContentType: obj.contentType,
	}
	return io.NopCloser(bytes.NewReader(obj.data)), info, nil
}

// Delete removes key. Deleting a missing key is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

// List returns the metadata of every object whose key starts with prefix,
// sorted ascending by key. An empty prefix matches all objects. The result is
// a non-nil (possibly empty) slice.
func (s *Store) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	s.mu.RLock()
	infos := make([]storage.ObjectInfo, 0, len(s.objects))
	for key, obj := range s.objects {
		if !hasPrefix(key, prefix) {
			continue
		}
		infos = append(infos, storage.ObjectInfo{
			Key:         key,
			Size:        int64(len(obj.data)),
			ETag:        obj.etag,
			ContentType: obj.contentType,
		})
	}
	s.mu.RUnlock()
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Key < infos[j].Key
	})
	return infos, nil
}

// hasPrefix reports whether key begins with prefix. An empty prefix matches
// everything.
func hasPrefix(key, prefix string) bool {
	return len(key) >= len(prefix) && key[:len(prefix)] == prefix
}
