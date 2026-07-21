# syntax=docker/dockerfile:1
# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# Multi-target build for DuruPages components.
#   docker build --target controller -t durupages/controller .
#   docker build --target router     -t durupages/router .
#   docker build --target hub        -t durupages/hub .
#   docker build --target worker     -t durupages/worker .   # shim + workerd

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/ ./cmd/...

# Fetch a workerd binary for the worker image. Until native/durupages-workerd
# is built, the official binary is used as a dev fallback (cpuTime metering
# and isolate limits are inactive with it — see native/durupages-workerd/README.md).
FROM debian:bookworm-slim AS workerd-fetch
ARG WORKERD_NPM_VERSION=latest
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://registry.npmjs.org/@cloudflare/workerd-linux-64/-/$( \
      curl -fsSL https://registry.npmjs.org/@cloudflare/workerd-linux-64 \
      | sed -n 's/.*"latest":"\([^"]*\)".*/workerd-linux-64-\1.tgz/p')" -o /tmp/workerd.tgz \
    && tar -xzf /tmp/workerd.tgz -C /tmp \
    && install -m 0755 /tmp/package/bin/workerd /usr/local/bin/workerd

FROM gcr.io/distroless/static-debian12:nonroot AS controller
COPY --from=build /out/durupages-controller /durupages-controller
ENTRYPOINT ["/durupages-controller"]

FROM gcr.io/distroless/static-debian12:nonroot AS router
COPY --from=build /out/durupages-router /durupages-router
ENTRYPOINT ["/durupages-router"]

FROM gcr.io/distroless/static-debian12:nonroot AS hub
COPY --from=build /out/durupages-hub /durupages-hub
ENTRYPOINT ["/durupages-hub"]

# The worker image needs a libc for workerd; use debian-slim (still nonroot).
FROM debian:bookworm-slim AS worker
RUN useradd -u 10001 -r shim && useradd -u 10002 -r workerd
COPY --from=workerd-fetch /usr/local/bin/workerd /usr/local/bin/workerd
COPY --from=build /out/durupages-worker-shim /durupages-worker-shim
USER 10001
ENV DURUPAGES_WORKERD_BIN=/usr/local/bin/workerd
ENTRYPOINT ["/durupages-worker-shim"]

# worker-native: the worker image bundling the custom durupages-workerd binary
# (real per-isolate heap limit — see native/durupages-workerd/) instead of the
# fetched official binary. The binary must already be built into
# native/durupages-workerd/bin/durupages-workerd (the workerd build workflow
# runs native/durupages-workerd/build.sh before `docker build --target
# worker-native`). It only depends on glibc (libc++ is statically linked).
FROM debian:bookworm-slim AS worker-native
RUN useradd -u 10001 -r shim && useradd -u 10002 -r workerd
COPY native/durupages-workerd/bin/durupages-workerd /usr/local/bin/durupages-workerd
COPY --from=build /out/durupages-worker-shim /durupages-worker-shim
USER 10001
ENV DURUPAGES_WORKERD_BIN=/usr/local/bin/durupages-workerd
ENTRYPOINT ["/durupages-worker-shim"]
