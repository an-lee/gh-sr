package tui

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/runner"
)

func PrintStatusTable(statuses []runner.RunnerStatus) {
	rows := make([][]string, len(statuses))
	for i, s := range statuses {
		rows[i] = runnerStatusCells(s)
	}
	PrintTable(os.Stdout, TablePrintOptions{
		Title:    "Runner Status",
		EmptyMsg: "No runners found.",
		Headers:  runnerStatusHeaders,
		Rows:     rows,
		Colorize: runnerStatusColorize,
	})
}

// FormatConfig returns a styled, redacted snapshot of the resolved configuration (stable host order).
func FormatConfig(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Resolved Configuration"))
	b.WriteString("\n")

	_, tokErr := config.ResolveToken(cfg)
	tokenDisplay := "(none)"
	if tokErr == nil {
		tokenDisplay = "(from gh CLI)"
	}
	// Token line: "  <styled "Token:"> <styled display>\n\n"
	// Direct concat avoids the fmt.Sprintf reflection + []interface{}
	// boxing round-trip (one alloc saved per call).
	b.WriteString("  ")
	b.WriteString(configKey.Render("Token:"))
	b.WriteByte(' ')
	b.WriteString(configVal.Render(tokenDisplay))
	b.WriteString("\n\n")

	b.WriteString(configKey.Render("Hosts:"))
	b.WriteString("\n")
	hostNames := make([]string, 0, len(cfg.Hosts))
	for name := range cfg.Hosts {
		hostNames = append(hostNames, name)
	}
	sort.Strings(hostNames)
	for _, name := range hostNames {
		h := cfg.Hosts[name]
		// Host line: "  <styled name>  addr=<addr>  os=<os>  arch=<arch>\n"
		// 4 string args + 4 interface{} boxing allocs avoided.
		b.WriteString("  ")
		b.WriteString(configVal.Render(name))
		b.WriteString("  addr=")
		b.WriteString(h.Addr)
		b.WriteString("  os=")
		b.WriteString(h.OS)
		b.WriteString("  arch=")
		b.WriteString(h.Arch)
		b.WriteByte('\n')
	}

	b.WriteString("\n")
	b.WriteString(configKey.Render("Runners:"))
	b.WriteString("\n")
	for _, r := range cfg.Runners {
		// Runner line: "  <styled name>  target=<target>  host=<host>  count=<n>  mode=<m>  labels=[<l>][  ephemeral]\n"
		// 7 string args + 1 int arg = 8 interface{} boxing allocs avoided.
		b.WriteString("  ")
		b.WriteString(configVal.Render(r.Name))
		b.WriteString("  target=")
		b.WriteString(r.DisplayTarget())
		b.WriteString("  host=")
		b.WriteString(r.Host)
		b.WriteString("  count=")
		b.WriteString(strconv.Itoa(r.Count))
		b.WriteString("  mode=")
		b.WriteString(r.EffectiveRunnerMode())
		b.WriteString("  labels=[")
		b.WriteString(strings.Join(r.Labels, ", "))
		b.WriteByte(']')
		if r.Ephemeral {
			b.WriteString("  ephemeral")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func PrintConfig(cfg *config.Config) {
	fmt.Print(FormatConfig(cfg))
}

func formatGitHubStatus(s runner.RunnerStatus) string {
	if s.Remote == "" {
		return "-"
	}
	if s.Busy {
		return "busy"
	}
	return s.Remote
}

func colorizeImageBuild(cell string) string {
	switch {
	case strings.HasPrefix(cell, "ok"):
		return statusOnline.Render(cell)
	case strings.HasPrefix(cell, "stale"):
		return statusStopped.Render(cell)
	case cell == "?":
		return statusUnknown.Render(cell)
	default:
		return cell
	}
}

func colorizeLocalStatus(status string) string {
	switch status {
	case "running":
		return statusRunning.Render(status)
	case "stopped":
		return statusStopped.Render(status)
	case "failed":
		return statusStopped.Render(status)
	case "restarting":
		return statusBusy.Render(status)
	case "service error":
		return statusBusy.Render(status)
	case "not installed":
		return statusUnknown.Render(status)
	case "unreachable":
		return statusStopped.Render(status)
	default:
		return statusUnknown.Render(status)
	}
}

func colorizeGitHubStatus(status string) string {
	switch status {
	case "online":
		return statusOnline.Render(status)
	case "offline":
		return statusOffline.Render(status)
	case "busy":
		return statusBusy.Render(status)
	default:
		return statusUnknown.Render(status)
	}
}
