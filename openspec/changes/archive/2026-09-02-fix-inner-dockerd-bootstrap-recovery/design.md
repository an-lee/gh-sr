## Context

Container-mode runners run a long-lived `actions/runner` process inside a privileged Docker container. Inside the container, the `entrypoint.sh` baked into `gh-sr/agentic-runner:<actions-version>` is responsible for:

1. Enabling cgroup v2 nesting (DinD prerequisite).
2. Selecting the inner-bridge gateway and writing `/etc/docker/daemon.json`.
3. Starting `dockerd` with `--data-root=/runner-state/docker-data` (DinD).
4. Starting `dnsmasq`, repointing `/etc/resolv.conf`, installing the AWF iptables bypass.
5. Registering the actions runner against GitHub (idempotent; one-time token).
6. Wiring per-job reset hooks and exec'ing `run.sh`.

Today, step 3 starts `dockerd` without any pre-cleanup of `/var/run/docker.pid`. When the daemon starts successfully it owns the file and removes it on graceful shutdown, but a host-side incident that hard-kills the daemon (OOM, `docker restart`, host reboot propagated to containers) leaves the file behind. The next start, the new `dockerd` reads `/var/run/docker.pid`, sees an unfamiliar PID whose process is no longer in the namespace's PID table, and depending on timing either sees "still running" (kill -0 hits the new dockerd's own pid if it happens to share the slot) or refuses to start in any case. After 5 consecutive failures the entrypoint's documented fallback path is `exec sleep infinity`, which is what holds the container alive while waiting for the operator to fix the host.

Independent of the daemon state, the actions runner inside an already-recovered container can hit `"A session for this runner already exists"` against GitHub when the previous run's session is still held server-side. The actions runner retries 240 s and then exits with `Runner listener exit with Session Conflict error, stop the service, no retry needed.` No process restarts. The container is still up but no longer polls for jobs, and `gh sr doctor` would only notice if it specifically checked job pickup — which it does not. `recoverContainerStaleRegistration` already handles the related `"registration deleted on GitHub"` log path, but does not currently detect `Session Conflict`.

## Goals / Non-Goals

**Goals**

- Make the inner-dockerd start robust against host-induced hard kills so the documented `exec sleep infinity` fallback fires only for genuine host problems, not stale overlay state
- Detect the SessionConflict retry-loop from container logs and route through the existing recovery path (`docker rm -f` + recreate + start)
- Keep all changes within the existing CLI surface — no new flags, no new config keys
- Cover the changes with unit tests

**Non-Goals**

- Replacing the `5 retries → exec sleep infinity` retry policy itself (that is the documented fallback; we only ensure it isn't triggered by stale state)
- Detecting or recovering runners that are healthy from the host's perspective but stuck on GitHub for unrelated reasons (e.g. runner offline > 14 days → auto-removed)
- Adding a periodic background reaper in `gh sr` for SessionConflict (out of scope; detection runs at `gh sr up`/`gh sr start` time)
- Touching host-Docker setup (`container-host-docker`) or native-mode runners
- Changing image tags, `actions-runner` version pinning, or any embedded asset except the `entrypoint.sh` body

## Decisions

### 1. Pre-clean `/var/run/docker.pid` inside entrypoint.sh

**Decision:** Immediately before the `dockerd &` line, add `rm -f /var/run/docker.pid` (and, defensively, `rm -rf /var/run/docker/*` excluding the socket — actually: only the pid file; the daemon will recreate `/var/run/docker/{containerd,libnetwork,metrics.sock,plugins}` on its own).

**Rationale:** Each `dockerd` invocation owns and re-creates everything under `/var/run/docker/` that it needs. The only state it inherits as an exclusive lock is `docker.pid`. Removing it is safe on every start: if it was left over from a dead predecessor, it's gone; if it doesn't exist, no harm done. `dockerd` itself does this on a clean shutdown via the same trap, so this is restoring the assumption the daemon already makes.

**Alternative considered:** Have `gh sr up` shell into the container to remove the pid file (mirrors the marker-file cleanup in `startContainer`). Rejected — that is a recovery action, not the steady state. The container should bootstrap successfully on its own; recovery should only be needed when something is genuinely broken.

**Alternative considered:** Wrap the `dockerd` invocation in a `pkill -F /var/run/docker.pid || true` first. Rejected — `pkill -F` reads the pid and signals it, but the pid may belong to an unrelated process by now (PID reuse in the new namespace), making the signal spurious. `rm -f` is unambiguous.

### 2. Detect SessionConflict via container logs, not API

**Decision:** Extend `containerLogsContainStaleRegistration` into (or alongside) a more general "runner recoverable from logs" predicate that matches `"SessionConflictException"` / `"Runner listener exit with Session Conflict error"` patterns in `docker logs --tail 200`. When matched during `Start`, the existing `recoverContainerStaleRegistration` path runs and recreates the container.

**Rationale:** The GitHub API does not expose the server-side session-state of a runner in a way that lets us determine whether a "session conflict" is recoverable without trying to register. Container logs already encode the failure mode cleanly and we already screen logs for the related stale-registration message, so this is a one-line pattern-extension rather than a new probe.

**Alternative considered:** Call the `remove_runner` API before re-registering. Rejected — `recoverContainerStaleRegistration` already removes the local `.runner` / `.credentials` files; the issue here is the server-side session, which clears itself within minutes on its own. A `remove_runner` round-trip per `gh sr up` would be heavier than the failure mode merits, and the local recreation already hands the runner a new token which unsticks it.

**Alternative considered:** Add a `time.Sleep` + retry in `Start` to wait out the conflict. Rejected — the GitHub-side retention window is not documented and could be longer than a reasonable wait, and the recreate path is already known to work end-to-end.

### 3. Single-line log-pattern detection, no regex engine

**Decision:** Use `grep -F` (fixed string) for both `"registration deleted on GitHub"` and the new `"Session Conflict"` substring. Do not introduce `regexp` into the predicate.

**Rationale:** The predicate is a log fingerprint, not a parser. Fixed-string matching is robust against any non-adversarial phrasing change GitHub makes, cheap on the remote shell, and consistent with how the existing stale-registration check is implemented (see `container.go:668`).

### 4. No change to the `exec sleep infinity` policy

**Decision:** Leave the 5-retry-then-sleep policy alone. The pid-file cleanup reduces how often it fires, but it stays as the canonical "host is broken, hold for operator" signal.

**Rationale:** That policy is the operator's safety net for genuine problems (host kernel regression, cgroup mismatch, OOM in the namespace). Removing it would cause silent retries and harder-to-diagnose failures. The fix is to make the policy trigger only when warranted.

### 5. Tests cover both new code paths

**Decision:** Add one table-driven test in `container_test.go` matching the existing `containerLogsContainStaleRegistration` style, covering:
- Container logs contain `"Session Conflict"` → predicate returns `true`
- Container logs contain only the existing `"registration deleted on GitHub"` substring → predicate returns `true` (regression coverage)
- Container logs contain neither → predicate returns `false`

Add a second test asserting the pid-file cleanup is the first thing `entrypoint.sh` does in the dockerd-start section (string-match on the embedded `agenticRunnerEntrypoint` constant).

**Rationale:** Both fixes are one-line textual changes; tests give reviewers confidence they still work after future refactors of the entrypoint blob or the recovery predicate.

## Risks

- **`rm -f /var/run/docker.pid` racing a real daemon**: impossible — the file belongs to either the current process group (which has not yet started dockerd) or to a stale predecessor (in which case there is no current daemon). The cleanup happens before `dockerd &`, so no concurrent writer exists.
- **Log-pattern false positives**: `grep -F "Session Conflict"` is unlikely to match unrelated content, but if it ever does the recovery path is benign (it just recreates the container). Net effect: a small extra cost on `gh sr up`, never a worse state.
- **Embedded `agenticRunnerEntrypoint` regeneration**: the build pipeline (`ContainerImageLayoutRevision`) hashes the entrypoint content, so any whitespace change invalidates image caches on hosts. Documented behavior; same as past fixes like the `mtu` pin.
