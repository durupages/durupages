# duru CLI

`duru` is the DuruPages command-line tool. It deploys build output and drives every
operation of the controller's [admin API](ARCHITECTURE.md#14-admin-api-선택).

```sh
go build -o duru ./cmd/duru      # or use a release binary
duru --version
```

---

## Connecting

Every command except `deploy --pg-dsn …` (direct mode) talks to the controller's admin API.

| Flag | Env | Meaning |
|---|---|---|
| `--tenant` | `DURUPAGES_TENANT` | Tenant the command acts on |
| `--page` | `DURUPAGES_PAGE` | Page the command acts on |
| `--admin-url` | `DURUPAGES_ADMIN_URL` | Admin API base URL, e.g. `http://controller:9450` |
| `--token` | `DURUPAGES_ADMIN_TOKEN` | Sent as `Authorization: Bearer <token>` |
| `--header "Name: value"` | – | Extra header, repeatable (non-bearer auth schemes) |
| `--timeout` | – | HTTP timeout (default `10m`; uploads can be large) |

The admin API must be enabled on the controller (`DURUPAGES_ADMIN_ENABLED=true`, or
`controller.adminApi.enabled=true` in the Helm chart) and listens on its own port,
`:9450` by default.

The shipped controller binary serves the admin API **without authentication**, so keep
it on a private network. In Kubernetes it is a ClusterIP port:

```sh
kubectl port-forward svc/durupages-controller 9450:9450
export DURUPAGES_ADMIN_URL=http://localhost:9450
```

`--token` / `--header` exist for deployments that put authentication in front of it (an
authenticating proxy, or custom middleware compiled into your own controller binary —
see [Adding authentication](../README.md#adding-authentication)).

---

## Command overview

```
duru [--tenant <id>] [--page <id>] <command> [subcommand] [flags]

duru deploy       [flags]                             deploy a build output directory
duru health                                           check the admin API

duru tenant  list | get | set | delete | pages        # target with --tenant
duru page    list | get | set | delete | domains      # target with --page
duru secret  list | put | delete | bulk               # target with --page
duru deployment  list | upload | activate             # target with --page
```

The resource a command acts on is a **flag, not a positional**:

```sh
duru --page blog secret put API_KEY
duru --tenant acme tenant get
```

`--tenant` and `--page` may appear before or after the command word, and fall back to
`DURUPAGES_TENANT` / `DURUPAGES_PAGE`:

```sh
export DURUPAGES_PAGE=blog
duru secret list                 # same as: duru --page blog secret list
```

Output convention: **stdout is JSON** (pretty-printed, pipeable, and directly reusable as
a `--file` input); short human-readable confirmations go to **stderr**. Exit codes are
`0` success, `1` runtime error (the API rejected the request, or a `--file` could not be
read/parsed), `2` usage error (a bad flag or argument, caught before anything is sent).

---

## deploy

The one-shot command for shipping a site. It scans a wrangler build output directory,
uploads the bundle, registers the deployment and switches it live.

```sh
# admin mode (recommended): no database or object storage credentials needed
duru deploy --dir ./build-output --tenant acme --page blog \
  --admin-url http://controller:9450

# direct mode: the CLI writes to Storage and PostgreSQL itself
duru deploy --dir ./build-output --tenant acme --page blog \
  --pg-dsn postgres://... --s3-bucket durupages
```

Admin mode is selected by `--admin-url`; otherwise `--pg-dsn` + `--s3-bucket` select
direct mode. In both modes the tenant and page are created only when missing, so a
redeploy never resets their configuration.

| Flag | Meaning |
|---|---|
| `--dir` | Build output directory (default `.`) |
| `--tenant`, `--page` | Target tenant/page id (required) |
| `--deployment` | Deployment id (default: generated `dep-<unixNano>`) |
| `--domain a.com,b.com` | Custom domains to set (replaces the existing set) |
| `--secrets-file <path>` | Replace **all** of the page's secrets from a JSON or `.env` file, applied before the deployment goes live ([format](#bulk-file-format)) |
| `--pages-domain` | Pages domain, used only for the printed URL. In admin mode the controller reports the domain it actually serves and that is what gets printed, so this is only needed in direct mode (or to override it) |
| direct mode | `--pg-dsn`, `--migrate`, `--s3-endpoint`, `--s3-region`, `--s3-bucket`, `--s3-access-key`, `--s3-secret-key`, `--s3-path-style` |

The directory you deploy is exactly the build output you would upload to Cloudflare
Pages. If your project uses `functions/`, compile it first with
`wrangler pages functions build`.

Shipping code and its secrets together, like `wrangler deploy --secrets-file`:

```sh
duru deploy --dir ./build-output --tenant acme --page blog \
  --admin-url http://controller:9450 \
  --secrets-file .env.production
```

The secrets are written **before** the new deployment is activated, so the worker never
serves a request with stale ones. Note that `--secrets-file` replaces the entire map —
to change a single key use [`duru secret put`](#secret).

---

## Configuration input: flags or JSON file

`tenant set` and `page set` accept configuration either as flags or as a JSON file.

- `--file <path>` (`-f`), or `--file -` to read stdin.
- **The file format is the API JSON**, so it round-trips:

```sh
duru --tenant acme tenant get > tenant.json
$EDITOR tenant.json
duru --tenant acme tenant set -f tenant.json
```

Precedence, lowest to highest:

```
current server state  <  --file  <  explicitly-set flags
```

Only fields actually present in the file are applied, and only flags you actually typed
override it (a flag left at its default value changes nothing).

### `set` is read-modify-write

By default `set` fetches the current object, applies your changes on top and writes the
result back. This matters: the API replaces `env`, `queueTimeout`, `requestTimeout`,
`logsEnabled` and the whole tenant config on write, so a partial update sent blindly
would wipe fields you did not mention.

```sh
duru --page blog page set --request-timeout 5s   # keeps the existing env untouched
```

Pass `--replace` to skip the read and write exactly what the file and flags specify.
Note that three fields survive `--replace` anyway, because the API itself treats an
absent value as "keep": `secret`, `customDomains` and `activeDeploymentId`. Clear those
explicitly (`--clear-secrets`, `--clear-domains`) if that is what you want.

### Maps merge, lists replace

- Repeatable **key/value** flags (`--env`, `--pod-label`, `--pod-annotation`) merge into
  the current map, changing only the keys you name.
- Repeatable **list** flags (`--domain`) replace the whole list.
- `--clear-env`, `--clear-secrets`, `--clear-domains`, `--clear-pod-labels`,
  `--clear-pod-annotations` empty the corresponding field first, so
  `--clear-env --env A=1` results in exactly `{"A":"1"}`.
- A `k=v` value without `=` is a usage error.

> **`--secret` is the exception: it replaces the whole secret map.** The API never
> returns secret *values* (only their key names), so the CLI has nothing to merge
> against. `duru --page blog page set --secret C=3` on a page holding `A` and `B`
> leaves it with `C` alone. Prefer [`duru secret put`](#secret), which changes one key
> server-side; or leave `--secret` off entirely — an omitted field keeps the stored
> secrets untouched.

---

## tenant

```sh
duru tenant list                             # every tenant
duru --tenant acme tenant get
duru --tenant acme tenant pages              # pages owned by this tenant
duru --tenant acme tenant delete             # cascades to its pages and deployments
```

```sh
duru --tenant acme tenant set \
  --max-concurrency 10 \
  --idle-ttl 5m \
  --cpu-limit 2 --mem-limit 1Gi \
  --pod-label team=web --pod-annotation cost-center=web
```

| Flag | Field | Meaning |
|---|---|---|
| `--max-concurrency` | `maxConcurrency` | Max worker Pods for the tenant |
| `--idle-ttl` | `idleTTL` | Idle time before a Pod is scaled down |
| `--cpu-limit`, `--mem-limit` | `workerCPULimit`, `workerMemLimit` | Worker Pod resource limits |
| `--pod-label`, `--pod-annotation` | `podLabels`, `podAnnotations` | Extra Pod metadata (repeatable `k=v`) |
| `--clear-pod-labels`, `--clear-pod-annotations` | | Empty the map first |

Tenant JSON:

```json
{
  "id": "acme",
  "config": {
    "maxConcurrency": 10,
    "idleTTL": "5m0s",
    "workerCPULimit": "2",
    "workerMemLimit": "1Gi",
    "podLabels": {"team": "web"}
  }
}
```

Durations are Go duration strings (`"30s"`, `"1h30m"`), never bare numbers.

---

## page

```sh
duru page list                               # every page
duru --tenant acme page list                 # one tenant's pages
duru --page blog page get
duru --page blog page delete                 # cascades to its deployments and domains
```

```sh
duru --page blog --tenant acme page set \
  --request-timeout 45s --queue-timeout 20s \
  --env STAGE=prod --env REGION=apac \
  --secret API_KEY=s3cr3t \
  --logs-enabled true
```

| Flag | Field | Meaning |
|---|---|---|
| `--tenant` | `tenantId` | Owning tenant (required when creating) |
| `--queue-timeout`, `--request-timeout` | `queueTimeout`, `requestTimeout` | Per-page timeouts |
| `--env` | `env` | Worker binding, repeatable `k=v` |
| `--secret` | `secret` | Worker binding whose value is redacted from logs, repeatable `k=v` |
| `--logs-enabled` | `logsEnabled` | Tri-state: unset inherits the global setting |
| `--domain` | `customDomains` | Custom domains (replaces the list) |
| `--clear-env`, `--clear-secrets`, `--clear-domains` | | Empty the field first |

`--env` and `--secret` are both exposed to the worker as bindings and read the same way
in worker code. The difference is that **Secret values are redacted** from logs,
exceptions and recorded headers before events leave the Pod.

**Secrets are write-only.** The API never returns their values, so `duru page get` shows
only the key names:

```json
{
  "id": "blog",
  "tenantId": "acme",
  "activeDeploymentId": "dep-1784",
  "customDomains": ["www.example.com"],
  "config": {
    "requestTimeout": "45s",
    "env": {"STAGE": "prod"},
    "secretKeys": ["API_KEY"]
  }
}
```

Because of that, a `get > edit > set` round-trip cannot rewrite existing secret values —
it leaves them untouched (an absent field is kept server-side). `--secret` on `page set`
replaces the whole map; for per-key changes use [`duru secret`](#secret) below, which
does the read-modify-write on the server where the values are visible.

### Custom domains

```sh
duru --page blog page domains --domain www.example.com --domain example.com
duru --page blog page domains --clear
```

Domains are stored lowercased, de-duplicated and sorted. `page domains` replaces the
whole set, matching the underlying `PUT`.

---

## secret

Per-key secret management, modelled on `wrangler secret`. Secrets are worker bindings
whose values are redacted from logs; see [page](#page) for how they differ from `--env`.

```sh
duru --page blog secret list             # key names only — values are never returned
duru --page blog secret put API_KEY      # create or update ONE secret
duru --page blog secret delete API_KEY   # remove ONE secret
duru --page blog secret bulk --file .env # apply a file of secrets (upsert)
```

`page set --secret` replaces the entire map because the client cannot see the values it
would have to preserve. The `secret` commands have no such limitation: the **controller**
reads the current values, applies your change and writes the map back, so the secrets you
did not name are untouched. Prefer these for day-to-day changes.

| Command | Effect on secrets the command does not name |
|---|---|
| `secret put` / `secret delete` | untouched |
| `secret bulk --file` | untouched (upsert, like `wrangler secret bulk`) |
| `secret bulk --replace --file` | **removed** (whole-map write) |
| `deploy --secrets-file` | **removed** (whole-map write) |
| `page set --secret` | **removed** (whole-map write) |

Secret names must be valid worker binding identifiers: `^[A-Za-z_][A-Za-z0-9_]*$`.

### Supplying the value

`secret put` never takes the value as a flag — that would leak it into shell history and
`ps`. It reads the value from stdin instead:

```sh
# piped (scripts, CI)
printf '%s' "$API_KEY" | duru --page blog secret put API_KEY
duru --page blog secret put API_KEY < secret.txt

# interactive: prompts and reads without echoing
duru --page blog secret put API_KEY
Enter the secret value for API_KEY:
```

A single trailing newline is stripped, so `echo` works as expected. An empty value is a
usage error.

### Bulk file format

`secret bulk --file` (`-f`) and `deploy --secrets-file` take the same file, in either
format (auto-detected — a body starting with `{` is JSON, anything else is dotenv). The
server caps one bulk write at 100 entries.

They differ in what happens to secrets the file does not mention:

```sh
duru --page blog secret bulk --file .env             # upsert: others are kept
duru --page blog secret bulk --replace --file .env   # replace: others are removed
duru deploy ... --secrets-file .env                  # replace
```

In a **JSON** file a `null` value deletes that one secret, as in `wrangler secret bulk`:

```json
{
  "API_KEY": "s3cr3t",
  "DB_PASSWORD": "hunter2",
  "RETIRED_KEY": null
}
```

dotenv cannot express `null`. A `null` combined with `--replace` (or with
`deploy --secrets-file`) is rejected rather than ignored — "delete this one" contradicts
"these are all of them".

```sh
# .env style
# comments and blank lines are ignored
API_KEY=s3cr3t
DB_PASSWORD="hunter2"          # a matching pair of surrounding quotes is stripped
GREETING='  hello world  '     # ...which is how leading/trailing spaces are kept
EQUALS=a=b=c                   # only the first '=' splits
```

`--file -` reads stdin. A line without `=` is an error naming the line
(`<stdin>:3: "OOPS" is not in KEY=value form`), and the last duplicate key wins.

**A file with no entries is rejected** rather than read as an instruction — a truncated
file must never be mistaken for one. Removing every secret is written out explicitly:

```sh
echo '{}' | duru --page blog secret bulk --replace --file -   # removes every secret
```

---

## deployment

```sh
duru --page blog deployment list                        # newest first, active one flagged
duru --page blog deployment upload --dir ./build-output
duru --page blog deployment upload --dir ./out --no-activate   # stage without going live
duru --page blog deployment activate dep-1784678730016019785   # switch/roll back
```

`deployment upload` is the lower-level half of `deploy`: it uploads a build output
directory to an **existing** page and (unless `--no-activate`) makes it live. `deploy`
additionally creates the tenant/page when missing.

Rollback is just activating an older deployment — deployments are immutable, so the
previous bundle is still in Storage:

```sh
duru --page blog deployment list
duru --page blog deployment activate dep-<previous>
```

The switch takes effect without restarting any worker Pod: the next request carries the
new deployment id and the shim hot-swaps the workerd process
([ARCHITECTURE 6.3](ARCHITECTURE.md)).

| Flag | Meaning |
|---|---|
| `--dir` | Build output directory to upload |
| `--deployment` | Deployment id (default: generated) |
| `--no-activate` | Register the deployment without making it live |

The upload streams the directory as a gzipped tar. The controller extracts it safely
(absolute paths, `..` segments, symlinks and hard links are rejected), scans it, writes
the objects to Storage and registers the deployment. Upload size is capped by the
controller (`--admin-max-upload-bytes`, 512 MiB by default).

---

## Scripting

stdout is always JSON, so the CLI composes with `jq`:

```sh
# every page of a tenant
duru --tenant acme page list | jq -r '.pages[].id'

# the currently active deployment of a page
duru --page blog page get | jq -r '.activeDeploymentId'

# roll back to the deployment before the active one
prev=$(duru --page blog deployment list | jq -r '.deployments[1].id')
duru --page blog deployment activate "$prev"

# copy a tenant's configuration to another environment
duru --tenant acme tenant get | duru --tenant acme tenant set -f - --admin-url "$STAGING_URL"
```

Mutating commands print their confirmation on stderr, so piping stdout stays clean.
