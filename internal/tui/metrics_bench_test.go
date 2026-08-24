package tui

import (
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/host"
)

// TestExtractTrailingPercent pins the contract that colorizePercent depends on:
// given the cell strings produced by metricsRow, extract the trailing
// percentage as a float (or 0 when no number is present). This locks the
// behavior before any optimization-driven refactor of the parser.
func TestExtractTrailingPercent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"paren_percent", "12.3/45.6 GiB (78.9%)", 78.9},
		{"zero_percent", "0.0/0.0 MiB (0%)", 0.0},
		{"integer_percent", "99.0/100.0 GiB (95.5%)", 95.5},
		{"bare_percent", "3.2%", 3.2},
		{"hundred_percent", "100%", 100.0},
		{"space_before_percent", "95.5 %", 95.5},
		{"no_number", "err", 0.0},
		{"dash_placeholder", "-", 0.0},
		{"unreachable_text", "unreachable", 0.0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := extractTrailingPercent(tc.in); got != tc.want {
				t.Errorf("extractTrailingPercent(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// extractTrailingPercentInputs mirrors the cell formats emitted by metricsRow
// in production (the three colored columns on the host-metrics table).
var extractTrailingPercentInputs = []string{
	"12.3/45.6 GiB (78.9%)",
	"0.0/0.0 MiB (0%)",
	"99.0/100.0 GiB (95.5%)",
	"err",
	"-",
	"unreachable",
	"3.2%",
	"100%",
}

// BenchmarkExtractTrailingPercent measures the per-cell parse cost that runs
// once per colored metrics cell per Bubble Tea View() call (per keypress and
// per refresh tick). With 10 hosts × 3 colored columns, that's 30 calls per
// render; over a long TUI session the cumulative cost is non-trivial.
func BenchmarkExtractTrailingPercent(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, in := range extractTrailingPercentInputs {
			_ = extractTrailingPercent(in)
		}
	}
}

// BenchmarkMetricsRow exercises the full per-host row construction that
// runs once per host per render. Combined with the colorize callback this is
// the hot path the TUI host-metrics panel hammers on every View() call.
func BenchmarkMetricsRow(b *testing.B) {
	samples := []host.HostMetrics{
		{Name: "h1", CPUPercent: 12.3, MemUsedMiB: 1024, MemTotalMiB: 4096, DiskUsedGiB: 50, DiskTotalGiB: 200, Load1: 0.5, Load5: 0.4, Load15: 0.3, Uptime: "5d"},
		{Name: "h2", CPUPercent: 0.0, MemUsedMiB: 0, MemTotalMiB: 0, DiskUsedGiB: 0, DiskTotalGiB: 0, Uptime: "-"},
		{Name: "h3", Err: errSynthetic("ssh: handshake timeout")},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, m := range samples {
			_ = metricsRow(m)
		}
	}
}

// BenchmarkFormatHostMetrics measures the full FormatHostMetricsTo path: the
// per-cell padding work that produces the TUI scroll-panel string. It runs
// once per host-metric refresh tick per View() call, so its alloc count
// compounds across long sessions.
func BenchmarkFormatHostMetrics(b *testing.B) {
	samples := []host.HostMetrics{
		{Name: "h1", CPUPercent: 12.3, MemUsedMiB: 1024, MemTotalMiB: 4096, DiskUsedGiB: 50, DiskTotalGiB: 200, Load1: 0.5, Load5: 0.4, Load15: 0.3, Uptime: "5d"},
		{Name: "h2", CPUPercent: 0.0, MemUsedMiB: 0, MemTotalMiB: 0, DiskUsedGiB: 0, DiskTotalGiB: 0, Uptime: "-"},
		{Name: "h3", Err: errSynthetic("ssh: handshake timeout")},
		{Name: "h4", CPUPercent: 78.9, MemUsedMiB: 8192, MemTotalMiB: 16384, DiskUsedGiB: 100, DiskTotalGiB: 250, Load1: 2.5, Load5: 2.1, Load15: 1.8, Uptime: "12d"},
		{Name: "h5", CPUPercent: 99.9, MemUsedMiB: 30000, MemTotalMiB: 32000, DiskUsedGiB: 480, DiskTotalGiB: 500, Load1: 8.1, Load5: 7.2, Load15: 6.5, Uptime: "1d"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sb strings.Builder
		sb.Grow(64 + len(samples)*128)
		FormatHostMetricsTo(&sb, samples)
	}
}

type syntheticError struct{ s string }

func (e *syntheticError) Error() string { return e.s }

func errSynthetic(s string) error { return &syntheticError{s: s} }

// TestLoadStr pins the contract that metricsRow depends on: a non-zero
// load-avg triple formats as "l1 l5 l15" with 2-decimal precision, and an
// all-zero triple (e.g. Windows hosts) formats as "-".
func TestLoadStr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		m    host.HostMetrics
		want string
	}{
		{"nonzero", host.HostMetrics{Load1: 1.23, Load5: 0.89, Load15: 0.45}, "1.23 0.89 0.45"},
		{"all_zero", host.HostMetrics{}, "-"},
		{"high_load", host.HostMetrics{Load1: 8.1, Load5: 7.2, Load15: 6.5}, "8.10 7.20 6.50"},
		{"fractional", host.HostMetrics{Load1: 0.5, Load5: 0.4, Load15: 0.3}, "0.50 0.40 0.30"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.m.LoadStr(); got != tc.want {
				t.Errorf("LoadStr() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatHostMetrics pins the multi-row string contract for the scroll
// panel. The output must be one header line, then one line per host, each
// line terminated by "\n" (the final "\n" is the buffer's final byte).
func TestFormatHostMetrics(t *testing.T) {
	t.Parallel()
	metrics := []host.HostMetrics{
		{Name: "h1", CPUPercent: 12.3, MemUsedMiB: 1024, MemTotalMiB: 4096, DiskUsedGiB: 50, DiskTotalGiB: 200, Load1: 0.5, Load5: 0.4, Load15: 0.3, Uptime: "5d"},
		{Name: "h2", CPUPercent: 78.9, MemUsedMiB: 8192, MemTotalMiB: 16384, DiskUsedGiB: 100, DiskTotalGiB: 250, Load1: 2.5, Load5: 2.1, Load15: 1.8, Uptime: "12d"},
		{Name: "h3", Err: errSynthetic("ssh: handshake timeout")},
	}
	var b strings.Builder
	b.Grow(64 + len(metrics)*128)
	FormatHostMetricsTo(&b, metrics)
	got := b.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 { // 1 header + 3 data rows
		t.Fatalf("FormatHostMetricsTo: got %d lines, want 4 (1 header + 3 data rows). Output:\n%s", len(lines), got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output should end with newline, got: %q", got[len(got)-5:])
	}
	if !strings.HasPrefix(lines[0], "HOST") {
		t.Errorf("header line should start with HOST, got: %q", lines[0])
	}
	if !strings.Contains(lines[1], "h1") || !strings.Contains(lines[1], "12.3%") {
		t.Errorf("row 1 should contain h1 and CPU%%=12.3%%, got: %q", lines[1])
	}
	if !strings.Contains(lines[2], "h2") || !strings.Contains(lines[2], "78.9%") {
		t.Errorf("row 2 should contain h2 and CPU%%=78.9%%, got: %q", lines[2])
	}
	if !strings.Contains(lines[3], "h3") || !strings.Contains(lines[3], "unreachable") {
		t.Errorf("row 3 should be error placeholder, got: %q", lines[3])
	}

	// Empty input → "No hosts found." (matches old behavior).
	var emptyB strings.Builder
	FormatHostMetricsTo(&emptyB, nil)
	empty := emptyB.String()
	if empty != "  No hosts found." {
		t.Errorf("empty input: got %q, want %q", empty, "  No hosts found.")
	}
}

// TestFormatHostMetrics_ColumnAlignment pins the left-justify contract that
// the original fmt.Sprintf("%-*s  ", ...) established: each cell is padded on
// the RIGHT so a short cell is followed by its padding spaces, not preceded by
// them. A regression that swaps to right-justify would shift every column's
// start position and misalign the table without changing any Contains-based
// assertion, so this asserts exact leading/trailing space structure.
func TestFormatHostMetrics_ColumnAlignment(t *testing.T) {
	t.Parallel()
	// "h1" is the shortest host name; the HOST column width is driven by the
	// header "HOST" (4), so "h1" must be padded to "h1  " (right-padded).
	metrics := []host.HostMetrics{
		{Name: "h1", CPUPercent: 12.3, MemUsedMiB: 1024, MemTotalMiB: 4096, DiskUsedGiB: 50, DiskTotalGiB: 200, Load1: 0.5, Load5: 0.4, Load15: 0.3, Uptime: "5d"},
	}
	var b strings.Builder
	b.Grow(64 + len(metrics)*128)
	FormatHostMetricsTo(&b, metrics)
	got := b.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	data := lines[1]
	if !strings.HasPrefix(data, "h1 ") {
		t.Errorf("HOST cell must be left-justified (padded on the right); expected %q to start with \"h1 \" (right-padded to width 4), got: %q", "h1", data)
	}
	if strings.HasPrefix(data, " h1") || strings.HasPrefix(data, "  h1") {
		t.Errorf("HOST cell must NOT be right-justified (padded on the left); got leading spaces before h1 in: %q", data)
	}
}
