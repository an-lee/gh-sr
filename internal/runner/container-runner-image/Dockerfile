# gh-sr container runner image.
#
# Base: ghcr.io/actions/actions-runner-dind (official GitHub-maintained
# Debian-based Docker-in-Docker image with the actions/runner binary
# pre-installed).
#
# Why this base and not docker:dind / docker:dind-rootless: docker:dind is
# Alpine (musl libc); docker:dind-rootless is also Alpine. The official
# actions/runner Linux binary is built for glibc and aborts on musl at
# run.sh with a dynamic-loader error. `actions-runner-dind` is the only
# upstream image that bundles Debian + DinD + actions/runner, and it is
# maintained by the actions/runner team at github.com/actions/runner
# (dockerfiles/Dockerfile-dind).
#
# Build arguments:
#   RUNNER_VERSION — actions/runner release tag (e.g. "2.337.0"). Default is
#                    overridden by the embedded Dockerfile callers in
#                    internal/runner/container.go at `gh sr setup` time so
#                    the image tag matches the running gh-sr version.
#
# Build-time network is required to fetch the upstream base image if it is
# not already cached locally.
ARG RUNNER_VERSION=2.337.0
FROM ghcr.io/actions/actions-runner-dind:v${RUNNER_VERSION}

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# The image's default ENTRYPOINT starts dockerd and execs the runner; we
# replace it with our entrypoint so the Sep-2026 pid-file cleanup runs
# before dockerd starts (otherwise the inner dockerd sees a stale pid
# from a previous hard kill and aborts).
ENTRYPOINT ["/entrypoint.sh"]