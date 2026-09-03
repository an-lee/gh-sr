---
name: perf-improver-commands
description: Validated build, test, lint, format, and benchmark commands for baizhiheizi/gh-sr (Go 1.25.9).
metadata:
  type: project
---

# Validated commands for baizhiheizi/gh-sr

All commands run from repo root `/runner-state/_work/gh-sr/gh-sr` on Linux. Go 1.25.9 installed at `/usr/local/go`.

## Build / test / lint

```bash
go build ./...                                         # build all packages
go test ./... -race -count=1                           # full test suite (CI-equivalent)
go test ./... -count=1 -short                          # quick local check (no SSH/network)
go vet ./...                                            # static analysis
gofmt -l .                                              # format check (CI parity; must be empty)
make ci                                                 # vet + fmt + test (one-shot)
make coverage                                           # per-package coverage breakdown + HTML
```

## Benchmarks

```bash
make bench                                              # 3× repetition across all packages
make bench-save                                         # tee to bench-results/bench-<UTC>.txt
go test -bench=BenchmarkX -benchtime=500ms -benchmem -count=3 ./path/to/pkg/
go tool pprof -alloc_objects -top -cum -nodecount=20 /tmp/mem.prof   # parse a -memprofile output
```

## Workflows (already in repo)

- `.github/workflows/ci.yml` — `vet` + `gofmt -l .` + `go test ./... -race -count=1` on every push/PR.
- `.github/workflows/bench-compare.yml` — runs `go test -bench=. -benchmem -count=3 -benchtime=500ms` on PRs, posts a benchstat comment.
- `.github/workflows/efficiency-improver.md` / `test-improver.md` / `grumpy-reviewer.md` — sibling agentic workflows (different domains).

## Notes / quirks

- The project tests via `gh run` (the actual GitHub CLI), not the local binary — see `CLAUDE.md`.
- `Makefile` defines `bench` and `bench-save` targets (already validated).
- The benchmark for Manager.Status uses a `testutil.MockExecutor` to replace the SSH round-trip — see `internal/runner/bench_test.go` for the pattern when adding new benchmarks that hit `h.Run`.
