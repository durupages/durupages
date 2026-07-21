// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command durupages-worker-shim is PID 1 of a worker pod: it registers with
// the controller, lazily loads page bundles from the hub, runs the workerd
// runtime with graceful swaps, and emits per-request usage events.
//
// All configuration arrives via environment variables injected by the
// controller at pod creation.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/durupages/durupages/internal/version"
	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/runtime/workerdruntime"
	"github.com/durupages/durupages/pkg/shim"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("warning: invalid %s=%q, using %s", key, os.Getenv(key), def)
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		log.Printf("warning: invalid %s=%q, using %d", key, os.Getenv(key), def)
	}
	return def
}

// proxyAdvertiseAddr returns the "<podIP>:8080" the shim should bind its request
// proxy to and advertise to the controller. It prefers an explicit
// DURUPAGES_PROXY_ADDR override, otherwise it picks the first non-loopback IPv4
// interface address (the pod IP inside Kubernetes). It falls back to ":8080"
// when no such interface exists (e.g. unit/dev runs), which binds every
// interface as before.
func proxyAdvertiseAddr() string {
	if v := os.Getenv("DURUPAGES_PROXY_ADDR"); v != "" {
		return v
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ":8080"
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		if v4 := ipn.IP.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), "8080")
		}
	}
	return ":8080"
}

func main() {
	version.MaybePrint()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tenantID := os.Getenv("DURUPAGES_TENANT_ID")
	podName := os.Getenv("DURUPAGES_POD_NAME")
	controllerAddr := os.Getenv("DURUPAGES_CONTROLLER_ADDR")
	hubAddr := os.Getenv("DURUPAGES_HUB_ADDR")
	workerJWT := os.Getenv("DURUPAGES_WORKER_JWT")
	leasePub := os.Getenv("DURUPAGES_LEASE_PUBKEY")
	if tenantID == "" || podName == "" || controllerAddr == "" || hubAddr == "" || workerJWT == "" || leasePub == "" {
		log.Fatal("durupages-worker-shim: DURUPAGES_TENANT_ID/POD_NAME/CONTROLLER_ADDR/HUB_ADDR/WORKER_JWT/LEASE_PUBKEY are required")
	}
	// The controller encodes the key with raw (unpadded) std encoding.
	pubRaw, err := base64.RawStdEncoding.DecodeString(leasePub)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		log.Fatalf("durupages-worker-shim: invalid DURUPAGES_LEASE_PUBKEY: %v", err)
	}

	bundleDir := envOr("DURUPAGES_BUNDLE_DIR", "/bundles")
	workDir := envOr("DURUPAGES_RUNTIME_WORKDIR", bundleDir+"/.runtime")
	rt := workerdruntime.New(workerdruntime.Options{
		WorkerdBin: envOr("DURUPAGES_WORKERD_BIN", "workerd"),
		WorkDir:    workDir,
		DropToUID:  int(envInt64("DURUPAGES_WORKERD_UID", 0)),
		DropToGID:  int(envInt64("DURUPAGES_WORKERD_GID", 0)),
	})

	opts := shim.Options{
		TenantID:       tenantID,
		PodName:        podName,
		ControllerAddr: controllerAddr,
		HubAddr:        hubAddr,
		WorkerJWT:      workerJWT,
		LeasePubKey:    ed25519.PublicKey(pubRaw),
		Runtime:        rt,
		BundleDir:      bundleDir,
		// The shim advertises this address to the controller (Register), which
		// hands it to the router as the lease endpoint. The controller does not
		// inject the pod IP, so the shim discovers its own routable pod IP here
		// and binds the proxy listener to it. Without this the default ":8080"
		// listener advertises the unroutable wildcard "[::]:8080".
		ProxyAddr:     proxyAdvertiseAddr(),
		MinIdle:       envDuration("DURUPAGES_BUNDLE_MIN_IDLE", time.Hour),
		CacheMaxBytes: envInt64("DURUPAGES_BUNDLE_CACHE_MAX_BYTES", 2<<30),
		SweepInterval: envDuration("DURUPAGES_BUNDLE_SWEEP_INTERVAL", 5*time.Minute),
		LogWriter:     os.Stdout,
	}

	// Log ingest is enabled when the controller propagates the hub's log
	// address; otherwise the shim stays in pod-log mode.
	if logAddr := os.Getenv("DURUPAGES_HUB_LOG_ADDR"); logAddr != "" {
		conn, err := grpc.NewClient(logAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("durupages-worker-shim: hub log dial: %v", err)
		}
		defer conn.Close()
		opts.LogClient = api.NewLogServiceClient(conn)
	}

	s, err := shim.New(opts)
	if err != nil {
		log.Fatalf("durupages-worker-shim: %v", err)
	}
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("durupages-worker-shim: %v", err)
	}
}
