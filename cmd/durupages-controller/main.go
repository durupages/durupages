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
	"errors"
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
	"google.golang.org/grpc/credentials"
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
	"github.com/durupages/durupages/pkg/tlsconf"
	"github.com/durupages/durupages/pkg/workerauth"
)

// getenv is the environment lookup used while resolving configuration. It is a
// parameter rather than a direct os.Getenv call so parseConfig is testable.
type getenv func(string) string

func envOr(env getenv, key, def string) string {
	if v := env(key); v != "" {
		return v
	}
	return def
}

func envOrInt64(env getenv, key string, def int64) int64 {
	if v := env(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// envBool reads a boolean env var, reporting whether it was set at all so a
// caller can tell "explicitly false" from "not configured".
func envBool(env getenv, key string) (value, set bool) {
	v := env(key)
	if v == "" {
		return false, false
	}
	return v == "true" || v == "1", true
}

// config is the resolved controller configuration.
type config struct {
	listen        string
	pgDSN         string
	pagesDomain   string
	migrate       bool
	signingKeyPEM string

	// TLS for this process's own listeners. Empty pair = plaintext.
	tlsCertFile string
	tlsKeyFile  string
	// Admin API certificate, defaulted to the pair above.
	adminTLSCertFile string
	adminTLSKeyFile  string

	// Advertised addresses: what workers are told to dial, which is not
	// necessarily what this process binds (--listen) -- a worker reaches the
	// controller through a Service, not through the controller's own bind
	// address.
	controllerAdvertiseAddr string
	hubAdvertiseAddr        string
	hubLogAdvertiseAddr     string

	// Advertised TLS facts about those endpoints, likewise propagated to
	// workers rather than used here.
	workerCACertFile       string
	advertiseControllerTLS bool
	advertiseHubLogTLS     bool
	controllerAdvertiseSNI string
	hubAdvertiseSNI        string
	hubLogAdvertiseSNI     string

	kubeconfig      string
	workerNamespace string
	workerImage     string
	workerSA        string
	workerCPULimit  string
	workerMemLimit  string
	// workerAnnotationsFile holds cluster-wide worker pod annotations as a YAML
	// string map. Empty = no common annotations.
	workerAnnotationsFile string

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
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return // -h already printed the usage
	}
	if err != nil {
		fatalf("durupages-controller: %v", err)
	}
	if err := run(cfg); err != nil {
		fatalf("durupages-controller: %v", err)
	}
}

// renamedEnv lists environment variables the controller no longer reads.
//
// DURUPAGES_HUB_ADDR / DURUPAGES_HUB_LOG_ADDR kept their names in the worker
// pod environment -- a worker really does dial those -- but as controller input
// they only ever described what to advertise, which the ADVERTISE names now say
// outright. Since the two sets of names would otherwise be
// indistinguishable in a manifest, an old name left behind is rejected rather
// than ignored: silently ignoring DURUPAGES_HUB_LOG_ADDR would drop workers
// into pod-log mode with no clue why, and the failure would only surface as
// missing logs long after the upgrade.
var renamedEnv = []struct{ old, new string }{
	{"DURUPAGES_HUB_ADDR", "DURUPAGES_HUB_ADVERTISE_ADDR"},
	{"DURUPAGES_HUB_LOG_ADDR", "DURUPAGES_HUB_LOG_ADVERTISE_ADDR"},
}

// parseConfig resolves flags (highest priority), environment and defaults into
// a config. It returns an error instead of exiting so it can be tested.
func parseConfig(args []string, env getenv) (config, error) {
	fs := flag.NewFlagSet("durupages-controller", flag.ContinueOnError)
	var (
		listen        = fs.String("listen", envOr(env, "DURUPAGES_LISTEN", ":9440"), "gRPC listen address")
		pgDSN         = fs.String("pg-dsn", envOr(env, "DURUPAGES_PG_DSN", ""), "PostgreSQL DSN (required)")
		pagesDomain   = fs.String("pages-domain", envOr(env, "DURUPAGES_PAGES_DOMAIN", "pages.local"), "pages domain")
		migrate       = fs.Bool("migrate", envOr(env, "DURUPAGES_MIGRATE", "true") == "true", "apply schema migrations on start")
		signingKeyPEM = fs.String("worker-jwt-signing-key", envOr(env, "DURUPAGES_WORKER_JWT_SIGNING_KEY", ""), "path to ed25519 private key PEM (required)")

		tlsCertFile      = fs.String("tls-cert-file", envOr(env, "DURUPAGES_TLS_CERT_FILE", ""), "PEM certificate for the gRPC listener (empty = plaintext)")
		tlsKeyFile       = fs.String("tls-key-file", envOr(env, "DURUPAGES_TLS_KEY_FILE", ""), "PEM private key for the gRPC listener")
		adminTLSCertFile = fs.String("admin-tls-cert-file", envOr(env, "DURUPAGES_ADMIN_TLS_CERT_FILE", ""), "PEM certificate for the admin API (empty = reuse --tls-cert-file)")
		adminTLSKeyFile  = fs.String("admin-tls-key-file", envOr(env, "DURUPAGES_ADMIN_TLS_KEY_FILE", ""), "PEM private key for the admin API (empty = reuse --tls-key-file)")

		controllerAdvertiseAddr = fs.String("controller-addr", envOr(env, "DURUPAGES_CONTROLLER_ADVERTISE_ADDR", ""), "address workers use to reach this controller (required)")
		hubAdvertiseAddr        = fs.String("hub-advertise-addr", envOr(env, "DURUPAGES_HUB_ADVERTISE_ADDR", ""), "hub bundle URL propagated to workers; an https:// scheme is how workers learn the hub speaks TLS (required)")
		hubLogAdvertiseAddr     = fs.String("hub-log-advertise-addr", envOr(env, "DURUPAGES_HUB_LOG_ADVERTISE_ADDR", ""), "hub log ingest address propagated to workers (empty = pod-log mode)")

		// These five carry the worker-facing env names unchanged: the controller
		// does not use them, it restates them verbatim to the pods it creates,
		// and a second spelling for the same value would only invite the two to
		// drift apart in a manifest. The flags say "advertise" because on a
		// command line there is no such context.
		workerCACertFile       = fs.String("worker-ca-cert-file", envOr(env, "DURUPAGES_WORKER_CA_CERT_FILE", ""), "PEM CA bundle handed to worker pods as DURUPAGES_CA_CERT_PEM; re-read per pod creation so rotations reach new pods")
		advertiseHubLogTLS     = fs.Bool("advertise-hub-log-tls", boolDefault(env, "DURUPAGES_HUB_LOG_TLS", false), "tell workers the hub log endpoint serves TLS")
		controllerAdvertiseSNI = fs.String("advertise-controller-server-name", envOr(env, "DURUPAGES_CONTROLLER_SERVER_NAME", ""), "server name workers verify in the controller certificate (empty = derived from the advertised address)")
		hubAdvertiseSNI        = fs.String("advertise-hub-server-name", envOr(env, "DURUPAGES_HUB_SERVER_NAME", ""), "server name workers verify in the hub certificate (empty = derived)")
		hubLogAdvertiseSNI     = fs.String("advertise-hub-log-server-name", envOr(env, "DURUPAGES_HUB_LOG_SERVER_NAME", ""), "server name workers verify in the hub log certificate (empty = derived)")
		// Default resolved after parsing: see below.
		advertiseControllerTLS = fs.Bool("advertise-controller-tls", false, "tell workers this controller serves TLS (default: whether --tls-cert-file is set)")

		kubeconfig      = fs.String("kubeconfig", envOr(env, "KUBECONFIG", ""), "kubeconfig path (empty = in-cluster)")
		workerNamespace = fs.String("worker-namespace", envOr(env, "DURUPAGES_WORKER_NAMESPACE", "durupages-workers"), "namespace for worker pods")
		workerImage     = fs.String("worker-image", envOr(env, "DURUPAGES_WORKER_IMAGE", ""), "worker image (shim + workerd) (required)")
		workerSA        = fs.String("worker-service-account", envOr(env, "DURUPAGES_WORKER_SA", "durupages-worker-noperm"), "worker pod service account")
		workerCPULimit  = fs.String("worker-cpu-limit", envOr(env, "DURUPAGES_WORKER_CPU_LIMIT", "1"), "default worker pod CPU limit")
		workerMemLimit  = fs.String("worker-mem-limit", envOr(env, "DURUPAGES_WORKER_MEM_LIMIT", "512Mi"), "default worker pod memory limit")
		// Read once at startup: the file is a mounted ConfigMap whose change
		// restarts this pod anyway.
		workerAnnotationsFile = fs.String("worker-annotations-file", envOr(env, "DURUPAGES_WORKER_ANNOTATIONS_FILE", ""),
			"YAML file of annotations (string map) applied to every worker pod; these win over a tenant's own PodAnnotations (empty = none)")

		defQueueTimeout   = fs.Duration("default-queue-timeout", 30*time.Second, "default per-page queue timeout")
		maxQueueTimeout   = fs.Duration("max-queue-timeout", 120*time.Second, "max per-page queue timeout")
		defRequestTimeout = fs.Duration("default-request-timeout", 60*time.Second, "default request timeout")
		defMaxConcurrency = fs.Int("default-max-concurrency", 5, "default max worker pods per tenant")
		maxConcPerPod     = fs.Int("max-concurrency-per-pod", 256, "per-pod in-flight hard cap")
		targetConcPerPod  = fs.Int("target-concurrency-per-pod", 32, "per-pod scale-up target concurrency")
		defIdleTTL        = fs.Duration("default-idle-ttl", 60*time.Second, "default idle TTL before scale-down")

		bundleMinIdle  = fs.String("worker-bundle-min-idle", envOr(env, "DURUPAGES_BUNDLE_MIN_IDLE", ""), "worker bundle LRU min idle (propagated)")
		bundleCacheMax = fs.String("worker-bundle-cache-max-bytes", envOr(env, "DURUPAGES_BUNDLE_CACHE_MAX_BYTES", ""), "worker bundle cache limit (propagated)")

		adminEnabled   = fs.Bool("admin-enabled", envOr(env, "DURUPAGES_ADMIN_ENABLED", "false") == "true", "enable the unauthenticated admin API on --admin-listen")
		adminListen    = fs.String("admin-listen", envOr(env, "DURUPAGES_ADMIN_LISTEN", ":9450"), "admin API listen address (separate port)")
		adminMaxUpload = fs.Int64("admin-max-upload-bytes", envOrInt64(env, "DURUPAGES_ADMIN_MAX_UPLOAD_BYTES", 512<<20), "admin API deployment upload size limit")
		adminTempDir   = fs.String("admin-temp-dir", envOr(env, "DURUPAGES_ADMIN_TEMP_DIR", ""), "directory for admin API upload extraction (empty = os.TempDir(); set to a writable volume when the root filesystem is read-only)")

		s3Endpoint  = fs.String("s3-endpoint", envOr(env, "DURUPAGES_S3_ENDPOINT", ""), "S3 endpoint (admin API uploads)")
		s3Region    = fs.String("s3-region", envOr(env, "DURUPAGES_S3_REGION", "us-east-1"), "S3 region (admin API uploads)")
		s3Bucket    = fs.String("s3-bucket", envOr(env, "DURUPAGES_S3_BUCKET", ""), "S3 bucket (required when the admin API is enabled)")
		s3AccessKey = fs.String("s3-access-key", envOr(env, "DURUPAGES_S3_ACCESS_KEY", ""), "S3 access key (admin API uploads)")
		s3SecretKey = fs.String("s3-secret-key", envOr(env, "DURUPAGES_S3_SECRET_KEY", ""), "S3 secret key (admin API uploads)")
		s3PathStyle = fs.Bool("s3-path-style", envOr(env, "DURUPAGES_S3_PATH_STYLE", "true") == "true", "path-style S3 addressing (admin API uploads)")
	)
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	for _, r := range renamedEnv {
		if env(r.old) != "" {
			return config{}, fmt.Errorf("%s is no longer read by the controller: rename it to %s "+
				"(%s remains the name injected into worker pods)", r.old, r.new, r.old)
		}
	}

	// Whether the controller serves TLS to workers is nearly always "yes if it
	// serves TLS at all", so derive it and let the flag override -- a TLS
	// terminating proxy in front of the controller is the case that needs the
	// override, in either direction.
	if !flagSet(fs, "advertise-controller-tls") {
		if v, ok := envBool(env, "DURUPAGES_CONTROLLER_TLS"); ok {
			*advertiseControllerTLS = v
		} else {
			*advertiseControllerTLS = *tlsCertFile != ""
		}
	}

	// The admin API shares the gRPC certificate unless given its own; a lone
	// admin cert or key is a typo, not a configuration.
	adminCert, adminKey := *adminTLSCertFile, *adminTLSKeyFile
	if adminCert == "" && adminKey == "" {
		adminCert, adminKey = *tlsCertFile, *tlsKeyFile
	}
	if err := checkPair("--tls-cert-file", *tlsCertFile, "--tls-key-file", *tlsKeyFile); err != nil {
		return config{}, err
	}
	if err := checkPair("--admin-tls-cert-file", adminCert, "--admin-tls-key-file", adminKey); err != nil {
		return config{}, err
	}

	cfg := config{
		listen: *listen, pgDSN: *pgDSN, pagesDomain: *pagesDomain, migrate: *migrate,
		signingKeyPEM: *signingKeyPEM,
		tlsCertFile:   *tlsCertFile, tlsKeyFile: *tlsKeyFile,
		adminTLSCertFile: adminCert, adminTLSKeyFile: adminKey,
		controllerAdvertiseAddr: *controllerAdvertiseAddr,
		hubAdvertiseAddr:        *hubAdvertiseAddr,
		hubLogAdvertiseAddr:     *hubLogAdvertiseAddr,
		workerCACertFile:        *workerCACertFile,
		advertiseControllerTLS:  *advertiseControllerTLS,
		advertiseHubLogTLS:      *advertiseHubLogTLS,
		controllerAdvertiseSNI:  *controllerAdvertiseSNI,
		hubAdvertiseSNI:         *hubAdvertiseSNI,
		hubLogAdvertiseSNI:      *hubLogAdvertiseSNI,
		kubeconfig:              *kubeconfig, workerNamespace: *workerNamespace, workerImage: *workerImage,
		workerSA: *workerSA, workerCPULimit: *workerCPULimit, workerMemLimit: *workerMemLimit,
		workerAnnotationsFile: *workerAnnotationsFile,
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
	if cfg.pgDSN == "" || cfg.signingKeyPEM == "" || cfg.controllerAdvertiseAddr == "" ||
		cfg.hubAdvertiseAddr == "" || cfg.workerImage == "" {
		return config{}, fmt.Errorf("--pg-dsn, --worker-jwt-signing-key, --controller-addr, " +
			"--hub-advertise-addr and --worker-image are required")
	}
	return cfg, nil
}

// flagSet reports whether name was given on the command line, as opposed to
// carrying its default.
func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// boolDefault is the env-derived default for a boolean flag.
func boolDefault(env getenv, key string, def bool) bool {
	if v, ok := envBool(env, key); ok {
		return v
	}
	return def
}

// checkPair rejects a half-configured certificate: one of the two files alone
// cannot serve TLS, and treating it as "TLS off" would silently serve
// plaintext on a listener the operator meant to protect.
func checkPair(certFlag, cert, keyFlag, key string) error {
	if (cert == "") != (key == "") {
		return fmt.Errorf("%s and %s must be set together", certFlag, keyFlag)
	}
	return nil
}

func run(cfg config) error {
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
	// An unreadable or malformed annotations file is an operator mistake, and
	// refusing to start says so far more clearly than worker pods that come up
	// without the annotations the operator declared.
	var commonAnnotations map[string]string
	if cfg.workerAnnotationsFile != "" {
		if commonAnnotations, err = controller.LoadWorkerAnnotationsFile(cfg.workerAnnotationsFile); err != nil {
			return err
		}
		log.Printf("worker annotations: %d common annotation(s) from %s",
			len(commonAnnotations), cfg.workerAnnotationsFile)
	}

	pods, err := controller.NewKubePods(controller.KubePodsOptions{
		Client:             clientset,
		Namespace:          cfg.workerNamespace,
		Image:              cfg.workerImage,
		ServiceAccountName: cfg.workerSA,
		Generation:         fmt.Sprintf("g%d", time.Now().Unix()),
		DefaultCPULimit:    cfg.workerCPULimit,
		DefaultMemLimit:    cfg.workerMemLimit,
		CommonAnnotations:  commonAnnotations,
	})
	if err != nil {
		return fmt.Errorf("kubepods: %w", err)
	}

	ctrl, err := controller.New(controller.Options{
		Provider:             prov,
		Queue:                inmemory.New(),
		Scaler:               defaultscaler.New(),
		Pods:                 pods,
		SigningKey:           priv,
		Defaults:             cfg.defaults,
		ControllerAddr:       cfg.controllerAdvertiseAddr,
		HubAddr:              cfg.hubAdvertiseAddr,
		HubLogAddr:           cfg.hubLogAdvertiseAddr,
		WorkerCACertFile:     cfg.workerCACertFile,
		ControllerTLS:        cfg.advertiseControllerTLS,
		HubLogTLS:            cfg.advertiseHubLogTLS,
		ControllerServerName: cfg.controllerAdvertiseSNI,
		HubServerName:        cfg.hubAdvertiseSNI,
		HubLogServerName:     cfg.hubLogAdvertiseSNI,
		BundleMinIdle:        cfg.bundleMinIdle,
		BundleCacheMaxBytes:  cfg.bundleCacheMax,
	})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	var grpcOpts []grpc.ServerOption
	if cfg.tlsCertFile != "" {
		// tlsconf.ServerConfig re-reads the pair when it changes on disk, so a
		// cert-manager renewal takes effect without restarting the controller.
		tlsCfg, err := tlsconf.ServerConfig(cfg.tlsCertFile, cfg.tlsKeyFile)
		if err != nil {
			return fmt.Errorf("grpc tls: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	ctrl.RegisterServices(grpcSrv)

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	errc := make(chan error, 3)
	go func() { errc <- grpcSrv.Serve(lis) }()
	go func() { errc <- ctrl.Run(ctx) }()
	log.Printf("controller listening on %s (tls=%t, advertising controller tls=%t to workers)",
		cfg.listen, cfg.tlsCertFile != "", cfg.advertiseControllerTLS)

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
		// Reported on every upload so `duru deploy` prints the URL this
		// controller actually serves, rather than the client's own default.
		PagesDomain: cfg.pagesDomain,
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
	if cfg.adminTLSCertFile != "" {
		tlsCfg, err := tlsconf.ServerConfig(cfg.adminTLSCertFile, cfg.adminTLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("admin api tls: %w", err)
		}
		srv.TLSConfig = tlsCfg
	}
	go func() {
		log.Printf("admin API listening on %s (tls=%t, UNAUTHENTICATED — keep this port private)",
			cfg.adminListen, srv.TLSConfig != nil)
		var err error
		if srv.TLSConfig != nil {
			// Empty file names: the certificate comes from TLSConfig, which
			// reloads it on renewal instead of pinning what was on disk at
			// startup.
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	return srv, nil
}

// fatalf reports a startup failure and exits. It exists because log.Fatalf
// routes through the slog bridge installed by setupLogging and lands at INFO,
// filing the failure that kills the process under the same level as routine
// progress -- exactly where an operator scanning for errors will not look.
func fatalf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
