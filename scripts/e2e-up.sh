#!/usr/bin/env bash
# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# Build all component images, bring up the compose stack, import the worker
# image into k3s, rewrite the kubeconfig for in-container use, and wait until the
# control/data plane is healthy. Idempotent: safe to re-run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e/env.sh
source "${SCRIPT_DIR}/../e2e/env.sh"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

cd "${REPO_ROOT}"
mkdir -p "${KEYS_DIR}" "${KUBE_DIR}" "${BUILD_DIR}"

# ---------------------------------------------------------------------------
# 1. Worker JWT keypair (Ed25519). Controller signs, hub verifies.
# ---------------------------------------------------------------------------
if [[ ! -f "${KEYS_DIR}/jwt-priv.pem" ]]; then
  log "Generating Ed25519 worker JWT keypair"
  openssl genpkey -algorithm ed25519 -out "${KEYS_DIR}/jwt-priv.pem"
  openssl pkey -in "${KEYS_DIR}/jwt-priv.pem" -pubout -out "${KEYS_DIR}/jwt-pub.pem"
fi
# The controller/hub run as distroless nonroot; make the mounted keys readable.
chmod 0644 "${KEYS_DIR}"/*.pem

# ---------------------------------------------------------------------------
# 2. Build component images from the multi-target root Dockerfile.
# ---------------------------------------------------------------------------
log "Building component images (controller/hub/router/worker)"
docker build --target controller -t durupages/controller:e2e .
docker build --target hub        -t durupages/hub:e2e .
docker build --target router     -t durupages/router:e2e .
docker build --target worker     -t "${WORKER_IMAGE}" .
# Router e2e wrapper (adds iproute2 + pod-network route). Built with `docker
# build` on purpose: it FROMs the local durupages/router:e2e image, which the
# compose builder would try (and fail) to pull from a registry.
docker build -f e2e/router-net/Dockerfile -t durupages/router-net:e2e .

# ---------------------------------------------------------------------------
# 3. Start infrastructure: k3s, postgres, minio, + bucket.
# ---------------------------------------------------------------------------
log "Starting infrastructure (k3s, postgres, minio)"
DC up -d k3s postgres minio

log "Waiting for MinIO to become ready"
until curl -fsS "${S3_ENDPOINT}/minio/health/live" >/dev/null 2>&1; do
  sleep 2
done

log "Creating MinIO bucket"
DC run --rm minio-setup

# ---------------------------------------------------------------------------
# 4. Wait for k3s, rewrite kubeconfig, ensure worker namespace.
# ---------------------------------------------------------------------------
log "Waiting for k3s kubeconfig"
until [[ -f "${KUBE_DIR}/kubeconfig.yaml" ]]; do sleep 2; done
# Point the controller (a separate container) at the k3s node's static IP.
sed -i "s#https://127.0.0.1:6443#https://${K3S_NODE_IP}:6443#g" "${KUBE_DIR}/kubeconfig.yaml"

log "Waiting for k3s node to be Ready"
until k3s_kubectl get nodes 2>/dev/null | grep -q ' Ready'; do sleep 2; done

log "Ensuring worker namespace ${WORKER_NS}"
k3s_kubectl create namespace "${WORKER_NS}" --dry-run=client -o yaml | k3s_kubectl apply -f -

# ---------------------------------------------------------------------------
# 5. Import the worker image into the k3s containerd image store.
#    (imagePullPolicy defaults to IfNotPresent for the non-":latest" tag, so
#    the pre-imported image is used without any registry pull.)
# ---------------------------------------------------------------------------
log "Importing worker image into k3s"
docker save "${WORKER_IMAGE}" -o "${BUILD_DIR}/worker.tar"
docker cp "${BUILD_DIR}/worker.tar" "${K3S_CTR}:/tmp/worker.tar"
k3s_ctr images import /tmp/worker.tar
k3s_ctr images ls -q | grep -q "durupages/worker:e2e" \
  && echo "worker image present in k3s"

# ---------------------------------------------------------------------------
# 6. Start the control/data plane (router image is built here).
# ---------------------------------------------------------------------------
log "Starting hub, controller, router"
DC up -d hub controller router

# ---------------------------------------------------------------------------
# 7. Wait for readiness.
# ---------------------------------------------------------------------------
log "Waiting for controller to listen"
for _ in $(seq 1 60); do
  if docker logs durupages-controller 2>&1 | grep -q "controller listening"; then break; fi
  sleep 1
done

log "Waiting for router HTTP :8080"
for _ in $(seq 1 60); do
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "Host: healthz.${PAGES_DOMAIN}" "${ROUTER_URL}/" || true)"
  if [[ -n "${code}" && "${code}" != "000" ]]; then break; fi
  sleep 1
done

log "Stack is up. Router at ${ROUTER_URL} (Host: <page>.${PAGES_DOMAIN})"
