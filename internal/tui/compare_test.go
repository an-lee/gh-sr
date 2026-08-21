package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/table"
)

// TestFormatHostMetrics_NewPath_ByteIdentical compares the new builder-direct
// FormatHostMetrics path output against the legacy RenderPlain path output
// for a representative set of metrics. The two paths must produce the
// exact same string for every input.
func TestFormatHostMetrics_NewPath_ByteIdentical(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		metrics []host.HostMetrics
	}{
		{
			"empty",
			nil,
		},
		{
			"single",
			[]host.HostMetrics{
				{Name: "h1", CPUPercent: 12.3, MemUsedMiB: 1024, MemTotalMiB: 4096, DiskUsedGiB: 50, DiskTotalGiB: 200, Load1: 0.5, Load5: 0.4, Load15: 0.3, Uptime: "5d"},
			},
		},
		{
			"mixed_with_error",
			[]host.HostMetrics{
				{Name: "h1", CPUPercent: 12.3, MemUsedMiB: 1024, MemTotalMiB: 4096, DiskUsedGiB: 50, DiskTotalGiB: 200, Load1: 0.5, Load5: 0.4, Load15: 0.3, Uptime: "5d"},
				{Name: "h2", CPUPercent: 78.9, MemUsedMiB: 8192, MemTotalMiB: 16384, DiskUsedGiB: 100, DiskTotalGiB: 250, Load1: 2.5, Load5: 2.1, Load15: 1.8, Uptime: "12d"},
				{Name: "h3", Err: errors.New("ssh: handshake timeout")},
				{Name: "long-host-name", CPUPercent: 99.9, MemUsedMiB: 30000, MemTotalMiB: 32000, DiskUsedGiB: 480, DiskTotalGiB: 500, Load1: 8.1, Load5: 7.2, Load15: 6.5, Uptime: "1d"},
				{Name: "h5", CPUPercent: 0.0, MemUsedMiB: 0, MemTotalMiB: 0, DiskUsedGiB: 0, DiskTotalGiB: 0, Uptime: "-"},
			},
		},
		{
			"zero_load_only",
			[]host.HostMetrics{
				{Name: "win1", CPUPercent: 50.0, MemUsedMiB: 1000, MemTotalMiB: 2000, DiskUsedGiB: 10, DiskTotalGiB: 100, Uptime: "3d"},
			},
		},
		{
			"empty_uptime",
			[]host.HostMetrics{
				{Name: "h1", CPUPercent: 50.0, MemUsedMiB: 1000, MemTotalMiB: 2000, DiskUsedGiB: 10, DiskTotalGiB: 100, Load1: 1.0, Load5: 1.0, Load15: 1.0},
			},
		},
		{
			"all_error",
			[]host.HostMetrics{
				{Name: "h1", Err: errors.New("dial timeout")},
				{Name: "h2", Err: errors.New("connection refused")},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FormatHostMetrics(tc.metrics)

			// Re-derive the expected output via the legacy path
			// (buildHostMetricsRows + table.RenderPlain). This locks
			// the byte contract against any drift in either
			// implementation.
			var legacy string
			if len(tc.metrics) == 0 {
				legacy = "  No hosts found."
			} else {
				rows := buildHostMetricsRows(tc.metrics)
				legacy = table.RenderPlain(table.Options{
					EmptyMsg: "  No hosts found.",
					Headers:  hostMetricsHeaders,
					Rows:     rows,
				})
			}

			if got != legacy {
				t.Errorf("FormatHostMetrics byte-mismatch with legacy RenderPlain path.\n--- new ---\n%s\n--- legacy ---\n%s\n--- diff ---\n%s",
					got, legacy, diffLines(got, legacy))
			}
		})
	}
}

// diffLines returns a simple line-level diff of two strings (just joined
// with line markers; tests use it for nicer error output).
func diffLines(a, b string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	var sb strings.Builder
	max := len(al)
	if len(bl) > max {
		max = len(bl)
	}
	for i := 0; i < max; i++ {
		var as, bs string
		if i < len(al) {
			as = al[i]
		}
		if i < len(bl) {
			bs = bl[i]
		}
		if as != bs {
			sb.WriteString("L")
			sb.WriteString(itoa(i))
			sb.WriteString(":\n  new:    ")
			sb.WriteString(as)
			sb.WriteString("\n  legacy: ")
			sb.WriteString(bs)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [16]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
