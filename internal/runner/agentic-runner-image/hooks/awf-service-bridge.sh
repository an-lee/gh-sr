#!/bin/bash
# awf-service-bridge.sh — gh-sr AWF service bridge (opt-in per runner).
#
# Joins the current job's GitHub Actions `services:` containers to the AWF
# topology network (awf-net) so the sandboxed agent can reach them by their
# service names over native TCP.
#
# Why this exists: under gh-aw >= v0.88 network isolation the agent sits on
# the internal `awf-net` bridge (no route to the runner host), while the
# actions runner places `services:` containers on its own
# `github_network_<hash>` bridge — two unrouted bridges on the same inner
# daemon. `docker network connect` adds a second interface to each service
# container on `awf-net` (the same mechanism AWF itself uses for trusted
# topology peers like the MCP gateway; the network name is pinned by awf's
# sandbox-network-policy.json exactly for this), and Docker's embedded DNS —
# the agent's only resolver — serves the service alias. This is the pattern
# proposed upstream in gh-aw#57988 ("direct agent access to services:
# containers via the topology network").
#
# Spawned detached by job-started.sh when the runner is configured with
# `awf_service_bridge: true` in runners.yml. This hook runs BEFORE the job's
# prepare_job creates the service containers/network and long before the
# agent step creates awf-net, so everything below is polling:
#
#   1. wait for this job's github_network_<hash> (a job without `services:`
#      never creates one — exit),
#   2. wait for awf-net (a job without an AWF sandbox never creates one —
#      exit),
#   3. docker network connect each service container with the alias the
#      runner already gave it (the `services:` key).
#
# Timeouts are belt-and-braces: job-completed.sh kills any lingering waiter,
# so a waiter never leaks into the next job on this runner.
#
# Security: services only GAIN an interface on the agent's internal network
# (no egress from it — internal networks have no route out); they keep their
# original runner-network interface, which is the same trust already extended
# by declaring them under `services:`. The join direction is load-bearing:
# never attach the agent to the runner's network — that would bypass the
# egress firewall.
#
# Runs as the runner user (docker group) against the inner dockerd socket.
# Logs to /tmp/gh-sr-awf-service-bridge.log inside the runner container.

set -u

LOG_FILE="${GH_SR_AWF_BRIDGE_LOG:-/tmp/gh-sr-awf-service-bridge.log}"

log() { echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] [gh-sr:awf-bridge] $*" >> "$LOG_FILE"; }

# 1. This job's service network. The actions runner creates
#    `github_network_<hash>` during prepare_job — seconds after job-started.
#    job-started.sh prunes leftover networks, so exactly one belongs to this
#    job; `tail -1` picks the newest as defence in depth.
SERVICE_NET=""
for _ in $(seq 1 150); do
    SERVICE_NET=$(docker network ls --format '{{.Name}}' 2>/dev/null | grep '^github_network_' | tail -1)
    [ -n "$SERVICE_NET" ] && break
    sleep 2
done
if [ -z "$SERVICE_NET" ]; then
    log "no github_network_* appeared after 5m (job has no services:) — nothing to bridge"
    exit 0
fi

# 2. Service containers may still be creating/starting on it.
SERVICE_CONTAINERS=""
for _ in $(seq 1 30); do
    SERVICE_CONTAINERS=$(docker network inspect -f '{{range .Containers}}{{.Name}} {{end}}' "$SERVICE_NET" 2>/dev/null)
    [ -n "$SERVICE_CONTAINERS" ] && break
    sleep 2
done
if [ -z "$SERVICE_CONTAINERS" ]; then
    log "network $SERVICE_NET has no containers after 1m — nothing to bridge"
    exit 0
fi

# 3. The AWF topology network. awf-net is created inside the agent step
#    (after pre-agent steps) and removed at sandbox teardown, so it is the
#    signal that this job actually runs a sandbox worth bridging into.
for _ in $(seq 1 600); do
    docker network inspect awf-net >/dev/null 2>&1 && break
    sleep 2
done
if ! docker network inspect awf-net >/dev/null 2>&1; then
    log "awf-net never appeared after 20m (no AWF sandbox in this job) — nothing to bridge"
    exit 0
fi

# 4. Join each service container, reusing the alias the runner already set
#    on the service network (the `services:` key — `--network-alias`), so the
#    agent resolves the same name a workflow would use. `docker network
#    connect` fails on an already-attached container; treat that as done.
for name in $SERVICE_CONTAINERS; do
    [ -n "$name" ] || continue
    alias_name=$(docker inspect "$name" --format '{{json .NetworkSettings.Networks}}' 2>/dev/null \
        | jq -r --arg net "$SERVICE_NET" '.[$net].Aliases[0] // empty' 2>/dev/null)
    [ -n "$alias_name" ] || alias_name="$name"
    if docker network connect --alias "$alias_name" awf-net "$name" >/dev/null 2>&1; then
        log "joined $name to awf-net as '$alias_name'"
    else
        log "skip $name (already attached to awf-net, or connect failed)"
    fi
done

log "bridge complete for $SERVICE_NET"
