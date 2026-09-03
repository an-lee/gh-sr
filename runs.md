---
name: perf-improver-runs
description: Round-robin task history — which tasks ran when, plus backlog cursor for next run.
metadata:
  type: project
---

# Round-robin task history

Use this to spread work across runs: prefer tasks that haven't run for the longest.

## 2026-09-03 (run_id 33764880799) — First perf-improver run

All 7 tasks completed in a single run (fresh repo, no prior state).

- ✅ Task 1 (Discover commands): validated `go build/test/vet`, `gofmt -l .`, `make bench/bench-save`, `make ci`. Stored in [[perf-improver-commands]].
- ✅ Task 2 (Identify opportunities): profiled View() (322 allocs/op from lipgloss internals), found two SplitSeq offenders in agentic package. Stored in [[perf-improver-opportunities]].
- ✅ Task 3 (Implement improvements): opened PR #458 (`[perf-improver] perf(agentic): use SplitSeq for tagged-output parsers on doctor path`). See [[perf-improver-work]].
- ✅ Task 4 (Maintain PRs): no prior perf-improver PRs to maintain.
- ✅ Task 5 (Comment on perf issues): no open performance-labelled issues.
- ✅ Task 6 (Measurement infra): repo already has CI bench-compare workflow + `make bench/bench-save`. No infra work needed.
- ✅ Task 7 (Monthly summary): created issue #459 `[perf-improver] Monthly Activity 2026-09`.

## 2026-09-03 (run_id 33809625237) — Second perf-improver run

- ✅ Task 1 (Discover commands): re-validated `go build`, `gofmt -l .`, `go vet` all OK. Commands in [[perf-improver-commands]] still current.
- ✅ Task 2 (Identify opportunities): picked up at top of [[perf-improver-opportunities]]. After agentic SplitSeq merged and repo-assist covered non-agentic SplitSeq, only two `strings.Split(out, "\n")` callers remained on the Status hot path: `ProbeDinDContainerReadiness` (container.go) and `linuxInstanceProbe` (linux_instance_probe.go). Backlog updated.
- ✅ Task 3 (Implement improvements): opened draft PR `[perf-improver] perf(runner): use SplitSeq for per-instance probe parsers on Status path` on branch `perf-assist/runner-probes-splitseq`. `LinuxInstanceProbe` shows -8.7% bytes, -1 alloc; `ProbeDinDContainerReadiness` wash (mock harness swamping the saving). See [[perf-improver-work]].
- ✅ Task 4 (Maintain PRs): prior PR #458 was MERGED at 2026-09-03T22:55:38Z (between this run and the prior one). No other perf-improver PRs to maintain.
- ✅ Task 5 (Comment on perf issues): no open performance-labelled issues besides the Monthly Activity issue.
- ✅ Task 6 (Measurement infra): added `internal/runner/runner_probe_bench_test.go` (5 sub-benches covering healthy/inner-down/user-systemd/system-systemd/with-dir cases). Side-effect of Task 3.
- ✅ Task 7 (Monthly summary): updating issue #459 this run — removes merged PR #458 action item, adds new SplitSeq probe PR action item, prepends run history entry.

## Backlog cursor for next run

Pick up at the top of [[perf-improver-opportunities]]:

1. **View() alloc reduction in TUI dashboard** — HIGH impact, HIGH risk. Needs more analysis to find a safe cache key.
2. **renderRow / renderHighlightedRow cell padding** — MEDIUM impact, LOW risk. Speculative.
3. **Periodic SplitSeq grep** — quick sweep each run to catch new offenders.
4. **Manager.Status further per-instance optimization** — LOW priority.