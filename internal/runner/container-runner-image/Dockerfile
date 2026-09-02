# gh-sr container runner image.
#
# Base: docker:dind-rootless (Debian/glibc-based Docker Engine-in-container).
#
# Why docker:dind-rootless and not the more familiar docker:dind: docker:dind is
# built on Alpine (musl libc), but the official actions/runner Linux binary is
# built for glibc — running it on Alpine aborts at run.sh with a dynamic-loader
# error. The Debian-based dind-rootless image uses glibc, so a single FROM is
# enough — no multi-stage build, no qemu, no apt-get dance to install glibc on
# Alpine. The rootless flavour is incidental — we run the container with
# --privileged from `gh sr up`, so the inner dockerd runs as root and the
# rootless scaffolding is harmless.
#
# Build arguments:
#   RUNNER_VERSION — actions/runner release tag (e.g. "2.330.0"). Default is
#                    overridden by the embedded Dockerfile callers in
#                    internal/runner/container.go at `gh sr setup` time so
#                    the image tag matches the running gh-sr version.
#   TARGETARCH     — linux/amd64 or linux/arm64.
#
# Build-time network is required to fetch the actions/runner tarball.
FROM docker:${DOCKER_VERSION:-dind-rootless}

ARG RUNNER_VERSION=2.330.0
ARG TARGETARCH=amd64

# jq parses the GitHub registration payload at startup; git is required by
# actions/checkout. curl fetches the tarball at build time. ca-certificates
# lets the runner talk to github.com over TLS.
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl jq git ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

# Fetch and unpack the actions/runner binary. SHA verification is deferred to
# the runner's own signature checks at registration.
RUN curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${TARGETARCH}-${RUNNER_VERSION}.tar.gz" \
    | tar -xz -C /home/runner \
 && /home/runner/bin/installdependencies.sh

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# The DinD image expects to run as root so dockerd can write to
# /var/run/docker.pid, which is also what the actions/runner registration
# flow needs. The outer runner that hosts this container is launched with
# --privileged from `gh sr up`.
ENTRYPOINT ["/entrypoint.sh"]