// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package bundle

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// epoch is the fixed modification time stamped on every tar entry, so the
// output is reproducible.
var epoch = time.Unix(0, 0).UTC()

// tarEntry is one member of the worker tar: either a directory or a regular
// file with its content.
type tarEntry struct {
	name string
	dir  bool
	data []byte
}

// WriteWorkerTar writes the worker bundle tar consumed by the worker shim to w.
// Layout inside the tar:
//
//	manifest.json      the deployment manifest
//	worker/...         the worker module files (a _worker.js file becomes
//	                   worker/index.js; a _worker.js/ directory's contents are
//	                   copied under worker/ preserving relative paths)
//	static/{sha256}    every static asset content-addressed by its hash —
//	                   the same key the manifest's Static entries reference,
//	                   which is what the shim's env.ASSETS service opens.
//	                   Duplicate content is stored once.
//
// The output is deterministic: entries are sorted by path and given a fixed
// epoch mtime, mode 0644 for files and 0755 for directories.
func WriteWorkerTar(w io.Writer, s *Scanned) error {
	var buf bytes.Buffer
	if err := s.Manifest.Encode(&buf); err != nil {
		return fmt.Errorf("bundle: encode manifest: %w", err)
	}

	files := []tarEntry{{name: "manifest.json", data: buf.Bytes()}}
	for _, wm := range s.workers {
		data, err := os.ReadFile(wm.absPath)
		if err != nil {
			return fmt.Errorf("bundle: read worker module: %w", err)
		}
		files = append(files, tarEntry{name: "worker/" + wm.tarRel, data: data})
	}
	seenHash := make(map[string]bool)
	for _, st := range s.statics {
		if seenHash[st.hash] {
			continue
		}
		seenHash[st.hash] = true
		data, err := os.ReadFile(st.absPath)
		if err != nil {
			return fmt.Errorf("bundle: read static asset: %w", err)
		}
		files = append(files, tarEntry{name: "static/" + st.hash, data: data})
	}

	entries := withDirs(files)
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:    e.name,
			ModTime: epoch,
			Format:  tar.FormatUSTAR,
		}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Name = e.name + "/"
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0o644
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("bundle: write tar header %q: %w", e.name, err)
		}
		if !e.dir {
			if _, err := tw.Write(e.data); err != nil {
				return fmt.Errorf("bundle: write tar body %q: %w", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("bundle: close tar: %w", err)
	}
	return nil
}

// withDirs returns files plus a directory entry for every unique parent
// directory of a file, so the archive is self-contained.
func withDirs(files []tarEntry) []tarEntry {
	seen := make(map[string]bool)
	out := make([]tarEntry, 0, len(files))
	out = append(out, files...)
	for _, f := range files {
		parts := strings.Split(f.name, "/")
		for i := 1; i < len(parts); i++ {
			d := strings.Join(parts[:i], "/")
			if !seen[d] {
				seen[d] = true
				out = append(out, tarEntry{name: d, dir: true})
			}
		}
	}
	return out
}
