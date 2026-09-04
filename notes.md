---
name: notes
description: Test Improver repo notes for baizhiheizi/gh-sr
metadata:
  type: project
---

## Build / test commands (validated 2026-09-04)

```bash
go build ./...                          # full-repo build
go vet ./...                            # CI static analysis
gofmt -l .                              # CI format check
go test ./... -race -count=1            # CI parity
go test ./... -cover -count=1           # per-package coverage summary
go test ./... -count=1 -short           # local quick check (skips SSH/network)
make ci                                 # vet + fmt + test
make coverage                           # coverage summary sorted asc + total
```

## Coverage snapshot (2026-09-04 baseline)

```
cmd/gh-sr           0.0%   (Cobra wiring only, no _test.go per memory)
internal/agentic   95.3%
internal/autostart 94.6%
internal/cache     77.0%
internal/config    85.3%
internal/diskschedule 88.4%
internal/doctor    75.9%   -> 81.4% after this run (+5.5 pp)
internal/editor    92.3%
internal/host      66.8%   (SSH/auth heavy, out of scope)
internal/hostshell 93.0%
internal/hostshell/ps 100.0%
internal/ops       80.2%
internal/runner    81.9%
internal/strfmt    100.0%
internal/table     86.0%
internal/testutil  88.2%
internal/tui       34.8%   (low priority per memory)
scripts/benchstat  91.4%
```

## Top low-covered functions (sorted asc, 2026-09-04)

internal/cache/cache.go:
- Inspect (0%) - best-effort health probe, needs host mock
- EffectivePort (0%) - trivial wrapper
- image (66.7%)

internal/cache/config.go:
- SettingsFromConfig (0%) - trivial

internal/doctor/doctor.go:
- checkCacheReachability (0%)
- probeCacheFromRunner (0%)
- effectiveBind (0%)
- checkContainerHostPrereqs (60%)

internal/ops/cache.go: 6 functions at 0%
internal/ops/ops.go: ConnectHost (0%)

internal/runner/orphans.go: PlanOrphanCleanup (38.1%), CleanupOrphanInstance (50%), instanceDirectoryExists (0%)

internal/runner/native.go Windows branches: startNative (0%), statusNativeOneshotNonLinux (44.4%), statusNativeFromProbe (52.9%), nativeRunnerVersion (35.7%), removeNativeServices (60%), logsNative (66.7%)

## Reusable test patterns

- `httptest.NewServer` + `runner.NewGitHubClientWithHTTP(token, srv.Client(), srv.URL)` for GitHub API mocks.
- `internal/testutil.MockExecutor` + `host.SetConn(mock)` for SSH/host mocks.
- Pure-function tests use `t.Parallel()` heavily with sub-tests.
- OS command seams must reset with `t.Cleanup`.
- For `gh sr doctor` orchestrators (`checkLockWorkflows` etc), a small `httptest` server mocking both the directory listing and base64-encoded per-file fetch endpoints is enough — see `lockRepoGitHubStub` in `internal/doctor/lockfiles_orchestrator_test.go` for the reusable shape.
- `printLine` format is `%-5s [%-12s] %s\n` — severity is bracketed with the scope, so substring assertions must include `[scope` not just `scope`.

## Completed work

- 2026-09-04: Cover `checkLockWorkflows` orchestrator (0% → 100%); internal/doctor 75.9% → 81.4% (+5.5 pp). PR draft created on branch `test-assist/lockfiles-orchestrator` (commit 1197043). 7 sub-tests in `internal/doctor/lockfiles_orchestrator_test.go`.
