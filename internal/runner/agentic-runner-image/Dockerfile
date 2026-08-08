# gh-sr container runner image (Docker tag: gh-sr/agentic-runner — historical name)
#
# Used for every runner_mode: container instance, with or without profile: agentic.
# Each instance runs in its own container, providing:
#   - isolated /tmp/gh-aw when running agentic workflows (per-container filesystem)
#   - isolated dockerd and iptables (per-container network namespace)
#   - isolated MCP gateway on port 80 for agentic jobs (no per-host port labeling)
#
# Build: docker build -t gh-sr/agentic-runner:<version> .
# Run:   see internal/runner/container.go

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Core system dependencies (manifest: apt-packages-core.txt)
COPY apt-packages-core.txt /tmp/apt-packages-core.txt
RUN apt-get update \
    && grep -v '^[[:space:]]*#' /tmp/apt-packages-core.txt \
        | grep -v '^[[:space:]]*$' \
        | sed 's/\r$//' \
        | xargs -r apt-get install -y --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

# Docker CE (for DinD — the inner dockerd that gh-aw uses)
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
        | gpg --dearmor -o /etc/apt/keyrings/docker.gpg \
    && chmod a+r /etc/apt/keyrings/docker.gpg \
    && echo \
       "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
        https://download.docker.com/linux/ubuntu \
        $(lsb_release -cs) stable" \
       > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        docker-ce \
        docker-ce-cli \
        containerd.io \
        docker-buildx-plugin \
        docker-compose-plugin \
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

# Node.js LTS (NodeSource) — gh-aw activation setup installs @actions/artifact via npm
# before actions/setup-node runs in the agent job.
RUN curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && node -v \
    && npm -v \
    && rm -rf /var/lib/apt/lists/*

# runner user (non-root, but in docker group so it can talk to the inner dockerd)
# Full passwordless sudo is required: AWF installs binaries to system paths
# (/usr/local/lib/awf/) and manipulates iptables rules at job runtime.
# The container is already --privileged and single-purpose, so this is safe.
RUN useradd --create-home --shell /bin/bash runner \
    && usermod -aG docker runner \
    && echo "runner ALL=(ALL) NOPASSWD: ALL" \
       > /etc/sudoers.d/runner-nopasswd \
    && chmod 0440 /etc/sudoers.d/runner-nopasswd

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

# Actions runner binary — installed at image build time so startup is fast.
# RUNNER_VERSION is injected by `docker build --build-arg RUNNER_VERSION=<ver>`.
ARG RUNNER_VERSION
ARG RUNNER_ARCH=x64
RUN if [ -z "$RUNNER_VERSION" ]; then \
        echo "ERROR: RUNNER_VERSION build-arg is required" >&2; exit 1; \
    fi \
    && mkdir -p /home/runner/actions-runner \
    && curl -fsSL \
       "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
       | tar -xz -C /home/runner/actions-runner \
    && chown -R runner:runner /home/runner/actions-runner \
    && /home/runner/actions-runner/bin/installdependencies.sh

# gh-aw CLI extension
RUN su - runner -c "curl -sL https://raw.githubusercontent.com/github/gh-aw/main/install-gh-aw.sh | bash"

# Agentic Workflow Firewall CLI. Compiled gh-aw workflows invoke `sudo -E awf`
# directly, so this must be installed globally instead of relying on the gh-aw
# extension installer.
RUN curl -sSL https://raw.githubusercontent.com/github/gh-aw-firewall/main/install.sh \
        | AWF_FORCE_BINARY=1 bash \
    && awf --version

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

# Deterministic inner-Docker networking (Pillar 2): pin the default-bridge gateway
# and point container DNS at the bundled dnsmasq so host.docker.internal resolves
# without a runtime daemon.json rewrite + dockerd restart. The single baked
# daemon.json is read at the one dockerd start in entrypoint.sh.
COPY daemon.json /etc/docker/daemon.json
COPY dnsmasq-gh-sr.conf /etc/dnsmasq.d/gh-sr.conf
RUN chmod 0644 /etc/docker/daemon.json /etc/dnsmasq.d/gh-sr.conf

# Per-job reset hooks (Pillar 1): the Actions runner runs these before/after every
# job (wired via ACTIONS_RUNNER_HOOK_* in entrypoint.sh's .env). They guarantee a
# pristine inner environment per job — no leftover containers, networks, AWF iptables,
# or /tmp/gh-aw — while preserving the image-layer cache. Stored OUTSIDE the actions
# runner application directory, as GitHub requires.
RUN mkdir -p /opt/gh-sr/hooks
COPY hooks/job-started.sh /opt/gh-sr/hooks/job-started.sh
COPY hooks/job-completed.sh /opt/gh-sr/hooks/job-completed.sh
RUN chmod +x /opt/gh-sr/hooks/job-started.sh /opt/gh-sr/hooks/job-completed.sh

# gh-sr Docker CLI shim for gh-aw inside container-mode runners (see docker-wrapper.sh).
# It is NOT what isolates concurrent runners on the host — that is runner_mode: container
# (separate inner dockerd, network namespace, MCP gateway port, /tmp/gh-aw per outer container).
# The minimal shim only touches gh-aw-mcpg launches: (1) injects --hostname gh-aw-mcpg so the
# gateway skips its flaky self-inspect under inner --network host/DinD, and (2) injects a
# deterministic --name gh-aw-mcpg-ghsr-* on `docker run` so the per-job reset hooks can reap a
# leftover gateway. host.docker.internal resolution is handled by baked DNS (daemon.json +
# dnsmasq); per-job cleanup is handled by the job hooks — the shim has no supervisor/cidfile/
# URL-rewriter. Install as /opt/gh-sr/docker-shim/docker only — entrypoint and root keep
# /usr/bin/docker for dockerd checks. Runner jobs get this dir first via /etc/profile.d, sudo
# secure_path, and PATH=… in entrypoint.sh RUNNER_ENV.
RUN mkdir -p /opt/gh-sr/docker-shim
COPY docker-wrapper.sh /opt/gh-sr/docker-shim/docker
RUN chmod +x /opt/gh-sr/docker-shim/docker \
    && printf '%s\n' 'export PATH="/opt/gh-sr/docker-shim:$PATH"' \
        > /etc/profile.d/gh-sr-docker-shim.sh \
    && chmod 644 /etc/profile.d/gh-sr-docker-shim.sh \
    && printf '%s\n' \
        'Defaults:runner secure_path="/opt/gh-sr/docker-shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"' \
        > /etc/sudoers.d/runner-secure-path \
    && chmod 0440 /etc/sudoers.d/runner-secure-path \
    && visudo -cf /etc/sudoers.d/runner-secure-path

# Entrypoint
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
