// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package router implements the DuruPages data-plane entrypoint
// (docs/ARCHITECTURE.md 3.1, 5.1-5.3). A Router is an http.Handler that maps a
// request host to a page/tenant/deployment, loads that deployment's manifest,
// and then either serves a static asset directly (with Cloudflare Pages
// _redirects/_headers semantics and an on-disk LRU cache) or proxies the
// request to a tenant worker pod through the controller's slot queue.
//
// The router is a library so operators can assemble a custom binary; all of its
// collaborators (controller RPC client, storage, static cache, log client) are
// injected through Options.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/manifest"
	"github.com/durupages/durupages/pkg/pagesspec"
	"github.com/durupages/durupages/pkg/router/staticcache"
	"github.com/durupages/durupages/pkg/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// manifestCacheSize bounds the number of parsed manifests kept in memory.
const manifestCacheSize = 4096

// Options configures a Router. Resolver, Storage and Cache are required.
type Options struct {
	// Resolver is the controller RouterService client (host resolution and the
	// worker slot queue).
	Resolver api.RouterServiceClient
	// Storage provides manifests and content-addressed static files.
	Storage storage.Storage
	// Cache is the on-disk LRU cache for static file bodies.
	Cache *staticcache.Cache
	// ResolveCacheTTL is the fallback TTL for host resolution when the
	// controller response does not carry one.
	ResolveCacheTTL time.Duration
	// PagesDomain is the base pages domain (informational; e.g. logging).
	PagesDomain string
	// LogClient, when set, receives StaticAccess events via LogService.Ingest.
	// When nil the router runs in pod-log mode and writes JSON lines instead.
	LogClient api.LogServiceClient
	// LogWriter receives pod-log JSON lines when LogClient is nil. Defaults to
	// os.Stdout.
	LogWriter io.Writer
	// Now overrides the clock (for tests). Defaults to time.Now.
	Now func() time.Time
}

// staticCache is the subset of *staticcache.Cache the router depends on,
// declared as an interface so tests may substitute a fake. The concrete type is
// pkg/router/staticcache.Cache.
type staticCache interface {
	Get(hash string) (path string, ok bool)
	Put(hash string, r io.Reader) (path string, err error)
}

// Router is the data-plane HTTP handler. Construct it with New.
type Router struct {
	resolver api.RouterServiceClient
	storage  storage.Storage
	cache    staticCache

	resolveTTL  time.Duration
	pagesDomain string
	now         func() time.Time

	resolveCache *resolveCache
	manifests    *manifestCache
	logger       *accessLogger
}

// errUnknownHost is returned internally when the controller does not know the
// request host; it maps to a 404 response.
var errUnknownHost = errors.New("router: unknown host")

// New validates opts and returns a ready Router. In LogService mode it starts
// the background batching goroutine; call Close to stop it.
func New(opts Options) (*Router, error) {
	if opts.Resolver == nil {
		return nil, errors.New("router: Resolver is required")
	}
	if opts.Storage == nil {
		return nil, errors.New("router: Storage is required")
	}
	if opts.Cache == nil {
		return nil, errors.New("router: Cache is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	writer := opts.LogWriter
	if writer == nil {
		writer = os.Stdout
	}
	rt := &Router{
		resolver:     opts.Resolver,
		storage:      opts.Storage,
		cache:        opts.Cache,
		resolveTTL:   opts.ResolveCacheTTL,
		pagesDomain:  opts.PagesDomain,
		now:          now,
		resolveCache: newResolveCache(),
		manifests:    newManifestCache(manifestCacheSize),
		logger:       newAccessLogger(opts.LogClient, writer, now),
	}
	return rt, nil
}

// Close releases background resources (the LogService batching goroutine). It
// is safe to call multiple times.
func (rt *Router) Close() error {
	rt.logger.close()
	return nil
}

// ServeHTTP implements the request pipeline of docs/ARCHITECTURE.md 3.1.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// (a) Strip inbound internal headers so external callers cannot spoof the
	// worker routing/lease headers, and determine the host.
	stripInternalHeaders(r.Header)
	host := hostOnly(r.Host)

	// (b) Resolve host -> page.
	page, err := rt.resolve(r.Context(), host)
	if err != nil {
		if errors.Is(err, errUnknownHost) {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}

	// (c) Load the deployment manifest.
	m, err := rt.loadManifest(r.Context(), page)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}

	// (d) Pipeline decision: worker or static.
	if m.HasWorker && pagesspec.MatchRoutes(m.Routes, r.URL.Path) {
		rt.serveDynamic(w, r, page, m)
		return
	}
	rt.serveStatic(w, r, host, page, m)
}

// resolve maps host to a PageInfo, using the in-memory TTL cache first.
func (rt *Router) resolve(ctx context.Context, host string) (*api.PageInfo, error) {
	if page, ok := rt.resolveCache.get(host, rt.now()); ok {
		return page, nil
	}
	resp, err := rt.resolver.ResolvePage(ctx, &api.ResolvePageRequest{Host: host})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, errUnknownHost
		}
		return nil, err
	}
	if resp.GetPage() == nil {
		return nil, errUnknownHost
	}
	ttl := rt.resolveTTL
	if resp.GetTtlSeconds() > 0 {
		ttl = time.Duration(resp.GetTtlSeconds()) * time.Second
	}
	rt.resolveCache.put(host, resp.GetPage(), rt.now().Add(ttl))
	return resp.GetPage(), nil
}

// loadManifest fetches and parses the manifest for the page's active
// deployment, caching it permanently by (immutable) deployment ID.
func (rt *Router) loadManifest(ctx context.Context, page *api.PageInfo) (*manifest.Manifest, error) {
	dep := page.GetActiveDeploymentId()
	if m, ok := rt.manifests.get(dep); ok {
		return m, nil
	}
	key := fmt.Sprintf(storage.ManifestKeyFmt, page.GetTenantId(), page.GetPageId(), dep)
	rc, _, err := rt.storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	m, err := manifest.Decode(rc)
	if err != nil {
		return nil, err
	}
	rt.manifests.add(dep, m)
	return m, nil
}

// stripInternalHeaders removes any inbound X-DuruPages-* headers so external
// clients cannot inject worker routing or lease headers (security).
func stripInternalHeaders(h http.Header) {
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Durupages-") {
			h.Del(name)
		}
	}
}

// hostOnly returns host without any :port suffix. It tolerates IPv6 literals.
func hostOnly(host string) string {
	if host == "" {
		return ""
	}
	// IPv6 literal in brackets, optionally with a port.
	if host[0] == '[' {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[1:end]
		}
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}
