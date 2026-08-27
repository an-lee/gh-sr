package table

import (
	"bytes"
	"testing"
)

func TestColumnWidths(t *testing.T) {
	t.Parallel()

	t.Run("empty rows returns header widths", func(t *testing.T) {
		t.Parallel()
		headers := []string{"A", "BB", "CCC"}
		got := ColumnWidths(headers, nil)
		want := []int{1, 2, 3}
		assertIntSlice(t, "widths", got, want)
	})

	t.Run("row cell longer than header grows column", func(t *testing.T) {
		t.Parallel()
		headers := []string{"H1", "H2"}
		rows := [][]string{{"a", "longercell"}, {"another", "b"}}
		got := ColumnWidths(headers, rows)
		want := []int{7, 10}
		assertIntSlice(t, "widths", got, want)
	})

	t.Run("ragged row does not panic and stops at header length", func(t *testing.T) {
		t.Parallel()
		headers := []string{"A", "B", "C"}
		rows := [][]string{{"short"}}
		got := ColumnWidths(headers, rows)
		want := []int{5, 1, 1}
		assertIntSlice(t, "widths", got, want)
	})

	t.Run("empty cell does not grow column", func(t *testing.T) {
		t.Parallel()
		headers := []string{"X"}
		rows := [][]string{{""}, {""}}
		got := ColumnWidths(headers, rows)
		if got[0] != 1 {
			t.Errorf("width = %d, want 1 (header only)", got[0])
		}
	})

	t.Run("unicode cell width uses byte length", func(t *testing.T) {
		t.Parallel()
		headers := []string{"A"}
		rows := [][]string{{"☃"}}
		got := ColumnWidths(headers, rows)
		if got[0] != 3 {
			t.Errorf("width = %d, want 3 (byte length of ☃)", got[0])
		}
	})
}

func TestPrintRow(t *testing.T) {
	t.Parallel()

	t.Run("pads each cell to its column width", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		printRow(&buf, []string{"a", "bb"}, []int{3, 5})
		got := buf.String()
		want := "a    bb     \n"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("truncates cells beyond the widths slice", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		printRow(&buf, []string{"a", "b", "c", "d"}, []int{1, 1})
		got := buf.String()
		want := "a  b  \n"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("empty cells are still padded and printed", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		printRow(&buf, []string{"", "x"}, []int{2, 1})
		got := buf.String()
		want := "    x  \n"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
}

func TestPrintPlain_empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	ok := PrintPlain(&buf, Options{EmptyMsg: "nothing here"})
	if ok {
		t.Fatal("PrintPlain should return false for empty rows")
	}
	if got := buf.String(); got != "nothing here\n" {
		t.Errorf("got %q want %q", got, "nothing here\n")
	}
}

func TestRenderPlain_empty(t *testing.T) {
	t.Parallel()
	got := RenderPlain(Options{EmptyMsg: "nothing here"})
	if got != "nothing here" {
		t.Errorf("got %q want %q", got, "nothing here")
	}
}

func TestRenderPlain_rows(t *testing.T) {
	t.Parallel()
	got := RenderPlain(Options{
		Headers: []string{"HOST", "CPU", "MEMORY"},
		Rows: [][]string{
			{"h1", "12.3%", "1.0/4.0 GiB (25%)"},
			{"h2", "78.9%", "8.0/16.0 GiB (50%)"},
		},
	})
	// Column widths: HOST=4, CPU=max(3,5,5)=5, MEMORY=max(6,17,18)=18.
	// Each cell gets "<cell>  " with right-pad to its column width.
	want := "HOST  CPU    MEMORY              \n" +
		"h1    12.3%  1.0/4.0 GiB (25%)   \n" +
		"h2    78.9%  8.0/16.0 GiB (50%)  \n"
	if got != want {
		t.Errorf("RenderPlain mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// TestRenderPlain_ragged pins the contract that callers (FormatHostMetricsTo,
// PrintHostMetricsTable, disk-usage tables) rely on: ragged rows (cells beyond
// len(widths)) are truncated at the widths boundary instead of panicking.
// Mirrors TestPrintRow's behavior.
func TestRenderPlain_ragged(t *testing.T) {
	t.Parallel()
	got := RenderPlain(Options{
		Headers: []string{"A", "B"},
		Rows:    [][]string{{"long", "x", "extra"}},
	})
	// Widths: A=max(1,4)=4, B=max(1,1)=1.
	want := "A     B  \n" +
		"long  x  \n"
	if got != want {
		t.Errorf("RenderPlain ragged mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func assertIntSlice(t *testing.T, label string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len(got)=%d, len(want)=%d", label, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %d, want %d", label, i, got[i], want[i])
		}
	}
}
