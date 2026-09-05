#!/bin/bash
# entrypoint.sh — runs inside each gh-sr container runner (root: DinD needs it).
#
# Startup (deterministic, single dockerd start):
#   1. Enable cgroup v2 nesting (DinD requirement).
#   2. Pin the inner/outer MTU when GH_SR_HOST_MTU says the host path MTU is
#      reduced (writes a mtu-only daemon.json BEFORE the single dockerd start).
#   3. Start the inner dockerd ONCE. No bip/dns pinning: dockerd's IPAM avoids
#      subnets already routed on the container's interfaces (verified with
#      docker 29 — a runner container on the host's 172.17 bridge gets an
#      inner bridge at 172.18), and gh-aw's gateway/AWF pass their own
#      --add-host=host.docker.internal:host-gateway, so no baked DNS layer.
#   4. Register the actions runner against GitHub (idempotent; one-time token,
#      --disableupdate so the fork's CUSTOM_ACTIONS_RESULTS_URL-aware runner
#      never self-updates back to stock behavior).
#   5. Wire the per-job reset hooks + cache URL into the runner .env.
#   6. exec run.sh (the actions runner loop).
#
# Per-job environment hygiene (clean /tmp/gh-aw, remove leftover containers,
# prune networks) is handled by /opt/gh-sr/hooks/job-started.sh and
# /opt/gh-sr/hooks/job-completed.sh, so every job runs from a known-clean state
# on this long-lived runner. The inner Docker image-layer cache under
# /runner-state/docker-data is preserved across jobs (never pruned).
#
# Environment variables injected by `docker run`:
#   GH_SR_RUNNER_NAME   — unique runner name (e.g. "myrepo-agentic-1")
#   GH_SR_RUNNER_TOKEN  — registration token from GitHub API
#   GH_SR_RUNNER_URL    — https://github.com/<owner>/<repo> or https://github.com/<org>
#   GH_SR_RUNNER_LABELS — comma-separated extra labels (e.g. "self-hosted,Linux,X64,agentic")
#   GH_SR_RUNNER_GROUP  — runner group (optional, default: "Default")
#   GH_SR_RUNNER_EPHEMERAL — "true" to register as ephemeral
#   GH_SR_CACHE_URL     — base URL of the per-host Actions cache server; when set,
#                         CUSTOM_ACTIONS_RESULTS_URL is exported so the fork runner
#                         sends cache traffic there (must include the trailing slash)
#   GH_SR_HOST_MTU      — host egress MTU to pin the inner/outer Docker MTU to when it is
#                         below 1500 (reduced-MTU host networks); unset/≥1500 ⇒ Docker default
#   GH_SR_DOCKERD_START_TIMEOUT — seconds to wait for inner dockerd (default 90)
#   GH_SR_BOOTSTRAP_MAX_RETRIES — consecutive dockerd-start failures before giving up (default 5)
#   GH_SR_AWF_SERVICE_BRIDGE — "1" to arm the AWF service bridge (runners.yml
#                         `awf_service_bridge: true`): forwarded to the runner
#                         .env so job-started.sh spawns the per-job waiter that
#                         joins this job's `services:` containers to awf-net

set -euo pipefail

RUNNER_DIR="/home/runner"
RUNNER_STATE_DIR="/runner-state"
RUNNER_WORK_DIR="${RUNNER_STATE_DIR}/_work"
RUNNER_TEMP_DIR="${RUNNER_STATE_DIR}/_temp"

# Persistent inner-Docker image-layer cache. This is the ONLY state preserved across
# jobs; per-job runtime state (/tmp/gh-aw, leftover containers, networks) is reset by
# the job hooks. Keeping the cache here avoids re-pulling gh-aw's images.
DOCKER_DATA_ROOT="${RUNNER_STATE_DIR}/docker-data"
mkdir -p "${DOCKER_DATA_ROOT}"

# ── 1. cgroup v2 nesting ───────────────────────────────────────────────────────
# Enable cgroup v2 nesting so the inner dockerd can create child cgroups for its
# containers (otherwise awf's compose stack fails with
# `cannot enter cgroupv2 "/sys/fs/cgroup/docker" with domain controllers --
# it is in threaded mode`). Mirrors upstream docker:dind (moby/hack/dind).
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    echo "[entrypoint] enabling cgroup v2 nesting..."
    mkdir -p /sys/fs/cgroup/init
    xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
    sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers \
        > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || \
        echo "[entrypoint] WARNING: failed to enable cgroup controllers (continuing anyway)"
fi

# ── 2. Reduced-MTU pinning (optional; strictly before the single dockerd start) ─
# GH_SR_HOST_MTU is injected by `docker create` (internal/runner/container.go) with the
# MTU of the HOST's primary egress interface when it is below Docker's 1500 default —
# e.g. cloud overlay networks (GCP defaults to 1460), VPN/WireGuard, or nested
# virtualisation. It can also be forced via runners.yml (container_runner_image.mtu).
#
# WHY THIS EXISTS: the outer runner container sits on the host's Docker bridge (MTU
# 1500) and the inner dockerd networks also default to 1500. When the real host path
# MTU is smaller and PMTUD is black-holed (ICMP "fragmentation needed" filtered — very
# common), small packets pass (DNS, TCP SYN/ACK) so connections OPEN, but large packets
# are silently dropped. TLS handshakes (ServerHello + certificate chain span several
# full-size segments) then stall and the socket is torn down mid-handshake. Node-based
# downloads surface this as "Client network socket disconnected before secure TLS
# connection was established" — exactly how actions/setup-go fails on such hosts while
# the host itself downloads fine (its real NIC never emits oversized frames).
#
# Pinning the inner networks' MTU (daemon.json `mtu`, applied to every network the
# inner dockerd creates) AND the outer container's egress interface MTU (below) to the
# host's real MTU makes TCP advertise a matching MSS in BOTH directions, so large TLS
# packets fit and never depend on PMTUD. We only ever LOWER the MTU; an unset/≥1500
# value leaves Docker's 1500 default untouched and no daemon.json is written at all.
BRIDGE_MTU=""
case "${GH_SR_HOST_MTU:-}" in
    '' | *[!0-9]*) ;;  # unset or non-numeric → keep Docker's 1500 default
    *)
        if [ "${GH_SR_HOST_MTU}" -ge 576 ] && [ "${GH_SR_HOST_MTU}" -lt 1500 ]; then
            BRIDGE_MTU="${GH_SR_HOST_MTU}"
        fi
        ;;
esac

if [ -n "${BRIDGE_MTU}" ]; then
    # Single-key daemon.json, written once before the only dockerd start (never
    # after, and no dockerd restart — the daemon reads the final config here).
    printf '{\n  "mtu": %s\n}\n' "${BRIDGE_MTU}" > /etc/docker/daemon.json
    echo "[entrypoint] inner Docker MTU pinned to ${BRIDGE_MTU} (host egress MTU)"

    # Lower the runner container's OWN egress interface MTU too. Workflow setup steps
    # such as actions/setup-go run directly in THIS (outer) container — not the inner
    # Docker — so the inner-bridge MTU alone would not fix their downloads. Lowering
    # eth0's MTU makes the kernel advertise a matching TCP MSS for every connection
    # the runner opens. Best-effort: needs NET_ADMIN, which the --privileged runner
    # container always has.
    _egress_if=$(ip -o route show default 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="dev") {print $(i+1); exit}}' || true)
    [ -n "${_egress_if}" ] || _egress_if=eth0
    if ip link set dev "${_egress_if}" mtu "${BRIDGE_MTU}" 2>/dev/null; then
        echo "[entrypoint] ${_egress_if} MTU pinned to ${BRIDGE_MTU} (host egress MTU)"
    else
        echo "[entrypoint] WARNING: could not set ${_egress_if} MTU to ${BRIDGE_MTU}"
    fi
fi

# ── 3. Inner dockerd (single start) ─────────────────────────────────────────────
# A previous boot may have ended without dockerd shutting down cleanly (crash,
# registration failure, host power loss). Its pid/socket files survive a
# `docker restart` in the container filesystem, and the stale containerd pid
# file makes the new dockerd refuse to start ("process with PID N is still
# running") for as long as the fresh pid namespace happens to reuse that PID.
# No dockerd is running at this point, so any leftover is stale — remove it.
rm -rf /run/docker /var/run/docker.pid /var/run/docker.sock 2>/dev/null || true
echo "[entrypoint] starting dockerd..."
dockerd \
    --data-root="${DOCKER_DATA_ROOT}" \
    --host=unix:///var/run/docker.sock \
    --log-level=warn \
    &>/runner-state/dockerd.log &

DOCKERD_PID=$!

# Wait until the socket is available (configurable; default 90s).
DOCKERD_START_TIMEOUT="${GH_SR_DOCKERD_START_TIMEOUT:-90}"
case "${DOCKERD_START_TIMEOUT}" in
    '' | *[!0-9]*) DOCKERD_START_TIMEOUT=90 ;;
    *)
        if [ "${DOCKERD_START_TIMEOUT}" -lt 30 ]; then DOCKERD_START_TIMEOUT=30; fi
        if [ "${DOCKERD_START_TIMEOUT}" -gt 300 ]; then DOCKERD_START_TIMEOUT=300; fi
        ;;
esac
BOOTSTRAP_MAX_RETRIES="${GH_SR_BOOTSTRAP_MAX_RETRIES:-5}"
case "${BOOTSTRAP_MAX_RETRIES}" in
    '' | *[!0-9]*) BOOTSTRAP_MAX_RETRIES=5 ;;
    *)
        if [ "${BOOTSTRAP_MAX_RETRIES}" -lt 1 ]; then BOOTSTRAP_MAX_RETRIES=1; fi
        if [ "${BOOTSTRAP_MAX_RETRIES}" -gt 20 ]; then BOOTSTRAP_MAX_RETRIES=20; fi
        ;;
esac
DOCKERD_FAILURES_FILE="${RUNNER_STATE_DIR}/dockerd-start-failures"
BOOTSTRAP_FAILED_FILE="${RUNNER_STATE_DIR}/bootstrap-failed"

for i in $(seq 1 "${DOCKERD_START_TIMEOUT}"); do
    if docker info &>/dev/null 2>&1; then
        echo "[entrypoint] dockerd is up"
        rm -f "${DOCKERD_FAILURES_FILE}"
        # Self-heal a marker left by a previous failed boot: a successful boot
        # must clear it, or status/doctor keep reporting "failed" for a healthy
        # runner (a host-side rm cannot reach it — the state dir may be
        # root-owned, while this entrypoint runs as root in the container).
        rm -f "${BOOTSTRAP_FAILED_FILE}"
        break
    fi
    if [ "$i" -eq "${DOCKERD_START_TIMEOUT}" ]; then
        _failures=0
        if [ -f "${DOCKERD_FAILURES_FILE}" ]; then
            _failures=$(cat "${DOCKERD_FAILURES_FILE}" 2>/dev/null || echo 0)
            case "${_failures}" in '' | *[!0-9]*) _failures=0 ;; esac
        fi
        _failures=$(( _failures + 1 ))
        echo "${_failures}" > "${DOCKERD_FAILURES_FILE}"
        echo "[entrypoint] ERROR: dockerd did not start within ${DOCKERD_START_TIMEOUT} seconds (attempt ${_failures}/${BOOTSTRAP_MAX_RETRIES})" >&2
        if [ "${_failures}" -ge "${BOOTSTRAP_MAX_RETRIES}" ]; then
            date -u +"%Y-%m-%dT%H:%M:%SZ dockerd bootstrap failed after ${_failures} attempts" > "${BOOTSTRAP_FAILED_FILE}"
            echo "[entrypoint] ERROR: bootstrap failed after ${BOOTSTRAP_MAX_RETRIES} consecutive dockerd start failures; holding container (run gh sr up to retry after fixing the host)" >&2
            kill "${DOCKERD_PID}" 2>/dev/null || true
            exec sleep infinity
        fi
        exit 1
    fi
    sleep 1
done

# ── 4. Register the actions runner ──────────────────────────────────────────────
cd "${RUNNER_DIR}"

RUNNER_NAME="${GH_SR_RUNNER_NAME:-gh-sr-runner}"
RUNNER_URL="${GH_SR_RUNNER_URL:?GH_SR_RUNNER_URL is required}"
RUNNER_TOKEN="${GH_SR_RUNNER_TOKEN:?GH_SR_RUNNER_TOKEN is required}"
RUNNER_LABELS="${GH_SR_RUNNER_LABELS:-self-hosted,Linux,X64,agentic}"
RUNNER_GROUP="${GH_SR_RUNNER_GROUP:-Default}"

mkdir -p "${RUNNER_WORK_DIR}" "${RUNNER_TEMP_DIR}"
# Work and temp dirs are created as root; give the runner user ownership so it
# can create job workspaces inside them.
chown runner:runner "${RUNNER_WORK_DIR}" "${RUNNER_TEMP_DIR}"

CONFIG_ARGS=(
    --url "${RUNNER_URL}"
    --token "${RUNNER_TOKEN}"
    --name "${RUNNER_NAME}"
    --labels "${RUNNER_LABELS}"
    --work "${RUNNER_WORK_DIR}"
    --runnergroup "${RUNNER_GROUP}"
    --unattended
    --replace
    --disableupdate
)

if [ "${GH_SR_RUNNER_EPHEMERAL:-false}" = "true" ]; then
    CONFIG_ARGS+=(--ephemeral)
fi

# RUNNER_TEMP must not be /tmp (gh-aw explicitly requires a non-/tmp path).
export RUNNER_TEMP="${RUNNER_TEMP_DIR}"

# RUNNER_TOOL_CACHE must live OUTSIDE /opt/* — gh-aw's compiled AWF invocation
# (e.g. repo-assist.lock.yml) does:
#     if [[ "$GH_AW_TOOL_CACHE" != /opt/* ]]; then
#         GH_AW_TOOL_CACHE_MOUNT="$GH_AW_TOOL_CACHE:$GH_AW_TOOL_CACHE:ro"
#     fi
# i.e. it deliberately skips mounting any tool cache under /opt to avoid leaking
# other /opt/<x> (e.g. /opt/gh-aw control-plane files) into the agent container.
# /home/runner/.toolcache is owned by the runner user, ephemeral per container
# (same lifetime as the old /opt/hostedtoolcache), and passes gh-aw's guard so
# setup-* actions (Node, Go, Flutter, Python, …) are reachable from inside AWF.
#
# The Dockerfile also installs a /opt/hostedtoolcache -> /home/runner/.toolcache
# symlink so legacy actions (notably ruby/setup-ruby@v1, which hardcodes
# `/opt/hostedtoolcache` when it doesn't detect a self-hosted runner) can still
# `mkdir -p` their tool-cache subdir — they land in the same writable location,
# while $RUNNER_TOOL_CACHE and the AWF mount guard keep seeing /home/runner/...
export RUNNER_TOOL_CACHE="/home/runner/.toolcache"

# ── 5. Wire per-job reset hooks + local cache into the runner .env ──────────────
# The Actions runner reads .env at startup and applies the variables to its own
# process environment, so both the runner (fork Runner.cs reads
# CUSTOM_ACTIONS_RESULTS_URL to redirect cache traffic) and the job hooks see
# everything. We write it deterministically (the file lives in the image rootfs
# and persists across container restarts, so overwrite rather than append to stay
# idempotent).
{
    echo "RUNNER_TEMP=${RUNNER_TEMP_DIR}"
    echo "RUNNER_TOOL_CACHE=/home/runner/.toolcache"
    if [ -n "${GH_SR_CACHE_URL:-}" ]; then
        echo "CUSTOM_ACTIONS_RESULTS_URL=${GH_SR_CACHE_URL}"
    fi
    if [ "${GH_SR_AWF_SERVICE_BRIDGE:-0}" = "1" ]; then
        echo "GH_SR_AWF_SERVICE_BRIDGE=1"
    fi
    echo "ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/gh-sr/hooks/job-started.sh"
    echo "ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/gh-sr/hooks/job-completed.sh"
} > "${RUNNER_DIR}/.env"
chown runner:runner "${RUNNER_DIR}/.env"

# su - resets the environment to a login shell's defaults, so RUNNER_TEMP,
# RUNNER_TOOL_CACHE and (when set) CUSTOM_ACTIONS_RESULTS_URL are re-exported on
# the runner-user side of every invocation below.
RUNNER_ENV="RUNNER_TEMP='${RUNNER_TEMP_DIR}' RUNNER_TOOL_CACHE='${RUNNER_TOOL_CACHE}'"
if [ -n "${GH_SR_CACHE_URL:-}" ]; then
    RUNNER_ENV="${RUNNER_ENV} CUSTOM_ACTIONS_RESULTS_URL='${GH_SR_CACHE_URL}'"
fi

# Only configure if not already done. GitHub registration tokens are one-time-use;
# running config.sh on every restart would consume the token and fail on the second start.
# (Same behaviour as native runners — configure once, restart many times.)
if [ ! -f "${RUNNER_DIR}/.runner" ]; then
    echo "[entrypoint] configuring runner..."
    su - runner -c "cd '${RUNNER_DIR}' && ${RUNNER_ENV} ./config.sh ${CONFIG_ARGS[*]@Q}" 2>&1
else
    echo "[entrypoint] runner already configured, skipping config.sh"
fi

# ── 6. Pre-pull gh-aw images (best-effort, background) ──────────────────────────
# The job's own download_docker_images.sh step pulls the digest-pinned copies it
# actually uses; this warms the inner cache with the floating tags so first jobs
# start faster.
su - runner -c "
    docker pull ghcr.io/github/gh-aw-firewall/agent:latest &>/dev/null &
    docker pull ghcr.io/github/gh-aw-firewall/squid:latest &>/dev/null &
    docker pull ghcr.io/github/gh-aw-firewall/api-proxy:latest &>/dev/null &
    docker pull ghcr.io/github/gh-aw-mcpg:latest &>/dev/null &
    docker pull ghcr.io/github/gh-aw-node:latest &>/dev/null &
" 2>/dev/null || true

# ── 7. Graceful shutdown handler ────────────────────────────────────────────────
# On SIGTERM (sent by `docker stop`, the Docker daemon, or systemd), tear down
# the inner dockerd so its containers/networks do not leak. We deliberately do
# NOT call `config.sh remove --token ...` here:
#
#   - RUNNER_TOKEN is the GitHub *registration* token (one-time, ~1h TTL). After
#     it expires, `config.sh remove` fails silently and the call is a no-op.
#   - On the next start the entrypoint runs `config.sh --replace` (idempotent
#     against an existing runner with the same name), so the GitHub-side
#     registration is renewed automatically without us having to deregister.
#   - This matches native runners, whose `svc.sh` / systemd unit never
#     deregister on stop — they just let the next `gh sr up` re-attach.
#
# Exit 0 so `docker stop` is honored as an explicit operator action; the
# `--restart unless-stopped` policy on the container is what decides whether
# the next event (crash, OOM, daemon restart, host reboot) brings it back.
_shutdown() {
    echo "[entrypoint] received SIGTERM — stopping runner..."
    # Shut down the inner dockerd.
    kill "${DOCKERD_PID}" 2>/dev/null && wait "${DOCKERD_PID}" 2>/dev/null || true
    exit 0
}
trap _shutdown SIGTERM SIGINT

# ── 8. Run ──────────────────────────────────────────────────────────────────────
echo "[entrypoint] starting actions runner as user 'runner'..."
exec su - runner -c "cd '${RUNNER_DIR}' && ${RUNNER_ENV} ./run.sh"
