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

ENTRYPOINT ["/entrypoint.sh"]
