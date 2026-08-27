package autostart

import (
	"fmt"
	"strings"

	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
)

// Kind describes how autostart is installed for an instance.
type Kind string

const (
	KindNone          Kind = ""
	KindSystemdUser   Kind = "systemd-user"
	KindSystemdSystem Kind = "systemd-system"
	KindLaunchd       Kind = "launchd"
	KindWindowsTask   Kind = "windows-task"
)

// ServiceBasename returns the systemd unit basename (without .service) for ghsr-runner-<sanitized>.
func ServiceBasename(instanceSanitized string) string {
	return "ghsr-runner-" + instanceSanitized
}

func remoteHome(h *host.Host) (string, error) {
	if h.OS == "windows" {
		out, err := h.RunShell(`Write-Output $env:USERPROFILE`)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	}
	out, err := h.Run(`printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func absRunnerDir(h *host.Host, home, instance string) string {
	home = strings.TrimRight(home, `/\`)
	if h.OS == "windows" {
		return home + `\` + `.gh-sr` + `\` + `runners` + `\` + instance
	}
	return home + "/.gh-sr/runners/" + instance
}

// writeRemoteBytes and powerShellSingleQuoted were moved to internal/hostshell.
// Call sites use hostshell.WriteRemoteBytes and hostshell.PowerShellSingleQuote directly.

// Detect reports whether an autostart unit is installed for this instance.
func Detect(h *host.Host, instance string) (Kind, error) {
	san, err := SanitizeInstance(instance)
	if err != nil {
		return KindNone, err
	}
	base := ServiceBasename(san)

	switch h.OS {
	case "linux":
		// Combine the two candidate unit paths into a single SSH call.
		// Mirrors the win-class of runner.removeContainer (PR #264) and
		// runner.EnsureHostDocker (PR #269): when a helper answers with
		// multiple correlated values from the same host state, the probes
		// belong in one shell invocation. Saves 1 SSH round-trip on every
		// autostart touch for Linux runners — material on the TUI per-tick
		// path (Manager.Status → statusNative → Detect runs once per
		// instance every 5 s) and on `gh sr up` / `gh sr down` per-instance
		// paths. The if/elif form is equivalent to the previous
		// "&& echo ... || true" pair: user unit wins (matches the previous
		// short-circuit on `out == "user"`), and an empty stdout means no
		// unit of either kind is installed.
		userPath := fmt.Sprintf(`"$HOME/.config/systemd/user/%s.service"`, base)
		sysPath := fmt.Sprintf(`/etc/systemd/system/%s.service`, base)
		script := fmt.Sprintf(
			`if [ -f %s ]; then echo user; elif [ -f %s ]; then echo system; fi`,
			userPath, sysPath,
		)
		out, err := h.Run(script)
		if err != nil {
			return KindNone, err
		}
		switch strings.TrimSpace(out) {
		case "user":
			return KindSystemdUser, nil
		case "system":
			return KindSystemdSystem, nil
		}
		return KindNone, nil

	case "darwin":
		label := LaunchdLabel(san)
		check := fmt.Sprintf(`test -f "$HOME/Library/LaunchAgents/%s" && echo yes || true`, label+".plist")
		out, err := h.Run(check)
		if err != nil {
			return KindNone, err
		}
		if strings.TrimSpace(out) == "yes" {
			return KindLaunchd, nil
		}
		return KindNone, nil

	case "windows":
		name := WindowsTaskName(san)
		exists, err := hostshell.ScheduledTaskExists(h.RunShell, name)
		if err != nil {
			return KindNone, err
		}
		if exists {
			return KindWindowsTask, nil
		}
		return KindNone, nil

	default:
		return KindNone, fmt.Errorf("unsupported host OS %q", h.OS)
	}
}

// InstallOpts configures Install.
type InstallOpts struct {
	// System (Linux only) install unit under /etc/systemd/system with User=/Group=.
	System bool
}

// Install writes platform autostart definitions and enables them.
func Install(h *host.Host, instance string, opts InstallOpts) error {
	san, err := SanitizeInstance(instance)
	if err != nil {
		return err
	}
	home, err := remoteHome(h)
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	runnerDir := absRunnerDir(h, home, instance)

	switch h.OS {
	case "linux":
		if opts.System {
			return installSystemdSystem(h, instance, san, home, runnerDir)
		}
		return installSystemdUser(h, instance, san, home, runnerDir)
	case "darwin":
		return installLaunchd(h, instance, san, runnerDir, home)
	case "windows":
		return installWindowsTask(h, instance, san, runnerDir)
	default:
		return fmt.Errorf("autostart install not supported on OS %q", h.OS)
	}
}

func installSystemdUser(h *host.Host, instance, san, home, absRunnerDir string) error {
	unit := SystemdUserUnit(san, absRunnerDir)
	base := ServiceBasename(san)
	fullUnitPath := home + "/.config/systemd/user/" + base + ".service"

	if err := hostshell.WriteRemoteBytes(h, fullUnitPath, []byte(unit)); err != nil {
		return fmt.Errorf("writing systemd user unit: %w", err)
	}

	cmd := systemdEnableUserScript(base)
	if _, err := h.Run(cmd); err != nil {
		return fmt.Errorf("enabling systemd user unit: %w", err)
	}
	_ = instance
	return nil
}

func installSystemdSystem(h *host.Host, instance, san, home, absRunnerDir string) error {
	// Combine `id -un` and `id -gn` into a single SSH call. Both probes
	// answer the same correlated question ("who am I, what group?") from the
	// same host state, so they belong in one shell invocation. Saves 1 SSH
	// round-trip on every `gh sr service install --system` call. Mirrors the
	// win-class of Detect (PR #264, PR #269, autostart.Detect Linux probe):
	// when a helper needs multiple values from the same host state, fetch
	// them together.
	whoOut, err := h.Run(`printf '%s\n' "$(id -un)" "$(id -gn)"`)
	if err != nil {
		return fmt.Errorf("id -un/-gn: %w", err)
	}
	whoLines := strings.Split(strings.TrimRight(whoOut, "\n"), "\n")
	if len(whoLines) != 2 {
		return fmt.Errorf("id -un/-gn: expected 2 lines, got %d", len(whoLines))
	}
	user, group := strings.TrimSpace(whoLines[0]), strings.TrimSpace(whoLines[1])
	unit := SystemdSystemUnit(san, absRunnerDir, user, group)
	base := ServiceBasename(san)
	sysPath := "/etc/systemd/system/" + base + ".service"

	tmpPath := home + "/.gh-sr/" + base + ".service.tmp"
	if err := hostshell.WriteRemoteBytes(h, tmpPath, []byte(unit)); err != nil {
		return fmt.Errorf("staging systemd system unit: %w", err)
	}

	script := systemdEnableSystemScript(base, tmpPath, sysPath)

	if _, err := h.Run(script); err != nil {
		return fmt.Errorf("installing system systemd unit: %w", err)
	}
	_ = instance
	return nil
}

func installLaunchd(h *host.Host, instance, san, absRunnerDir, home string) error {
	plist := LaunchdPlist(san, absRunnerDir)
	label := LaunchdLabel(san)
	plistName := label + ".plist"
	plistPath := home + "/Library/LaunchAgents/" + plistName

	if err := hostshell.WriteRemoteBytes(h, plistPath, []byte(plist)); err != nil {
		return fmt.Errorf("writing LaunchAgent plist: %w", err)
	}

	qplist := hostshell.PosixSingleQuote(plistPath)
	qlabel := hostshell.PosixSingleQuote(label)
	cmd := "mkdir -p \"$HOME/Library/LaunchAgents\"\n" +
		launchdActivateScript(qlabel, qplist, plistName, true)

	if _, err := h.Run(cmd); err != nil {
		return fmt.Errorf("loading LaunchAgent: %w", err)
	}
	_ = instance
	return nil
}

func installWindowsTask(h *host.Host, instance, san, absRunnerDir string) error {
	taskName := WindowsTaskName(san)
	// Use cmd.exe directly (no PowerShell profile) and S4U logon type so the
	// process runs in Session 0 (non-interactive, no visible console window).
	ps := fmt.Sprintf(`
$tn = %s
$rd = %s
Unregister-ScheduledTask -TaskName $tn -Confirm:$false -ErrorAction SilentlyContinue
$act = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument '/c run.cmd' -WorkingDirectory $rd
$tr = New-ScheduledTaskTrigger -AtLogOn
$st = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType S4U -RunLevel Limited
Register-ScheduledTask -TaskName $tn -Action $act -Trigger $tr -Settings $st -Principal $principal -Force | Out-Null
Start-ScheduledTask -TaskName $tn
`,
		hostshell.PowerShellSingleQuote(taskName),
		hostshell.PowerShellSingleQuote(absRunnerDir),
	)
	if _, err := h.RunShell(strings.TrimSpace(ps)); err != nil {
		return fmt.Errorf("registering scheduled task: %w", err)
	}
	_ = instance
	_ = san
	return nil
}

// resolveAutostartTarget bundles the repeated "detect + sanitize + base-name"
// preamble used by Uninstall, Start, Stop, and Status. The four callers each
// handle KindNone differently (Uninstall is a no-op, the others are errors,
// Status records the state), so the KindNone check stays at the call site.
// Returns the detected Kind, sanitized instance token, unit basename (without
// the ".service" suffix), and any error from Detect or SanitizeInstance.
func resolveAutostartTarget(h *host.Host, instance string) (Kind, string, string, error) {
	kind, err := Detect(h, instance)
	if err != nil {
		return KindNone, "", "", err
	}
	san, err := SanitizeInstance(instance)
	if err != nil {
		return KindNone, "", "", err
	}
	return kind, san, ServiceBasename(san), nil
}

// Uninstall removes autostart and stops the supervised process where applicable.
func Uninstall(h *host.Host, instance string) error {
	kind, san, base, err := resolveAutostartTarget(h, instance)
	if err != nil {
		return err
	}
	if kind == KindNone {
		return nil
	}

	switch kind {
	case KindSystemdUser:
		_, err := h.Run(systemdDisableUserScript(base))
		return err

	case KindSystemdSystem:
		_, err := h.Run(systemdDisableSystemScript(base))
		return err

	case KindLaunchd:
		label := LaunchdLabel(san)
		cmd := LaunchdBootoutScript(hostshell.PosixSingleQuote(label), label+".plist")
		_, err := h.Run(cmd)
		return err

	case KindWindowsTask:
		name := WindowsTaskName(san)
		ps := fmt.Sprintf(
			`Unregister-ScheduledTask -TaskName %s -Confirm:$false -ErrorAction SilentlyContinue`,
			hostshell.PowerShellSingleQuote(name),
		)
		_, err := h.RunShell(ps)
		return err

	default:
		return nil
	}
}

// Start launches the autostart-backed runner (systemd / launchd / scheduled task).
func Start(h *host.Host, instance string) error {
	kind, san, base, err := resolveAutostartTarget(h, instance)
	if err != nil {
		return err
	}
	if kind == KindNone {
		return fmt.Errorf("autostart is not installed for %s", instance)
	}

	switch kind {
	case KindSystemdUser:
		_, err := h.Run("systemctl --user start " + base + ".service")
		return err
	case KindSystemdSystem:
		script := sudoPrelude() + fmt.Sprintf(`
$SUDO systemctl start %s.service
`, base)
		_, err := h.Run(script)
		return err
	case KindLaunchd:
		label := LaunchdLabel(san)
		home, herr := remoteHome(h)
		if herr != nil {
			return herr
		}
		plistPath := home + "/Library/LaunchAgents/" + label + ".plist"
		cmd := launchdActivateScript(hostshell.PosixSingleQuote(label), hostshell.PosixSingleQuote(plistPath), label+".plist", false)
		_, err := h.Run(cmd)
		return err
	case KindWindowsTask:
		name := WindowsTaskName(san)
		ps := fmt.Sprintf(`Start-ScheduledTask -TaskName %s`, hostshell.PowerShellSingleQuote(name))
		_, err := h.RunShell(ps)
		return err
	default:
		return fmt.Errorf("unknown autostart kind %q", kind)
	}
}

// Stop stops the autostart-backed runner without removing the unit.
func Stop(h *host.Host, instance string) error {
	kind, san, base, err := resolveAutostartTarget(h, instance)
	if err != nil {
		return err
	}
	if kind == KindNone {
		return fmt.Errorf("autostart is not installed for %s", instance)
	}

	switch kind {
	case KindSystemdUser:
		_, err := h.Run("systemctl --user stop " + base + ".service")
		return err
	case KindSystemdSystem:
		script := sudoPrelude() + fmt.Sprintf(`
$SUDO systemctl stop %s.service
`, base)
		_, err := h.Run(script)
		return err
	case KindLaunchd:
		label := LaunchdLabel(san)
		cmd := fmt.Sprintf(`UID=$(id -u); LABEL=%s; for _DOMAIN in %s; do launchctl bootout "$_DOMAIN/$LABEL" 2>/dev/null || true; done`,
			hostshell.PosixSingleQuote(label), launchdDomainList())
		_, err := h.Run(cmd)
		return err
	case KindWindowsTask:
		name := WindowsTaskName(san)
		ps := fmt.Sprintf(`Stop-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue`, hostshell.PowerShellSingleQuote(name))
		_, err := h.RunShell(ps)
		return err
	default:
		return fmt.Errorf("unknown autostart kind %q", kind)
	}
}

// StatusRow is one line of `gh sr service status` output.
type StatusRow struct {
	Instance string
	Host     string
	Mode     string // native / docker
	Kind     Kind
	Detail   string // active/inactive, etc.
}

// Status describes autostart and service state for a runner instance (native only).
func Status(h *host.Host, hostName, instance, mode string) (StatusRow, error) {
	row := StatusRow{Instance: instance, Host: hostName, Mode: mode}
	if mode != "native" {
		row.Kind = KindNone
		row.Detail = "docker: containers use --restart on-failure with a bootstrap retry cap; gh sr down stops the container so it will not auto-start on boot until gh sr up"
		return row, nil
	}

	kind, san, base, err := resolveAutostartTarget(h, instance)
	if err != nil {
		return row, err
	}
	row.Kind = kind
	if kind == KindNone {
		row.Detail = "autostart not installed"
		return row, nil
	}

	out, err := runActiveCheck(h, kind, san, base)
	if err != nil {
		row.Detail = "installed (" + kindLabel(kind) + "): check failed: " + err.Error()
		return row, nil
	}

	switch kind {
	case KindSystemdUser:
		row.Detail = "installed (user): " + strings.TrimSpace(out)
		return row, nil

	case KindSystemdSystem:
		row.Detail = "installed (system): " + strings.TrimSpace(out)
		return row, nil

	case KindLaunchd:
		// Preserve the original `| head -n 5` post-pipe behavior: cap to
		// first 5 lines of launchd output before flattening newlines to
		// spaces (runActiveCheck returns the full launchd print).
		lines := strings.Split(out, "\n")
		if len(lines) > 5 {
			lines = lines[:5]
		}
		row.Detail = "installed (launchd): " + strings.TrimSpace(strings.Join(lines, " "))
		return row, nil

	case KindWindowsTask:
		row.Detail = "installed (task): " + strings.TrimSpace(out)
		return row, nil
	}

	return row, nil
}
