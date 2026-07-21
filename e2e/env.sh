# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# Shared configuration for the DuruPages e2e scripts. Sourced, not executed.
# shellcheck shell=bash

# Resolve the repo root regardless of where the caller invoked the script from.
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${E2E_DIR}/.." && pwd)"

COMPOSE="${COMPOSE:-docker-compose}"
COMPOSE_FILE="${REPO_ROOT}/docker-compose.yaml"
DC() { ${COMPOSE} -f "${COMPOSE_FILE}" "$@"; }

K3S_CTR="durupages-k3s"
K3S_NODE_IP="172.28.0.5"
WORKER_NS="durupages-workers"
WORKER_IMAGE="durupages/worker:e2e"

PAGES_DOMAIN="pages.local"
# Router is published on host port 18080 (8080 is often taken by dev servers).
ROUTER_URL="http://localhost:18080"

# Host-facing connection strings for the deploy CLI (published compose ports).
PG_DSN="postgres://duru:duru@localhost:55432/duru?sslmode=disable"
S3_ENDPOINT="http://localhost:59000"
S3_BUCKET="durupages"
S3_ACCESS_KEY="minioadmin"
S3_SECRET_KEY="minioadmin"

KEYS_DIR="${REPO_ROOT}/e2e/.keys"
KUBE_DIR="${REPO_ROOT}/e2e/.kube"
BUILD_DIR="${REPO_ROOT}/e2e/.build"

# kubectl / ctr against the k3s node (avoids needing tooling on the host). This
# k3s image dispatches purely on argv[0] symlinks (kubectl, ctr, crictl -> k3s)
# and does not accept the "k3s <subcommand>" form.
k3s_kubectl() { docker exec -i "${K3S_CTR}" kubectl "$@"; }
# Pod images live in the containerd "k8s.io" namespace.
k3s_ctr() { docker exec -i "${K3S_CTR}" ctr -n k8s.io "$@"; }

# duru deploy CLI run from the host, pointed at the published ports.
duru_deploy() {
  ( cd "${REPO_ROOT}" && go run ./cmd/duru deploy \
      --pg-dsn "${PG_DSN}" \
      --pages-domain "${PAGES_DOMAIN}" \
      --s3-endpoint "${S3_ENDPOINT}" \
      --s3-bucket "${S3_BUCKET}" \
      --s3-access-key "${S3_ACCESS_KEY}" \
      --s3-secret-key "${S3_SECRET_KEY}" \
      --s3-path-style=true \
      "$@" )
}
