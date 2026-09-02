#!/bin/bash
# gh-sr job-started hook — runs after a job is assigned, before its steps run.
#
# Wired via ACTIONS_RUNNER_HOOK_JOB_STARTED (see entrypoint.sh). Runs as a
# synchronous "Set up runner" step. It guarantees a pristine inner environment
# for the job:
#
#   1. Aggressively reset leftover inner state. ACTIONS_RUNNER_HOOK_JOB_STARTED runs
#      "when a job has been assigned to a runner, but before the job starts running"
#      (GitHub docs) — i.e. BEFORE the job lifecycle's prepare_job step that creates the
#      job/service (`services:`) containers and network. So removing ALL inner containers
#      here cannot touch the current job's containers; it only reaps leftovers from a
#      previous job whose completed-hook did not run (e.g. the runner was killed mid-job).
#   2. Reclaim /runner-state/_work and _temp for the runner user. The bind-mount
#      survives container recreation; leftover files owned by uid 1000 (host user
#      or a previous native-mode job) make the next job fail with
#      "Access to the path '/runner-state/_work/...' is denied".
#   3. (Re)assert the AWF service-routing bypass so workflow `services:` reachability
#      survives any flush.
#   4. Verify the inner dockerd is responsive. This is the only hard failure: if
#      dockerd is down the job cannot run anyway, and failing here surfaces a clear
#      message in "Set up runner" instead of a confusing failure minutes later.
#
# Never removes images / volumes — the /runner-state/docker-data image cache is
# preserved so the job does not re-pull service or agent images.

set +e
export PATH="/opt/gh-sr/docker-shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

AWF_SUBNET="${GH_SR_AWF_SUBNET:-172.30.0.0/24}"

log() { echo "[gh-sr:job-started] $*"; }

# 1. Aggressive reset — safe because no job is running on this runner yet.
log "resetting inner environment to a pristine state (image cache preserved)..."
docker ps -aq 2>/dev/null | xargs -r docker rm -f >/dev/null 2>&1
docker network prune -f >/dev/null 2>&1
if command -v iptables >/dev/null 2>&1; then
    sudo -n iptables -F DOCKER-USER >/dev/null 2>&1
fi
sudo -n rm -rf /tmp/gh-aw >/dev/null 2>&1 || rm -rf /tmp/gh-aw >/dev/null 2>&1

# 2. Reclaim bind-mounted work/temp for uid 1001 (runner). Do not touch
#    docker-data (root-owned inner image cache).
sudo -n chown -R runner:runner /runner-state/_work /runner-state/_temp 2>/dev/null \
    || chown -R runner:runner /runner-state/_work /runner-state/_temp 2>/dev/null \
    || log "WARNING: could not chown /runner-state/_work (next job may hit permission denied)"

# 3. (Re)assert the AWF service-routing bypass (idempotent): exempt AWF-subnet
#    traffic destined to a local IP from inner dockerd's DOCKER chain DNAT so AWF
#    agents can reach workflow `services:` published ports. Mirrors entrypoint.sh.
if command -v iptables >/dev/null 2>&1; then
    while sudo -n iptables -t nat -D PREROUTING -s "${AWF_SUBNET}" -m addrtype --dst-type LOCAL -j RETURN >/dev/null 2>&1; do :; done
    sudo -n iptables -t nat -I PREROUTING -s "${AWF_SUBNET}" -m addrtype --dst-type LOCAL -j RETURN >/dev/null 2>&1
fi

# 4. Ensure the inner dockerd is responsive (entrypoint starts it before run.sh;
#    this guards against a daemon that died between jobs).
for i in $(seq 1 30); do
    if docker info >/dev/null 2>&1; then
        log "inner dockerd healthy; environment is clean"
        exit 0
    fi
    sleep 1
done

log "ERROR: inner dockerd is not responding; cannot run job"
exit 1
