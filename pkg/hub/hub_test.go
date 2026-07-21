// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"testing"

	"github.com/durupages/durupages/pkg/storage/memstorage"
	"github.com/durupages/durupages/pkg/workerauth"
)

func TestNewValidation(t *testing.T) {
	pub, _, _ := workerauth.GenerateKey()

	if _, err := New(Options{JWTPublicKey: pub}); err == nil {
		t.Error("expected error when Storage is nil")
	}
	if _, err := New(Options{Storage: memstorage.New()}); err == nil {
		t.Error("expected error when JWTPublicKey is missing")
	}

	h, err := New(Options{Storage: memstorage.New(), JWTPublicKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	if h.maxLogsPerRequest != DefaultMaxLogsPerRequest {
		t.Errorf("maxLogsPerRequest = %d, want default %d", h.maxLogsPerRequest, DefaultMaxLogsPerRequest)
	}
	if h.maxLogBytesPerRequest != DefaultMaxLogBytesPerRequest {
		t.Errorf("maxLogBytesPerRequest = %d, want default", h.maxLogBytesPerRequest)
	}
	if h.cache != nil {
		t.Error("cache should be nil when CacheDir is empty")
	}
}

func TestNewDefaultsCacheMaxBytes(t *testing.T) {
	pub, _, _ := workerauth.GenerateKey()
	h, err := New(Options{
		Storage:      memstorage.New(),
		JWTPublicKey: pub,
		CacheDir:     t.TempDir(),
		// CacheMaxBytes left 0 -> DefaultCacheMaxBytes.
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.cache == nil {
		t.Fatal("cache should be created when CacheDir is set")
	}
	if h.cache.maxBytes != DefaultCacheMaxBytes {
		t.Errorf("cache maxBytes = %d, want %d", h.cache.maxBytes, DefaultCacheMaxBytes)
	}
}
