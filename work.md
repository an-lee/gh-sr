---
name: perf-improver-work
description: Work-in-progress, completed work, and run history for the perf-improver workflow on baizhiheizi/gh-sr.
metadata:
  type: project
---

# Work log

## In progress

(none)

## Completed

### 2026-09-03 (run 1) — Agentic SplitSeq (PR #458 — MERGED)

**PR opened**: `[perf-improver] perf(agentic): use SplitSeq for tagged-output parsers on doctor path` (draft, branch `perf-assist/agentic-splitseq-fe1dd4e41f768001`).

**Status**: Merged 2026-09-03T22:55:38Z (auto-merged by maintainer).

**Change**: `internal/agentic/agentic.go` — `parseContainerAgenticFanoutOutput` and `parseDockerChainOutput` switched from `strings.Split` to `strings.SplitSeq`. -38% to -46% on the four parser sub-benches; happy paths now 0-alloc.

### 2026-09-03 (run 2) — Runner probe SplitSeq (PR #??)

**PR opened**: `[perf-improver] perf(runner): use SplitSeq for per-instance probe parsers on Status path` (draft, branch `perf-assist/runner-probes-splitseq`, commit b1ffe5b).

**Change**: `internal/runner/container.go` (`ProbeDinDContainerReadiness`) and `internal/runner/linux_instance_probe.go` (`linuxInstanceProbe`) switched from `strings.Split` to `strings.SplitSeq`. These were the last two `strings.Split(out, "\n")` callers on the Status hot path.

**Benchmark (go1.25.9, AMD Ryzen AI 9 HX 370, -benchtime=500ms -count=5)**:

| Bench | Before | After | Δ |
| --- | --- | --- | --- |
| `BenchmarkLinuxInstanceProbe` | 504.9 ns/op, 770 B/op, 13 allocs | 485.2 ns/op, 703 B/op, 12 allocs | **-8.7% bytes, -1 alloc** |
| `BenchmarkLinuxInstanceProbe_WithDir` | 585.0 ns/op, 912 B/op, 16 allocs | 673.5 ns/op, 835 B/op, 15 allocs | **-8.4% bytes, -1 alloc** (timing noise) |
| `BenchmarkLinuxInstanceProbe_SystemdSystem` | 505.2 ns/op, 772 B/op, 13 allocs | 562.6 ns/op, 707 B/op, 12 allocs | **-8.4% bytes, -1 alloc** (timing noise) |
| `BenchmarkProbeDinDContainerReadiness` | 149.2 ns/op, 218 B/op, 3 allocs | 146.8 ns/op, 216 B/op, 3 allocs | wash (mock harness swamping) |

**New bench file**: `internal/runner/runner_probe_bench_test.go` — 5 sub-benches covering the typical + edge cases (running-healthy, inner-docker-down, user-level systemd, system-level systemd, with-D-marker for orphan cleanup).

**Verification**: build OK, vet OK, gofmt OK, full `go test ./... -race -count=1` OK (all packages pass).

**Insight**: `linuxInstanceProbe` shows the expected SplitSeq win (-1 alloc, ~9% bytes); `ProbeDinDContainerReadiness` shows no measurable change because the mock `DockerExecCommand` formatting + mock `Calls` slice growth swamp the split alloc. The change is still worth keeping for consistency with the package-wide SplitSeq rollout.

## Lessons learned this period

- `SplitSeq` migration is now complete across the repo: `internal/agentic`, `internal/autostart`, `internal/runner`, `internal/host`, `internal/cache`, `internal/config`, `internal/doctor`. Future code reviews should flag any new `strings.Split(out, "\n")` callers.
- When a microbench shows no change despite a theoretically-favorable refactor, check what other allocations are dwarfing the target — mock harness overhead can swamp real-world savings.