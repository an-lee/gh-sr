# gh-sr container runner image (Docker tag: gh-sr/agentic-runner — historical name)
#
# Used for every runner_mode: container instance, with or without profile: agentic.
# Base image is the falcondev-oss fork of the official actions runner image, which
# backports the CUSTOM_ACTIONS_RESULTS_URL override (local Actions cache) and skips
# runner self-update while it is set. The base ships: Ubuntu 24.04, the runner
# binaries at /home/runner (user `runner`, uid 1001, docker group 123), a full
# static Docker bundle (dockerd/containerd/runc/docker CLI) — but NO gh, Node,
# zstd, or iptables; those are layered below. No ENTRYPOINT: /entrypoint.sh is
# ours.
#
# From gh-aw's point of view the container is a GitHub-hosted-VM equivalent:
# runner FS == inner daemon FS, default bridge network, socket reachable, and the
# default `docker` sandbox profile runs AWF rootless (no sudo, no NET_ADMIN,
# network isolation purely via inner-Docker topology). No DNS shimming, NAT
# rules, or docker-CLI shimming is baked in — inner dockerd's IPAM picks a
# non-colliding bridge subnet automatically, and gh-aw's gateway/AWF pass their
# own --add-host=host.docker.internal:host-gateway.
#
# Build: docker build --build-arg FORK_RUNNER_IMAGE=ghcr.io/falcondev-oss/actions-runner:2.337.0 -t gh-sr/agentic-runner:<version> .
# Run:   see internal/runner/container.go

ARG FORK_RUNNER_IMAGE
FROM ${FORK_RUNNER_IMAGE}

# The fork base ends with `USER runner`; every build step below needs root
# (apt-get, keyrings, chown). Runtime must stay root too: entrypoint.sh starts
# the inner dockerd and drops to the runner user itself via `su -`.
USER root

ENV DEBIAN_FRONTEND=noninteractive

# Core system dependencies (manifest: apt-packages-core.txt). iptables/iproute2
# are what the inner dockerd needs for bridge NAT and route inspection; zstd is
# the Actions-cache compression codec the runner uses for cache upload/download.
COPY apt-packages-core.txt /tmp/apt-packages-core.txt
RUN apt-get update \
    && grep -v '^[[:space:]]*#' /tmp/apt-packages-core.txt \
        | grep -v '^[[:space:]]*$' \
        | sed 's/\r$//' \
        | xargs -r apt-get install -y --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI (gh) — required by gh-aw
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] \
             https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Node.js LTS (NodeSource) — speeds up gh-aw framework steps; the compiler also
# emits an actions/setup-node step for non-standard runners, so this is an
# accelerator, not a hard dependency.
RUN curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && node -v \
    && npm -v \
    && rm -rf /var/lib/apt/lists/*

# Google Chrome stable — Selenium system tests in consumer workflows (Selenium
# WebDriver 4.x auto-manages the matching chromedriver, so no driver package is
# installed here; the manifest's chromium-chromedriver entry is the Ubuntu snap
# stub that ships nothing). The dl.google.com .deb registers Google's apt repo
# and keyring itself, so the build-time version is pinned per image like every
# other layer. Google publishes no arm64 Linux build, so arm64 builds skip the
# install (same defensive TARGETARCH case as the compose step below).
RUN case "${TARGETARCH}" in arm64) : ;; *) \
      apt-get update \
      && curl -fsSL -o /tmp/chrome.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
      && apt-get install -y --no-install-recommends /tmp/chrome.deb \
      && rm -f /tmp/chrome.deb \
      && google-chrome --version ;; esac \
    && rm -rf /var/lib/apt/lists/*

# Docker Compose CLI plugin — gh-aw's rootless sandbox (AWF) starts its squid
# proxy and agent containers via `docker compose`. The fork base's static Docker
# bundle ships buildx but no compose, and the plugin lookup then fails with the
# CLI's "unknown shorthand flag: 'd' in -d". Installed into the system-wide
# plugin dir so the runner user (whom AWF runs as) finds it too. TARGETARCH is
# docker's amd64/arm64; compose release assets use Go arch names (x86_64/aarch64).
ARG COMPOSE_VERSION=v5.5.0
RUN mkdir -p /usr/local/lib/docker/cli-plugins /tmp/compose \
    && case "${TARGETARCH}" in arm64) _carch=aarch64 ;; *) _carch=x86_64 ;; esac \
    && curl -fsSL "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-${_carch}" \
        -o "/tmp/compose/docker-compose-linux-${_carch}" \
    && curl -fsSL "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-${_carch}.sha256" \
        -o "/tmp/compose/docker-compose-linux-${_carch}.sha256" \
    && (cd /tmp/compose && sha256sum -c "docker-compose-linux-${_carch}.sha256") \
    && install -m 0755 "/tmp/compose/docker-compose-linux-${_carch}" /usr/local/lib/docker/cli-plugins/docker-compose \
    && rm -rf /tmp/compose \
    && docker compose version

# Tool cache — must NOT live under /opt/* because gh-aw's compiled AWF invocation
# (see repo-assist.lock.yml) intentionally SKIPS mounting any RUNNER_TOOL_CACHE
# whose path matches `/opt/*` to avoid leaking the rest of /opt (e.g. /opt/gh-aw
# control-plane files) into the agent container. Using /home/runner/.toolcache
# instead keeps it owned by the runner user, ephemeral per container (same lifetime
# as the prior /opt/hostedtoolcache), and — crucially — outside gh-aw's exclusion,
# so setup-* actions (Node, Go, Flutter, Python, …) land where the AWF agent can
# actually see them.
#
# Compatibility symlink: a handful of legacy actions (notably ruby/setup-ruby@v1)
# hardcode `/opt/hostedtoolcache` and deliberately ignore $RUNNER_TOOL_CACHE when
# they don't detect a self-hosted runner. Point /opt/hostedtoolcache at the new
# tool-cache root so those `mkdir -p` calls (and pre-extracted Ruby installs)
# resolve to the same writable, runner-owned location. The symlink target is
# /home/runner/.toolcache, so $RUNNER_TOOL_CACHE and gh-aw's mount guard continue
# to see the new path — the gh-aw fix from 5a8ae78 still works. (issue #405)
RUN mkdir -p /home/runner/.toolcache && chown runner:runner /home/runner/.toolcache \
    && ln -s /home/runner/.toolcache /opt/hostedtoolcache

# Optional extra apt packages (from runners.yml; empty file skips this step)
COPY apt-packages-extra.txt /tmp/apt-packages-extra.txt
RUN apt-get update \
    && if [ -s /tmp/apt-packages-extra.txt ]; then \
         grep -v '^[[:space:]]*#' /tmp/apt-packages-extra.txt \
             | grep -v '^[[:space:]]*$' \
             | sed 's/\r$//' \
             | xargs -r apt-get install -y --no-install-recommends; \
       fi \
    && rm -rf /var/lib/apt/lists/*

# Runner state directory (bind-mounted from host at runtime)
RUN mkdir -p /runner-state && chown runner:runner /runner-state

# Per-job reset hooks (Pillar 1): the Actions runner runs these before/after every
# job (wired via ACTIONS_RUNNER_HOOK_* in entrypoint.sh's .env). They guarantee a
# pristine inner environment per job — no leftover containers, networks, or
# /tmp/gh-aw — while preserving the image-layer cache. Stored OUTSIDE the actions
# runner application directory, as GitHub requires.
RUN mkdir -p /opt/gh-sr/hooks
COPY hooks/job-started.sh /opt/gh-sr/hooks/job-started.sh
COPY hooks/job-completed.sh /opt/gh-sr/hooks/job-completed.sh
RUN chmod +x /opt/gh-sr/hooks/job-started.sh /opt/gh-sr/hooks/job-completed.sh

# Entrypoint
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Container starts as root (see USER root above): the entrypoint boots the inner
# dockerd before switching to the runner user for the actions runner loop.
USER root
ENTRYPOINT ["/entrypoint.sh"]
