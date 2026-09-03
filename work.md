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

### 2026-09-03 — First perf-improver run

**PR opened**: `[perf-improver] perf(agentic): use SplitSeq for tagged-output parsers on doctor path` (draft, branch `perf-assist/agentic-splitseq`).

**Change**: `internal/agentic/agentic.go` — `parseContainerAgenticFanoutOutput` and `parseDockerChainOutput` switched from `strings.Split` to `strings.SplitSeq`. Both functions were the last two parsers in `internal/agentic` still paying for the upfront `[]string` allocation; every other parser in the package had already been migrated to SplitSeq.

**Benchmark (go1.25.9, AMD Ryzen AI 9 HX 370, -benchtime=500ms -count=3)**:

| Bench | Before | After | Δ |
| --- | --- | --- | --- |
| `ParseContainerAgenticFanoutOutput/happy` | 142 ns/op, 160 B/op, 1 alloc | 87 ns/op, 0 B/op, 0 allocs | -38% time, -1 alloc |
| `ParseContainerAgenticFanoutOutput/all_failed` | 514 ns/op, 1424 B/op, 5 allocs | 478 ns/op, 1264 B/op, 4 allocs | -7% time, -1 alloc |
| `ParseDockerChainOutput/clean` | 77 ns/op, 64 B/op, 1 alloc | 42 ns/op, 0 B/op, 0 allocs | -46% time, -1 alloc |
| `ParseDockerChainOutput/cli_missing` | 112 ns/op, 144 B/op, 2 allocs | 83 ns/op, 80 B/op, 1 alloc | -26% time, -1 alloc |

**New bench file**: `internal/agentic/agentic_parse_bench_test.go` — happy + failure-mode sub-benches per parser, mirrors the project's `b.ReportAllocs()` / `b.ResetTimer()` pattern.

**Verification**: build OK, vet OK, gofmt OK, full `go test ./... -race -count=1` OK (all packages pass).

**Insight**: The two parsers missed the SplitSeq rollout because the agentic fanout parser landed in a separate PR (likely while SplitSeq was still being adopted) and the docker-chain parser predates the rollout entirely. Easy win — searching for `strings.Split\b` (word boundary) versus `strings.SplitSeq` quickly surfaces any remaining offenders.
