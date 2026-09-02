## 1. Inner-dockerd pid-file cleanup

- [x] 1.1 Read `internal/runner/assets.go` and locate the embedded `agenticRunnerEntrypoint` constant. Confirmed the existing one-line block quote for the `single dockerd start` section starts with `echo "[entrypoint] starting dockerd..."`
- [x] 1.2 Insert `rm -f /var/run/docker.pid` as the line immediately preceding `dockerd \`, plus a comment explaining why
- [x] 1.3 Verify the embedded entrypoint's byte size changes and `ContainerImageLayoutRevision` shifts. Source `agentic-runner-image/entrypoint.sh` grew by 359 bytes; new test `TestAgenticRunnerEntrypointUnlinksStaleDockerdPid` exercises both bytes and ordering, so a regression in either attribute will fail CI
- [x] 1.4 String-match unit test added (`TestAgenticRunnerEntrypointUnlinksStaleDockerdPid`) asserting the cleanup line is the last line before `dockerd \` in the embedded entrypoint constant

## 2. SessionConflict log detection

- [x] 2.1 Renamed `containerLogsContainStaleRegistration` to `containerLogsContainRecoverableRegistrationError` in `internal/runner/container.go`
- [x] 2.2 Extended the `docker logs --tail 200 … | grep -F …` to match both `staleRegistrationMsg` and the new `sessionConflictMsg` via `grep -F -e A -e B >/dev/null` (single SSH round-trip)
- [x] 2.3 Sibling constant `sessionConflictMsg = "Session Conflict"` added next to the existing marker-file constants in `internal/runner/container.go`. Uses the existing `hostshell.PosixSingleQuote` call when interpolated
- [x] 2.4 Updated the caller in `internal/runner/runner.go` (`Start`) to use the renamed helper. Existing `TestManagerStartContainerRecoversStaleRegistration` still passes — it matches the substring in the cmd shape, which now also covers SessionConflict

## 3. Tests

- [x] 3.1 Table-driven test added in `internal/runner/container_test.go` (`TestManagerStartContainerRecoversSessionConflict`) covering:
  - The recovery branch triggers when ONLY `sessionConflictMsg` is in the logs
  - Exactly one docker logs probe per instance
  - The cmd shape covers both `staleRegistrationMsg` and `sessionConflictMsg`
- [x] 3.2 String assertion test added (`TestAgenticRunnerEntrypointUnlinksStaleDockerdPid`) verifying the embedded entrypoint contains the pid-file cleanup line and that it precedes the `dockerd \` line

## 4. Verification

- [x] 4.1 `go build ./...` — clean
- [x] 4.2 `go test ./...` — all 16 testable packages pass
- [x] 4.3 `go test -race ./...` — all 16 testable packages pass with race detector enabled
- [x] 4.4 `go vet ./...` — clean
- [ ] 4.5 Manual: rebuild one container-mode runner against the new image (`gh sr rebuild <runner>`), confirm entrypoint logs show `[entrypoint] starting dockerd...` followed by `[entrypoint] dockerd is up` on the same line, without any `process with PID … is still running` failures
- [ ] 4.6 Manual: stop a healthy container's dockerd via `docker exec <container> kill -9 <dockerd-pid>` and `docker restart`, confirm the next start reaches `dockerd is up` without a 90-second `ERROR: dockerd did not start` timeout
- [ ] 4.7 Manual: induce a SessionConflict by registering the runner twice (e.g. run `gh sr setup <runner>` twice in quick succession), confirm the second `gh sr up` detects the conflict via logs and routes through `recoverContainerStaleRegistration`, ending with the runner registered and polling
