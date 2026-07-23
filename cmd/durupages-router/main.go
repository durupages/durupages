// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command durupages-router runs the data-plane entrypoint: host routing,
// static serving with a disk LRU cache, and dynamic request proxying via
// controller-issued leases.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/durupages/durupages/internal/version"
	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/router"
	"github.com/durupages/durupages/pkg/router/staticcache"
	"github.com/durupages/durupages/pkg/storage/s3"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// schemeFixResolver adapts the controller RouterService client to work around a
// shim/router endpoint mismatch. durupages.proto documents the worker endpoint
// as a bare "host:port", and that is what the shim advertises, but
// router.proxyToWorker feeds the lease endpoint straight into url.Parse, which
// requires a scheme (a scheme-less "host:port" fails to parse and yields a 502).
// This wrapper prepends "http://" to any granted lease endpoint that lacks a
// scheme, leaving already-qualified endpoints untouched.
type schemeFixResolver struct{ api.RouterServiceClient }

func (r schemeFixResolver) AcquireSlot(ctx context.Context, in *api.AcquireSlotRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[api.AcquireSlotEvent], error) {
	st, err := r.RouterServiceClient.AcquireSlot(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schemeFixStream{st}, nil
}

// schemeFixStream rewrites granted lease endpoints as they are received.
type schemeFixStream struct {
	grpc.ServerStreamingClient[api.AcquireSlotEvent]
}

func (s schemeFixStream) Recv() (*api.AcquireSlotEvent, error) {
	ev, err := s.ServerStreamingClient.Recv()
	if err != nil {
		return ev, err
	}
	if g := ev.GetGranted(); g != nil {
		if ep := g.GetEndpoint(); ep != "" && !strings.Contains(ep, "://") {
			g.Endpoint = "http://" + ep
		}
	}
	return ev, nil
}

// setupLogging installs a JSON slog handler on stderr as the process default,
// at the requested level. This mirrors durupages-controller: one JSON stream on
// stderr for the whole binary.
//
// slog.SetDefault also redirects the standard log package through this handler,
// so the log.Printf/log.Fatalf calls in this file end up in the same stream
// instead of a second, differently formatted one.
//
// The level is the access-log switch: pkg/router logs the cause of every
// 4xx/5xx at warn/error (visible at the default info level) and one access line
// per request at debug, so "debug" turns on full access logging for the data
// plane and "info" keeps it to failures only.
func setupLogging(level string) {
	lvl := slog.LevelInfo
	bad := false
	if level != "" && lvl.UnmarshalText([]byte(level)) != nil {
		// Not fatal: an unparsable level must not keep the data plane down.
		lvl, bad = slog.LevelInfo, true
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})))
	if bad {
		slog.Warn("unknown log level, defaulting to info", "value", level)
	}
}

func main() {
	version.MaybePrint()
	var (
		logLevel        = flag.String("log-level", envOr("DURUPAGES_LOG_LEVEL", "info"), "operational log level: debug, info, warn or error (debug enables the per-request access log)")
		listen          = flag.String("listen", envOr("DURUPAGES_LISTEN", ":8080"), "HTTP listen address")
		controllerAddr  = flag.String("controller-addr", envOr("DURUPAGES_CONTROLLER_ADDR", ""), "controller gRPC address (required)")
		hubLogAddr      = flag.String("hub-log-addr", envOr("DURUPAGES_HUB_LOG_ADDR", ""), "hub log ingest gRPC address (empty = pod-log mode)")
		pagesDomain     = flag.String("pages-domain", envOr("DURUPAGES_PAGES_DOMAIN", "pages.local"), "pages domain")
		cacheDir        = flag.String("static-cache-dir", envOr("DURUPAGES_STATIC_CACHE_DIR", "/var/cache/durupages"), "static LRU cache directory")
		cacheMax        = flag.Int64("static-cache-max-bytes", envOrInt64("DURUPAGES_STATIC_CACHE_MAX_BYTES", 1<<30), "static cache size limit")
		resolveCacheTTL = flag.Duration("resolve-cache-ttl", 10*time.Second, "page resolve cache TTL")

		s3Endpoint  = flag.String("s3-endpoint", envOr("DURUPAGES_S3_ENDPOINT", ""), "S3 endpoint")
		s3Region    = flag.String("s3-region", envOr("DURUPAGES_S3_REGION", "us-east-1"), "S3 region")
		s3Bucket    = flag.String("s3-bucket", envOr("DURUPAGES_S3_BUCKET", ""), "S3 bucket (required)")
		s3AccessKey = flag.String("s3-access-key", envOr("DURUPAGES_S3_ACCESS_KEY", ""), "S3 access key")
		s3SecretKey = flag.String("s3-secret-key", envOr("DURUPAGES_S3_SECRET_KEY", ""), "S3 secret key")
		s3PathStyle = flag.Bool("s3-path-style", envOr("DURUPAGES_S3_PATH_STYLE", "true") == "true", "path-style S3 addressing")

		// Client TLS. Off by default: an untouched deployment keeps dialing
		// plaintext, and a cluster is converted one hop at a time.
		controllerTLSOn      = flag.Bool("controller-tls", envBool("DURUPAGES_CONTROLLER_TLS"), "dial the controller over TLS")
		hubLogTLSOn          = flag.Bool("hub-log-tls", envBool("DURUPAGES_HUB_LOG_TLS"), "dial hub log ingest over TLS")
		controllerServerName = flag.String("controller-server-name", envOr("DURUPAGES_CONTROLLER_SERVER_NAME", ""), "server name to verify in the controller certificate (default: host part of --controller-addr)")
		hubLogServerName     = flag.String("hub-log-server-name", envOr("DURUPAGES_HUB_LOG_SERVER_NAME", ""), "server name to verify in the hub certificate (default: host part of --hub-log-addr)")
		caCertPEM            = flag.String("ca-cert-pem", envOr("DURUPAGES_CA_CERT_PEM", ""), "inline PEM CA certificates (takes precedence over --ca-cert-file)")
		caCertFile           = flag.String("ca-cert-file", envOr("DURUPAGES_CA_CERT_FILE", ""), "path to PEM CA certificates; re-read when it changes on disk")
		tlsSkipVerify        = flag.Bool("tls-insecure-skip-verify", envBool("DURUPAGES_TLS_INSECURE_SKIP_VERIFY"), "do not verify server certificates (encrypted but unauthenticated; for bringing a cluster up before its PKI exists)")
	)
	flag.Parse()
	setupLogging(*logLevel)
	if *controllerAddr == "" || *s3Bucket == "" {
		fatalf("durupages-router: --controller-addr and --s3-bucket are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := s3.New(ctx, s3.Options{Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
		AccessKey: *s3AccessKey, SecretKey: *s3SecretKey, UsePathStyle: *s3PathStyle})
	if err != nil {
		fatalf("durupages-router: s3: %v", err)
	}

	cache, err := staticcache.New(*cacheDir, *cacheMax)
	if err != nil {
		fatalf("durupages-router: static cache: %v", err)
	}

	// Server TLS only: the router authenticates the controller and the hub, and
	// is itself authenticated at the application layer (lease tokens), not by a
	// client certificate.
	tlsf := tlsFlags{caPEM: caCertPEM, caFile: caCertFile, skipVerify: tlsSkipVerify}
	controllerTLS, err := tlsf.clientTLS(*controllerAddr, *controllerTLSOn, *controllerServerName)
	if err != nil {
		fatalf("durupages-router: controller TLS: %v", err)
	}
	hubLogTLS, err := tlsf.clientTLS(*hubLogAddr, *hubLogAddr != "" && *hubLogTLSOn, *hubLogServerName)
	if err != nil {
		fatalf("durupages-router: hub log TLS: %v", err)
	}
	tlsf.logTLSStatus(controllerTLS, hubLogTLS)

	conn, err := grpc.NewClient(*controllerAddr, grpc.WithTransportCredentials(grpcCreds(controllerTLS)))
	if err != nil {
		fatalf("durupages-router: controller dial: %v", err)
	}
	defer conn.Close()

	opts := router.Options{
		Resolver:        schemeFixResolver{api.NewRouterServiceClient(conn)},
		Storage:         store,
		Cache:           cache,
		ResolveCacheTTL: *resolveCacheTTL,
		PagesDomain:     *pagesDomain,
		// Usage log (StaticAccess records): stdout, one JSON object per line,
		// which is what the pod-log collector parses.
		LogWriter: os.Stdout,
		// Operational log: the same stderr JSON stream as the rest of the
		// binary. Distinct from LogWriter above on purpose — mixing the two
		// would corrupt the usage stream.
		Logger: slog.Default().With("component", "router"),
	}
	if *hubLogAddr != "" {
		logConn, err := grpc.NewClient(*hubLogAddr, grpc.WithTransportCredentials(grpcCreds(hubLogTLS)))
		if err != nil {
			fatalf("durupages-router: hub dial: %v", err)
		}
		defer logConn.Close()
		opts.LogClient = api.NewLogServiceClient(logConn)
	}

	r, err := router.New(opts)
	if err != nil {
		fatalf("durupages-router: %v", err)
	}
	defer r.Close()

	srv := &http.Server{Addr: *listen, Handler: r}
	go func() {
		<-ctx.Done()
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("router listening on %s (pages domain %s)", *listen, *pagesDomain)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatalf("durupages-router: %v", err)
	}
}

// fatalf reports a startup failure and exits. It exists because log.Fatalf
// routes through the slog bridge installed by setupLogging and lands at INFO,
// filing the failure that kills the process under the same level as routine
// progress -- exactly where an operator scanning for errors will not look.
func fatalf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
