// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package bundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/storage"
)

// Deploy scans dir and uploads the resulting deployment to st. It is a
// convenience wrapper over Scan + Upload and returns the generated manifest.
func Deploy(ctx context.Context, st storage.Storage, dir string, opts ScanOptions) (*manifest.Manifest, error) {
	s, err := Scan(dir, opts)
	if err != nil {
		return nil, err
	}
	if err := Upload(ctx, st, s); err != nil {
		return nil, err
	}
	return s.Manifest, nil
}

// Upload writes a scanned deployment to Storage under the canonical key layout:
//
//	.../manifest.json                 the manifest (application/json)
//	.../static/{sha256}               each static asset (content-addressed)
//	.../worker.tar                    the worker bundle (only when HasWorker)
//
// Static assets are content-addressed by sha256, so a file that appears under
// several request paths is uploaded once. Before writing a static object Upload
// does a cheap existence check (Get) and skips the upload when the key already
// holds an object, making re-runs of the same deployment idempotent.
func Upload(ctx context.Context, st storage.Storage, s *Scanned) error {
	m := s.Manifest

	// manifest.json
	var mbuf bytes.Buffer
	if err := m.Encode(&mbuf); err != nil {
		return fmt.Errorf("bundle: encode manifest: %w", err)
	}
	mKey := fmt.Sprintf(storage.ManifestKeyFmt, m.TenantID, m.PageID, m.DeploymentID)
	if err := st.Put(ctx, mKey, bytes.NewReader(mbuf.Bytes()), int64(mbuf.Len()), "application/json"); err != nil {
		return fmt.Errorf("bundle: put manifest: %w", err)
	}

	// Static assets, content-addressed and de-duplicated by hash.
	done := make(map[string]bool)
	for _, sf := range s.statics {
		if done[sf.hash] {
			continue
		}
		done[sf.hash] = true
		key := fmt.Sprintf(storage.StaticKeyFmt, m.TenantID, m.PageID, m.DeploymentID, sf.hash)
		if exists, err := objectExists(ctx, st, key); err != nil {
			return err
		} else if exists {
			continue
		}
		if err := putFile(ctx, st, key, sf.absPath, sf.size, sf.ct); err != nil {
			return err
		}
	}

	// worker.tar (only when a worker is present).
	if m.HasWorker {
		var tbuf bytes.Buffer
		if err := WriteWorkerTar(&tbuf, s); err != nil {
			return err
		}
		key := fmt.Sprintf(storage.WorkerBundleKeyFmt, m.TenantID, m.PageID, m.DeploymentID)
		if err := st.Put(ctx, key, bytes.NewReader(tbuf.Bytes()), int64(tbuf.Len()), "application/x-tar"); err != nil {
			return fmt.Errorf("bundle: put worker.tar: %w", err)
		}
	}

	return nil
}

// objectExists reports whether key already holds an object. It uses Get (and
// immediately closes the body) rather than List because content-addressed keys
// are looked up individually; storage.ErrNotFound means absent.
func objectExists(ctx context.Context, st storage.Storage, key string) (bool, error) {
	rc, _, err := st.Get(ctx, key)
	if err == nil {
		_ = rc.Close()
		return true, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("bundle: stat %s: %w", key, err)
}

// putFile streams the file at path into Storage under key.
func putFile(ctx context.Context, st storage.Storage, key, path string, size int64, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("bundle: open %s: %w", path, err)
	}
	defer f.Close()
	if err := st.Put(ctx, key, f, size, contentType); err != nil {
		return fmt.Errorf("bundle: put %s: %w", key, err)
	}
	return nil
}
