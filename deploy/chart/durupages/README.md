# DuruPages Helm Chart

Installs the DuruPages control/data plane — **controller**, **router**, **hub** —
plus the worker namespace, ServiceAccount, RBAC, NetworkPolicy and worker JWT key
material.

Worker pods are **not** created by this chart. The controller creates them at
runtime; the chart only passes the worker image tag (`worker.image`) and a couple
of propagated tunables to the controller.

External dependencies (PostgreSQL and S3) are **not** bundled — provide connection
information via values.

## Prerequisites

- Kubernetes >= 1.21 (the NetworkPolicy relies on the automatic
  `kubernetes.io/metadata.name` namespace label).
- A CNI that enforces `NetworkPolicy` (otherwise set `networkPolicy.enabled=false`).
- A reachable PostgreSQL database (PageProvider) and S3-compatible object store.

## 1. Generate the worker JWT keypair (required)

The controller **signs** worker tokens with an Ed25519 private key; the hub
**verifies** them with the matching public key. Helm cannot derive an Ed25519
public key from a private key inside a template, so you must supply both (or point
at an existing secret). If neither is provided the chart **fails with an
instructive message** rather than installing something broken.

```sh
openssl genpkey -algorithm ed25519 -out worker-jwt.key
openssl pkey -in worker-jwt.key -pubout -out worker-jwt.pub
```

## 2. Install

```sh
helm install durupages deploy/chart/durupages \
  --namespace durupages --create-namespace \
  --set-file workerJwt.privateKeyPEM=worker-jwt.key \
  --set-file workerJwt.publicKeyPEM=worker-jwt.pub \
  --set postgres.dsn='postgres://durupages:secret@pg:5432/durupages?sslmode=require' \
  --set s3.endpoint=https://minio.example.com \
  --set s3.bucket=durupages \
  --set s3.accessKey=AKIA... \
  --set s3.secretKey=... \
  --set pagesDomain=pages.example.com
```

Reusing pre-created secrets instead of inline values:

```sh
  --set workerJwt.existingSecret=durupages-worker-jwt \
  --set postgres.existingSecret=durupages-pg --set postgres.existingSecretKey=dsn \
  --set s3.existingSecret=durupages-s3
```

`workerJwt.existingSecret` must contain the keys `worker-jwt.key` and
`worker-jwt.pub` (names configurable via `workerJwt.privateKeyKey` /
`workerJwt.publicKeyKey`). `s3.existingSecret` must contain `accessKey` and
`secretKey` (configurable via `s3.accessKeySecretKey` / `s3.secretKeySecretKey`).

## What gets installed

| Kind | Name | Notes |
|---|---|---|
| Deployment | `<release>-controller` | replicas 1, `strategy: Recreate` (in-memory queue) |
| Service | `<release>-controller` | gRPC `:9440` |
| ServiceAccount | `<release>-controller` | bound to the worker-pod Role |
| Deployment | `<release>-router` | replicas `router.replicas` (default 2) |
| Service | `<release>-router` | HTTP `:8080`, type `router.service.type` |
| Deployment | `<release>-hub` | replicas `hub.replicas` (default 1) |
| Service | `<release>-hub` | bundle HTTP `:9080`, log gRPC `:9443` |
| Namespace | `workerNamespace` | guarded by `createWorkerNamespace` |
| ServiceAccount | `durupages-worker-noperm` | in the worker namespace, `automountServiceAccountToken: false` |
| Role + RoleBinding | `<release>-worker-pods` | controller may get/list/watch/create/delete pods in the worker namespace |
| NetworkPolicy | `<release>-worker` | default-deny; see below. Toggle with `networkPolicy.enabled` |
| Secret | `<release>-worker-jwt` | unless `workerJwt.existingSecret` |
| Secret | `<release>-postgres` | unless `postgres.existingSecret` |
| Secret | `<release>-s3` | only when inline S3 creds are given |

## NetworkPolicy (worker namespace)

Implements ARCHITECTURE section 8. Applied to pods labelled
`app.kubernetes.io/name: durupages-worker`:

- **Ingress**: only from the controller and router pods (release namespace).
- **Egress**: only to the controller Service, the hub Service (bundle + log
  ports), DNS (kube-dns `:53`), and the external internet (`0.0.0.0/0`) with the
  cluster CIDRs (`networkPolicy.clusterCIDRs`) and the metadata endpoint
  `169.254.169.254/32` excluded.

Set `networkPolicy.clusterCIDRs` to match your cluster's pod/service CIDRs, and
`networkPolicy.dns` to your DNS provider's namespace/labels. Disable the whole
policy with `networkPolicy.enabled=false` where the CNI has no enforcement.

## Key values

| Key | Default | Description |
|---|---|---|
| `clusterDomain` | `cluster.local` | Cluster DNS domain for Service addresses |
| `postgres.dsn` / `postgres.existingSecret` | `""` | PageProvider DSN (one required) |
| `s3.endpoint` / `s3.bucket` | `""` | Storage endpoint / bucket (bucket required) |
| `s3.accessKey` / `s3.secretKey` / `s3.existingSecret` | `""` | Storage creds (optional — default chain if empty) |
| `s3.pathStyle` | `true` | Path-style addressing (MinIO) |
| `workerJwt.privateKeyPEM` / `publicKeyPEM` | `""` | Ed25519 keypair (or `existingSecret`) |
| `logIngest.enabled` | `false` | Propagate hub log address to router/controller |
| `workerNamespace` | `durupages-workers` | Namespace for worker pods |
| `createWorkerNamespace` | `true` | Whether the chart creates the worker namespace |
| `workerServiceAccount` | `durupages-worker-noperm` | Worker pod ServiceAccount |
| `worker.image.repository` / `.tag` | `ghcr.io/durupages/durupages-worker` / appVersion | Worker image the controller stamps onto pods |
| `worker.bundleMinIdle` | `""` | `DURUPAGES_BUNDLE_MIN_IDLE` propagated to workers |
| `worker.bundleCacheMaxBytes` | `""` | `DURUPAGES_BUNDLE_CACHE_MAX_BYTES` propagated to workers |
| `networkPolicy.enabled` | `true` | Toggle the worker NetworkPolicy |
| `networkPolicy.clusterCIDRs` | RFC1918 blocks | CIDRs excluded from worker external egress |
| `pagesDomain` | `pages.local` | Pages domain, shared by router and controller |
| `controller.defaultQueueTimeout` | `30s` | `--default-queue-timeout` |
| `controller.maxQueueTimeout` | `120s` | `--max-queue-timeout` |
| `controller.defaultRequestTimeout` | `60s` | `--default-request-timeout` |
| `controller.defaultMaxConcurrency` | `5` | `--default-max-concurrency` |
| `controller.maxConcurrencyPerPod` | `256` | `--max-concurrency-per-pod` |
| `controller.targetConcurrencyPerPod` | `32` | `--target-concurrency-per-pod` |
| `controller.defaultIdleTTL` | `60s` | `--default-idle-ttl` |
| `controller.migrate` | `true` | Apply schema migrations on start |
| `controller.tmpSize` | `4Gi` | writable `/tmp` `emptyDir` sizeLimit (admin API upload staging) |
| `controller.adminApi.enabled` | `false` | Serve the unauthenticated admin API on its own port |
| `controller.adminApi.maxUploadBytes` | `""` | Deployment upload body cap (`""` = 512 MiB) |
| `router.replicas` | `2` | Router replica count |
| `router.service.type` | `ClusterIP` | `ClusterIP` / `NodePort` / `LoadBalancer` |
| `router.staticCacheMaxBytes` | `1073741824` | `--static-cache-max-bytes` (1GiB) |
| `router.staticCacheSize` | `2Gi` | static cache `emptyDir` sizeLimit |
| `router.resolveCacheTTL` | `10s` | `--resolve-cache-ttl` |
| `hub.bundleCacheMaxBytes` | `4294967296` | `--bundle-cache-max-bytes` (4GiB) |
| `hub.bundleCacheSize` | `8Gi` | bundle cache `emptyDir` sizeLimit |
| `<component>.resources` / `.nodeSelector` / `.tolerations` / `.affinity` | see values | Standard passthroughs |
| `<component>.podSecurityContext` / `.securityContext` | non-root, RO rootfs, drop ALL | Hardened defaults |

See [`values.yaml`](values.yaml) for the complete list.

## Upgrades

`helm upgrade` rolls controller/router/hub only. Existing worker pods are adopted
by the controller's reconcile loop, so there is no request interruption; workers
are drained and replaced gradually only when the worker image changes.
