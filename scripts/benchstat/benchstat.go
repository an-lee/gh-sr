// Package benchstat parses two `go test -bench=.` outputs and produces a
// markdown regression report.
//
// Used by .github/workflows/bench-compare.yml to surface `ns/op`, `B/op` and
// `allocs/op` regressions on pull requests. Stdlib only, no module deps.
package benchstat

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Result is a single benchmark measurement. When the producing `go test`
// run used -count>1, the per-metric values are medians across the
// repetitions (see ParseFile).
type Result struct {
	Name        string
	Package     string // import path from the "pkg:" header, "" if absent
	N           int64
	NsPerOp     float64
	BPerOp      float64
	AllocsPerOp float64
}

// Thresholds defines warn/fail percent-delta cutoffs per metric.
type Thresholds struct {
	NsWarn, NsFail      float64
	BWarn, BFail        float64
	AllocsWarn, AllocsF float64
}

// DefaultThresholds returns the warn/fail cutoffs used by the bench-compare
// workflow. The numbers are intentionally conservative: real refactors should
// stay well below them, while genuine regressions (e.g. +50% ns/op) fail.
func DefaultThresholds() Thresholds {
	return Thresholds{
		NsWarn: 10, NsFail: 30,
		BWarn: 15, BFail: 50,
		AllocsWarn: 10, AllocsF: 25,
	}
}

var (
	benchRe = regexp.MustCompile(`^Benchmark(\w+)(?:-\d+)?\s+(\d+)\s+(.*)$`)
	pkgRe   = regexp.MustCompile(`^pkg:\s+(\S+)`)
)

// ParseFile reads `go test -bench` output and returns one Result per
// benchmark name. Names without `-N` suffix or with various metric columns
// (ns/op only, ns/op + B/op, ns/op + B/op + allocs/op) are all accepted.
//
// With -count>1 the same benchmark name appears once per repetition. The
// returned Result aggregates the repetitions by taking the per-metric
// MEDIAN — the single-sample last-write-wins behaviour this function had
// before made CI comparisons on the shared self-hosted runner flap by
// ±30-66% on sub-microsecond benchmarks (runner contention from concurrent
// test/bench jobs). The median discards those one-off outliers.
func ParseFile(path string) (map[string]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	samples := make(map[string][]Result)
	pkg := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if m := pkgRe.FindStringSubmatch(line); m != nil {
			pkg = m[1]
			continue
		}
		m := benchRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		r := Result{Name: name, Package: pkg, N: n}
		fields := strings.Fields(m[3])
		for i := 0; i+1 < len(fields); i += 2 {
			val, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				continue
			}
			switch fields[i+1] {
			case "ns/op":
				r.NsPerOp = val
			case "B/op":
				r.BPerOp = val
			case "allocs/op":
				r.AllocsPerOp = val
			}
		}
		samples[name] = append(samples[name], r)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	results := make(map[string]Result, len(samples))
	for name, rs := range samples {
		agg := rs[0]
		agg.NsPerOp = median(metricColumn(rs, func(r Result) float64 { return r.NsPerOp }))
		agg.BPerOp = median(metricColumn(rs, func(r Result) float64 { return r.BPerOp }))
		agg.AllocsPerOp = median(metricColumn(rs, func(r Result) float64 { return r.AllocsPerOp }))
		results[name] = agg
	}
	return results, nil
}

// metricColumn extracts one metric from every repetition, skipping zero
// values (metrics the run did not print). Never returns an empty slice when
// called on a non-empty sample set, since reps[0] always exists for ns/op.
func metricColumn(reps []Result, get func(Result) float64) []float64 {
	vals := make([]float64, 0, len(reps))
	for _, r := range reps {
		if v := get(r); v > 0 {
			vals = append(vals, v)
		}
	}
	return vals
}

// median returns the median of vals; for an even count it averages the two
// middle values. Zero-length input returns 0 (no samples printed the metric).
func median(vals []float64) float64 {
	switch len(vals) {
	case 0:
		return 0
	case 1:
		return vals[0]
	}
	sort.Float64s(vals)
	mid := len(vals) / 2
	if len(vals)%2 == 1 {
		return vals[mid]
	}
	return (vals[mid-1] + vals[mid]) / 2
}

// Row is one benchmark's status in the comparison.
type Row struct {
	Name    string
	Package string
	Status  string // "ok", "warn", "fail", "new", "removed", "ungated"
	Base    Result
	Head    Result
	HasNs   bool
	HasB    bool
	HasAll  bool
	NsD     float64
	BD      float64
	AllD    float64
	NsF     bool
	NsW     bool
	BF      bool
	BW      bool
	AllF    bool
	AllW    bool
}

// Compare returns rows comparing head to base. Rows are sorted by severity
// (fail, warn, new, removed, ok) and then alphabetically.
func Compare(base, head map[string]Result, t Thresholds) []Row {
	var rows []Row
	seen := make(map[string]bool)

	for name, h := range head {
		b, ok := base[name]
		if !ok {
			rows = append(rows, Row{Name: name, Package: h.Package, Status: "new", Head: h})
			seen[name] = true
			continue
		}
		r := Row{Name: name, Package: h.Package, Base: b, Head: h}
		if b.NsPerOp > 0 {
			r.HasNs = true
			r.NsD = (h.NsPerOp - b.NsPerOp) / b.NsPerOp * 100
			switch {
			case r.NsD >= t.NsFail:
				r.NsF = true
				r.Status = "fail"
			case r.NsD >= t.NsWarn:
				r.NsW = true
				if r.Status != "fail" {
					r.Status = "warn"
				}
			}
		}
		if b.BPerOp > 0 {
			r.HasB = true
			r.BD = (h.BPerOp - b.BPerOp) / b.BPerOp * 100
			switch {
			case r.BD >= t.BFail:
				r.BF = true
				r.Status = "fail"
			case r.BD >= t.BWarn:
				r.BW = true
				if r.Status != "fail" {
					r.Status = "warn"
				}
			}
		}
		if b.AllocsPerOp > 0 {
			r.HasAll = true
			r.AllD = (h.AllocsPerOp - b.AllocsPerOp) / b.AllocsPerOp * 100
			switch {
			case r.AllD >= t.AllocsF:
				r.AllF = true
				r.Status = "fail"
			case r.AllD >= t.AllocsWarn:
				r.AllW = true
				if r.Status != "fail" {
					r.Status = "warn"
				}
			}
		}
		if r.Status == "" {
			r.Status = "ok"
		}
		rows = append(rows, r)
		seen[name] = true
	}

	for name, b := range base {
		if !seen[name] {
			rows = append(rows, Row{Name: name, Package: b.Package, Status: "removed", Base: b})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		score := func(s string) int {
			switch s {
			case "fail":
				return 4
			case "warn":
				return 3
			case "new":
				return 2
			case "removed":
				return 1
			default:
				return 0
			}
		}
		if si, sj := score(rows[i].Status), score(rows[j].Status); si != sj {
			return si > sj
		}
		return rows[i].Name < rows[j].Name
	})

	return rows
}

// HasFail reports whether any row is at fail severity.
func HasFail(rows []Row) bool {
	for _, r := range rows {
		if r.Status == "fail" {
			return true
		}
	}
	return false
}

// ApplyPackageScope demotes rows whose benchmark lives outside pkgs to
// "ungated" status: they stay in the report with their deltas visible, but
// they no longer count toward HasFail/HasWarn. A PR that only touches
// package A should not fail because an unrelated micro-benchmark in package
// B flapped — on the shared self-hosted runner, unchanged code moved
// ±30-66% between consecutive runs of the same PR.
//
// pkgs holds directory-style import paths as produced by
// `git diff --name-only | xargs dirname` (e.g. "internal/runner"), matched
// against full import paths with a path-boundary rule so "internal/runner"
// also gates subpackages but never "internal/runnerx". An empty pkgs gates
// everything (pre-scope behaviour). Returns (gated, ungated) row counts and
// reorders rows so gated rows stay first, preserving Compare's ordering
// within each group.
func ApplyPackageScope(rows []Row, pkgs []string) (gated, ungated int) {
	if len(pkgs) == 0 {
		return len(rows), 0
	}
	gatedRows := make([]Row, 0, len(rows))
	var ungatedRows []Row
	for _, r := range rows {
		if packageMatches(r.Package, pkgs) {
			gatedRows = append(gatedRows, r)
		} else {
			r.Status = "ungated"
			ungatedRows = append(ungatedRows, r)
		}
	}
	out := append(gatedRows, ungatedRows...)
	copy(rows, out)
	return len(gatedRows), len(ungatedRows)
}

// packageMatches reports whether the import path pkg is covered by any of
// the directory-style filters. "internal/runner" matches the package itself,
// its subpackages, and is never confused with "internal/runnerx".
func packageMatches(pkg string, pkgs []string) bool {
	for _, f := range pkgs {
		if pkg == f || strings.HasSuffix(pkg, "/"+f) || strings.Contains(pkg, f+"/") {
			return true
		}
	}
	return false
}

// writeNumber appends a benchmark measurement to the markdown builder,
// dropping decimals on larger numbers so the table stays narrow. Writes the
// digits directly to the builder (no intermediate string alloc) using a
// stack-allocated 32-byte buffer — comfortably above the worst-case
// "%.2f" ceiling for nanosecond/microsecond-range measurements.
func writeNumber(sb *strings.Builder, f float64) {
	var b [32]byte
	if f >= 100 {
		sb.Write(strconv.AppendFloat(b[:0], f, 'f', 0, 64))
		return
	}
	sb.Write(strconv.AppendFloat(b[:0], f, 'f', 2, 64))
}

// formatDeltaTo appends the percent delta of d (with sign) into dst and
// returns the resulting slice. Single-allocation helper that mirrors the
// pre-1.21 strconv.AppendFloat pattern; 24 bytes is comfortably above the
// worst-case formatted delta length.
func formatDeltaTo(dst []byte, d float64) []byte {
	if d == 0 {
		return append(dst, "0%"...)
	}
	if d > 0 {
		dst = append(dst, '+')
	}
	dst = strconv.AppendFloat(dst, d, 'f', 1, 64)
	return append(dst, '%')
}

// RenderMarkdown returns a human-readable regression report.
func RenderMarkdown(rows []Row, baseRef, headRef string) string {
	// Pre-size: header + per-row line. A typical row is ~80 bytes; header is
	// fixed; summary line is ~80 bytes. Pre-growing avoids repeated
	// strings.Builder growth reallocations under load.
	var sb strings.Builder
	sb.Grow(128 + 96*len(rows))
	sb.WriteString("## Benchstat: `")
	sb.WriteString(headRef)
	sb.WriteString("` → `")
	sb.WriteString(baseRef)
	sb.WriteString("`\n\n")

	hasFail, hasWarn, hasUngated := false, false, false
	for _, r := range rows {
		switch r.Status {
		case "fail":
			hasFail = true
		case "warn":
			hasWarn = true
		case "ungated":
			hasUngated = true
		}
	}

	switch {
	case hasFail:
		sb.WriteString("🔥 **Fail-level regression(s) detected.** Job exits non-zero.\n\n")
	case hasWarn:
		sb.WriteString("⚠️ Warn-level regression(s) detected.\n\n")
	case hasUngated:
		sb.WriteString("✅ No regressions in the packages this PR changes.\n\n")
	default:
		sb.WriteString("✅ No regressions detected.\n\n")
	}

	sb.WriteString("| Benchmark | ns/op (Δ) | B/op (Δ) | allocs/op (Δ) | Status |\n")
	sb.WriteString("|-----------|-----------|----------|---------------|--------|\n")

	for _, r := range rows {
		sb.WriteString("| ")
		sb.WriteString(r.Name)
		sb.WriteString(" | ")
		writeMetricCell(&sb, r.Status, r.HasNs, r.Head.NsPerOp, r.Base.NsPerOp, r.NsD, r.NsF, r.NsW)
		sb.WriteString(" | ")
		writeMetricCell(&sb, r.Status, r.HasB, r.Head.BPerOp, r.Base.BPerOp, r.BD, r.BF, r.BW)
		sb.WriteString(" | ")
		writeMetricCell(&sb, r.Status, r.HasAll, r.Head.AllocsPerOp, r.Base.AllocsPerOp, r.AllD, r.AllF, r.AllW)
		sb.WriteString(" | ")
		sb.WriteString(rowStatus(r.Status))
		sb.WriteString(" |\n")
	}

	sb.WriteString("\n_Thresholds: ns/op ±10%/30%, B/op ±15%/50%, allocs/op ±10%/25% (warn/fail). Each value is the median of the -count repetitions._\n")
	if hasUngated {
		sb.WriteString("\n_⚪ rows live in packages this PR does not change — shown for visibility, but they do not fail the job (unchanged code moved ±30-66% between runs on the shared runner)._\n")
	}
	return sb.String()
}

func rowStatus(s string) string {
	switch s {
	case "fail":
		return "🔥"
	case "warn":
		return "⚠️"
	case "new":
		return "🆕"
	case "removed":
		return "🗑️"
	case "ungated":
		return "⚪"
	default:
		return "✅"
	}
}

// writeMetricCell appends the rendered metric cell directly to the markdown
// builder. The shape is "<base> → <head> (<delta>)[ mark]" where:
//   - base and head go through writeNumber's zero/2-decimal rule
//   - delta goes through formatDeltaTo (signed "+X.X%"/"-X.X%"/"0%")
//   - mark is " 🔥" / " ⚠️" for fail/warn, else ""
//
// new/removed short-circuit and the !hasMetric "—" path mirror the public
// metricCell contract tested in benchstat_test.go.
func writeMetricCell(sb *strings.Builder, status string, hasMetric bool, headVal, baseVal, delta float64, fail, warn bool) {
	switch status {
	case "new":
		sb.WriteString("— → ")
		writeNumber(sb, headVal)
		return
	case "removed":
		writeNumber(sb, baseVal)
		sb.WriteString(" → —")
		return
	}
	if !hasMetric {
		sb.WriteString("—")
		return
	}
	writeNumber(sb, baseVal)
	sb.WriteString(" → ")
	writeNumber(sb, headVal)
	sb.WriteString(" (")
	var b [24]byte
	sb.Write(formatDeltaTo(b[:0], delta))
	sb.WriteByte(')')
	if fail {
		sb.WriteString(" 🔥")
	} else if warn {
		sb.WriteString(" ⚠️")
	}
}
