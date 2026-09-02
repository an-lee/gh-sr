## Why

On 2026-09-02 all four `baizhiheizi-*` containers on host `x1` were stuck with PID 1 of each being `sleep infinity` — the inner dockerd never running yet GitHub still listing the runners online. Root cause: dockerd writes `/var/run/docker.pid` on first successful start and does not clean up after a hard kill. The pid file lives on the container's overlay writable layer and persists across `docker start`. Each subsequent dockerd reads the stale pid (e.g. `16`) and bails with `process with PID 16 is still running`; after 5 failures the entrypoint's documented `exec sleep infinity` fallback holds the container — but `gh sr up` only restarts it, leaving the stale state in place.

Once the dockerd barrier clears, a second failure mode can surface: the actions runner reports `Error: Conflict. A session for this runner already exists.` and stops after 240 s of retry when GitHub still caches the previous session. `recoverContainerStaleRegistration` already covers the related "registration deleted" log line but does not detect SessionConflict.

## What Changes

- `entrypoint.sh` (baked into `gh-sr/agentic-runner`) MUST remove any stale `/var/run/docker.pid` (and the matching `/var/run/docker/` runtime dir) before starting dockerd on every container start
- The single inner-dockerd start logic, host-MTU pinning, and gated-restart behavior stay unchanged
- `recoverContainerStaleRegistration` (or its caller in `Start`) MUST additionally recognize the SessionConflict retry-and-exit pattern via container logs and route the affected container through the existing recovery path (`docker rm -f` + recreate)
- Doctor output continues to surface the bootstrap-failed marker verbatim; no new health probe is introduced
- No change to host-Docker setup requirements (`container-host-docker` spec) or to native-mode runners
- No new configuration fields or CLI flags

## Capabilities

### New Capabilities

- `inner-dockerd-bootstrap`: Requirements for the per-container inner-dockerd startup sequence, including idempotent state cleanup before each start
- `runner-session-recovery`: Requirements for gh-sr to detect when the actions runner inside a container has hit a stale GitHub session and recover by recreating the container with fresh registration

### Modified Capabilities

<!-- None — no existing spec covers inner dockerd startup or session-conflict recovery -->

## Impact

- **Image**: `internal/runner/assets.go` (embedded `agenticRunnerEntrypoint` constant) — add the pid-file cleanup before the dockerd start
- **Code**: `internal/runner/container.go` — extend stale-detection to also match the SessionConflict message in container logs
- **Tests**: Add a test in `internal/runner/container_test.go` covering the SessionConflict log path
- **Risk**: Low. The pid-file cleanup is a one-line addition before an existing `dockerd` invocation; removing a stale file already containing the current pid is a no-op for a healthy start. The SessionConflict detection piggy-backs on existing `recoverContainerStaleRegistration` machinery which is already used in the related "registration deleted" path.
