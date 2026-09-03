---
name: perf-improver-runs
description: Round-robin task history — which tasks ran when, plus backlog cursor for next run.
metadata:
  type: project
---

# Round-robin task history

Use this to spread work across runs: prefer tasks that haven't run for the longest.

## 2026-09-03 (run_id 33764880799)

All 7 tasks completed in a single run (fresh repo, no prior state).

- ✅ Task 1 (Discover commands): validated `go build/test/vet`, `gofmt -l .`, `make bench/bench-save`, `make ci`. Stored in [[perf-improver-commands]].
- ✅ Task 2 (Identify opportunities): profiled View() (322 allocs/op from lipgloss internals), found two SplitSeq offenders in agentic package. Stored in [[perf-improver-opportunities]].
- ✅ Task 3 (Implement improvements): opened PR `[perf-improver] perf(agentic): use SplitSeq for tagged-output parsers on doctor path`. See [[perf-improver-work]].
- ✅ Task 4 (Maintain PRs): no prior perf-improver PRs to maintain.
- ✅ Task 5 (Comment on perf issues): no open performance-labelled issues.
- ✅ Task 6 (Measurement infra): repo already has CI bench-compare workflow + `make bench/bench-save`. No infra work needed.
- ✅ Task 7 (Monthly summary): TBD this run.

## Backlog cursor for next run

Pick up at the top of [[perf-improver-opportunities]]. The two real candidates left are:
1. View() alloc reduction in TUI dashboard (HIGH impact, HIGH risk) — needs more analysis.
2. renderRow / renderHighlightedRow cell padding (MEDIUM impact, LOW risk) — speculative.
