# gh-sr container runner image.
#
# Base: ubuntu:22.04 (Debian/glibc-based Ubuntu LTS).
#
# Why we can't reuse an upstream image: the official `ghcr.io/actions/actions-
# runner-dind` image is the natural fit (Debian + DinD + actions/runner in one
# box) but it's a private GitHub package that returns 401 to anonymous pulls.
# The official `docker:dind` and `docker:dind-rootless` images are both Alpine
# (musl libc), and the official actions/runner Linux binary is glibc-linked —
# running it on musl aborts at run.sh with a dynamic-loader error. So we build
# a thin wrapper: Ubuntu base + official dockerd via get.docker.com + the
# official actions/runner tarball + our pid-cleanup entrypoint.
#
# Build arguments:
#   RUNNER_VERSION — actions/runner release tag (e.g. "2.337.0"). Default is
#                    overridden by the embedded Dockerfile callers in
#                    internal/runner/container.go at `gh sr setup` time so
#                    the image tag matches the running gh-sr version.
#   TARGETARCH     — linux/amd64 or linux/arm64.
#
# Build-time network is required to fetch dockerd and actions/runner.

FROM ubuntu:22.04

ARG RUNNER_VERSION=2.337.0
ARG TARGETARCH=amd64

ENV DEBIAN_FRONTEND=noninteractive

# Base packages: jq (GitHub registration payload), curl (registrations +
# image pulls), git (actions/checkout), ca-certificates (TLS).
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl jq git \
 && rm -rf /var/lib/apt/lists/*

# Install dockerd via the official convenience script (same path GitHub's
# setup-docker action uses for the host). Ubuntu 22.04 (jammy) is supported;
# get.docker.com detects the distro and writes the right repo.
RUN curl -fsSL https://get.docker.com | sh \
 && rm -rf /var/lib/apt/lists/*

# Fetch and unpack the actions/runner binary. SHA verification is deferred to
# the runner's own signature checks at registration.
RUN curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${TARGETARCH}-${RUNNER_VERSION}.tar.gz" \
    | tar -xz -C /home/runner \
 && /home/runner/bin/installdependencies.sh

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# The outer runner that hosts this container is launched with --privileged
# from `gh sr up`, so the inner dockerd runs as root.
ENTRYPOINT ["/entrypoint.sh"]