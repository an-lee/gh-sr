---
name: perf-improver-opportunities
description: Performance optimization backlog for baizhiheizi/gh-sr, prioritized by impact and feasibility.
metadata:
  type: project
---

# Optimization backlog (prioritized)

Status as of 2026-09-03. The repo is small (~14k LOC) and already heavily optimized — most hot paths have dedicated benchmarks. The `strings.Split` → `strings.SplitSeq` rollout is now COMPLETE across `internal/agentic`, `internal/autostart`, `internal/runner`, `internal/host`, `internal/cache`, `internal/config`, `internal/doctor`.

## Recently completed

- **[perf-improver] perf(agentic): use SplitSeq for tagged-output parsers on doctor path** — PR #458 (MERGED 2026-09-03T22:55:38Z). -38% to -46% on the four parser sub-benches; happy paths now 0-alloc. See work.md.
- **[repo-assist] refactor: use SplitSeq for line-iteration in non-agentic parsers** — PR #461 (draft). Covers `internal/cache`, `internal/config`, `internal/doctor`. Ships the rest of the package-wide SplitSeq rollout.
- **[perf-improver] perf(runner): use SplitSeq for per-instance probe parsers on Status path** — PR #?? (draft). Covers `internal/runner/container.go` (`ProbeDinDContainerReadiness`) and `internal/runner/linux_instance_probe.go` (`linuxInstanceProbe`). -8.7% bytes / -1 alloc on the linux probe; mock harness swamps the saving on the docker probe. See work.md.

## Backlog cursor

With SplitSeq complete, the next runs should look at:

1. View() alloc reduction in TUI dashboard (HIGH impact, HIGH risk) — `BenchmarkViewMain/one_status` allocates 322 allocs/op, 96% from lipgloss internals.
2. renderRow / renderHighlightedRow cell padding (MEDIUM impact, LOW risk) — speculative.
3. Manager.Status further per-instance optimization (LOW priority).
4. Periodic `strings.Split` grep — keep checking for new offenders as code lands.

## Identified opportunities (priority order)

### 1. View() alloc reduction in TUI dashboard (HIGH impact, HIGH risk)
- `BenchmarkViewMain/one_status`: 38,827 ns/op, 9,958 B/op, 322 allocs/op.
- 96% of allocations come from `lipgloss.Style.Render` and downstream `strings.Split`/`strings.Builder.WriteString` inside lipgloss.
- Options: cache rendered cells keyed on content hash; replace `Width()` chain; emit ANSI directly for static-color cells.
- Risk: visual regression; lipgloss handles unicode/ANSI edge cases we don't want to reimplement.
- Status: ANALYZED. Not started.

### 2. renderRow / renderHighlightedRow cell padding (MEDIUM impact, LOW risk)
- `BenchmarkRenderRow`: 30,633 ns/op, 6,258 B/op, 248 allocs/op.
- 32% of allocs from `strings.Split` inside lipgloss line-wrapping (per-cell).
- Could skip cells already at column width (no padding needed).
- Status: SPECULATIVE.

### 3. Manager.Status further per-instance optimization (LOW impact)
- `BenchmarkManager_Status`: 28,607 ns/op, 120,343 B/op, 74 allocs/op.
- Already heavily optimized (mode/repo/labels hoisted per inline comment).
- Remaining allocs likely from per-instance `RunnerStatus` struct construction + mock SSH output parsing.
- Status: ANALYZED. Marginal gains expected.

### 4. Periodic SplitSeq grep
- `git grep -nE 'strings\.Split\b' -- '*.go'` — keep checking for new offenders as code lands.
- Status: ONGOING. Worth a 30-second sweep every few runs.

## Cross-cutting notes

- The `strings.SplitSeq` migration is now complete across the repo. Future code reviews should flag any new `strings.Split(out, "\n")` callers.
- TUI render performance is dominated by lipgloss internals — measurable optimizations all live behind a major library change.
- Benchmark infrastructure is comprehensive: `make bench` / `make bench-save` + bench-compare.yml CI workflow. New benchmarks fit the same shape (`b.ReportAllocs()` + `b.ResetTimer()` + `for i := 0; i < b.N; i++`).