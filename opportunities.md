---
name: perf-improver-opportunities
description: Performance optimization backlog for baizhiheizi/gh-sr, prioritized by impact and feasibility.
metadata:
  type: project
---

# Optimization backlog (prioritized)

Status as of 2026-09-03. The repo is small (~14k LOC) and already heavily optimized — most hot paths have dedicated benchmarks.

## Recently completed

- **[perf-improver] perf(agentic): use SplitSeq for tagged-output parsers on doctor path** — switched `parseContainerAgenticFanoutOutput` and `parseDockerChainOutput` from `strings.Split` to `strings.SplitSeq`. Happy paths now 0-alloc; `gh sr doctor` probes benefit on every invocation. See `work.md` for measurements.

## Backlog cursor

The next runs should resume at the top of this list. Items lower down are speculative or low-impact.

## Identified opportunities (priority order)

### 1. View() alloc reduction in TUI dashboard (HIGH impact, HIGH risk)
- `BenchmarkViewMain/one_status`: 38,827 ns/op, 9,958 B/op, 322 allocs/op.
- Profile (`-alloc_objects`) shows 96% of allocations come from `lipgloss.Style.Render` and downstream `strings.Split`/`strings.Builder.WriteString` inside lipgloss internals.
- Optimization options: cache rendered cells when content unchanged, replace `Width()` chain calls, replace lipgloss with direct ANSI emission for static-color cells. Most of the allocs are inside lipgloss, not our code — would require either swapping lipgloss or carefully caching full rendered lines.
- Risk: visual regression; lipgloss handles unicode/ANSI edge cases we don't want to reimplement.
- Status: ANALYZED. Not started.

### 2. renderRow / renderHighlightedRow cell padding (MEDIUM impact, LOW risk)
- `BenchmarkRenderRow`: 30,633 ns/op, 6,258 B/op, 248 allocs/op.
- `BenchmarkRenderHighlightedRow`: 32,147 ns/op, 6,815 B/op, 284 allocs/op.
- 32% of allocs from `strings.Split` inside lipgloss line-wrapping (called per cell). Not directly fixable without bypassing lipgloss.
- Could skip cells already at column width (no padding needed). Currently every cell pays full pad even if `len(cell) == widths[j]`.
- Status: SPECULATIVE. Untested.

### 3. Manager.Status further per-instance optimization (LOW impact)
- `BenchmarkManager_Status`: 28,607 ns/op, 120,343 B/op, 74 allocs/op.
- The `mode`/`repo`/`labels` hoisting comment shows this was already heavily optimized. Remaining allocs likely from per-instance `RunnerStatus` struct construction + mock SSH output parsing.
- Status: ANALYZED. Marginal gains expected.

### 4. parseContainerAgenticFanoutOutput / parseDockerChainOutput (DONE — see work.md)
- Switched `strings.Split` → `strings.SplitSeq`. -38% to -46% on happy paths, now 0-alloc.

## Cross-cutting notes

- The `strings.SplitSeq` migration is the standard low-effort optimization in this repo — see `internal/autostart/cleanup.go`, `internal/runner/disk.go` (splitNonEmptyLines), `internal/host/metrics.go` (parseUnixMetrics) for the established comment pattern.
- TUI render performance is dominated by lipgloss internals — measurable optimizations all live behind a major library change.
- All benchmark infrastructure is in place: `make bench` / `make bench-save` + bench-compare.yml CI workflow. New benchmarks fit the same shape (`b.ReportAllocs()` + `b.ResetTimer()` + `for i := 0; i < b.N; i++`).
