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
- Only for `tls.certManager.enabled`: cert-manager installed, plus an Issuer or
  ClusterIssuer. TLS itself is optional and off by default.

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

## 3. Addresses handed to the router and the worker pods

The controller tells every worker pod where to reach the controller and the hub.
By default those are the in-cluster Service FQDNs, which is what a normal cluster
wants. Override them when the Service FQDN is not reachable or not the name on
the certificate — an isolated worker network routed through a fixed VIP, an
ingress, a separate DNS zone:

```yaml
controller:
  advertiseAddr: controller.internal:9440      # gRPC target: bare host:port
hub:
  advertiseAddr: https://hub.internal:9443     # URL: SCHEME IS MANDATORY
  logAdvertiseAddr: logs.internal:9443         # gRPC target: bare host:port
```

- `hub.advertiseAddr` **must** carry a scheme. The worker shim uses it as an HTTP
  URL prefix; without one, `net/url` reads the hostname as the scheme and every
  bundle download fails with `unsupported protocol scheme`. The chart refuses to
  render a scheme-less value, and refuses `http://` while `tls.enabled=true`.
- `controller.advertiseAddr` and `hub.logAdvertiseAddr` are gRPC dial targets and
  must **not** carry a scheme; the chart rejects one.
- Left empty, the hub URL follows `tls.enabled` (`https://` when TLS is on).
- With cert-manager, the host of each advertised address is added to that
  listener's SANs automatically.

## 4. TLS (optional)

TLS is opt-in and off by default: every hop stays plaintext, exactly as before.
`tls.enabled=true` makes three listeners serve TLS on their existing ports (so
the NetworkPolicy is unchanged):

| Listener | Port | Server env |
|---|---|---|
| controller gRPC | 9440 | `DURUPAGES_TLS_CERT_FILE` / `_KEY_FILE` |
| hub bundle HTTP | 9080 | `DURUPAGES_TLS_CERT_FILE` / `_KEY_FILE` |
| hub log gRPC | 9443 | `DURUPAGES_LOG_TLS_CERT_FILE` / `_KEY_FILE` |
| controller admin API | 9450 | reuses the controller certificate, or `DURUPAGES_ADMIN_TLS_CERT_FILE` / `_KEY_FILE` |

Clients are configured to match: the router mounts the CA
(`DURUPAGES_CA_CERT_FILE`), and the controller passes the CA to every worker pod
it creates as **inline PEM** (`DURUPAGES_CA_CERT_PEM`) — worker pods live in
their own namespace and are created at runtime, so the bundle travels in the pod
spec instead of as a replicated Secret. A CA certificate is public material.

This is server TLS, not mTLS: workers still authenticate with their JWT.

### 4a. cert-manager (automatic issuance)

```sh
helm upgrade --install durupages deploy/chart/durupages \
  --set tls.enabled=true \
  --set tls.certManager.enabled=true \
  --set tls.certManager.issuerRef.name=my-ca-issuer \
  --set tls.certManager.issuerRef.kind=ClusterIssuer
```

The chart creates one `cert-manager.io/v1` Certificate per listener. Each gets
the Service name in its four in-cluster forms as SANs —
`<svc>`, `<svc>.<ns>`, `<svc>.<ns>.svc`, `<svc>.<ns>.svc.<clusterDomain>` — plus
the host of that listener's advertise address:

```yaml
dnsNames:
  - durupages-controller
  - durupages-controller.durupages
  - durupages-controller.durupages.svc
  - durupages-controller.durupages.svc.cluster.local
  - controller.internal          # from controller.advertiseAddr
```

The CA comes from the issued Secret's `ca.crt`, which private-PKI issuers (CA,
SelfSigned, Vault, …) populate. **Publicly issued (ACME) certificates have no
`ca.crt`** — for those set `tls.caSecret.systemRoots=true` so clients verify
against the system trust store instead.

Everything cert-manager exposes is customisable:

```yaml
tls:
  certManager:
    duration: 2160h            # 90d
    renewBefore: 360h          # 15d
    privateKey:
      algorithm: ECDSA
      size: 256
      rotationPolicy: Always
    secretTemplate:
      annotations: {}
    usages: []
    annotations: {}            # on the Certificate resources
    labels: {}
```

`duration` / `renewBefore` can also be set per listener
(`tls.controller.duration`, …), which overrides the global value.

### 4b. Certificates you issued yourself

Create `kubernetes.io/tls` Secrets and point each listener at one. No Certificate
resource is created for a listener that has an `existingSecret`, so the two modes
can be mixed freely.

```yaml
tls:
  enabled: true
  controller:
    existingSecret: durupages-controller-tls
  hub:
    existingSecret: durupages-hub-tls
    certKey: server.crt        # when your Secret does not use tls.crt/tls.key
    keyKey: server.key
  caSecret:
    name: durupages-ca         # required: the chart cannot guess where the CA is
    key: ca.crt
```

The chart fails at template time — not at runtime — if a listener has no
certificate source, if cert-manager is on without an `issuerRef.name`, or if no
CA is configured and `systemRoots` is not set.

### 4c. A separate domain for the log listener (or the admin API)

The hub log gRPC listener reuses the hub certificate unless you give it its own.
Setting any of `existingSecret` / `dnsNames` / `ipAddresses` / `commonName` under
`tls.hubLog` is what asks for a separate certificate:

```yaml
hub:
  advertiseAddr: https://bundles.example.com:9443
  logAdvertiseAddr: logs.example.com:9443
tls:
  enabled: true
  certManager:
    enabled: true
    issuerRef: {name: my-ca-issuer, kind: ClusterIssuer}
  hub:
    dnsNames: [bundles.example.com]
  hubLog:
    dnsNames: [logs.example.com]      # own Certificate + own domain
```

That renders `DURUPAGES_LOG_TLS_CERT_FILE=/etc/durupages/tls-log/tls.crt` on the
hub, mounted from its own Secret, while the bundle listener keeps
`/etc/durupages/tls/tls.crt`.

The controller admin API works the same way through `tls.admin`. It is normally
reached with `kubectl port-forward`, so its generated SANs include `localhost`;
add `tls.admin.ipAddresses: [127.0.0.1]` if your client dials by IP.

### 4d. When the advertised address is not on the certificate

Point clients at the name the certificate actually carries:

```yaml
tls:
  controller: {serverName: controller.example.com}
  hub:        {serverName: bundles.example.com}
  hubLog:     {serverName: logs.example.com}
```

These become `DURUPAGES_*_SERVER_NAME` on the router and on worker pods. Use them
when an endpoint is reached by cluster IP or through a VIP.

### 4e. Rotation

Server certificates and the mounted CA are re-read from disk when they change, so
a cert-manager renewal takes effect without a restart. The worker CA is inline in
the pod environment, so **already running worker pods keep the CA they were born
with** — during a CA change the bundle must hold both the old and the new CA
until those pods are replaced.

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
| Certificate | `<release>-controller-tls` | only with `tls.certManager.enabled`, unless `tls.controller.existingSecret` |
| Certificate | `<release>-hub-tls` | only with `tls.certManager.enabled`, unless `tls.hub.existingSecret` |
| Certificate | `<release>-hub-log-tls` | only when `tls.hubLog` asks for its own certificate |
| Certificate | `<release>-admin-tls` | only when `tls.admin` asks for its own certificate and the admin API is on |

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
| `controller.advertiseAddr` | `""` | Controller address given to router/workers. Empty = Service FQDN. Bare `host:port` |
| `hub.advertiseAddr` | `""` | Bundle URL given to workers. Empty = Service URL. **Scheme required** |
| `hub.logAdvertiseAddr` | `""` | Log address given to router/workers. Empty = Service FQDN. Bare `host:port` |
| `tls.enabled` | `false` | Master switch. Off = every hop plaintext |
| `tls.certManager.enabled` | `false` | Issue the certificates as cert-manager Certificates |
| `tls.certManager.issuerRef.name` / `.kind` / `.group` | `""` / `Issuer` / `cert-manager.io` | Issuer the Certificates reference (name required) |
| `tls.certManager.duration` / `.renewBefore` | `""` | Lifetime / renewal window (empty = issuer default) |
| `tls.certManager.privateKey` / `.secretTemplate` / `.usages` | `{}` / `{}` / `[]` | cert-manager spec passthroughs |
| `tls.certManager.annotations` / `.labels` | `{}` | Extra metadata on the Certificate resources |
| `tls.caSecret.name` / `.key` | `""` / `ca.crt` | CA bundle clients verify against. Empty derives it from the issued controller Secret |
| `tls.caSecret.systemRoots` | `false` | Verify against the system trust store instead (ACME/public certificates) |
| `tls.controller.*` | see values | controller gRPC certificate |
| `tls.hub.*` | see values | hub bundle HTTP certificate |
| `tls.hubLog.*` | see values | hub log gRPC certificate; empty reuses `tls.hub` |
| `tls.admin.*` | see values | admin API certificate; empty reuses `tls.controller` |
| `tls.<listener>.existingSecret` | `""` | Use your own `kubernetes.io/tls` Secret; skips Certificate creation |
| `tls.<listener>.certKey` / `.keyKey` | `tls.crt` / `tls.key` | Keys within that Secret |
| `tls.<listener>.dnsNames` / `.ipAddresses` / `.commonName` | `[]` / `[]` / `""` | Extra SANs on top of the Service names |
| `tls.<listener>.duration` / `.renewBefore` | `""` | Per-listener override of the cert-manager values |
| `tls.controller.serverName` / `tls.hub.serverName` / `tls.hubLog.serverName` | `""` | Name clients verify (`DURUPAGES_*_SERVER_NAME`) |
| `workerNamespace` | `durupages-workers` | Namespace for worker pods |
| `createWorkerNamespace` | `true` | Whether the chart creates the worker namespace |
| `workerServiceAccount` | `durupages-worker-noperm` | Worker pod ServiceAccount |
| `worker.image.repository` / `.tag` | `ghcr.io/durupages/durupages-worker` / appVersion | Worker image the controller stamps onto pods |
| `<component>.image.digest` | `""` | Pin an exact image (`sha256:…`); wins over `.tag`. Applies to `controller`, `router`, `hub`, `worker` |
| `worker.bundleMinIdle` | `""` | `DURUPAGES_BUNDLE_MIN_IDLE` propagated to workers |
| `worker.bundleCacheMaxBytes` | `""` | `DURUPAGES_BUNDLE_CACHE_MAX_BYTES` propagated to workers |
| `worker.podAnnotations` | `{}` | Annotations put on **every** worker pod — see [Worker pod annotations](#worker-pod-annotations) |
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
| `<component>.service.*` | see below | Full Service surface — see [Services](#services) |
| `router.staticCacheMaxBytes` | `1073741824` | `--static-cache-max-bytes` (1GiB) |
| `router.staticCacheSize` | `2Gi` | static cache `emptyDir` sizeLimit |
| `router.resolveCacheTTL` | `10s` | `--resolve-cache-ttl` |
| `hub.bundleCacheMaxBytes` | `4294967296` | `--bundle-cache-max-bytes` (4GiB) |
| `hub.bundleCacheSize` | `8Gi` | bundle cache `emptyDir` sizeLimit |
| `<component>.resources` / `.nodeSelector` / `.tolerations` / `.affinity` | see values | Standard passthroughs |
| `<component>.podSecurityContext` / `.securityContext` | non-root, RO rootfs, drop ALL | Hardened defaults |
| `<component>.extraArgs` / `.extraEnv` | `[]` | Appended last, so they override generated args/env |

See [`values.yaml`](values.yaml) for the complete list.

## Upgrades

`helm upgrade` rolls controller/router/hub only. Existing worker pods are adopted
by the controller's reconcile loop, so there is no request interruption; workers
are drained and replaced gradually only when the worker image changes.
