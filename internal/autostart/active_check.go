package autostart

import (
	"fmt"

	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
)

// runActiveCheck runs the per-kind "is-active" probe and returns the raw
// (untrimmed) command output. It is the single source of truth for the
// platform-specific probe commands, shared between IsServiceActive (active.go)
// and Status (autostart.go). Callers do their own output parsing/trimming.
//
// For KindSystemdUser, KindSystemdSystem, and KindLaunchd the command runs via
// h.Run (POSIX). For KindWindowsTask it runs via h.RunShell (PowerShell).
// Returns ("", nil) for KindNone and any unknown kind.
//
// Note: this helper does NOT replicate the Status-side `| head -n 5`
// post-pipe on KindLaunchd output — that is a display-only concern and lives
// at the Status call site so IsServiceActive sees the full launchd print.
func runActiveCheck(h *host.Host, kind Kind, san, base string) (string, error) {
	switch kind {
	case KindSystemdUser:
		return h.Run(fmt.Sprintf(`systemctl --user is-active %s.service 2>/dev/null || echo inactive`, base))

	case KindSystemdSystem:
		return h.Run(sudoPrelude() + fmt.Sprintf(`
$SUDO systemctl is-active %s.service 2>/dev/null || echo inactive
`, base))

	case KindLaunchd:
		return h.Run(launchdPrintScript(hostshell.PosixSingleQuote(LaunchdLabel(san))))

	case KindWindowsTask:
		name := WindowsTaskName(san)
		return hostshell.ScheduledTaskState(h.RunShell, name)
	}
	return "", nil
}

// kindLabel returns the human-readable suffix used in Status detail rows
// ("installed (user): ...", "installed (system): ...", etc.). Returns the
// raw kind string for unknown kinds.
func kindLabel(kind Kind) string {
	switch kind {
	case KindSystemdUser:
		return "user"
	case KindSystemdSystem:
		return "system"
	case KindLaunchd:
		return "launchd"
	case KindWindowsTask:
		return "task"
	}
	return string(kind)
}
