// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command durupages-hub runs the worker support service: tenant-scoped bundle
// distribution (HTTP) and usage/log ingest (gRPC). Default assembly: S3
// storage + stdout LogSink. No database dependency.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"google.golang.org/grpc"

	"github.com/durupages/durupages/internal/version"
	"github.com/durupages/durupages/pkg/hub"
	"github.com/durupages/durupages/pkg/hub/stdoutsink"
	"github.com/durupages/durupages/pkg/storage/s3"
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

func main() {
	version.MaybePrint()
	var (
		listenHTTP    = flag.String("listen-http", envOr("DURUPAGES_LISTEN_HTTP", ":9080"), "bundle HTTP listen address")
		listenGRPC    = flag.String("listen-grpc", envOr("DURUPAGES_LISTEN_GRPC", ":9443"), "log ingest gRPC listen address")
		cacheDir      = flag.String("bundle-cache-dir", envOr("DURUPAGES_BUNDLE_CACHE_DIR", "/var/cache/durupages-hub"), "bundle disk cache directory")
		cacheMax      = flag.Int64("bundle-cache-max-bytes", envOrInt64("DURUPAGES_BUNDLE_CACHE_MAX_BYTES", 4<<30), "bundle disk cache size limit")
		jwtPubKeyFile = flag.String("worker-jwt-pubkey", envOr("DURUPAGES_WORKER_JWT_PUBKEY", ""), "path to the worker JWT ed25519 public key (PEM)")
		maxLogs       = flag.Int("max-logs-per-request", int(envOrInt64("DURUPAGES_MAX_LOGS_PER_REQUEST", 256)), "per-request log entry cap")
		maxLogBytes   = flag.Int("max-log-bytes-per-request", int(envOrInt64("DURUPAGES_MAX_LOG_BYTES_PER_REQUEST", 128<<10)), "per-request log byte cap")

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
		s3: s3.Options{Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
			AccessKey: *s3AccessKey, SecretKey: *s3SecretKey, UsePathStyle: *s3PathStyle},
	}); err != nil {
		log.Fatalf("durupages-hub: %v", err)
	}
}

type config struct {
	listenHTTP, listenGRPC string
	cacheDir               string
	cacheMax               int64
	jwtPubKeyFile          string
	maxLogs, maxLogBytes   int
	s3                     s3.Options
}

func run(ctx context.Context, cfg config) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
	})
	if err != nil {
		return fmt.Errorf("hub: %w", err)
	}

	httpSrv := &http.Server{Addr: cfg.listenHTTP, Handler: h.HTTPHandler()}
	grpcSrv := grpc.NewServer()
	h.RegisterLogService(grpcSrv)

	errc := make(chan error, 2)
	go func() {
		log.Printf("bundle HTTP listening on %s", cfg.listenHTTP)
		errc <- httpSrv.ListenAndServe()
	}()
	go func() {
		lis, err := net.Listen("tcp", cfg.listenGRPC)
		if err != nil {
			errc <- err
			return
		}
		log.Printf("log ingest gRPC listening on %s", cfg.listenGRPC)
		errc <- grpcSrv.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
		grpcSrv.GracefulStop()
		return httpSrv.Shutdown(context.Background())
	case err := <-errc:
		return err
	}
}
