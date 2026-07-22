// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command durupages-hub runs the worker support service: tenant-scoped bundle
// distribution (HTTP) and usage/log ingest (gRPC). Default assembly: S3
// storage + stdout LogSink. No database dependency.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/durupages/durupages/internal/version"
	"github.com/durupages/durupages/pkg/hub"
	"github.com/durupages/durupages/pkg/hub/stdoutsink"
	"github.com/durupages/durupages/pkg/storage/s3"
	"github.com/durupages/durupages/pkg/tlsconf"
	"github.com/durupages/durupages/pkg/workerauth"
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

// setupLogging installs a JSON slog handler on stderr as the process default.
//
// slog.SetDefault also redirects the standard log package through this handler,
// so the log.Printf calls in this binary and the hub's structured request lines
// end up in one consistent JSON stream instead of two competing formats.
//
// stderr, not stdout: stdout carries the usage/log event stream written by the
// default stdout sink, and mixing the hub's own operational log into it would
// corrupt that feed for whatever consumes it.
func setupLogging() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
}

func main() {
	version.MaybePrint()
	setupLogging()
	var (
		listenHTTP    = flag.String("listen-http", envOr("DURUPAGES_LISTEN_HTTP", ":9080"), "bundle HTTP listen address")
		listenGRPC    = flag.String("listen-grpc", envOr("DURUPAGES_LISTEN_GRPC", ":9443"), "log ingest gRPC listen address")
		cacheDir      = flag.String("bundle-cache-dir", envOr("DURUPAGES_BUNDLE_CACHE_DIR", "/var/cache/durupages-hub"), "bundle disk cache directory")
		cacheMax      = flag.Int64("bundle-cache-max-bytes", envOrInt64("DURUPAGES_BUNDLE_CACHE_MAX_BYTES", 4<<30), "bundle disk cache size limit")
		jwtPubKeyFile = flag.String("worker-jwt-pubkey", envOr("DURUPAGES_WORKER_JWT_PUBKEY", ""), "path to the worker JWT ed25519 public key (PEM)")
		maxLogs       = flag.Int("max-logs-per-request", int(envOrInt64("DURUPAGES_MAX_LOGS_PER_REQUEST", 256)), "per-request log entry cap")
		maxLogBytes   = flag.Int("max-log-bytes-per-request", int(envOrInt64("DURUPAGES_MAX_LOG_BYTES_PER_REQUEST", 128<<10)), "per-request log byte cap")

		tlsCertFile = flag.String("tls-cert-file", envOr("DURUPAGES_TLS_CERT_FILE", ""), "PEM certificate for the bundle HTTP listener (empty = plaintext)")
		tlsKeyFile  = flag.String("tls-key-file", envOr("DURUPAGES_TLS_KEY_FILE", ""), "PEM private key for the bundle HTTP listener")
		// The two listeners can be published under different hostnames (bundle
		// downloads through an ingress, log ingest on an internal address), so
		// the gRPC side may carry its own certificate. Unset, it reuses the
		// bundle pair, which is the single-hostname deployment.
		logTLSCertFile = flag.String("log-tls-cert-file", envOr("DURUPAGES_LOG_TLS_CERT_FILE", ""), "PEM certificate for the log ingest gRPC listener (empty = reuse --tls-cert-file)")
		logTLSKeyFile  = flag.String("log-tls-key-file", envOr("DURUPAGES_LOG_TLS_KEY_FILE", ""), "PEM private key for the log ingest gRPC listener")

		s3Endpoint  = flag.String("s3-endpoint", envOr("DURUPAGES_S3_ENDPOINT", ""), "S3 endpoint (empty = AWS default)")
		s3Region    = flag.String("s3-region", envOr("DURUPAGES_S3_REGION", "us-east-1"), "S3 region")
		s3Bucket    = flag.String("s3-bucket", envOr("DURUPAGES_S3_BUCKET", ""), "S3 bucket (required)")
		s3AccessKey = flag.String("s3-access-key", envOr("DURUPAGES_S3_ACCESS_KEY", ""), "S3 access key (empty = default chain)")
		s3SecretKey = flag.String("s3-secret-key", envOr("DURUPAGES_S3_SECRET_KEY", ""), "S3 secret key")
		s3PathStyle = flag.Bool("s3-path-style", envOr("DURUPAGES_S3_PATH_STYLE", "true") == "true", "use path-style S3 addressing (MinIO)")
	)
	flag.Parse()
	if err := run(context.Background(), config{
		listenHTTP: *listenHTTP, listenGRPC: *listenGRPC,
		cacheDir: *cacheDir, cacheMax: *cacheMax, jwtPubKeyFile: *jwtPubKeyFile,
		maxLogs: *maxLogs, maxLogBytes: *maxLogBytes,
		httpTLS: tlsFiles{certFile: *tlsCertFile, keyFile: *tlsKeyFile},
		logTLS:  tlsFiles{certFile: *logTLSCertFile, keyFile: *logTLSKeyFile},
		s3: s3.Options{Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
			AccessKey: *s3AccessKey, SecretKey: *s3SecretKey, UsePathStyle: *s3PathStyle},
	}); err != nil {
		fatalf("durupages-hub: %v", err)
	}
}

type config struct {
	listenHTTP, listenGRPC string
	cacheDir               string
	cacheMax               int64
	jwtPubKeyFile          string
	maxLogs, maxLogBytes   int
	// httpTLS serves the bundle HTTP listener; logTLS serves the log ingest
	// gRPC listener and falls back to httpTLS when unset.
	httpTLS, logTLS tlsFiles
	s3              s3.Options
}

// tlsFiles is one listener's certificate pair. Both empty means plaintext.
type tlsFiles struct {
	certFile, keyFile string
}

// requested reports whether the operator asked for TLS at all. Either field
// alone counts, so that a half-configured pair is diagnosed instead of being
// read as "plaintext, as intended".
func (t tlsFiles) requested() bool { return t.certFile != "" || t.keyFile != "" }

// orElse returns t when it names a pair, and fallback otherwise. This is what
// lets the log listener reuse the bundle certificate while still being able to
// carry its own, for a deployment where the two listeners answer to different
// hostnames.
func (t tlsFiles) orElse(fallback tlsFiles) tlsFiles {
	if t.requested() {
		return t
	}
	return fallback
}

// serverTLS builds the TLS configuration for one listener, or nil for
// plaintext when no certificate was named.
//
// Every failure here aborts startup. A hub told to use a certificate must
// never come up in plaintext because the file was missing, unreadable or
// mismatched: clients would either fail to connect or, worse, keep talking to
// it unencrypted while the operator believes the hop is protected.
func serverTLS(listener string, t tlsFiles) (*tls.Config, error) {
	if !t.requested() {
		return nil, nil
	}
	if t.certFile == "" || t.keyFile == "" {
		return nil, fmt.Errorf("%s TLS: certificate and key must both be set (cert=%q key=%q)",
			listener, t.certFile, t.keyFile)
	}
	// tlsconf.ServerConfig, not tls.LoadX509KeyPair: cert-manager renews by
	// rewriting the mounted Secret in place, and only the reloading config
	// picks that up without a restart.
	cfg, err := tlsconf.ServerConfig(t.certFile, t.keyFile)
	if err != nil {
		return nil, fmt.Errorf("%s TLS: %w", listener, err)
	}
	return cfg, nil
}

// newBundleServer builds the bundle download listener. A nil tlsCfg serves
// plaintext.
func newBundleServer(addr string, h *hub.Hub, tlsCfg *tls.Config) *http.Server {
	return &http.Server{Addr: addr, Handler: h.HTTPHandler(), TLSConfig: tlsCfg}
}

// newLogServer builds the log ingest gRPC server. A nil tlsCfg serves
// plaintext.
func newLogServer(h *hub.Hub, tlsCfg *tls.Config) *grpc.Server {
	var opts []grpc.ServerOption
	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	s := grpc.NewServer(opts...)
	h.RegisterLogService(s)
	return s
}

// serveHTTP runs srv on lis, over TLS when a certificate was configured.
func serveHTTP(srv *http.Server, lis net.Listener) error {
	if srv.TLSConfig == nil {
		return srv.Serve(lis)
	}
	// The empty cert/key paths are required, not a shortcut: ServeTLS loads the
	// named files only when TLSConfig supplies neither Certificates nor
	// GetCertificate, and a pair loaded that way is read once and never again.
	// Passing paths here would therefore shadow the reloading GetCertificate
	// installed by tlsconf.ServerConfig and keep serving the old certificate
	// after a renewal.
	return srv.ServeTLS(lis, "", "")
}

// listenAttrs describes a listener for the startup log: whether TLS is on and,
// if so, which files it serves. An operator reading the log must be able to
// tell a plaintext listener from a protected one without probing the port.
func listenAttrs(addr string, t tlsFiles, enabled bool) []any {
	attrs := []any{"addr", addr, "tls", enabled}
	if enabled {
		attrs = append(attrs, "certFile", t.certFile, "keyFile", t.keyFile)
	}
	return attrs
}

func run(ctx context.Context, cfg config) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Transport security is resolved before anything else so that a bad
	// certificate path fails the process immediately, and in particular before
	// any listener could accept a plaintext connection.
	httpTLS, err := serverTLS("bundle HTTP", cfg.httpTLS)
	if err != nil {
		return err
	}
	logTLSFiles := cfg.logTLS.orElse(cfg.httpTLS)
	logTLS, err := serverTLS("log ingest gRPC", logTLSFiles)
	if err != nil {
		return err
	}

	if cfg.s3.Bucket == "" {
		return fmt.Errorf("--s3-bucket is required")
	}
	if cfg.jwtPubKeyFile == "" {
		return fmt.Errorf("--worker-jwt-pubkey is required")
	}
	pemData, err := os.ReadFile(cfg.jwtPubKeyFile)
	if err != nil {
		return fmt.Errorf("read jwt pubkey: %w", err)
	}
	pub, err := workerauth.ParsePublicKeyPEM(pemData)
	if err != nil {
		return fmt.Errorf("parse jwt pubkey: %w", err)
	}

	store, err := s3.New(ctx, cfg.s3)
	if err != nil {
		return fmt.Errorf("s3: %w", err)
	}

	h, err := hub.New(hub.Options{
		Storage:               store,
		JWTPublicKey:          pub,
		CacheDir:              cfg.cacheDir,
		CacheMaxBytes:         cfg.cacheMax,
		Sink:                  stdoutsink.New(os.Stdout),
		MaxLogsPerRequest:     cfg.maxLogs,
		MaxLogBytesPerRequest: cfg.maxLogBytes,
		Logger:                slog.Default().With("component", "hub"),
	})
	if err != nil {
		return fmt.Errorf("hub: %w", err)
	}

	httpSrv := newBundleServer(cfg.listenHTTP, h, httpTLS)
	grpcSrv := newLogServer(h, logTLS)

	// Both sockets are opened before either is served, so that a port already
	// in use is reported as a startup failure rather than after the other
	// listener has begun answering requests.
	httpLis, err := net.Listen("tcp", cfg.listenHTTP)
	if err != nil {
		return fmt.Errorf("listen bundle HTTP: %w", err)
	}
	grpcLis, err := net.Listen("tcp", cfg.listenGRPC)
	if err != nil {
		httpLis.Close()
		return fmt.Errorf("listen log ingest gRPC: %w", err)
	}

	errc := make(chan error, 2)
	go func() {
		slog.Info("bundle HTTP listening", listenAttrs(cfg.listenHTTP, cfg.httpTLS, httpTLS != nil)...)
		errc <- serveHTTP(httpSrv, httpLis)
	}()
	go func() {
		slog.Info("log ingest gRPC listening", listenAttrs(cfg.listenGRPC, logTLSFiles, logTLS != nil)...)
		errc <- grpcSrv.Serve(grpcLis)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		grpcSrv.GracefulStop()
		return httpSrv.Shutdown(context.Background())
	case err := <-errc:
		return err
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
