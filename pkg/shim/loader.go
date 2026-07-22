// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/runtime"
)

// deployment is a bundle unpacked on disk plus its derived metadata.
type deployment struct {
	pageID       string
	deploymentID string
	dir          string // BundleDir/<deploymentID>
	staticDir    string // dir/static
	manifest     *manifest.Manifest
	env          map[string]string
	secret       map[string]string
	sizeBytes    int64
	lastUsed     time.Time
}

// loadCall is a singleflight entry for one page's in-progress load.
type loadCall struct {
	deploymentID string
	done         chan struct{}
	err          error
}

// maxBundleBytes caps a downloaded bundle to guard against runaway archives.
const maxBundleBytes = 512 << 20 // 512 MiB

// ensureLoaded makes sure the page is serving deploymentID, downloading and
// swapping when necessary. Concurrent requests for the same page share one
// load (singleflight); a request for a newer deployment triggers a fresh swap.
func (s *Shim) ensureLoaded(ctx context.Context, pageID, deploymentID string) error {
	for {
		s.mu.Lock()
		if s.active[pageID] == deploymentID {
			if d := s.deployments[deploymentID]; d != nil {
				d.lastUsed = s.now()
			}
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()

		s.loadingMu.Lock()
		call, inProgress := s.loading[pageID]
		if inProgress {
			s.loadingMu.Unlock()
			<-call.done
			if call.deploymentID == deploymentID && call.err != nil {
				return call.err
			}
			continue // re-evaluate against the (possibly updated) load set
		}
		call = &loadCall{deploymentID: deploymentID, done: make(chan struct{})}
		s.loading[pageID] = call
		s.loadingMu.Unlock()

		err := s.load(ctx, pageID, deploymentID)
		call.err = err
		close(call.done)
		s.loadingMu.Lock()
		delete(s.loading, pageID)
		s.loadingMu.Unlock()
		return err
	}
}

// load downloads the bundle and performs a graceful swap that adds the page to
// the load set. On swap failure the old instance keeps serving.
//
// Loading is the slowest and by far the most failure-prone thing the shim does
// (hub reachability, hub authorization, bundle integrity, controller page
// config, workerd startup), so both outcomes are logged: success at info with
// the timings that explain a slow first request, failure at error with the
// stage that broke. Callers still get the error and log their own response.
func (s *Shim) load(ctx context.Context, pageID, deploymentID string) error {
	start := s.now()
	requestID := requestIDFromContext(ctx)
	logAttrs := func(extra ...slog.Attr) []slog.Attr {
		base := []slog.Attr{
			slog.String("tenantId", s.opts.TenantID),
			slog.String("pageId", pageID),
			slog.String("deploymentId", deploymentID),
		}
		base = attrIf(base, "requestId", requestID)
		return append(base, extra...)
	}
	fail := func(stage string, err error) error {
		s.log().LogAttrs(ctx, slog.LevelError, logMsgLoadFailed, logAttrs(
			slog.String("stage", stage),
			slog.String("hubAddr", s.opts.HubAddr),
			slog.Int64("elapsedMs", s.now().Sub(start).Milliseconds()),
			slog.String("error", err.Error()),
		)...)
		return err
	}

	dep, err := s.fetchBundle(ctx, pageID, deploymentID)
	if err != nil {
		return fail("fetchBundle", err)
	}
	if err := s.fetchPageConfig(ctx, dep); err != nil {
		_ = os.RemoveAll(dep.dir)
		return fail("fetchPageConfig", err)
	}
	s.mu.Lock()
	s.deployments[deploymentID] = dep
	s.mu.Unlock()

	if err := s.swap(ctx, func(cur map[string]string) map[string]string {
		cur[pageID] = deploymentID
		// Load-set shrink applies at swap time: drop other pages that have been
		// idle beyond MinIdle (their old deployments are cleaned by the sweep).
		for p, d := range cur {
			if p == pageID {
				continue
			}
			if od := s.deployments[d]; od != nil && s.now().Sub(od.lastUsed) >= s.minIdle {
				delete(cur, p)
			}
		}
		return cur
	}); err != nil {
		return fail("swap", err)
	}

	s.log().LogAttrs(ctx, slog.LevelInfo, logMsgLoaded, logAttrs(
		slog.Int64("bundleBytes", dep.sizeBytes),
		slog.Int64("elapsedMs", s.now().Sub(start).Milliseconds()),
	)...)
	return nil
}

// setInFlightFunc is the optional runtime.Instance extension the shim uses to
// hand the runtime its per-instance in-flight counter (for Drain).
type setInFlightFunc interface {
	SetInFlightFunc(func() int)
}

// swap performs a graceful blue-green swap. It launches a new instance for the
// mutated load set, waits until it is ready, atomically switches new traffic to
// it, then drains and closes the old instance in the background. The whole swap
// is serialized so concurrent loads/evictions do not race.
func (s *Shim) swap(ctx context.Context, mutate func(cur map[string]string) map[string]string) error {
	s.swapMu.Lock()
	defer s.swapMu.Unlock()

	s.mu.Lock()
	desired := mutate(copyStringMap(s.active))
	spec, err := s.buildSpecLocked(desired)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	inst, err := s.opts.Runtime.Launch(ctx, spec)
	if err != nil {
		return fmt.Errorf("shim: launch: %w", err)
	}
	if err := inst.WaitReady(ctx); err != nil {
		_ = inst.Close()
		return fmt.Errorf("shim: wait ready: %w", err)
	}

	li := &liveInstance{inst: inst}
	if sf, ok := inst.(setInFlightFunc); ok {
		sf.SetInFlightFunc(func() int { return int(atomic.LoadInt64(&li.inFlight)) })
	}

	old := s.current.Swap(li)

	s.mu.Lock()
	s.active = desired
	s.rebuildRedactorLocked()
	s.mu.Unlock()

	if old != nil {
		go func() {
			dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_ = old.inst.Drain(dctx)
			_ = old.inst.Close()
		}()
	}
	return nil
}

// buildSpecLocked builds an InstanceSpec for the given load set. Callers hold
// s.mu. Pages are emitted in sorted order for deterministic configs.
func (s *Shim) buildSpecLocked(active map[string]string) (runtime.InstanceSpec, error) {
	pageIDs := make([]string, 0, len(active))
	for p := range active {
		pageIDs = append(pageIDs, p)
	}
	sort.Strings(pageIDs)

	spec := runtime.InstanceSpec{
		AssetsEndpoint: s.assetsEndpoint,
		TailEndpoint:   s.tailEndpoint,
	}
	for _, pageID := range pageIDs {
		dep := s.deployments[active[pageID]]
		if dep == nil {
			return runtime.InstanceSpec{}, fmt.Errorf("shim: deployment %q missing for page %q", active[pageID], pageID)
		}
		spec.Pages = append(spec.Pages, runtime.PageWorker{
			PageID:       dep.pageID,
			DeploymentID: dep.deploymentID,
			BundleDir:    dep.dir,
			StaticDir:    dep.staticDir,
			Manifest:     dep.manifest,
			Env:          dep.env,
			Secret:       dep.secret,
		})
	}
	return spec, nil
}

// fetchBundle downloads bundle.tar from the hub and unpacks it under
// BundleDir/<deploymentID>. The tar must contain manifest.json plus worker/ and
// static/ trees; optional env.json / secret.json carry the page bindings.
func (s *Shim) fetchBundle(ctx context.Context, pageID, deploymentID string) (*deployment, error) {
	url := fmt.Sprintf("%s/v1/tenants/%s/pages/%s/deployments/%s/bundle.tar",
		strings.TrimRight(s.opts.HubAddr, "/"), s.opts.TenantID, pageID, deploymentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("shim: fetch bundle %s: %w", url, err)
	}
	// The bearer is the worker JWT; it is never echoed into an error or a log.
	req.Header.Set("Authorization", "Bearer "+s.currentJWT())
	// Carry the request id across the last hop so the hub's log line for this
	// fetch joins the router's and the shim's under one id. Without it the
	// correlation chain breaks exactly where it is needed most: a 502 whose
	// real cause is the hub answering 401 or 404.
	if rid := requestIDFromContext(ctx); rid != "" {
		req.Header.Set(api.HeaderRequestID, rid)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Carry the URL: "which hub did we even talk to" is the first question
		// asked when a page starts answering 502 load failed.
		return nil, fmt.Errorf("shim: fetch bundle %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shim: fetch bundle %s: status %d%s", url, resp.StatusCode, errBodySuffix(resp.Body))
	}

	dir := filepath.Join(s.opts.BundleDir, deploymentID)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	size, err := untar(resp.Body, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("shim: unpack bundle %s: %w", url, err)
	}

	dep := &deployment{
		pageID:       pageID,
		deploymentID: deploymentID,
		dir:          dir,
		staticDir:    filepath.Join(dir, "static"),
		sizeBytes:    size,
		lastUsed:     s.now(),
	}
	if err := s.readBundleMeta(dep); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return dep, nil
}

// maxErrBodySnippet caps how much of a hub error body is quoted back into the
// wrapping error.
const maxErrBodySnippet = 256

// errBodySuffix reads a short, whitespace-collapsed snippet of an error
// response body so that a hub rejection ("page not found", "token expired")
// says why, not just "status 404". It returns "" when the body is empty. Only
// hub error bodies reach this: they carry no tenant data or credentials.
func errBodySuffix(body io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(body, maxErrBodySnippet))
	if err != nil {
		return ""
	}
	snippet := strings.Join(strings.Fields(string(b)), " ")
	if snippet == "" {
		return ""
	}
	return ": " + snippet
}

// fetchPageConfig asks the controller for the page's Env/Secret bindings and
// overrides any bundle-provided defaults with them. Page Env/Secret live in
// the PageProvider (not in the build output), so this is the authoritative
// source; the optional env.json/secret.json bundle files remain a fallback
// for controllers that do not implement the RPC (Unimplemented).
// Failing closed on other errors matters: serving a page without its Secret
// bindings would be a silent misconfiguration.
func (s *Shim) fetchPageConfig(ctx context.Context, dep *deployment) error {
	client, closeConn, err := s.workerClient()
	if err != nil {
		return fmt.Errorf("shim: page config: %w", err)
	}
	defer closeConn()

	resp, err := client.GetPageConfig(s.authCtx(ctx), &api.GetPageConfigRequest{PageId: dep.pageID})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return nil // legacy controller: keep bundle-provided files
		}
		return fmt.Errorf("shim: page config: %w", err)
	}
	if env := resp.GetEnv(); len(env) > 0 {
		dep.env = env
	}
	if secret := resp.GetSecret(); len(secret) > 0 {
		dep.secret = secret
	}
	return nil
}

// readBundleMeta loads manifest.json and the optional env/secret binding files.
func (s *Shim) readBundleMeta(dep *deployment) error {
	f, err := os.Open(filepath.Join(dep.dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("shim: open manifest: %w", err)
	}
	defer f.Close()
	m, err := manifest.Decode(f)
	if err != nil {
		return err
	}
	dep.manifest = m
	dep.env = readStringMap(filepath.Join(dep.dir, "env.json"))
	dep.secret = readStringMap(filepath.Join(dep.dir, "secret.json"))
	return nil
}

// readStringMap reads an optional JSON object of string values; a missing file
// yields nil.
func readStringMap(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// untar extracts r into dir, rejecting path-traversal entries, and returns the
// total number of bytes written.
func untar(r io.Reader, dir string) (int64, error) {
	tr := tar.NewReader(r)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		target, ok := safeJoin(dir, hdr.Name)
		if !ok {
			return total, fmt.Errorf("shim: unsafe tar entry %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return total, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return total, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return total, err
			}
			n, err := io.Copy(out, io.LimitReader(tr, maxBundleBytes-total+1))
			closeErr := out.Close()
			if err != nil {
				return total, err
			}
			if closeErr != nil {
				return total, closeErr
			}
			total += n
			if total > maxBundleBytes {
				return total, fmt.Errorf("shim: bundle exceeds %d bytes", int64(maxBundleBytes))
			}
		default:
			// Skip symlinks and other special entries: bundles are plain files.
		}
	}
}

// safeJoin joins name onto dir, returning ok=false if the result would escape
// dir (path traversal) or name is absolute.
func safeJoin(dir, name string) (string, bool) {
	clean := filepath.Clean("/" + filepath.ToSlash(name))
	joined := filepath.Join(dir, clean)
	rel, err := filepath.Rel(dir, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return joined, true
}

// touch updates a loaded deployment's lastUsed to now.
func (s *Shim) touch(deploymentID string) {
	s.mu.Lock()
	if d := s.deployments[deploymentID]; d != nil {
		d.lastUsed = s.now()
	}
	s.mu.Unlock()
}

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
