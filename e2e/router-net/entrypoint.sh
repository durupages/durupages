#!/bin/sh
# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# Router entrypoint for the e2e compose environment.
#
# Worker pods live in the k3s pod network (default 10.42.0.0/16), which is only
# reachable through the k3s node container. We add a route to that CIDR via the
# k3s node's static compose IP so the router can reverse-proxy directly to pod
# IPs. Requires NET_ADMIN (granted in docker-compose.yaml).
set -e

: "${K3S_NODE_IP:?K3S_NODE_IP must be set (the k3s node's static compose IP)}"
POD_CIDR="${POD_CIDR:-10.42.0.0/16}"

echo "router-net: adding route ${POD_CIDR} via ${K3S_NODE_IP}"
ip route replace "${POD_CIDR}" via "${K3S_NODE_IP}" || {
  echo "router-net: WARNING failed to add pod-network route" >&2
}

exec /usr/local/bin/durupages-router "$@"
