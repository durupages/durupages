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
//
// # Two independent logs
//
// The router writes two log streams that must not be confused:
//
//   - The OPERATIONAL log (Options.Logger, a log/slog logger, defaulting to
//     slog.Default()). Every 4xx/5xx response records its cause here — failed
//     host resolution, a missing manifest, a lost controller lease, a worker
//     that answered 5xx, an unreadable static asset — together with the request
//     context (host, path, page attribution and, on the worker route, the lease
//     request ID that is also echoed to the client in X-DuruPages-Request-Id).
//     One access line per request is emitted at debug level. See oplog.go.
//   - The USAGE log (Options.LogClient / Options.LogWriter). These are the
//     per-request StaticAccess records the hub aggregates for billing and for
//     the customer-visible log stream. See log.go.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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
	//
	// This is the USAGE log (per-request billing/customer-visible records), not
	// the router's own operational log: see Logger.
	LogClient api.LogServiceClient
	// LogWriter receives pod-log JSON lines when LogClient is nil. Defaults to
	// os.Stdout.
	//
	// Like LogClient this carries the USAGE log only. It is unrelated to Logger
	// and the two must not be pointed at each other: an operator parsing the
	// pod-log stream expects one StaticAccess JSON object per line and nothing
	// else.
	LogWriter io.Writer
	// Logger receives the router's OPERATIONAL log: the cause behind every
	// 4xx/5xx response (at warn/error), plus one access line per request (at
	// debug). When nil the slog.Default() logger is used at the time of
	// logging, so a slog.SetDefault after New is still honoured.
	//
	// Defaulting to slog.Default() rather than to "logging disabled" is
	// deliberate, and matches pkg/adminapi: an embedder that wires nothing at
	// all still gets its 502 causes recorded, which is exactly the operational
	// gap this package used to have — an opaque "502 bad gateway" reached the
	// client while the server side stayed silent. Tests that must stay quiet
	// pass an explicit logger over io.Discard.
	Logger *slog.Logger
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

	// usageLog ships StaticAccess records (billing / customer log stream).
	usageLog *usageLogger
	// logger is the operational log/slog logger. It may be nil, meaning
	// "resolve slog.Default() per log line" so that a later slog.SetDefault
	// still takes effect. slog loggers are safe for concurrent use, so no
	// mutex is needed here.
	logger *slog.Logger

	// unknownHostLast / unknownHostSuppressed rate-limit the unknown-host
	// warning, whose trigger (the request Host) is entirely client-controlled.
	// See logUnknownHost.
	unknownHostLast       atomic.Int64 // unix-nano of the last emitted line
	unknownHostSuppressed atomic.Int64 // lines dropped since that emission
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
		usageLog:     newUsageLogger(opts.LogClient, writer, now),
		logger:       opts.Logger,
	}
	return rt, nil
}

// Close releases background resources (the LogService batching goroutine). It
// is safe to call multiple times.
func (rt *Router) Close() error {
	rt.usageLog.close()
	return nil
}

// ServeHTTP implements the request pipeline of docs/ARCHITECTURE.md 3.1.
//
// Every early return below responds with a fixed, deliberately terse body (the
// wire contract clients depend on) and records the actual cause on the
// operational log instead.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// (a) Strip inbound internal headers so external callers cannot spoof the
	// worker routing/lease headers, and determine the host.
	stripInternalHeaders(r.Header)
	host := hostOnly(r.Host)

	rl := &reqLog{host: host, start: rt.now()}
	rec := &respRecorder{ResponseWriter: w, status: http.StatusOK}
	w = rec
	defer func() { rt.logAccess(r, rec, rl) }()

	// (b) Resolve host -> page.
	page, err := rt.resolve(r.Context(), host)
	if err != nil {
		if errors.Is(err, errUnknownHost) {
			// Warn, not info: reaching the data plane with a host it cannot map
			// is normally a DNS/custom-domain misconfiguration worth surfacing.
			// It is also what internet background noise aimed at the bare
			// address produces -- and the Host is client-controlled -- so the
			// warning is rate-limited (logUnknownHost) rather than emitted once
			// per request.
			rt.logUnknownHost(r, rl)
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		rt.logEvent(r, rl, slog.LevelError, msgResolveFailed,
			slog.Int("status", http.StatusBadGateway),
			slog.String("grpcCode", status.Code(err).String()),
			slog.Any("error", err))
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}
	rl.page = page

	// (c) Load the deployment manifest.
	m, err := rt.loadManifest(r.Context(), page)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// A 4xx status, but a server-side fault: the page record points at
			// an active deployment whose manifest is not in storage. The
			// storage key rides along inside the wrapped error.
			rt.logEvent(r, rl, slog.LevelWarn, msgManifestMissing,
				slog.Int("status", http.StatusNotFound),
				slog.Any("error", err))
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		rt.logEvent(r, rl, slog.LevelError, msgManifestFailed,
			slog.Int("status", http.StatusBadGateway),
			slog.Any("error", err))
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}

	// (d) Pipeline decision: worker or static.
	if m.HasWorker && pagesspec.MatchRoutes(m.Routes, r.URL.Path) {
		rl.route = routeDynamic
		rt.serveDynamic(w, r, rl, page, m)
		return
	}
	rl.route = routeStatic
	rt.serveStatic(w, r, rl, host, page, m)
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
		// Wrapped so the operational log names the object that was missing or
		// unreadable; errors.Is(storage.ErrNotFound) still matches.
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer rc.Close()
	m, err := manifest.Decode(rc)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", key, err)
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
