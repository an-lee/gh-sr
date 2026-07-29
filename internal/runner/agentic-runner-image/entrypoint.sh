#!/bin/bash
# entrypoint.sh — runs inside each gh-sr container runner.
#
# Startup (deterministic, single dockerd start):
#   1. Enable cgroup v2 nesting (DinD requirement).
#   2. Start the inner dockerd ONCE. host.docker.internal DNS is baked into the
#      image (/etc/docker/daemon.json pins the default-bridge gateway to 10.200.0.1
#      and points container DNS at the bundled dnsmasq) — no runtime daemon.json
#      rewrite or dockerd restart. 10.200.0.1 is used (not 172.17.0.1) because the
#      outer runner container sits on the host's default Docker bridge
#      (172.17.0.0/16); a 172.17.x inner bridge would collide with the container's
#      own gateway/eth0 subnet and black-hole all outbound traffic.
#   3. Start dnsmasq (baked config + a startup-generated upstream drop-in seeded from
#      the runner's ORIGINAL resolvers), then repoint the runner container's
#      /etc/resolv.conf at dnsmasq. gh-aw's firewall auto-detects the agent sandbox
#      DNS from that resolv.conf, so this makes host.docker.internal resolve
#      authoritatively to the AWF-exempt inner-bridge gateway for the agent too —
#      otherwise the sandbox inherits the outer host resolver and intermittently
#      routes the MCP gateway request through Squid (ERR_INVALID_URL → MCP launch
#      failure).
#   4. Install the AWF service-routing bypass (one-shot; lets AWF agents reach
#      workflow `services:` published ports).
#   5. Register the actions runner against GitHub (idempotent; one-time token).
#   6. Wire the per-job reset hooks into the runner .env.
#   7. exec run.sh (the actions runner loop).
#
# Per-job environment hygiene (clean /tmp/gh-aw, remove leftover containers, prune
# networks, flush AWF iptables) is handled by /opt/gh-sr/hooks/job-started.sh and
# /opt/gh-sr/hooks/job-completed.sh, so every job runs from a known-clean state on
# this long-lived runner. The inner Docker image-layer cache under
# /runner-state/docker-data is preserved across jobs (never pruned).
#
# Environment variables injected by `docker run`:
#   GH_SR_RUNNER_NAME   — unique runner name (e.g. "myrepo-agentic-1")
#   GH_SR_RUNNER_TOKEN  — registration token from GitHub API
#   GH_SR_RUNNER_URL    — https://github.com/<owner>/<repo> or https://github.com/<org>
#   GH_SR_RUNNER_LABELS — comma-separated extra labels (e.g. "self-hosted,Linux,X64,agentic")
#   GH_SR_RUNNER_GROUP  — runner group (optional, default: "Default")
#   GH_SR_RUNNER_EPHEMERAL — "true" to register as ephemeral
#   GH_SR_AWF_SUBNET    — AWF bridge subnet for the service-routing bypass (default 172.30.0.0/24)
#   GH_SR_HOST_MTU      — host egress MTU to pin the inner/outer Docker MTU to when it is
#                         below 1500 (reduced-MTU host networks); unset/≥1500 ⇒ Docker default
#   GH_SR_DOCKERD_START_TIMEOUT — seconds to wait for inner dockerd (default 90)
#   GH_SR_BOOTSTRAP_MAX_RETRIES — consecutive dockerd-start failures before giving up (default 5)

set -euo pipefail

RUNNER_DIR="/home/runner/actions-runner"
RUNNER_STATE_DIR="/runner-state"
RUNNER_WORK_DIR="${RUNNER_STATE_DIR}/_work"
RUNNER_TEMP_DIR="${RUNNER_STATE_DIR}/_temp"

# Persistent inner-Docker image-layer cache. This is the ONLY state preserved across
# jobs; per-job runtime state (/tmp/gh-aw, leftover containers, networks, iptables) is
# reset by the job hooks. Keeping the cache here avoids re-pulling gh-aw's images.
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

# ── 2. Inner-bridge subnet collision avoidance (pre-start; no restart) ──────────
# The image bakes a deterministic inner-bridge subnet into /etc/docker/daemon.json
# (10.200.0.1/24) and the dnsmasq config. 10.200.0.0/24 is chosen because it sits
# OUTSIDE Docker's default address pools (172.16.0.0/12 and 192.168.0.0/16), so it
# does not normally collide with anything Docker auto-allocates — including the host
# default bridge 172.17.0.0/16 that the outer runner container's eth0 usually sits on.
#
# WHY THIS CHECK EXISTS (defence in depth):
#   The *outer* runner container's networks are decided by whoever runs the image
#   (the host's Docker / orchestrator), NOT by us. A custom host could attach the
#   runner container to an arbitrary subnet — conceivably even 10.200.0.0/24, or a
#   broad 10.0.0.0/8. If the baked inner-bridge subnet overlapped one of the runner
#   container's OWN interfaces, the inner docker0 would duplicate a directly-attached
#   or gateway IP and capture that route. Outbound traffic would then be black-holed
#   into the inner bridge: the host network stays fine, but every connection made
#   inside the runner (agent → model provider, git, package installs) crawls or times
#   out. This is exactly the failure that a hardcoded 172.17.0.1 bip caused, and the
#   reason we no longer pin to the default-bridge subnet.
#
#   Docker's own IPAM auto-avoids these conflicts when `bip` is unset, but we pin
#   `bip` for a deterministic dnsmasq listen address — which bypasses that safety
#   net. This block re-adds it: it validates the baked gateway against the container's
#   CURRENT interfaces and, only on conflict, rewrites daemon.json + the dnsmasq
#   config to the first collision-free candidate.
#
# CRITICAL ORDERING: this runs BEFORE the single dockerd start below. We never write
# daemon.json after dockerd is up and we never restart dockerd — the daemon reads the
# final config on its one and only start. (The old "write daemon.json then kill +
# restart dockerd" dance was a major instability source; do not reintroduce it.)
DEFAULT_BRIDGE_GW="10.200.0.1"
# Candidates are tried in order; all are /24s outside the host's default bridge.
BRIDGE_CANDIDATES=(10.200.0.1 10.201.0.1 10.210.0.1 192.168.221.1 172.28.0.1 172.20.0.1)

# subnet_is_free <gateway-ip>: true when the IP must be forwarded via a gateway
# rather than being inside a subnet directly attached to this container. `ip route
# get` returns an on-link route ("dev <if>", no "via") for an address inside a local
# subnet or equal to a directly-attached gateway, and a "via <gw>" route otherwise.
# docker0 does not exist yet (dockerd has not started), so this only tests against the
# runner container's pre-existing interfaces (eth0, lo, ...).
subnet_is_free() {
    ip -4 route get "$1" 2>/dev/null | head -n1 | grep -q ' via '
}

BRIDGE_GW=""
for _cand in "${BRIDGE_CANDIDATES[@]}"; do
    if subnet_is_free "${_cand}"; then
        BRIDGE_GW="${_cand}"
        break
    fi
done
# Last resort: if every candidate looked busy (or `ip route` is unavailable), keep the
# documented default rather than failing the whole runner.
BRIDGE_GW="${BRIDGE_GW:-$DEFAULT_BRIDGE_GW}"

# ── 2a. Reduced-MTU pinning (optional; strictly before the single dockerd start) ─
# GH_SR_HOST_MTU is injected by `docker create` (internal/runner/container.go) with the
# MTU of the HOST's primary egress interface when it is below Docker's 1500 default —
# e.g. cloud overlay networks (GCP defaults to 1460), VPN/WireGuard, or nested
# virtualisation. It can also be forced via runners.yml (container_runner_image.mtu).
#
# WHY THIS EXISTS: the outer runner container sits on the host's default Docker bridge
# (MTU 1500) and the inner dockerd bridge also defaults to 1500. When the real host path
# MTU is smaller and PMTUD is black-holed (ICMP "fragmentation needed" filtered — very
# common), small packets pass (DNS, TCP SYN/ACK) so connections OPEN, but large packets
# are silently dropped. TLS handshakes (ServerHello + certificate chain span several
# full-size segments) then stall and the socket is torn down mid-handshake. Node-based
# downloads surface this as "Client network socket disconnected before secure TLS
# connection was established" — exactly how actions/setup-go fails on such hosts while
# the host itself downloads fine (its real NIC never emits oversized frames).
#
# Pinning the inner bridge MTU (daemon.json, below) AND the outer container's egress
# interface MTU (further down) to the host's real MTU makes TCP advertise a matching MSS
# in BOTH directions, so large TLS packets fit and never depend on PMTUD. We only ever
# LOWER the MTU; an unset/≥1500 value leaves Docker's 1500 default untouched.
BRIDGE_MTU=""
case "${GH_SR_HOST_MTU:-}" in
    '' | *[!0-9]*) ;;  # unset or non-numeric → keep Docker's 1500 default
    *)
        if [ "${GH_SR_HOST_MTU}" -ge 576 ] && [ "${GH_SR_HOST_MTU}" -lt 1500 ]; then
            BRIDGE_MTU="${GH_SR_HOST_MTU}"
        fi
        ;;
esac

# write_daemon_json: emit /etc/docker/daemon.json from the resolved gateway (+ optional
# MTU). ONLY ever called below, strictly BEFORE the single dockerd start — never after
# (the historical "write then restart dockerd" dance is forbidden; see the test guard).
write_daemon_json() {
    {
        echo "{"
        echo "  \"bip\": \"${BRIDGE_GW}/24\","
        if [ -n "${BRIDGE_MTU}" ]; then
            # Single-quote the key so the literal "mtu": appears verbatim in this script
            # (the bip/dns lines must interpolate ${BRIDGE_GW}, so they stay double-quoted).
            echo '  "mtu": '"${BRIDGE_MTU}"','
        fi
        echo "  \"dns\": [\"${BRIDGE_GW}\", \"8.8.8.8\"]"
        echo "}"
    } > /etc/docker/daemon.json
}

if [ "${BRIDGE_GW}" != "${DEFAULT_BRIDGE_GW}" ]; then
    echo "[entrypoint] WARNING: baked inner-bridge gateway ${DEFAULT_BRIDGE_GW} overlaps an existing interface; using ${BRIDGE_GW} instead"
    # One-shot rewrite, strictly before the dockerd start below (no restart).
    write_daemon_json
    # Update only the gateway-bearing directives. Upstreams live in the runtime drop-in
    # generated below (section 3), which is keyed off the same ${BRIDGE_GW}.
    sed -i \
        -e "s#^address=/host.docker.internal/.*#address=/host.docker.internal/${BRIDGE_GW}#" \
        -e "s#^listen-address=.*#listen-address=${BRIDGE_GW}#" \
        /etc/dnsmasq.d/gh-sr.conf
elif [ -n "${BRIDGE_MTU}" ]; then
    # No gateway conflict, but the baked daemon.json carries no `mtu`; inject it (still
    # before the single dockerd start) so the inner bridge and every inner container
    # inherit the reduced MTU.
    write_daemon_json
    echo "[entrypoint] inner-bridge gateway ${BRIDGE_GW} (baked default); inner MTU pinned to ${BRIDGE_MTU}"
else
    echo "[entrypoint] inner-bridge gateway ${BRIDGE_GW} (baked default; no interface conflict detected)"
fi

# Lower the runner container's OWN egress interface MTU too. Workflow setup steps such
# as actions/setup-go run directly in THIS (outer) container — not the inner Docker — so
# the inner-bridge MTU alone would not fix their downloads. Lowering eth0's MTU makes the
# kernel advertise a matching TCP MSS for every connection the runner opens. Best-effort:
# needs NET_ADMIN, which the --privileged runner container always has.
if [ -n "${BRIDGE_MTU}" ]; then
    _egress_if=$(ip -o route show default 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="dev") {print $(i+1); exit}}' || true)
    [ -n "${_egress_if}" ] || _egress_if=eth0
    if ip link set dev "${_egress_if}" mtu "${BRIDGE_MTU}" 2>/dev/null; then
        echo "[entrypoint] ${_egress_if} MTU pinned to ${BRIDGE_MTU} (host egress MTU)"
    else
        echo "[entrypoint] WARNING: could not set ${_egress_if} MTU to ${BRIDGE_MTU}"
    fi
    # Belt-and-suspenders: clamp forwarded TCP SYNs (inner-container NAT egress) to the
    # path MTU in case a child netns keeps a stale 1500 MTU. mangle table, independent of
    # the filter-table rules AWF installs later. Idempotent.
    iptables -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null \
        || iptables -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null \
        || echo "[entrypoint] WARNING: could not install MSS clamp (mangle FORWARD)"
fi

# ── 2b. Inner dockerd (single start; DNS baked into /etc/docker/daemon.json) ────
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

# ── 3. host.docker.internal via dnsmasq (baked config + runtime upstreams) ──────
# gh-aw agent containers reach the MCP gateway via http://host.docker.internal:<port>.
# dnsmasq (config baked at build time) listens on the pinned bridge gateway and answers
# host.docker.internal there; daemon.json already points inner containers' DNS at the
# same gateway. dockerd is NOT restarted.
#
# Upstreams: the baked config sets `no-resolv`, so dnsmasq forwards non-local queries
# ONLY to the servers written here. We seed them from the runner container's ORIGINAL
# /etc/resolv.conf (captured before the repoint below) so enterprise/VPC DNS keeps
# working, dropping loopback stubs and the gateway itself, and fall back to public
# resolvers when nothing usable remains. This MUST run before the resolv.conf repoint.
UPSTREAM_NS=()
while read -r _ns; do
    case "${_ns}" in
        127.*|::1|"${BRIDGE_GW}"|"") continue ;;
        *) UPSTREAM_NS+=("${_ns}") ;;
    esac
done < <(awk '/^nameserver/ {print $2}' /etc/resolv.conf 2>/dev/null)
if [ "${#UPSTREAM_NS[@]}" -eq 0 ]; then
    UPSTREAM_NS=(8.8.8.8 1.1.1.1)
fi
{
    echo "# Generated by entrypoint.sh at startup — dnsmasq upstream resolvers."
    echo "# Seeded from the runner container's original /etc/resolv.conf (or public DNS"
    echo "# fallback). Kept separate from the baked gh-sr.conf so host.docker.internal"
    echo "# stays authoritative while upstream DNS matches the host environment."
    for _ns in "${UPSTREAM_NS[@]}"; do echo "server=${_ns}"; done
} > /etc/dnsmasq.d/gh-sr-upstream.conf

echo "[entrypoint] starting dnsmasq (upstreams: ${UPSTREAM_NS[*]})..."
if pgrep -x dnsmasq &>/dev/null; then
    kill -HUP "$(pgrep -x dnsmasq)" 2>/dev/null || true
else
    dnsmasq --conf-dir=/etc/dnsmasq.d 2>/dev/null || \
        echo "[entrypoint] WARNING: dnsmasq failed to start; host.docker.internal may not resolve"
fi

# Repoint the runner container's OWN resolver at the bundled dnsmasq. This is the crux
# of the host.docker.internal reliability fix: gh-aw's firewall auto-detects the agent
# sandbox's DNS from this very /etc/resolv.conf, so making dnsmasq (authoritative for
# host.docker.internal → the AWF-exempt bridge gateway) the sole resolver stops the
# sandbox from falling back to the outer host resolver and mapping host.docker.internal
# to a non-exempt IP — the intermittent failure that force-proxied the MCP gateway POST
# into Squid (ERR_INVALID_URL → "MCP server(s) failed to launch").
#
# Wait until dnsmasq actually answers on the gateway before repointing, so the runner's
# own resolution (config.sh registration, image pulls) is never briefly broken. `dig`
# ships with the image (dnsutils) and @<gw> bypasses resolv.conf during the probe.
for _i in $(seq 1 20); do
    if dig +short +time=1 +tries=1 host.docker.internal @"${BRIDGE_GW}" 2>/dev/null | grep -q .; then
        break
    fi
    sleep 0.25
done
# /etc/resolv.conf is a Docker-managed bind mount: rewrite its CONTENTS in place (never
# rename). On the rare read-only mount we fall back to the inherited resolver (degraded
# but functional) rather than aborting the runner.
if printf 'nameserver %s\n' "${BRIDGE_GW}" > /etc/resolv.conf 2>/dev/null; then
    echo "[entrypoint] /etc/resolv.conf now points at dnsmasq (${BRIDGE_GW})"
else
    echo "[entrypoint] WARNING: could not rewrite /etc/resolv.conf; agent sandbox may inherit the host resolver"
fi

# ── 4. AWF service-routing bypass (one-shot) ────────────────────────────────────
# Bypass inner dockerd's PREROUTING DNAT for traffic from awf-net to a local IP so
# AWF agents can reach workflow `services:` published ports (postgres/redis/etc.).
# The job-started hook re-asserts this per job; installing it once here keeps an idle
# runner consistent for `gh sr doctor`.
AWF_SUBNET="${GH_SR_AWF_SUBNET:-172.30.0.0/24}"
echo "[entrypoint] installing AWF service-routing bypass for ${AWF_SUBNET}..."
while iptables -t nat -D PREROUTING -s "${AWF_SUBNET}" -m addrtype --dst-type LOCAL -j RETURN 2>/dev/null; do :; done
iptables -t nat -I PREROUTING -s "${AWF_SUBNET}" -m addrtype --dst-type LOCAL -j RETURN \
    || echo "[entrypoint] WARNING: failed to install AWF service-routing bypass (iptables NAT PREROUTING)"

# ── 5. Register the actions runner ──────────────────────────────────────────────
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
export RUNNER_TOOL_CACHE="/home/runner/.toolcache"

# ── 6. Wire per-job reset hooks into the runner .env ────────────────────────────
# The Actions runner reads .env at startup. We write it deterministically (the file
# lives in the image rootfs and persists across container restarts, so overwrite
# rather than append to stay idempotent).
cat > "${RUNNER_DIR}/.env" <<EOF
RUNNER_TEMP=${RUNNER_TEMP_DIR}
RUNNER_TOOL_CACHE=/home/runner/.toolcache
ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/gh-sr/hooks/job-started.sh
ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/gh-sr/hooks/job-completed.sh
EOF
chown runner:runner "${RUNNER_DIR}/.env"

# su - resets the environment to a login shell's defaults, so RUNNER_TEMP
# and RUNNER_TOOL_CACHE are re-exported on the runner-user side of every
# invocation below.
# Prepend gh-sr docker shim so gh-aw / job steps use /opt/gh-sr/docker-shim/docker; root
# entrypoint above still uses /usr/bin/docker (shim is not on root's default PATH).
RUNNER_ENV="PATH=/opt/gh-sr/docker-shim:\$PATH RUNNER_TEMP='${RUNNER_TEMP_DIR}' RUNNER_TOOL_CACHE='${RUNNER_TOOL_CACHE}'"

# Only configure if not already done. GitHub registration tokens are one-time-use;
# running config.sh on every restart would consume the token and fail on the second start.
# (Same behaviour as native runners — configure once, restart many times.)
if [ ! -f "${RUNNER_DIR}/.runner" ]; then
    echo "[entrypoint] configuring runner..."
    su - runner -c "cd '${RUNNER_DIR}' && ${RUNNER_ENV} ./config.sh ${CONFIG_ARGS[*]@Q}" 2>&1
else
    echo "[entrypoint] runner already configured, skipping config.sh"
fi

# ── 7. Pre-pull gh-aw images (best-effort, background) ──────────────────────────
su - runner -c "
    docker pull ghcr.io/github/gh-aw-firewall/agent:latest &>/dev/null &
    docker pull ghcr.io/github/gh-aw-mcpg:latest &>/dev/null &
" 2>/dev/null || true

# ── 8. Graceful shutdown handler ────────────────────────────────────────────────
_shutdown() {
    echo "[entrypoint] received SIGTERM — stopping runner..."
    # Ask the runner to finish the current job then stop.
    if [ -f "${RUNNER_DIR}/.runner" ]; then
        su - runner -c "cd '${RUNNER_DIR}' && ./config.sh remove --token '${RUNNER_TOKEN}'" 2>/dev/null || true
    fi
    # Shut down the inner dockerd.
    kill "${DOCKERD_PID}" 2>/dev/null && wait "${DOCKERD_PID}" 2>/dev/null || true
    exit 0
}
trap _shutdown SIGTERM SIGINT

# ── 9. Run ──────────────────────────────────────────────────────────────────────
echo "[entrypoint] starting actions runner as user 'runner'..."
exec su - runner -c "cd '${RUNNER_DIR}' && ${RUNNER_ENV} ./run.sh"
