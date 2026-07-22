// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command durupages-controller runs the control plane. Default assembly:
// PostgreSQL PageProvider + in-memory Queue + default Scaler + Kubernetes
// PodManager.
//
// It optionally exposes the admin API (tenant/page/deployment management and
// deployment upload) on a SEPARATE port, enabled with DURUPAGES_ADMIN_ENABLED.
// That API is unauthenticated by design — bind it to a private network.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/durupages/durupages/internal/version"
	"github.com/durupages/durupages/pkg/adminapi"
	"github.com/durupages/durupages/pkg/controller"
	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/provider/postgres"
	"github.com/durupages/durupages/pkg/queue/inmemory"
	"github.com/durupages/durupages/pkg/scaler/defaultscaler"
	"github.com/durupages/durupages/pkg/storage"
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

// config is the resolved controller configuration.
type config struct {
	listen        string
	pgDSN         string
	pagesDomain   string
	migrate       bool
	signingKeyPEM string

	controllerAddr string
	hubAddr        string
	hubLogAddr     string

	kubeconfig      string
	workerNamespace string
	workerImage     string
	workerSA        string
	workerCPULimit  string
	workerMemLimit  string

	defaults       controller.Defaults
	bundleMinIdle  string
	bundleCacheMax string

	// Admin API (optional, separate port, unauthenticated).
	adminEnabled   bool
	adminListen    string
	adminMaxUpload int64
	adminTempDir   string
	s3             s3.Options
}

// setupLogging installs a JSON slog handler on stderr as the process default.
//
// slog.SetDefault also redirects the standard log package through this
// handler, so the log.Printf calls in this binary and the admin API's
// structured request lines end up in one consistent JSON stream instead of two
// competing formats.
func setupLogging() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
}

func main() {
	version.MaybePrint()
	setupLogging()
	var (
		listen        = flag.String("listen", envOr("DURUPAGES_LISTEN", ":9440"), "gRPC listen address")
		pgDSN         = flag.String("pg-dsn", envOr("DURUPAGES_PG_DSN", ""), "PostgreSQL DSN (required)")
		pagesDomain   = flag.String("pages-domain", envOr("DURUPAGES_PAGES_DOMAIN", "pages.local"), "pages domain")
		migrate       = flag.Bool("migrate", envOr("DURUPAGES_MIGRATE", "true") == "true", "apply schema migrations on start")
		signingKeyPEM = flag.String("worker-jwt-signing-key", envOr("DURUPAGES_WORKER_JWT_SIGNING_KEY", ""), "path to ed25519 private key PEM (required)")

		controllerAddr = flag.String("controller-addr", envOr("DURUPAGES_CONTROLLER_ADVERTISE_ADDR", ""), "address workers use to reach this controller (required)")
		hubAddr        = flag.String("hub-addr", envOr("DURUPAGES_HUB_ADDR", ""), "hub bundle address propagated to workers (required)")
		hubLogAddr     = flag.String("hub-log-addr", envOr("DURUPAGES_HUB_LOG_ADDR", ""), "hub log ingest address propagated to workers (empty = pod-log mode)")

		kubeconfig      = flag.String("kubeconfig", envOr("KUBECONFIG", ""), "kubeconfig path (empty = in-cluster)")
		workerNamespace = flag.String("worker-namespace", envOr("DURUPAGES_WORKER_NAMESPACE", "durupages-workers"), "namespace for worker pods")
		workerImage     = flag.String("worker-image", envOr("DURUPAGES_WORKER_IMAGE", ""), "worker image (shim + workerd) (required)")
		workerSA        = flag.String("worker-service-account", envOr("DURUPAGES_WORKER_SA", "durupages-worker-noperm"), "worker pod service account")
		workerCPULimit  = flag.String("worker-cpu-limit", envOr("DURUPAGES_WORKER_CPU_LIMIT", "1"), "default worker pod CPU limit")
		workerMemLimit  = flag.String("worker-mem-limit", envOr("DURUPAGES_WORKER_MEM_LIMIT", "512Mi"), "default worker pod memory limit")

		defQueueTimeout   = flag.Duration("default-queue-timeout", 30*time.Second, "default per-page queue timeout")
		maxQueueTimeout   = flag.Duration("max-queue-timeout", 120*time.Second, "max per-page queue timeout")
		defRequestTimeout = flag.Duration("default-request-timeout", 60*time.Second, "default request timeout")
		defMaxConcurrency = flag.Int("default-max-concurrency", 5, "default max worker pods per tenant")
		maxConcPerPod     = flag.Int("max-concurrency-per-pod", 256, "per-pod in-flight hard cap")
		targetConcPerPod  = flag.Int("target-concurrency-per-pod", 32, "per-pod scale-up target concurrency")
		defIdleTTL        = flag.Duration("default-idle-ttl", 60*time.Second, "default idle TTL before scale-down")

		bundleMinIdle  = flag.String("worker-bundle-min-idle", envOr("DURUPAGES_BUNDLE_MIN_IDLE", ""), "worker bundle LRU min idle (propagated)")
		bundleCacheMax = flag.String("worker-bundle-cache-max-bytes", envOr("DURUPAGES_BUNDLE_CACHE_MAX_BYTES", ""), "worker bundle cache limit (propagated)")

		adminEnabled   = flag.Bool("admin-enabled", envOr("DURUPAGES_ADMIN_ENABLED", "false") == "true", "enable the unauthenticated admin API on --admin-listen")
		adminListen    = flag.String("admin-listen", envOr("DURUPAGES_ADMIN_LISTEN", ":9450"), "admin API listen address (separate port)")
		adminMaxUpload = flag.Int64("admin-max-upload-bytes", envOrInt64("DURUPAGES_ADMIN_MAX_UPLOAD_BYTES", 512<<20), "admin API deployment upload size limit")
		adminTempDir   = flag.String("admin-temp-dir", envOr("DURUPAGES_ADMIN_TEMP_DIR", ""), "directory for admin API upload extraction (empty = os.TempDir(); set to a writable volume when the root filesystem is read-only)")

		s3Endpoint  = flag.String("s3-endpoint", envOr("DURUPAGES_S3_ENDPOINT", ""), "S3 endpoint (admin API uploads)")
		s3Region    = flag.String("s3-region", envOr("DURUPAGES_S3_REGION", "us-east-1"), "S3 region (admin API uploads)")
		s3Bucket    = flag.String("s3-bucket", envOr("DURUPAGES_S3_BUCKET", ""), "S3 bucket (required when the admin API is enabled)")
		s3AccessKey = flag.String("s3-access-key", envOr("DURUPAGES_S3_ACCESS_KEY", ""), "S3 access key (admin API uploads)")
		s3SecretKey = flag.String("s3-secret-key", envOr("DURUPAGES_S3_SECRET_KEY", ""), "S3 secret key (admin API uploads)")
		s3PathStyle = flag.Bool("s3-path-style", envOr("DURUPAGES_S3_PATH_STYLE", "true") == "true", "path-style S3 addressing (admin API uploads)")
	)
	flag.Parse()

	cfg := config{
		listen: *listen, pgDSN: *pgDSN, pagesDomain: *pagesDomain, migrate: *migrate,
		signingKeyPEM:  *signingKeyPEM,
		controllerAddr: *controllerAddr, hubAddr: *hubAddr, hubLogAddr: *hubLogAddr,
		kubeconfig: *kubeconfig, workerNamespace: *workerNamespace, workerImage: *workerImage,
		workerSA: *workerSA, workerCPULimit: *workerCPULimit, workerMemLimit: *workerMemLimit,
		defaults: controller.Defaults{
			QueueTimeout: *defQueueTimeout, MaxQueueTimeout: *maxQueueTimeout,
			RequestTimeout: *defRequestTimeout, MaxConcurrency: *defMaxConcurrency,
			MaxConcurrencyPerPod: *maxConcPerPod, TargetConcurrencyPerPod: *targetConcPerPod,
			IdleTTL: *defIdleTTL,
		},
		bundleMinIdle: *bundleMinIdle, bundleCacheMax: *bundleCacheMax,
		adminEnabled: *adminEnabled, adminListen: *adminListen, adminMaxUpload: *adminMaxUpload,
		adminTempDir: *adminTempDir,
		s3: s3.Options{Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
			AccessKey: *s3AccessKey, SecretKey: *s3SecretKey, UsePathStyle: *s3PathStyle},
	}
	if err := run(cfg); err != nil {
		log.Fatalf("durupages-controller: %v", err)
	}
}

func run(cfg config) error {
	if cfg.pgDSN == "" || cfg.signingKeyPEM == "" || cfg.controllerAddr == "" || cfg.hubAddr == "" || cfg.workerImage == "" {
		return fmt.Errorf("--pg-dsn, --worker-jwt-signing-key, --controller-addr, --hub-addr and --worker-image are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	keyData, err := os.ReadFile(cfg.signingKeyPEM)
	if err != nil {
		return fmt.Errorf("read signing key: %w", err)
	}
	priv, err := workerauth.ParsePrivateKeyPEM(keyData)
	if err != nil {
		return fmt.Errorf("parse signing key: %w", err)
	}

	prov, err := postgres.New(ctx, postgres.Options{DSN: cfg.pgDSN, PagesDomain: cfg.pagesDomain})
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer prov.Close()
	if cfg.migrate {
		if err := prov.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	var restCfg *rest.Config
	if cfg.kubeconfig != "" {
		restCfg, err = clientcmd.BuildConfigFromFlags("", cfg.kubeconfig)
	} else {
		restCfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}
	pods, err := controller.NewKubePods(controller.KubePodsOptions{
		Client:             clientset,
		Namespace:          cfg.workerNamespace,
		Image:              cfg.workerImage,
		ServiceAccountName: cfg.workerSA,
		Generation:         fmt.Sprintf("g%d", time.Now().Unix()),
		DefaultCPULimit:    cfg.workerCPULimit,
		DefaultMemLimit:    cfg.workerMemLimit,
	})
	if err != nil {
		return fmt.Errorf("kubepods: %w", err)
	}

	ctrl, err := controller.New(controller.Options{
		Provider:            prov,
		Queue:               inmemory.New(),
		Scaler:              defaultscaler.New(),
		Pods:                pods,
		SigningKey:          priv,
		Defaults:            cfg.defaults,
		ControllerAddr:      cfg.controllerAddr,
		HubAddr:             cfg.hubAddr,
		HubLogAddr:          cfg.hubLogAddr,
		BundleMinIdle:       cfg.bundleMinIdle,
		BundleCacheMaxBytes: cfg.bundleCacheMax,
	})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	grpcSrv := grpc.NewServer()
	ctrl.RegisterServices(grpcSrv)

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	errc := make(chan error, 3)
	go func() { errc <- grpcSrv.Serve(lis) }()
	go func() { errc <- ctrl.Run(ctx) }()
	log.Printf("controller listening on %s", cfg.listen)

	adminSrv, err := startAdminAPI(ctx, cfg, prov, errc)
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		grpcSrv.GracefulStop()
		if adminSrv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = adminSrv.Shutdown(shutdownCtx)
		}
		return nil
	case err := <-errc:
		return err
	}
}

// startAdminAPI serves the admin API on its own port when enabled. It returns
// nil when the API is disabled. Storage is only required here: the control
// plane itself never touches Storage.
func startAdminAPI(ctx context.Context, cfg config, prov *postgres.Provider, errc chan<- error) (*http.Server, error) {
	if !cfg.adminEnabled {
		return nil, nil
	}
	if cfg.s3.Bucket == "" {
		return nil, fmt.Errorf("--s3-bucket is required when the admin API is enabled")
	}
	var store storage.Storage
	store, err := s3.New(ctx, cfg.s3)
	if err != nil {
		return nil, fmt.Errorf("admin api s3: %w", err)
	}

	var admin provider.AdminProvider = prov
	h, err := adminapi.New(adminapi.Options{
		Provider:       prov,
		Admin:          admin,
		Storage:        store,
		MaxUploadBytes: cfg.adminMaxUpload,
		// New rejects an unusable TempDir right here, so a controller that
		// cannot extract uploads (read-only root filesystem, no writable
		// volume) fails to start instead of failing on the first deployment.
		TempDir: cfg.adminTempDir,
		// Same handler as the rest of the binary: one JSON stream on stderr.
		Logger: slog.Default().With("component", "adminapi"),
	})
	if err != nil {
		return nil, fmt.Errorf("admin api: %w", err)
	}

	srv := &http.Server{Addr: cfg.adminListen, Handler: h}
	go func() {
		log.Printf("admin API listening on %s (UNAUTHENTICATED — keep this port private)", cfg.adminListen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	return srv, nil
}
