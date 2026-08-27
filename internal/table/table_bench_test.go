package table

import "testing"

// renderPlainRows mirrors the FormatHostMetricsTo row shape (6 columns) so
// the bench exercises the same padding workload as the TUI host-metrics
// render path. Realistic cell widths are uneven so most cells trigger
// the right-pad branch.
var renderPlainRows = [][]string{
	{"h1", "12.3%", "1024.0/4096.0 MiB (25%)", "50.0/200.0 GiB (25%)", "0.50 0.40 0.30", "5d"},
	{"host-2", "78.9%", "8192.0/16384.0 MiB (50%)", "100.0/250.0 GiB (40%)", "2.50 2.10 1.80", "12d"},
	{"x", "99.9%", "30000.0/32000.0 MiB (94%)", "480.0/500.0 GiB (96%)", "8.10 7.20 6.50", "1d"},
	{"long-host-name-1", "12.3%", "1024.0/4096.0 MiB (25%)", "50.0/200.0 GiB (25%)", "0.50 0.40 0.30", "5d"},
	{"host-2", "78.9%", "8192.0/16384.0 MiB (50%)", "100.0/250.0 GiB (40%)", "2.50 2.10 1.80", "12d"},
	{"another", "99.9%", "30000.0/32000.0 MiB (94%)", "480.0/500.0 GiB (96%)", "8.10 7.20 6.50", "1d"},
	{"host-7", "0.0%", "0.0/0.0 MiB (0%)", "0.0/0.0 GiB (0%)", "-", "-"},
	{"host-8", "100.0%", "99999.0/99999.0 MiB (100%)", "999.0/999.0 GiB (100%)", "0.00 0.00 0.00", "30d"},
}

var renderPlainOpts = Options{
	Headers: []string{"HOST", "CPU", "MEMORY", "DISK", "LOAD AVG", "UPTIME"},
	Rows:    renderPlainRows,
}

// BenchmarkRenderPlain measures the per-call alloc count for the
// RenderPlain plain-text renderer. RenderPlain runs once per TUI
// host-metrics refresh tick, so dropping the strings.Repeat per padded
// cell lands directly in steady-state GC pressure on long dashboard
// sessions. Benchstat regression detection (PR #333) on this bench
// will catch any future regression in the RenderPlain path.
func BenchmarkRenderPlain(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RenderPlain(renderPlainOpts)
	}
}
