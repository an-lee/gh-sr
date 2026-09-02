# gh-sr container runner image.
#
# Base: docker:dind (Docker Engine in a container, official Docker image).
# Each runner container runs an inner dockerd so that `services:` blocks in
# workflows (postgres, redis, etc.) and `docker build` steps have a daemon to
# talk to without sharing the host's Docker.
#
# Build arguments:
#   RUNNER_VERSION — actions/runner release tag (e.g. "2.330.0"). Default is
#                    overridden by the embedded Dockerfile callers in
#                    internal/runner/container.go at `gh sr setup` time so
#                    the image tag matches the running gh-sr version.
#   TARGETARCH     — linux/amd64 or linux/arm64; matches docker:dind.
#
# Build-time network is required to fetch the actions/runner tarball.
FROM docker:${DOCKER_VERSION:-dind}

ARG RUNNER_VERSION=2.330.0
ARG TARGETARCH=amd64

# jq parses the GitHub registration payload at startup; bash is the entrypoint shell;
# curl fetches the tarball at build time. git is required by actions/checkout.
RUN apk add --no-cache bash curl jq git ca-certificates tzdata \
    || apt-get update && apt-get install -y --no-install-recommends bash curl jq git ca-certificates tzdata

# Fetch and unpack the actions/runner binary. SHA verification is deferred to
# the runner's own signature checks at registration.
RUN curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${TARGETARCH}-${RUNNER_VERSION}.tar.gz" \
    | tar -xz -C /home/runner \
 && /home/runner/bin/installdependencies.sh || true

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# The DinD image expects to run as root so dockerd can write to /var/run/docker.pid,
# which is also what the actions/runner registration flow needs (it bind-mounts the
# host's .runner dir into /home/runner). The outer runner that hosts this container
# is launched with --privileged from `gh sr up`.
ENTRYPOINT ["/entrypoint.sh"]