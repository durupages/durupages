#!/usr/bin/env bash
# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# Generate the throwaway PKI the TLS e2e run uses: one CA, one certificate for
# the controller and one for the hub. Idempotent -- existing material is reused
# so re-running e2e-up does not invalidate a stack that is already trusting it.
#
# The SANs are the crux. Worker pods run inside k3s and reach the control plane
# by the compose network's fixed IPs, so the certificates need IP SANs; the
# router reaches the controller by compose service name, so that needs a DNS SAN
# too. A certificate missing either one fails only at handshake time, in a pod,
# which is exactly the kind of failure this suite exists to catch early.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e/env.sh
source "${SCRIPT_DIR}/../e2e/env.sh"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

mkdir -p "${CERTS_DIR}"
cd "${CERTS_DIR}"

if [[ -f ca.crt && -f controller.crt && -f hub.crt ]]; then
  echo "certificates already present in ${CERTS_DIR}"
  exit 0
fi

log "Generating e2e CA and server certificates"

# --- CA -------------------------------------------------------------------
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout ca.key -out ca.crt -days 7 \
  -subj "/CN=durupages-e2e-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

# issue <name> <SAN list>
issue() {
  local name="$1" san="$2"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "${name}.key" -out "${name}.csr" -subj "/CN=${name}" 2>/dev/null
  openssl x509 -req -in "${name}.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out "${name}.crt" -days 7 \
    -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "${san}") 2>/dev/null
  rm -f "${name}.csr"
}

# The controller is dialled by workers at its compose IP and by the router at
# its compose service name.
issue controller "IP:${CONTROLLER_IP},DNS:controller,DNS:localhost,IP:127.0.0.1"
# The hub is dialled by workers at its compose IP (bundle HTTP and log gRPC).
issue hub "IP:${HUB_IP},DNS:hub,DNS:localhost,IP:127.0.0.1"

# The containers run as non-root; make the material readable to them. These are
# throwaway keys for a local test stack and never leave the working tree.
chmod 0644 ./*.crt ./*.key

log "PKI written to ${CERTS_DIR}"
openssl x509 -in controller.crt -noout -subject -ext subjectAltName | sed 's/^/  /'
openssl x509 -in hub.crt -noout -subject -ext subjectAltName | sed 's/^/  /'
