package tui

import (
	"testing"

	"github.com/an-lee/gh-sr/internal/runner"
)

// BenchmarkFooterMain measures the per-View() cost of rendering the bottom
// status line. The dashboard calls footerMain on every render (refresh tick
// + keypress), so the alloc count compounds across long sessions. The
// previous implementation used fmt.Sprintf with a single %s substitution
// for the loading indicator, which paid for reflection + []interface{}
// boxing on every call.
func BenchmarkFooterMain(b *testing.B) {
	m := &dashboardModel{}
	b.Run("idle", func(b *testing.B) {
		b.ReportAllocs()
		m.loading = false
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = m.footerMain()
		}
	})
	b.Run("loading", func(b *testing.B) {
		b.ReportAllocs()
		m.loading = true
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = m.footerMain()
		}
	})
}

// BenchmarkViewMain exercises the full viewMain render with one populated
// status row. This is the compositing benchmark that captures the marginal
// improvement from a cheaper footerMain in context: a typical session
// renders this dozens of times per minute.
func BenchmarkViewMain(b *testing.B) {
	m := newBenchDashboardModel()
	b.Run("one_status", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = m.viewMain()
		}
	})
}

// newBenchDashboardModel builds a dashboardModel wired with enough state
// for viewMain() to render the populated table branch (1+ status rows +
// matching widths). It is shared by BenchmarkViewMain and the surrounding
// pinning tests so the bench state matches what users actually see.
func newBenchDashboardModel() *dashboardModel {
	return &dashboardModel{
		statuses: []runner.RunnerStatus{
			{
				Instance:            "runner-1",
				Host:                "host1.example",
				Repo:                "o/r1",
				Mode:                "container",
				ContainerImage:      "gh-sr/agentic-runner:2.320.0",
				ContainerImageBuild: "-",
				Local:               "running",
				Remote:              "online",
				Labels:              "self-hosted,linux,x64",
			},
		},
	}
}

// TestFooterMain_idleAndLoading pins the visible text for the dashboard
// footer so an optimisation that swaps helpStyle.Render of a const string
// for a hand-rolled renderer doesn't silently regress the user-visible
// contents. The constants must differ ONLY in the trailing
// "  (refreshing…)" indicator — the rest of the help text is identical and
// must remain so to keep the muscle memory of users who have memorised the
// shortcut keys.
func TestFooterMain_idleAndLoading(t *testing.T) {
	t.Parallel()
	m := &dashboardModel{}

	idle := m.footerMain()
	m.loading = true
	loading := m.footerMain()

	// Both variants must be non-empty.
	if idle == "" {
		t.Fatal("idle footer must be non-empty")
	}
	if loading == "" {
		t.Fatal("loading footer must be non-empty")
	}
	// The two strings should differ — if they collapse to identical
	// strings the loading indicator has been lost.
	if idle == loading {
		t.Fatal("idle and loading footer strings must differ; the loading indicator must be visible")
	}
	// Strip ANSI escapes from both: the visible text must contain the
	// help shortcut prefix in both states.
	for _, tc := range []struct {
		name   string
		s      string
		marker string
	}{
		{"idle", idle, "j/k: move"},
		{"loading", loading, "j/k: move"},
	} {
		if !containsVisible(tc.s, tc.marker) {
			t.Errorf("%s footer must contain %q in visible text, got: %q", tc.name, tc.marker, tc.s)
		}
	}
	// Loading must additionally carry the refresh indicator.
	if !containsVisible(loading, "refreshing") {
		t.Error("loading footer must contain \"refreshing\" indicator")
	}
	if containsVisible(idle, "refreshing") {
		t.Error("idle footer must NOT contain \"refreshing\" indicator")
	}
}

// containsVisible returns true if substr appears in s after stripping ANSI
// CSI escape sequences (\x1b[...m). This lets the assertion test the visible
// string instead of brittle raw byte equality.
func containsVisible(s, substr string) bool {
	var b []byte
	inEscape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEscape {
			// CSI escapes end at the final letter byte in the range
			// 0x40-0x7e. The footer is rendered through lipgloss SGR
			// only, so the terminator is always an 'm'.
			if c == 'm' {
				inEscape = false
			}
			continue
		}
		if c == 0x1b {
			inEscape = true
			continue
		}
		b = append(b, c)
	}
	return indexOf(string(b), substr) >= 0
}

func indexOf(s, substr string) int {
	if len(substr) == 0 {
		return 0
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
