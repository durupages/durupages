# DuruPages

> English · [한국어](README.ko.md)

**DuruPages** is a multi-tenant platform that lets you self-host Cloudflare Pages anywhere. The name comes from the Korean word *duru* (두루, "widely / all-around") — making (Cloudflare) Pages usable all around.

It runs [workerd](https://github.com/cloudflare/workerd), Cloudflare's actual JS runtime, so **a Pages project built with wrangler deploys as-is, unmodified**. Static asset serving and SSR workers (Functions) behave the same way they do on Cloudflare Pages.

- Architecture deep-dive: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)

## Why DuruPages

- **Cloudflare Pages compatible** — implements the Pages serving rules directly: `_worker.js`, `_routes.json`, `_redirects`, `_headers`, `env.ASSETS`, nearest-`404.html` lookup, SPA fallback, pretty URLs. Point it at your existing build output directory and go.
- **Real multi-tenancy** — workers run in **per-tenant Pods**. Pods (process, network, filesystem) are never shared across tenants.
- **Fast cold starts** — a Pod becomes Ready with no bundles loaded, and each page is **lazy-loaded on its first request**. Cold start time is therefore independent of how many pages a tenant owns.
- **Zero-downtime deploys** — rolling out a new deployment never restarts the Pod. The shim fetches the new bundle and **blue-green swaps only the workerd process**, letting in-flight requests finish on the old one.
- **Per-request metering** — wall time, CPU, logs, exceptions and response status are captured per request. A page's Secret values are redacted automatically before events leave the Pod.
- **No infrastructure lock-in** — Storage, PageProvider (DB), Queue, Scaler and Runtime are all Go interfaces. Swap the defaults (S3 / PostgreSQL / in-memory / workerd) and assemble your own.

## How it works

```
Client ──▶ durupages-router ──▶ durupages-controller ──▶ (k8s) durupages-worker
            serves static assets       queue · lease · scaling         shim + workerd
                                                                        │
                                                    durupages-hub ◀─────┘
                                              bundle delivery · usage/log ingest
```

1. A request arrives at `{pageId}.{pagesDomain}` or a custom domain.
2. The **router** resolves the host to a page and decides static vs. dynamic using `_routes.json`. Static assets are served straight from its local LRU cache.
3. For dynamic requests it asks the **controller** for a slot. The controller creates tenant worker Pods as needed (autoscaling) and issues a lease.
4. The shim in the **worker** Pod fetches that page's bundle from the **hub** (if it does not have it yet), runs it on workerd, and responds.
5. The shim and router ship per-request usage and logs to the **hub** (or to Pod logs only, when log ingest is disabled).

## Components

| Binary | Role |
|---|---|
| `durupages-controller` | Control plane: request queue/lease, worker Pod lifecycle, autoscaling, reconcile |
| `durupages-router` | Public entrypoint: host routing, static serving (disk LRU cache), `_redirects`/`_headers`, dynamic proxy |
| `durupages-hub` | Worker support: tenant-scoped bundle delivery (JWT-authorized), request usage/log ingest |
| `durupages-worker-shim` | PID 1 of a worker Pod: lazy loading, workerd graceful swap, LRU eviction, metering |
| `durupages-workerd` | Custom workerd embedder (C++) implementing real per-isolate resource limits — [native/durupages-workerd](native/durupages-workerd) |
| `duru` | Deploy CLI: scan build output → upload → atomically switch the active deployment |

## Quick start

Bring up the whole stack locally (k3s + PostgreSQL + MinIO + every component) and run the e2e scenarios:

```sh
make e2e        # build images → start stack → verify scenarios → tear down
```

To bring up the stack and deploy something yourself:

```sh
make e2e-up

go run ./cmd/duru deploy \
  --dir ./e2e/fixtures/worker-site \
  --tenant acme --page app \
  --pg-dsn 'postgres://duru:duru@localhost:55432/duru?sslmode=disable' \
  --s3-endpoint http://localhost:59000 --s3-bucket durupages \
  --s3-access-key minioadmin --s3-secret-key minioadmin

curl -H 'Host: app.pages.local' http://localhost:18080/api/hello
```

The directory you deploy is **exactly the build output you would upload to Cloudflare Pages** (if you use `functions/`, compile it first with `wrangler pages functions build`).

## Deployment

Kubernetes deployment uses the Helm chart.

```sh
# Ed25519 keypair for worker JWTs (controller signs, hub verifies)
openssl genpkey -algorithm ed25519 -out worker-jwt.key
openssl pkey -in worker-jwt.key -pubout -out worker-jwt.pub

helm install durupages deploy/chart/durupages \
  --set-file workerJwt.privateKeyPEM=worker-jwt.key \
  --set-file workerJwt.publicKeyPEM=worker-jwt.pub \
  --set postgres.dsn='postgres://...' \
  --set s3.bucket=durupages \
  --set router.pagesDomain=pages.example.com
```

The chart installs controller/router/hub plus the worker namespace, ServiceAccount, RBAC and NetworkPolicy. Worker Pods themselves are created by the controller at runtime. See [deploy/chart/durupages/README.md](deploy/chart/durupages/README.md) for the full values reference.

Container images are published to GHCR:

```
ghcr.io/<owner>/durupages-controller:<version>
ghcr.io/<owner>/durupages-router:<version>
ghcr.io/<owner>/durupages-hub:<version>
```

## Extension points

Use the defaults, or implement an interface and assemble your own binary.

| Interface | Default | Purpose |
|---|---|---|
| `Storage` | S3 (MinIO compatible) | Stores static assets and worker bundles |
| `PageProvider` | PostgreSQL | Source of truth for tenants/pages/deployments; host resolution |
| `Queue` | in-memory | Per-tenant waiting queue (swap in Redis, etc.) |
| `Scaler` | target/max concurrency | Worker Pod scale up/down policy |
| `Runtime` | workerd | Worker execution engine |

```go
ctrl, err := controller.New(controller.Options{
    Provider: myProvider,   // custom PageProvider
    Storage:  s3storage.New(...),
    Queue:    redisqueue.New(...),
    Scaler:   myScaler.New(...),
})
```

## Development

```sh
go build ./...
go test -race ./...      # or: make test
make e2e                 # integration e2e (requires Docker)
```

Every binary prints its build-stamped version with `--version`. Release builds cross-compile `linux/amd64` and `linux/arm64` binaries with Go, and the images simply package those artifacts.

## Project status

The core — static serving, SSR workers, multi-tenancy, lazy loading, zero-downtime deploys, autoscaling and usage metering — works and is covered by e2e tests. It is still early, with these known limits:

- **Per-request CPU metering and limits are not implemented yet.** Stock workerd reports a request CPU time of 0. Per-isolate **memory (heap) limits** are implemented and verified in `durupages-workerd`, but CPU metering/limiting is follow-up work; until then `cpuTime` is recorded as 0.
- **No KV / D1 / R2 / Durable Objects bindings** (out of scope for the initial version).
- Preview deployments, branch aliases and other Cloudflare platform extras are out of scope.

The design rationale and trade-offs are documented in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## License

[Eclipse Public License 2.0](LICENSE) — Copyright JC-Lab
