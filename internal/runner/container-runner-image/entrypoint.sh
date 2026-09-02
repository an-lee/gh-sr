#!/usr/bin/env bash
#
# gh-sr container runner entrypoint.
#
# Starts an inner dockerd (DinD), waits for it to be reachable, and finally
# execs the actions/runner entrypoint so the runner can register with GitHub
# and start polling for jobs.
#
# Idempotent: safe to re-run on every container start. The stale-pid cleanup
# is the Sept-2026 fix for `process with PID … is still running` errors that
# surfaced after a hard kill left the previous dockerd's pid file behind on
# the overlay writable layer.

set -euo pipefail

# 1. Stale-pid cleanup (see Sep 2026 dockerd-bootstrap-recovery change).
#    dockerd writes /var/run/docker.pid on first successful start and does
#    not clean up after a hard kill. The pid file lives on this container's
#    overlay writable layer and persists across `docker start`, so each
#    subsequent dockerd reads the stale pid and bails. Remove it before
#    every start.
rm -f /var/run/docker.pid /var/run/docker/* 2>/dev/null || true

# 2. Start the inner dockerd.
dockerd \
    --host=unix:///var/run/docker.sock \
    --host=tcp://0.0.0.0:2375 \
    --storage-driver=overlay2 \
    >/var/log/dockerd.log 2>&1 &

DOCKERD_PID=$!

# 3. Wait for the daemon to be reachable. Bail if it doesn't come up within
#    GH_SR_DOCKERD_START_TIMEOUT (default 60s) so the outer runner orchestrator
#    can detect a stuck host via the per-instance failure counter.
TIMEOUT="${GH_SR_DOCKERD_START_TIMEOUT:-60}"
DEADLINE=$((SECONDS + TIMEOUT))

while ! docker info >/dev/null 2>&1; do
    if ! kill -0 "${DOCKERD_PID}" 2>/dev/null; then
        echo "inner dockerd exited before becoming ready; tail of /var/log/dockerd.log:" >&2
        tail -n 50 /var/log/dockerd.log >&2 || true
        exit 1
    fi
    if (( SECONDS >= DEADLINE )); then
        echo "inner dockerd did not become ready within ${TIMEOUT}s" >&2
        tail -n 50 /var/log/dockerd.log >&2 || true
        # Persist the per-instance failure counter so the orchestrator can
        # detect the cap-and-hold pattern (5 consecutive failures = bootstrap
        # failed). The host bind-mounts /home/runner/_work into this directory.
        mkdir -p "${RUNNER_STATE_DIR:-/home/runner/_work}"
        FAIL_FILE="${RUNNER_STATE_DIR:-/home/runner/_work}/dockerd-start-failures"
        COUNT=0
        if [ -f "${FAIL_FILE}" ]; then
            COUNT=$(cat "${FAIL_FILE}" 2>/dev/null || echo 0)
        fi
        COUNT=$((COUNT + 1))
        echo "${COUNT}" > "${FAIL_FILE}"
        if (( COUNT >= "${GH_SR_BOOTSTRAP_MAX_RETRIES:-5}" )); then
            ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
            echo "${ts}:${COUNT}" > "${RUNNER_STATE_DIR:-/home/runner/_work}/bootstrap-failed"
            exec sleep infinity
        fi
        exit 1
    fi
    sleep 1
done

# Reset the per-instance failure counter on a clean start.
FAIL_FILE="${RUNNER_STATE_DIR:-/home/runner/_work}/dockerd-start-failures"
rm -f "${FAIL_FILE}" 2>/dev/null || true

# 4. Hand off to the actions/runner entrypoint. The runner binary's own
# entrypoint (run.sh) reads GH_SR_* env vars (set by the outer gh-sr
# orchestrator) and registers with GitHub.
exec /home/runner/run.sh