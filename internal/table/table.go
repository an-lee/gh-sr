// Package table provides shared column-width and plain-text table printing
// used by internal/tui (styled CLI tables) and internal/ops (disk usage output).
package table

import (
	"fmt"
	"io"
	"strings"
)

// Options configures PrintPlain.
type Options struct {
	EmptyMsg string
	Headers  []string
	Rows     [][]string
}

// ColumnWidths returns the maximum rendered width per column (header vs cells).
func ColumnWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for j, cell := range row {
			if j < len(widths) && len(cell) > widths[j] {
				widths[j] = len(cell)
			}
		}
	}
	return widths
}

// PrintPlain writes a plain-text table to w. When Rows is empty, EmptyMsg is
// printed and false is returned.
func PrintPlain(w io.Writer, opts Options) bool {
	if len(opts.Rows) == 0 {
		if opts.EmptyMsg != "" {
			fmt.Fprintln(w, opts.EmptyMsg)
		}
		return false
	}
	widths := ColumnWidths(opts.Headers, opts.Rows)
	printRow(w, opts.Headers, widths)
	for _, row := range opts.Rows {
		printRow(w, row, widths)
	}
	return true
}

func printRow(w io.Writer, cells []string, widths []int) {
	for i, cell := range cells {
		if i >= len(widths) {
			break
		}
		fmt.Fprintf(w, "%-*s  ", widths[i], cell)
	}
	fmt.Fprintln(w)
}

// RenderPlain returns the plain-text table as a string. When Rows is empty,
// EmptyMsg is returned as the sole line (without a trailing newline — callers
// append their own terminator as needed).
//
// Unlike PrintPlain, RenderPlain uses a strings.Builder for the per-cell
// padding loop instead of fmt.Fprintf so the per-cell alloc count stays at
// one. This matters on the TUI host-metrics render path, which calls into
// this helper once per refresh tick per View() and whose alloc count
// compounds across long sessions.
func RenderPlain(opts Options) string {
	if len(opts.Rows) == 0 {
		return opts.EmptyMsg
	}
	widths := ColumnWidths(opts.Headers, opts.Rows)

	var b strings.Builder
	// Header row + N data rows, with per-cell padding budget.
	b.Grow((len(opts.Headers) + len(opts.Rows)) * 32)
	appendRowPlain(&b, opts.Headers, widths)
	b.WriteByte('\n')
	for _, row := range opts.Rows {
		appendRowPlain(&b, row, widths)
		b.WriteByte('\n')
	}
	return b.String()
}

// spaces80 is a 80-space string used by appendRowPlain to right-pad cells
// without allocating per cell. The maximum realistic column width in the
// gh-sr renderers (FormatHostMetricsTo, PrintHostMetricsTable, disk-usage
// tables) is well under 80 bytes, so slicing into spaces80 covers every
// observed cell without ever needing strings.Repeat. If a caller ever
// exceeds 80, appendRowPlain falls back to strings.Repeat for the excess.
const spaces80 = "                                                                                " // 80 spaces

// appendRowPlain writes one padded row (without a trailing newline) to b.
// Inlining the right-pad as builder writes is a 1-alloc drop per cell vs
// fmt.Fprintf's format-string parser + reflection. Slicing into spaces80
// for the right-pad spaces drops the strings.Repeat allocation that the
// previous version paid per padded cell — RenderPlain runs once per
// TUI host-metrics refresh tick, and most cells in that table are
// shorter than their column width, so the per-cell Repeat savings
// compound across long dashboard sessions.
func appendRowPlain(b *strings.Builder, cells []string, widths []int) {
	for i, cell := range cells {
		if i >= len(widths) {
			break
		}
		b.WriteString(cell)
		pad := widths[i] - len(cell)
		if pad > 0 {
			if pad <= len(spaces80) {
				b.WriteString(spaces80[:pad])
			} else {
				// Defensive: pad > 80 has never been observed in the
				// gh-sr renderers, but stay correct if a future caller
				// exceeds the spaces80 budget.
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		b.WriteString("  ")
	}
}
