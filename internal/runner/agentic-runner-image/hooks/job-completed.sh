#!/bin/bash
# gh-sr job-completed hook — runs after every job's steps finish.
#
# Wired via ACTIONS_RUNNER_HOOK_JOB_COMPLETED (see entrypoint.sh). The Actions
# runner always runs this hook (even on job failure) as a synchronous "Complete
# runner" step. Its job is to tear down the gh-aw / AWF runtime state the job
# created so the NEXT job on this long-lived runner container starts pristine.
#
# IMPORTANT:
#   * Always exit 0. A non-zero exit from this hook fails an otherwise-successful
#     job (there is no continue-on-error for runner hooks).
#   * Never remove images / volumes. The inner Docker image-layer cache under
#     /runner-state/docker-data is the persistent cache that lets the next job
#     start without re-pulling gh-aw's (large) images. We only remove containers,
#     unused networks, and the /tmp/gh-aw runtime tree.
#   * Scope container removal to gh-aw / AWF names so we do not race the runner's
#     own teardown of workflow `services:` containers. `docker network prune`
#     only touches networks with no attached containers, so it is safe here too.
#   * gh-aw's default `docker` sandbox profile runs AWF rootless — no iptables
#     rules are installed at job runtime (isolation is pure inner-Docker
#     topology), so there is nothing to flush here.
#
# Name vocabulary (gh-aw ≥ v0.88): the MCP gateway is `awmg-mcpg` and the
# optional CLI-proxy sidecar `awmg-cli-proxy` (prefix `awmg-`); the firewall
# stack is `awf-*`; other gh-aw-created containers match `gh-aw`. The cli-proxy
# sidecar reuses the gh-aw-mcpg image, so the ancestor filter below covers it.

set +e

log() { echo "[gh-sr:job-completed] $*"; }

log "tearing down gh-aw/AWF runtime state (image cache preserved)..."

# 1. Remove gh-aw / AWF containers by name.
for filter in 'name=awmg-' 'name=awf-' 'name=gh-aw'; do
    docker ps -aq --filter "$filter" 2>/dev/null | xargs -r docker rm -f >/dev/null 2>&1
done

# 2. Defense-in-depth: also remove gh-aw / AWF containers by ancestor image, in case one
#    was started without a gh-aw-recognisable name (the name filters above would miss it).
#    This is scoped to gh-aw images only, so it never touches workflow `services:` containers.
for img in ghcr.io/github/gh-aw-mcpg \
           ghcr.io/github/gh-aw-firewall/agent \
           ghcr.io/github/gh-aw-firewall/squid \
           ghcr.io/github/gh-aw-firewall/api-proxy; do
    docker ps -aq --filter "ancestor=$img" 2>/dev/null | xargs -r docker rm -f >/dev/null 2>&1
done

# 3. Kill any lingering AWF service-bridge waiter armed by job-started. The
#    waiter exits on its own once it joins, when a network never appears
#    (timeout), or here — so it can never leak into the next job.
pkill -f /opt/gh-sr/hooks/awf-service-bridge.sh >/dev/null 2>&1

# 4. Prune unused networks (awf-net, awmg-* sidecar networks, github_network_* once
#    their containers are gone).
docker network prune -f >/dev/null 2>&1

# 5. Remove the gh-aw runtime tree so the next job's setup starts from a clean slate.
rm -rf /tmp/gh-aw >/dev/null 2>&1

log "teardown complete"
exit 0
