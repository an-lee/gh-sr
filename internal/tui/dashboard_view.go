package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/an-lee/gh-sr/internal/table"
)

func (m *dashboardModel) View() tea.View {
	switch m.panel {
	case panelScroll:
		return m.viewScroll()
	case panelConfirmCleanup:
		return m.viewConfirmCleanup()
	case panelConfirmRemove:
		return m.viewConfirmRemove()
	case panelActionMenu:
		return m.viewActionMenu()
	case panelGlobalMenu:
		return m.viewGlobalMenu()
	case panelFilterMenu:
		return m.viewFilterMenu()
	case panelFilterHost:
		return m.viewFilterList(m.filterHostChoices, "Select host filter (esc cancel)")
	case panelFilterRepo:
		return m.viewFilterList(m.filterRepoChoices, "Select repo filter (esc cancel)")
	case panelHostMetrics:
		return m.viewHostMetrics()
	default:
		return m.viewMain()
	}
}

// newAltView wraps tea.NewView with the AltScreen flag the dashboard enables
// on every panel. Centralising it keeps every View() return path consistent
// (one of the 11 panels forgetting the flag used to render in the main screen
// buffer instead of the alternate buffer — easy to miss in a code review).
func newAltView(s string) tea.View {
	v := tea.NewView(s)
	v.AltScreen = true
	return v
}

// renderMenuItems builds the per-item list shared by every cursor-driven menu
// panel (action / global / filter / dynamic filter list). The selected item is
// prefixed with the "  > " marker through selectedStyle; the remaining items
// use the plain "    " indent so the column of arrow markers stays aligned.
// The trailing newline is part of the returned string so callers can append
// it directly to a strings.Builder.
func renderMenuItems(items []string, cursor int) string {
	var b strings.Builder
	for i, label := range items {
		if i == cursor {
			b.WriteString(selectedStyle.Render("  > "+label) + "\n")
		} else {
			b.WriteString("    " + label + "\n")
		}
	}
	return b.String()
}

func (m *dashboardModel) viewMain() tea.View {
	var b strings.Builder

	b.WriteString(titleStyle.Render("gh sr dashboard"))
	b.WriteString("\n\n")

	filterParts := []string{}
	if m.tuiHostFilter != "" {
		filterParts = append(filterParts, "host="+m.tuiHostFilter)
	}
	if m.tuiRepoFilter != "" {
		filterParts = append(filterParts, "repo="+m.tuiRepoFilter)
	}
	if len(filterParts) == 0 {
		b.WriteString(helpStyle.Render("  Filters: (none)  — press f to change") + "\n")
	} else {
		b.WriteString(helpStyle.Render("  Filters: "+strings.Join(filterParts, "  ")) + "\n")
	}

	if m.busy && m.busyOp != "" {
		b.WriteString(statusUnknown.Render("  … "+m.busyOp+" in progress") + "\n")
	}
	if m.toast != "" {
		b.WriteString(statusOnline.Render("  "+m.toast) + "\n")
	}
	b.WriteString("\n")

	if m.loading && len(m.statuses) == 0 {
		b.WriteString("  Loading...\n")
		return newAltView(b.String())
	}

	if m.lastErr != "" {
		b.WriteString(statusStopped.Render("  Error: "+m.lastErr) + "\n\n")
	}

	if len(m.statuses) == 0 {
		b.WriteString("  No runners in view (check filters or config).\n")
		b.WriteString(m.footerMain())
		if m.showHelp {
			b.WriteString("\n" + helpOverlay())
		}
		return newAltView(b.String())
	}

	rows := make([][]string, len(m.statuses))
	for i, s := range m.statuses {
		rows[i] = runnerStatusCells(s)
	}
	widths := table.ColumnWidths(runnerStatusHeaders, rows)

	b.WriteString(renderHeader(runnerStatusHeaders, widths) + "\n")

	for i, cells := range rows {
		if i == m.cursor {
			b.WriteString(renderHighlightedRow(cells, widths, runnerStatusColorize) + "\n")
		} else {
			b.WriteString(renderRow(cells, widths, runnerStatusColorize) + "\n")
		}
	}

	b.WriteString(m.footerMain())
	if m.showHelp {
		b.WriteString("\n" + helpOverlay())
	}

	return newAltView(b.String())
}

// footerMainIdle and footerMainLoading are the two pre-built help-line
// renderings footerMain returns. The body of the line is identical in both
// states — the only difference is the trailing "  (refreshing…)" indicator
// during a refresh tick. Pre-rendering once at package init avoids a
// per-View() fmt.Sprintf (reflection + []interface{} boxing) for a string
// with exactly two possible states. footerMain just picks the right one.
var (
	footerMainIdle    = helpStyle.Render("\n  j/k: move  enter: runner actions  g: global menu  h: host metrics  f: filters  r: refresh  ?: help  q: quit")
	footerMainLoading = helpStyle.Render("\n  j/k: move  enter: runner actions  g: global menu  h: host metrics  f: filters  r: refresh  ?: help  q: quit  (refreshing…)")
)

func (m *dashboardModel) footerMain() string {
	if m.loading {
		return footerMainLoading
	}
	return footerMainIdle
}

func helpOverlay() string {
	return helpStyle.Render(`  — Help —
  Main: j/k navigate rows, enter opens actions for the selected instance.
  Actions: setup, up, down, restart, update, logs (esc back).
  Host metrics (h): CPU, memory, disk, load average, uptime per host.
  Global (g): doctor, host metrics, cleanup, show/validate config, edit yaml/env, filters.
  Filters (f): narrow by host or repo; clear restores full list.
  Scroll views (logs, doctor, config): j/k line, ctrl+u/ctrl+d page, home/end, esc back.
  Cleanup asks for y/n confirmation. Run gh sr init from a shell (not in the TUI).`)
}

func (m *dashboardModel) viewActionMenu() tea.View {
	var b strings.Builder
	inst, _ := m.selectedInstance()
	b.WriteString(titleStyle.Render("Runner actions"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Instance: %s\n\n", configVal.Render(inst)))
	b.WriteString(renderMenuItems(actionMenuLabels, m.menuCursor))
	b.WriteString(helpStyle.Render("\n  enter: run  esc: back") + "\n")
	return newAltView(b.String())
}

func (m *dashboardModel) viewGlobalMenu() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Global menu"))
	b.WriteString("\n\n")
	b.WriteString(renderMenuItems(globalMenuLabels, m.menuCursor))
	b.WriteString(helpStyle.Render("\n  enter: choose  esc: back") + "\n")
	return newAltView(b.String())
}

func (m *dashboardModel) viewFilterMenu() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Filters"))
	b.WriteString("\n\n")
	b.WriteString(renderMenuItems(filterMenuLabels, m.menuCursor))
	b.WriteString(helpStyle.Render("\n  enter: choose  esc: back") + "\n")
	return newAltView(b.String())
}

func (m *dashboardModel) viewFilterList(choices []string, subtitle string) tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Pick filter"))
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("  "+subtitle) + "\n\n")
	if len(choices) == 0 {
		b.WriteString("  (no values)\n")
	} else {
		b.WriteString(renderMenuItems(choices, m.menuCursor))
	}
	b.WriteString(helpStyle.Render("\n  enter: apply  esc: back") + "\n")
	return newAltView(b.String())
}

func (m *dashboardModel) viewConfirmCleanup() tea.View {
	s := titleStyle.Render("Confirm cleanup") + "\n\n" +
		"  Remove offline self-hosted runners via the GitHub API?\n\n" +
		helpStyle.Render("  y: confirm   n / esc: cancel") + "\n"
	return newAltView(s)
}

func (m *dashboardModel) viewConfirmRemove() tea.View {
	s := titleStyle.Render("Confirm remove") + "\n\n" +
		fmt.Sprintf("  Remove runner %s? This will deregister it from GitHub,\n  remove it from the host, and delete it from config.\n\n",
			configVal.Render(m.confirmRemoveInst)) +
		helpStyle.Render("  y: confirm   n / esc: cancel") + "\n"
	return newAltView(s)
}

func (m *dashboardModel) viewScroll() tea.View {
	h := m.height
	if h < 8 {
		h = 24
	}
	visible := max(3, h-6)
	end := min(m.scrollOff+visible, len(m.scrollLines))

	var b strings.Builder
	b.WriteString(titleStyle.Render(m.scrollTitle))
	b.WriteString("\n\n")
	if m.scrollOff > 0 || end < len(m.scrollLines) {
		b.WriteString(helpStyle.Render(fmt.Sprintf(
			"  lines %d–%d of %d  (j/k scroll, ctrl+u/ctrl+d page, esc back)\n\n",
			m.scrollOff+1, end, len(m.scrollLines),
		)))
	} else {
		b.WriteString(helpStyle.Render("  j/k scroll · ctrl+u/ctrl+d page · esc back\n\n"))
	}
	for i := m.scrollOff; i < end; i++ {
		b.WriteString("  " + m.scrollLines[i] + "\n")
	}
	return newAltView(b.String())
}

func (m *dashboardModel) viewHostMetrics() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Host Metrics"))
	b.WriteString("\n\n")

	if m.metricsLoading && len(m.hostMetrics) == 0 {
		b.WriteString("  Loading metrics...\n")
		return newAltView(b.String())
	}

	if len(m.hostMetrics) == 0 {
		b.WriteString("  No hosts found.\n")
	} else {
		rows := buildHostMetricsRows(m.hostMetrics)

		widths := table.ColumnWidths(hostMetricsHeaders, rows)

		b.WriteString(renderHeader(hostMetricsHeaders, widths) + "\n")

		for _, row := range rows {
			b.WriteString(renderRow(row, widths, hostMetricsColorize) + "\n")
		}
	}

	b.WriteString(helpStyle.Render("\n  r: refresh  esc: back  q: back"))
	return newAltView(b.String())
}
