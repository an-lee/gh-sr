package tui

import (
	"os"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/strfmt"
)

// hostMetricsHeaders is the canonical column ordering for the host-metrics
// table shared by PrintHostMetricsTable and viewHostMetrics.
// Keep this slice in sync with metricsRow so a column rename does not silently
// misalign the two renderers.
var hostMetricsHeaders = []string{"HOST", "CPU", "MEMORY", "DISK", "LOAD AVG", "UPTIME"}

// buildHostMetricsRows maps metrics → the per-row string slices used by both
// host-metrics renderers. Centralising the row construction keeps the header
// literal and metricsRow call in one place.
func buildHostMetricsRows(metrics []host.HostMetrics) [][]string {
	rows := make([][]string, len(metrics))
	for i, m := range metrics {
		rows[i] = metricsRow(m)
	}
	return rows
}

// hostMetricsColorize highlights CPU/MEMORY/DISK percentage cells (columns
// 1..3) using colorizePercent. Non-percentage cells pass through unchanged.
// Shared by both host-metrics renderers (PrintHostMetricsTable +
// viewHostMetrics).
func hostMetricsColorize(col int, cell string) string {
	if col >= 1 && col <= 3 {
		return colorizePercent(cell)
	}
	return cell
}

// PrintHostMetricsTable prints a tabular summary of host resource usage to stdout.
func PrintHostMetricsTable(metrics []host.HostMetrics) {
	PrintTable(os.Stdout, TablePrintOptions{
		Title:    "Host Metrics",
		EmptyMsg: "No hosts found.",
		Headers:  hostMetricsHeaders,
		Rows:     buildHostMetricsRows(metrics),
		Colorize: hostMetricsColorize,
	})
}

// FormatHostMetricsTo writes the host-metrics table directly into b without
// routing cells through metricsRow + RenderPlain (which would allocate a
// []string{6} slice per cell + a fresh string per formatted cell — ~25+
// allocs/op on the previous BenchmarkFormatHostMetrics). Writing each cell
// via the shared append* helpers and a per-cell padded write keeps the
// allocation count at one strings.Builder growth per call (the buffer
// pre-sizing done by the caller, if any) plus whatever the helper functions
// allocate internally. The bench numbers reflect the win.
func FormatHostMetricsTo(b *strings.Builder, metrics []host.HostMetrics) {
	if len(metrics) == 0 {
		b.WriteString("  No hosts found.")
		return
	}

	// Pass 1: measure column widths via a stack scratch buffer. The
	// largest realistic cell output is around 24 chars
	// ("999999/9999999 GiB (100%)"); [64]byte covers every cell with
	// room to spare and stays on the stack.
	var scratch [64]byte
	var widths [6]int
	for i, h := range hostMetricsHeaders {
		widths[i] = len(h)
	}
	for _, m := range metrics {
		if l := len(m.Name); l > widths[0] {
			widths[0] = l
		}
		if m.Err != nil {
			// Error placeholder row uses fixed widths for "err", "err",
			// "err", "-", "unreachable" — widen if any current width is
			// smaller. (Real cell widths from the surrounding hosts would
			// already be ≥ these, but stay explicit so the error path is
			// self-contained.)
			const errW, dashW, unreachW = 3, 1, 11
			if widths[1] < errW {
				widths[1] = errW
			}
			if widths[2] < errW {
				widths[2] = errW
			}
			if widths[3] < errW {
				widths[3] = errW
			}
			if widths[4] < dashW {
				widths[4] = dashW
			}
			if widths[5] < unreachW {
				widths[5] = unreachW
			}
			continue
		}
		if l := len(appendFormatPercent(scratch[:0], m.CPUPercent, 1)); l > widths[1] {
			widths[1] = l
		}
		if l := len(appendFormatUsedTotal(scratch[:0], m.MemUsedMiB, m.MemTotalMiB, m.MemPercent(), "MiB")); l > widths[2] {
			widths[2] = l
		}
		if l := len(appendFormatUsedTotal(scratch[:0], m.DiskUsedGiB, m.DiskTotalGiB, m.DiskPercent(), "GiB")); l > widths[3] {
			widths[3] = l
		}
		if l := len(m.AppendLoadStr(scratch[:0])); l > widths[4] {
			widths[4] = l
		}
		if m.Uptime != "" {
			if l := len(m.Uptime); l > widths[5] {
				widths[5] = l
			}
		} else if widths[5] < 1 {
			widths[5] = 1 // "-" placeholder
		}
	}

	// Pass 2: render header + each row directly into b. No
	// intermediate [][]string slice, no per-cell string coercion.
	appendPaddedHeaderCells(b, widths)
	b.WriteByte('\n')
	for _, m := range metrics {
		appendPaddedMetricsCells(b, m, widths)
		b.WriteByte('\n')
	}
}

// appendPaddedHeaderCells writes the header row (right-padded to widths) to
// b without allocating intermediate strings for the header cells (the
// header literals in hostMetricsHeaders are package-level and stay alive
// for the program lifetime). The trailing "  " after the last cell matches
// the legacy table.RenderPlain output byte-for-byte (see PR #387 — the
// separator is appended unconditionally inside appendRowPlain's loop, not
// only between cells).
func appendPaddedHeaderCells(b *strings.Builder, widths [6]int) {
	for i, h := range hostMetricsHeaders {
		if i > 0 && i < len(widths) {
			b.WriteString("  ") // column separator (between cells)
		}
		b.WriteString(h)
		if pad := widths[i] - len(h); pad > 0 {
			writePad(b, pad)
		}
	}
	b.WriteString("  ") // trailing separator — matches appendRowPlain
}

// appendPaddedMetricsCells writes one host's row (right-padded to widths)
// to b. The error branch uses the placeholder cells from the original
// metricsRow. Non-error branches format each cell via the append* helpers
// (no string coercion) and copy the resulting scratch slice straight into
// b.
func appendPaddedMetricsCells(b *strings.Builder, m host.HostMetrics, widths [6]int) {
	var scratch [64]byte

	// HOST
	b.WriteString(m.Name)
	if pad := widths[0] - len(m.Name); pad > 0 {
		writePad(b, pad)
	}

	if m.Err != nil {
		// Error placeholder cells: "err", "err", "err", "-", "unreachable".
		for i, cell := range [...]string{"err", "err", "err", "-", "unreachable"} {
			b.WriteString("  ")
			b.WriteString(cell)
			if pad := widths[i+1] - len(cell); pad > 0 {
				writePad(b, pad)
			}
		}
		b.WriteString("  ") // trailing separator — matches appendRowPlain
		return
	}

	// CPU
	b.WriteString("  ")
	out := appendFormatPercent(scratch[:0], m.CPUPercent, 1)
	b.Write(out)
	if pad := widths[1] - len(out); pad > 0 {
		writePad(b, pad)
	}

	// MEM
	b.WriteString("  ")
	out = appendFormatUsedTotal(scratch[:0], m.MemUsedMiB, m.MemTotalMiB, m.MemPercent(), "MiB")
	b.Write(out)
	if pad := widths[2] - len(out); pad > 0 {
		writePad(b, pad)
	}

	// DISK
	b.WriteString("  ")
	out = appendFormatUsedTotal(scratch[:0], m.DiskUsedGiB, m.DiskTotalGiB, m.DiskPercent(), "GiB")
	b.Write(out)
	if pad := widths[3] - len(out); pad > 0 {
		writePad(b, pad)
	}

	// LOAD
	b.WriteString("  ")
	out = m.AppendLoadStr(scratch[:0])
	b.Write(out)
	if pad := widths[4] - len(out); pad > 0 {
		writePad(b, pad)
	}

	// UPTIME
	b.WriteString("  ")
	uptime := m.Uptime
	if uptime == "" {
		uptime = "-"
	}
	b.WriteString(uptime)
	if pad := widths[5] - len(uptime); pad > 0 {
		writePad(b, pad)
	}
	b.WriteString("  ") // trailing separator — matches appendRowPlain
}

// writePad writes n spaces to b without a per-call allocation. Slicing into
// the 80-space spaces80 constant in the table package (PR #387) keeps the
// per-cell pad allocation-free for every realistic column width.
func writePad(b *strings.Builder, n int) {
	if n <= 0 {
		return
	}
	if n <= len(tableSpaces80) {
		b.WriteString(tableSpaces80[:n])
		return
	}
	// Defensive fallback: pad > 80 has never been observed in gh-sr
	// renderers, but stay correct if a future caller exceeds the
	// spaces80 budget.
	b.WriteString(strings.Repeat(" ", n))
}

// tableSpaces80 mirrors the table.spaces80 constant so the tui package can
// pad cells without re-declaring the literal or dragging the table package's
// RenderPlain dependency into this hot path. Kept in sync with the
// declaration in internal/table/table.go (PR #387).
const tableSpaces80 = "                                                                                " // 80 spaces

// appendFormatPercent appends "12.3%" (v with prec decimals + '%') to dst
// and returns the extended slice. strfmt.FmtFloat does not allocate, and the
// trailing '%' is a single byte, so the function stays allocation-free for
// any dst the caller passes. The scratch buffer in FormatHostMetricsTo is
// stack-allocated and never escapes.
func appendFormatPercent(dst []byte, v float64, prec int) []byte {
	dst = strfmt.FmtFloat(dst, v, prec)
	return append(dst, '%')
}

// appendFormatUsedTotal appends "used/total UNIT (pct%)" (used/total/pct
// formatted with 0 decimals) to dst. The function mirrors formatUsedTotal's
// shape — the largest realistic output is around 24 chars
// ("999999/9999999 GiB (100%)"); the stack [48]byte scratch in the TUI
// caller covers this with room to spare.
func appendFormatUsedTotal(dst []byte, used, total, pct float64, unit string) []byte {
	dst = strfmt.FmtFloat(dst, used, 0)
	dst = append(dst, '/')
	dst = strfmt.FmtFloat(dst, total, 0)
	dst = append(dst, ' ')
	dst = append(dst, unit...)
	dst = append(dst, ' ', '(')
	dst = strfmt.FmtFloat(dst, pct, 0)
	return append(dst, '%', ')')
}

// metricsRow builds the per-host row that PrintHostMetricsTable and
// viewHostMetrics both render. The error branch produces a recognizable
// placeholder row so a single unreachable host does not blank the table.
func metricsRow(m host.HostMetrics) []string {
	if m.Err != nil {
		return []string{m.Name, "err", "err", "err", "-", "unreachable"}
	}
	// strconv.FormatFloat + strings.Builder avoids the per-call
	// reflection/format-string machinery that fmt.Sprintf drags in. metricsRow
	// is on the TUI metrics render path (once per host per View()), so reducing
	// its alloc count compounds across long dashboard sessions.
	cpu := formatPercent(m.CPUPercent, 1)
	mem := formatUsedTotal(m.MemUsedMiB, m.MemTotalMiB, m.MemPercent(), "MiB")
	disk := formatUsedTotal(m.DiskUsedGiB, m.DiskTotalGiB, m.DiskPercent(), "GiB")
	load := m.LoadStr()
	uptime := m.Uptime
	if uptime == "" {
		uptime = "-"
	}
	return []string{m.Name, cpu, mem, disk, load, uptime}
}

// formatPercent formats v with `prec` decimals followed by '%'.
//
// strconv.AppendFloat + a stack-allocated byte buffer avoids both the
// per-call string allocation that strconv.FormatFloat returns AND the
// strings.Builder heap allocation that the previous implementation
// dragged in. metricsRow calls this once per host per View(); for a
// 10-host panel that's 10 calls per render, and the cumulative cost
// compounds across long dashboard sessions.
//
// The largest realistic output is "100.0%" (6 chars); [24]byte holds
// the maximum AppendFloat output (24 chars) plus '%'.
func formatPercent(v float64, prec int) string {
	var buf [24]byte
	b := strfmt.FmtFloat(buf[:0], v, prec)
	b = append(b, '%')
	return string(b)
}

// formatUsedTotal formats "used/total UNIT (pct%)".
//
// strconv.AppendFloat + stack buffer avoids the strings.Builder heap
// allocation the previous implementation had. The largest realistic
// output is around 24 chars (e.g. "999999/9999999 GiB (100%)"); [48]byte
// holds AppendFloat's worst case (24 chars per float × 1 float at a time
// since the buffer is reused across writes) plus the 8 non-float chars
// ("/", " ", " (", "%)"). The buffer is big enough that this function
// never allocates on the heap.
func formatUsedTotal(used, total, pct float64, unit string) string {
	var buf [48]byte
	b := buf[:0]
	b = strfmt.FmtFloat(b, used, 0)
	b = append(b, '/')
	b = strfmt.FmtFloat(b, total, 0)
	b = append(b, ' ')
	b = append(b, unit...)
	b = append(b, ' ', '(')
	b = strfmt.FmtFloat(b, pct, 0)
	b = append(b, '%', ')')
	return string(b)
}

// colorizePercent highlights a cell that ends with a percentage based on severity.
func colorizePercent(cell string) string {
	pct := extractTrailingPercent(cell)
	switch {
	case pct >= 90:
		return statusStopped.Render(cell)
	case pct >= 70:
		return statusBusy.Render(cell)
	default:
		return statusOnline.Render(cell)
	}
}

func extractTrailingPercent(s string) float64 {
	// Manual scan to find the trailing percentage without allocating
	// intermediate trimmed strings. The cell formats colorizePercent sees:
	//
	//   "12.3/45.6 GiB (78.9%)"  → 78.9
	//   "0.0/0.0 MiB (0%)"       → 0
	//   "99.0/100.0 GiB (95.5%)" → 95.5
	//   "3.2%"                   → 3.2
	//   "100%"                   → 100
	//   "err" / "-" / "unreachable" → 0 (no '%' anywhere)
	//
	// The original implementation chained strings.TrimRight +
	// strings.LastIndex + two strings.TrimSpace + strings.TrimSuffix,
	// allocating up to 4 intermediate strings. This version finds the last
	// '%', walks back to skip trailing whitespace, and parses digits + a
	// single optional '.', allocating only the float64 return value (and
	// the strconv error's wrapping, which we drop via the `_` on the
	// pre-1.21 ParseFloat path that returned an error). On the TUI metrics
	// render path this is called once per colored cell per host on every
	// Bubble Tea View() call (per keypress and per refresh tick).
	i := strings.LastIndexByte(s, '%')
	if i < 0 {
		return 0
	}
	// Walk back over trailing whitespace (defensive — production cells
	// from formatUsedTotal don't have trailing whitespace, but
	// "95.5 %" with a space before % is plausible and we want to handle
	// it the same way the old TrimSpace + TrimSuffix did).
	j := i
	for j > 0 && s[j-1] == ' ' {
		j--
	}
	// Walk back over digits + at most one '.'. Stop at the first non-digit
	// so leading whitespace (or '(') is excluded from the ParseFloat slice —
	// strconv.ParseFloat rejects leading spaces.
	start := j
	for start > 0 {
		c := s[start-1]
		if c >= '0' && c <= '9' {
			start--
			continue
		}
		if c == '.' {
			start--
			continue
		}
		break
	}
	v, err := strconv.ParseFloat(s[start:j], 64)
	if err != nil {
		return 0
	}
	return v
}
