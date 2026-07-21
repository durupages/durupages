// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package bundle turns a wrangler build output directory into a deployment:
// it scans and validates the directory, generates the derived manifest,
// packages the worker module bundle as a tar, and uploads everything to
// Storage under the canonical key layout (see docs/ARCHITECTURE.md 2.2, 2.3).
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/pagesspec"
)

// Special file and directory names that Cloudflare Pages (and DuruPages) treat
// as configuration rather than static assets. They are recognised only at the
// root of the build output directory.
const (
	fileHeaders   = "_headers"
	fileRedirects = "_redirects"
	fileRoutes    = "_routes.json"
	nameWorker    = "_worker.js" // file (single module) or directory (multi module)
	dirFunctions  = "functions"
	workerEntry   = "index.js" // required entry of a _worker.js/ directory
)

// init registers content types for the extensions the platform must map
// deterministically, independent of the host's mime database.
func init() {
	for ext, ct := range map[string]string{
		".html": "text/html; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".mjs":  "text/javascript; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".json": "application/json",
		".wasm": "application/wasm",
		".svg":  "image/svg+xml",
	} {
		_ = mime.AddExtensionType(ext, ct)
	}
}

// ScanOptions carries the deployment identity stamped into the manifest.
type ScanOptions struct {
	TenantID     string
	PageID       string
	DeploymentID string
}

// staticFile is a single static asset discovered during the scan.
type staticFile struct {
	// reqPath is the request path, "/"-prefixed with forward slashes.
	reqPath string
	// absPath is the file's location on disk.
	absPath string
	hash    string
	size    int64
	ct      string
}

// workerModule is a single file of the worker bundle.
type workerModule struct {
	// tarRel is the path under "worker/" inside the tar ("index.js",
	// "chunks/a.js", ...).
	tarRel string
	// absPath is the file's location on disk.
	absPath string
}

// Scanned is the result of scanning a build output directory: the built
// manifest together with the file lists needed to package and upload the
// deployment.
type Scanned struct {
	// Manifest is the derived deployment metadata.
	Manifest *manifest.Manifest

	statics []staticFile
	workers []workerModule // empty when there is no worker
}

// Scan walks a wrangler build output directory and produces a Scanned
// deployment: the static asset inventory, the parsed special files, the worker
// module list and the compatibility settings, all assembled into a manifest.
// Parse errors in special files abort the scan with an error naming the file.
func Scan(dir string, opts ScanOptions) (*Scanned, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("bundle: scan %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle: scan %q: not a directory", dir)
	}

	workers, hasWorker, err := scanWorker(dir)
	if err != nil {
		return nil, err
	}

	statics, err := scanStatics(dir)
	if err != nil {
		return nil, err
	}

	routes, redirects, headers, err := parseSpecials(dir)
	if err != nil {
		return nil, err
	}

	compat, err := parseCompat(dir)
	if err != nil {
		return nil, err
	}

	m := &manifest.Manifest{
		Version:      manifest.Version,
		TenantID:     opts.TenantID,
		PageID:       opts.PageID,
		DeploymentID: opts.DeploymentID,
		HasWorker:    hasWorker,
		Static:       make(map[string]manifest.StaticEntry, len(statics)),
		Routes:       routes,
		Redirects:    redirects,
		Headers:      headers,
		Compat:       compat,
	}
	for _, s := range statics {
		m.Static[s.reqPath] = manifest.StaticEntry{
			Hash:        s.hash,
			Size:        s.size,
			ContentType: s.ct,
		}
	}

	return &Scanned{Manifest: m, statics: statics, workers: workers}, nil
}

// scanWorker detects the worker module(s). A `_worker.js` file yields a single
// module (packaged as worker/index.js); a `_worker.js/` directory yields a
// multi-module worker whose entry index.js must exist. When neither is present
// but a functions/ directory is, it returns a descriptive error: the platform
// only accepts compiled workers.
func scanWorker(dir string) ([]workerModule, bool, error) {
	workerPath := filepath.Join(dir, nameWorker)
	wInfo, err := os.Stat(workerPath)
	switch {
	case err == nil && !wInfo.IsDir():
		// Single-file worker.
		return []workerModule{{tarRel: workerEntry, absPath: workerPath}}, true, nil
	case err == nil && wInfo.IsDir():
		// Multi-module worker: entry index.js is required.
		if e, err := os.Stat(filepath.Join(workerPath, workerEntry)); err != nil || e.IsDir() {
			return nil, false, fmt.Errorf("bundle: %s/ is missing its %s entry", nameWorker, workerEntry)
		}
		var mods []workerModule
		walkErr := filepath.WalkDir(workerPath, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(workerPath, p)
			if err != nil {
				return err
			}
			mods = append(mods, workerModule{tarRel: filepath.ToSlash(rel), absPath: p})
			return nil
		})
		if walkErr != nil {
			return nil, false, fmt.Errorf("bundle: scan %s/: %w", nameWorker, walkErr)
		}
		return mods, true, nil
	case errors.Is(err, fs.ErrNotExist):
		// No worker. If functions/ is present, the caller must precompile.
		fInfo, ferr := os.Stat(filepath.Join(dir, dirFunctions))
		if ferr == nil && fInfo.IsDir() {
			return nil, false, fmt.Errorf(
				"bundle: %s/ directory found but no compiled worker; run "+
					"`wrangler pages functions build` to produce a %s bundle first "+
					"(the platform only accepts compiled workers)", dirFunctions, nameWorker)
		}
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("bundle: stat %s: %w", nameWorker, err)
	}
}

// scanStatics walks dir and records every file that is not a root-level special
// file, the worker, or the functions/ directory, as a static asset.
func scanStatics(dir string) ([]staticFile, error) {
	var statics []staticFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		top := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			top = rel[:i]
		}
		if d.IsDir() {
			// The worker directory and functions/ are not static assets.
			if top == nameWorker || top == dirFunctions {
				return fs.SkipDir
			}
			return nil
		}
		if isRootSpecialFile(rel) {
			return nil
		}
		// A single-file worker at the root is not a static asset.
		if rel == nameWorker {
			return nil
		}
		hash, size, err := hashFile(p)
		if err != nil {
			return err
		}
		statics = append(statics, staticFile{
			reqPath: "/" + rel,
			absPath: p,
			hash:    hash,
			size:    size,
			ct:      contentType(rel),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bundle: scan static assets: %w", err)
	}
	sort.Slice(statics, func(i, j int) bool { return statics[i].reqPath < statics[j].reqPath })
	return statics, nil
}

// isRootSpecialFile reports whether rel names a root-level special/config file
// that must not be served as a static asset. Wrangler config files are treated
// as configuration and excluded as well, matching Cloudflare (which never
// deploys wrangler.toml as an asset).
func isRootSpecialFile(rel string) bool {
	if strings.ContainsRune(rel, '/') {
		return false // special files are only recognised at the root
	}
	switch rel {
	case fileHeaders, fileRedirects, fileRoutes,
		"wrangler.toml", "wrangler.json", "wrangler.jsonc":
		return true
	}
	return false
}

// parseSpecials parses the optional _routes.json, _redirects and _headers
// files with pkg/pagesspec. A missing file is not an error; a parse error is,
// wrapped with the file name.
func parseSpecials(dir string) (*manifest.Routes, []manifest.Redirect, []manifest.HeaderRule, error) {
	var routes *manifest.Routes
	if data, ok, err := readOptional(filepath.Join(dir, fileRoutes)); err != nil {
		return nil, nil, nil, err
	} else if ok {
		routes, err = pagesspec.ParseRoutes(data)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bundle: %w", err)
		}
	}

	var redirects []manifest.Redirect
	if data, ok, err := readOptional(filepath.Join(dir, fileRedirects)); err != nil {
		return nil, nil, nil, err
	} else if ok {
		redirects, err = pagesspec.ParseRedirects(data)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bundle: %w", err)
		}
	}

	var headers []manifest.HeaderRule
	if data, ok, err := readOptional(filepath.Join(dir, fileHeaders)); err != nil {
		return nil, nil, nil, err
	} else if ok {
		headers, err = pagesspec.ParseHeaders(data)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bundle: %w", err)
		}
	}

	return routes, redirects, headers, nil
}

// readOptional reads path, returning ok=false when it does not exist.
func readOptional(path string) (data []byte, ok bool, err error) {
	data, err = os.ReadFile(path)
	switch {
	case err == nil:
		return data, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("bundle: read %s: %w", filepath.Base(path), err)
	}
}

// hashFile returns the hex sha256 and byte size of the file at path.
func hashFile(path string) (hash string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// contentType returns the content type for a request path, falling back to
// application/octet-stream when the extension is unknown.
func contentType(name string) string {
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
