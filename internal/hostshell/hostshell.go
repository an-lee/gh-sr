// Package hostshell provides shell-quoting and remote-write helpers shared by
// the runner and autostart packages. It centralises the byte-for-byte identical
// helpers that used to live in both packages so they cannot drift apart.
package hostshell

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell/ps"
)

// PosixSingleQuote wraps s in single quotes for POSIX shell (safe for remote sh -c).
// Single quotes inside s are escaped using the standard POSIX idiom: ' -> '\”.
func PosixSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// PowerShellSingleQuote wraps s in single quotes for PowerShell (safe for remote powershell -c).
// Single quotes inside s are doubled, which is the PowerShell escape rule.
func PowerShellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// PowerShellExec runs script locally via powershell.exe and returns stdout.
func PowerShellExec(script string) ([]byte, error) {
	return ps.Exec(script)
}

// PowerShellCombinedOutput runs script locally via powershell.exe.
func PowerShellCombinedOutput(script string) ([]byte, error) {
	return ps.CombinedOutput(script)
}

// PowerShellCommand builds a full powershell.exe command string for host.Host.Run.
func PowerShellCommand(script string) string {
	return ps.CommandLine(script)
}

// PlistEscape escapes s for embedding in XML/plist string values.
func PlistEscape(s string) string {
	s = strings.ReplaceAll(s, `&`, `&amp;`)
	s = strings.ReplaceAll(s, `"`, `&quot;`)
	s = strings.ReplaceAll(s, `<`, `&lt;`)
	s = strings.ReplaceAll(s, `>`, `&gt;`)
	return s
}

// RemotePathExists reports whether path exists on the remote host (file or
// directory). It is the canonical helper for the
// `test -e <path> && echo yes || echo no` POSIX probe, replacing the hand-built
// copies that had drifted across the runner and autostart packages (issue #274).
// path is shell-quoted via PosixSingleQuote, so callers pass the raw path.
//
// This is a POSIX-only helper; Windows callers build their own PowerShell
// `Test-Path` probes (the dir/run.cmd/.runner triple) because there is no
// single PowerShell idiom that composes the same way as `test -e`. Callers
// gate on h.OS themselves before reaching for it.
func RemotePathExists(h *host.Host, path string) (bool, error) {
	return RemoteBoolCheck(h, "test -e "+PosixSingleQuote(path))
}

// RemoteFileExists is RemotePathExists specialized to a regular file
// (`test -f`). Use it instead of RemotePathExists when a directory at the same
// path must read as "absent" (e.g. a .runner file vs. a directory).
func RemoteFileExists(h *host.Host, path string) (bool, error) {
	return RemoteBoolCheck(h, "test -f "+PosixSingleQuote(path))
}

// RemoteDirExists is RemotePathExists specialized to a directory
// (`test -d`).
func RemoteDirExists(h *host.Host, path string) (bool, error) {
	return RemoteBoolCheck(h, "test -d "+PosixSingleQuote(path))
}

// RemoteBoolCheck runs a POSIX shell condition (condCmd) — any command whose
// exit status encodes the boolean — wrapped as `condCmd && echo yes || echo no`
// and returns whether it printed "yes". It is the generic yes/no probe shared
// by RemotePathExists / RemoteFileExists / RemoteDirExists and by call sites
// whose condition is not a bare test (e.g.
// `docker --version 2>/dev/null | grep -q "Docker version"`). Callers are
// responsible for shell-quoting any embedded values; the path helpers above
// already quote via PosixSingleQuote.
//
// A non-nil error from h.Run is returned alongside false. Many existing call
// sites treat the error as non-fatal and treat any failure as "does not
// exist" — they can drop the error with `ok, _ := RemoteBoolCheck(...)`.
func RemoteBoolCheck(h *host.Host, condCmd string) (bool, error) {
	out, err := h.Run(condCmd + " && echo yes || echo no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// PowerShellRunner is the function signature shared by h.RunShell (remote)
// and local PowerShell exec adapters. It runs a PowerShell script and returns
// the string output. Defined so the Windows helpers below work for both local
// (diskschedule) and remote (autostart, runner) callers.
type PowerShellRunner func(script string) (string, error)

// PowerShellBoolCheck is the Windows/PowerShell counterpart of RemoteBoolCheck.
// It wraps condPS in the canonical `if (condPS) { 'yes' } else { 'no' }`
// idiom, runs it via run, and returns whether the output was "yes".
// condPS must be a complete PowerShell condition expression (e.g.
// `Get-ScheduledTask -TaskName 'foo' -ErrorAction SilentlyContinue`).
func PowerShellBoolCheck(run PowerShellRunner, condPS string) (bool, error) {
	out, err := run(fmt.Sprintf(`if (%s) { 'yes' } else { 'no' }`, condPS))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// ScheduledTaskExists reports whether a Windows scheduled task with the given
// name exists on the target host. The name is escaped via
// PowerShellSingleQuote. Works for both local and remote callers — pass
// hostshell.PowerShellExec adapted to PowerShellRunner for local, or
// h.RunShell for remote.
func ScheduledTaskExists(run PowerShellRunner, name string) (bool, error) {
	cond := fmt.Sprintf(`Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue`, PowerShellSingleQuote(name))
	return PowerShellBoolCheck(run, cond)
}

// ScheduledTaskState returns the raw State property (e.g. "Ready", "Running")
// of the named Windows scheduled task. Returns an error if the task does not
// exist or the query fails. The name is escaped via PowerShellSingleQuote.
func ScheduledTaskState(run PowerShellRunner, name string) (string, error) {
	out, err := run(fmt.Sprintf(
		`(Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue | Select-Object -ExpandProperty State)`,
		PowerShellSingleQuote(name),
	))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// RemoteWindowsDirExists reports whether a directory exists at path on a
// Windows host. It is the Windows counterpart of RemoteDirExists, using
// `Test-Path -LiteralPath ... -PathType Container`. The path is escaped via
// PowerShellSingleQuote.
func RemoteWindowsDirExists(h *host.Host, path string) (bool, error) {
	out, err := h.RunShell(fmt.Sprintf(
		`if (Test-Path -LiteralPath %s -PathType Container) { 'yes' } else { 'no' }`,
		PowerShellSingleQuote(path),
	))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// WriteRemoteBytes writes data to a remote path using base64 (POSIX) or PowerShell (Windows).
// Parent directories are created on the host.
func WriteRemoteBytes(h *host.Host, remotePath string, data []byte) error {
	if h.OS == "windows" {
		b64 := base64.StdEncoding.EncodeToString(data)
		ps := fmt.Sprintf(
			"$p = %s; $d = [Convert]::FromBase64String(%s); $dir = Split-Path -Parent $p; New-Item -ItemType Directory -Force -Path $dir | Out-Null; [IO.File]::WriteAllBytes($p, $d)",
			PowerShellSingleQuote(remotePath),
			PowerShellSingleQuote(b64),
		)
		_, err := h.RunShell(ps)
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	qpath := PosixSingleQuote(remotePath)
	cmd := fmt.Sprintf(`set -e; d=$(dirname %s); mkdir -p "$d"; printf '%%s' %s | base64 -d > %s`,
		qpath, PosixSingleQuote(b64), qpath)
	_, err := h.Run(cmd)
	return err
}

// LinuxElevatePrelude returns a shell fragment for non-interactive SSH sessions
// that sets $SUDO to ” when already root, or to "sudo -n" when passwordless
// sudo works. If neither is true, it prints failureMsg to stderr and exits 1.
// Plain sudo is unsafe here because SSH Run() has no TTY.
//
// failureMsg is package-specific so each caller can keep its own user-facing
// wording (e.g. runner mentions `gh sr doctor`; autostart mentions autostart).
func LinuxElevatePrelude(failureMsg string) string {
	return `
SUDO=''
if [ "$(id -u)" -ne 0 ]; then
	if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
		SUDO='sudo -n'
	else
		echo ` + PosixSingleQuote(failureMsg) + ` >&2
		exit 1
	fi
fi
`
}

// LinuxElevatePreludeSoft is the soft-failure sibling of LinuxElevatePrelude:
// it sets $SUDO to "sudo -n" when passwordless sudo works, or leaves it empty
// otherwise. It never prints or exits — callers are expected to gate usage of
// "$SUDO" with `if [ -n "$SUDO" ] || [ "$(id -u)" -eq 0 ]` (or similar) so the
// surrounding script can keep going and report the per-command failure rather
// than aborting the entire pipeline.
//
// Use this in scripts that run several elevated commands sequentially and need
// to surface each one's failure individually (e.g. disk prune, dir removal).
// For commands that have a single natural failure mode, prefer the strict
// LinuxElevatePrelude so the user gets a clear, early error.
func LinuxElevatePreludeSoft() string {
	return `
SUDO=''
if [ "$(id -u)" -ne 0 ]; then
	if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
		SUDO='sudo -n'
	fi
fi
`
}
