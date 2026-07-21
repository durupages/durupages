#!/usr/bin/env bash
# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# End-to-end scenarios for the DuruPages stack. Assumes the stack is already up
# (scripts/e2e-up.sh). Deploys the static and worker fixtures with the duru CLI,
# then asserts behaviour through the router. Prints PASS/FAIL per check, dumps
# diagnostics on any failure, and exits non-zero if anything failed.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=env.sh
source "${SCRIPT_DIR}/env.sh"

FIXTURES="${REPO_ROOT}/e2e/fixtures"
FAILED=0

c_pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
c_fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILED=1; }
scenario() { printf '\n\033[1;36m### %s\033[0m\n' "$*"; }

# curl_get <host> <path> [extra curl args...]
# Populates globals: CODE, BODY (file), HDRS (file), TIME
curl_get() {
  local host="$1" path="$2"; shift 2
  BODY="$(mktemp)"; HDRS="$(mktemp)"
  CODE="$(curl -s --max-time 90 -o "${BODY}" -D "${HDRS}" \
      -w '%{http_code} %{time_total}' -H "Host: ${host}" "$@" "${ROUTER_URL}${path}")"
  TIME="${CODE#* }"; CODE="${CODE%% *}"
}

# Retry a worker request until it returns 200 (covers the cold-start window).
curl_get_worker() {
  local host="$1" path="$2" i
  for i in $(seq 1 12); do
    curl_get "${host}" "${path}"
    [[ "${CODE}" == "200" ]] && return 0
    sleep 3
  done
  return 0
}

dump_diagnostics() {
  scenario "DIAGNOSTICS"
  echo "----- kubectl get pods -A -----";        k3s_kubectl get pods -A -o wide 2>&1 | sed 's/^/  /'
  echo "----- worker pods (describe) -----";      k3s_kubectl describe pods -n "${WORKER_NS}" 2>&1 | tail -60 | sed 's/^/  /'
  echo "----- controller logs (tail) -----";      docker logs --tail 80 durupages-controller 2>&1 | sed 's/^/  /'
  echo "----- router logs (tail) -----";          docker logs --tail 40 durupages-router 2>&1 | sed 's/^/  /'
  echo "----- hub logs (tail) -----";             docker logs --tail 40 durupages-hub 2>&1 | sed 's/^/  /'
  echo "----- worker pod logs (tail) -----"
  for p in $(k3s_kubectl get pods -n "${WORKER_NS}" -o name 2>/dev/null); do
    echo "  == ${p} =="; k3s_kubectl logs -n "${WORKER_NS}" "${p}" 2>&1 | tail -40 | sed 's/^/    /'
  done
}

# ---------------------------------------------------------------------------
# Deploy fixtures.
# ---------------------------------------------------------------------------
scenario "DEPLOY fixtures"
if duru_deploy --dir "${FIXTURES}/static-site" --tenant acme --page static; then
  c_pass "deployed static-site as static.${PAGES_DOMAIN}"
else
  c_fail "deploy static-site"; dump_diagnostics; exit 1
fi
if duru_deploy --dir "${FIXTURES}/worker-site" --tenant acme --page app; then
  c_pass "deployed worker-site as app.${PAGES_DOMAIN}"
else
  c_fail "deploy worker-site"; dump_diagnostics; exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 1: static serving, 404, redirect, custom header.
# ---------------------------------------------------------------------------
scenario "1. Static site (serve / 404 / redirect / header)"

curl_get "static.${PAGES_DOMAIN}" "/"
if [[ "${CODE}" == "200" ]] && grep -q "DURUPAGES_STATIC_INDEX_OK" "${BODY}"; then
  c_pass "GET / -> 200 index.html (${TIME}s)"
else
  c_fail "GET / -> got ${CODE} in ${TIME}s, body: $(head -c120 "${BODY}")"
fi

if grep -qi '^X-Duru-Test: static-header-ok' "${HDRS}"; then
  c_pass "_headers rule applied (X-Duru-Test)"
else
  c_fail "_headers rule missing; headers: $(tr -d '\r' < "${HDRS}" | paste -sd'|')"
fi

curl_get "static.${PAGES_DOMAIN}" "/does-not-exist"
if [[ "${CODE}" == "404" ]] && grep -q "DURUPAGES_STATIC_404_OK" "${BODY}"; then
  c_pass "GET /does-not-exist -> 404 404.html (${TIME}s)"
else
  c_fail "unknown path -> got ${CODE} in ${TIME}s, body: $(head -c120 "${BODY}")"
fi

curl_get "static.${PAGES_DOMAIN}" "/old"
LOC="$(grep -i '^Location:' "${HDRS}" | tr -d '\r' | awk '{print $2}')"
if [[ "${CODE}" == "301" ]] && [[ "${LOC}" == *"/contact.html" ]]; then
  c_pass "GET /old -> 301 Location ${LOC} (${TIME}s)"
else
  c_fail "_redirects rule -> got ${CODE} in ${TIME}s, Location=${LOC}"
fi

# ---------------------------------------------------------------------------
# Scenario 2: worker dynamic path + static passthrough.
# ---------------------------------------------------------------------------
scenario "2. Worker site (dynamic /api/hello + static /)"

curl_get_worker "app.${PAGES_DOMAIN}" "/api/hello"
if [[ "${CODE}" == "200" ]] && grep -q '"message":"hello from worker"' "${BODY}" && grep -q '"version":"v1"' "${BODY}"; then
  c_pass "GET /api/hello -> 200 worker JSON v1 in ${TIME}s (cold start: pod create -> register -> lazy bundle load -> workerd)"
else
  c_fail "GET /api/hello -> got ${CODE} in ${TIME}s, body: $(head -c200 "${BODY}")"
  dump_diagnostics
fi

curl_get "app.${PAGES_DOMAIN}" "/"
if [[ "${CODE}" == "200" ]] && grep -q "DURUPAGES_WORKER_STATIC_INDEX_OK" "${BODY}"; then
  c_pass "GET / (excluded by _routes.json) -> 200 static from router (${TIME}s)"
else
  c_fail "GET / worker-site static -> got ${CODE} in ${TIME}s, body: $(head -c120 "${BODY}")"
fi

# ---------------------------------------------------------------------------
# Scenario 3: redeploy worker -> response changes, pod kept (restartCount 0).
# ---------------------------------------------------------------------------
scenario "3. Redeploy worker (in-place bundle swap, pod preserved)"

POD_BEFORE="$(k3s_kubectl get pods -n "${WORKER_NS}" -l durupages.io/tenant-id=acme -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
RC_BEFORE="$(k3s_kubectl get pods -n "${WORKER_NS}" -l durupages.io/tenant-id=acme -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)"
echo "  pod before redeploy: ${POD_BEFORE} (restartCount=${RC_BEFORE})"

V2="$(mktemp -d)"
cp -a "${FIXTURES}/worker-site/." "${V2}/"
sed -i 's/version: "v1"/version: "v2"/' "${V2}/_worker.js"
if ! grep -q 'version: "v2"' "${V2}/_worker.js"; then
  c_fail "fixture mutation for v2 did not apply"; exit 1
fi
if duru_deploy --dir "${V2}" --tenant acme --page app; then
  c_pass "redeployed worker-site (v2)"
else
  c_fail "redeploy worker-site v2"
fi

# Poll until the new deployment's output is observed (lease carries new deploymentId).
V2_OK=0
for _ in $(seq 1 12); do
  curl_get "app.${PAGES_DOMAIN}" "/api/hello"
  if [[ "${CODE}" == "200" ]] && grep -q '"version":"v2"' "${BODY}"; then V2_OK=1; break; fi
  sleep 2
done
if [[ "${V2_OK}" == "1" ]]; then
  c_pass "GET /api/hello -> now returns v2 in ${TIME}s (workerd graceful swap)"
else
  c_fail "worker response did not change to v2; last request ${TIME}s, body: $(head -c200 "${BODY}")"
fi

POD_AFTER="$(k3s_kubectl get pods -n "${WORKER_NS}" -l durupages.io/tenant-id=acme -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
RC_AFTER="$(k3s_kubectl get pods -n "${WORKER_NS}" -l durupages.io/tenant-id=acme -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)"
echo "  pod after redeploy:  ${POD_AFTER} (restartCount=${RC_AFTER})"
if [[ -n "${POD_BEFORE}" && "${POD_BEFORE}" == "${POD_AFTER}" && "${RC_AFTER}" == "0" ]]; then
  c_pass "same pod reused, restartCount 0 (no pod deletion/restart across redeploy)"
else
  c_fail "pod changed or restarted: before=${POD_BEFORE}/${RC_BEFORE} after=${POD_AFTER}/${RC_AFTER}"
fi

# ---------------------------------------------------------------------------
# Scenario 4: second request is served from the already-loaded worker (fast,
# no bundle re-download). Best effort.
# ---------------------------------------------------------------------------
scenario "4. Warm request latency / no re-download (best effort)"

curl_get "app.${PAGES_DOMAIN}" "/api/hello"
echo "  warm /api/hello: code=${CODE} time=${TIME}s"
# A warm, already-loaded worker should answer well under a second.
if [[ "${CODE}" == "200" ]] && awk "BEGIN{exit !(${TIME} < 2.0)}"; then
  c_pass "warm request served fast (${TIME}s) without re-download"
else
  c_fail "warm request slow/failed: code=${CODE} time=${TIME}s"
fi
echo "  hub bundle activity:"
docker logs durupages-hub 2>&1 | grep -iE "bundle|GET /|download" | tail -8 | sed 's/^/    /' || true

# ---------------------------------------------------------------------------
# Result.
# ---------------------------------------------------------------------------
if [[ "${FAILED}" -ne 0 ]]; then
  dump_diagnostics
  printf '\n\033[1;31mE2E RESULT: FAIL\033[0m\n'
  exit 1
fi
printf '\n\033[1;32mE2E RESULT: PASS\033[0m\n'
