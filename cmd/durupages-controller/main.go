// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command durupages-controller runs the control plane. Default assembly:
// PostgreSQL PageProvider + in-memory Queue + default Scaler + Kubernetes
// PodManager.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/durupages/durupages/internal/version"
	"github.com/durupages/durupages/pkg/controller"
	"github.com/durupages/durupages/pkg/provider/postgres"
	"github.com/durupages/durupages/pkg/queue/inmemory"
	"github.com/durupages/durupages/pkg/scaler/defaultscaler"
	"github.com/durupages/durupages/pkg/workerauth"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	version.MaybePrint()
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
	)
	flag.Parse()
	if err := run(*listen, *pgDSN, *pagesDomain, *migrate, *signingKeyPEM, *controllerAddr, *hubAddr, *hubLogAddr,
		*kubeconfig, *workerNamespace, *workerImage, *workerSA, *workerCPULimit, *workerMemLimit,
		controller.Defaults{
			QueueTimeout: *defQueueTimeout, MaxQueueTimeout: *maxQueueTimeout,
			RequestTimeout: *defRequestTimeout, MaxConcurrency: *defMaxConcurrency,
			MaxConcurrencyPerPod: *maxConcPerPod, TargetConcurrencyPerPod: *targetConcPerPod,
			IdleTTL: *defIdleTTL,
		}, *bundleMinIdle, *bundleCacheMax); err != nil {
		log.Fatalf("durupages-controller: %v", err)
	}
}

func run(listen, pgDSN, pagesDomain string, migrate bool, signingKeyPEM, controllerAddr, hubAddr, hubLogAddr,
	kubeconfig, workerNamespace, workerImage, workerSA, cpuLimit, memLimit string,
	defaults controller.Defaults, bundleMinIdle, bundleCacheMax string) error {

	if pgDSN == "" || signingKeyPEM == "" || controllerAddr == "" || hubAddr == "" || workerImage == "" {
		return fmt.Errorf("--pg-dsn, --worker-jwt-signing-key, --controller-addr, --hub-addr and --worker-image are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	keyData, err := os.ReadFile(signingKeyPEM)
	if err != nil {
		return fmt.Errorf("read signing key: %w", err)
	}
	priv, err := workerauth.ParsePrivateKeyPEM(keyData)
	if err != nil {
		return fmt.Errorf("parse signing key: %w", err)
	}

	prov, err := postgres.New(ctx, postgres.Options{DSN: pgDSN, PagesDomain: pagesDomain})
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer prov.Close()
	if migrate {
		if err := prov.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	var restCfg *rest.Config
	if kubeconfig != "" {
		restCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
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
		Namespace:          workerNamespace,
		Image:              workerImage,
		ServiceAccountName: workerSA,
		Generation:         fmt.Sprintf("g%d", time.Now().Unix()),
		DefaultCPULimit:    cpuLimit,
		DefaultMemLimit:    memLimit,
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
		Defaults:            defaults,
		ControllerAddr:      controllerAddr,
		HubAddr:             hubAddr,
		HubLogAddr:          hubLogAddr,
		BundleMinIdle:       bundleMinIdle,
		BundleCacheMaxBytes: bundleCacheMax,
	})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	grpcSrv := grpc.NewServer()
	ctrl.RegisterServices(grpcSrv)

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- grpcSrv.Serve(lis) }()
	go func() { errc <- ctrl.Run(ctx) }()
	log.Printf("controller listening on %s", listen)

	select {
	case <-ctx.Done():
		grpcSrv.GracefulStop()
		return nil
	case err := <-errc:
		return err
	}
}
