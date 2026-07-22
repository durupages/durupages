#!/usr/bin/env bash
# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# Tear down the e2e stack and remove volumes and generated artifacts.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e/env.sh
source "${SCRIPT_DIR}/../e2e/env.sh"

cd "${REPO_ROOT}"
echo "==> docker-compose down -v"
DC down -v --remove-orphans || true

# Remove generated runtime artifacts (keys/kubeconfig/build tar).
rm -rf "${KUBE_DIR}" "${BUILD_DIR}" "${CERTS_DIR}"
echo "==> e2e environment removed"
