# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# e2e-only wrapper around the distroless router image.
#
# The root Dockerfile builds durupages/router on gcr.io/distroless/static, which
# has no shell and no iproute2. In the compose e2e environment the router must
# install a route to the k3s pod CIDR (10.42.0.0/16) so it can reverse-proxy
# directly to worker pod IPs. This stage repackages the already-built router
# binary on top of alpine + iproute2 and wraps it with entrypoint.sh, which adds
# the route before exec'ing the binary. The root Dockerfile is left untouched.
#
# Build context is the repo root; durupages/router:e2e must already exist in the
# local image store (scripts/e2e-up.sh builds it first).
FROM durupages/router:e2e AS router

FROM alpine:3.20
RUN apk add --no-cache iproute2 ca-certificates
COPY --from=router /durupages-router /usr/local/bin/durupages-router
COPY e2e/router-net/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
