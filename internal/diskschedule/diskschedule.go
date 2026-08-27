package diskschedule

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/hostshell"
)

const (
	serviceBase = "ghsr-disk-prune"
	labelBase   = "com.an-lee.gh-sr.disk-prune"
	// DefaultAtTime is the local daily schedule when AtTime is empty.
	DefaultAtTime = "03:00"
)

var (
	runtimeGOOS        = runtime.GOOS
	execLookPath       = exec.LookPath
	execCombinedOutput = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
	execRun = func(name string, args ...string) error {
		return exec.Command(name, args...).Run()
	}
	execRunInDir = func(dir, name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		return cmd.Run()
	}
	powerShellExec           = hostshell.PowerShellExec
	powerShellCombinedOutput = hostshell.PowerShellCombinedOutput
)

// localPSRunner adapts a ([]byte, error) PowerShell executor to the
// hostshell.PowerShellRunner (string, error) signature so the hostshell
// Windows helpers can be used for local execution.
func localPSRunner(exec func(string) ([]byte, error)) hostshell.PowerShellRunner {
	return func(script string) (string, error) {
		out, err := exec(script)
		return string(out), err
	}
}

// ScheduleKind describes how disk prune scheduling is installed locally.
type ScheduleKind string

const (
	KindNone        ScheduleKind = ""
	KindSystemdUser ScheduleKind = "systemd-user-timer"
	KindLaunchd     ScheduleKind = "launchd"
	KindWindowsTask ScheduleKind = "windows-task"
)

// InstallOpts configures local schedule installation.
type InstallOpts struct {
	ConfigPath string
	GhPath     string
	// AtTime is local daily time HH:MM (default 03:00).
	AtTime string
}

// Detect reports whether a disk prune schedule is installed on this machine.
func Detect() (ScheduleKind, error) {
	switch runtimeGOOS {
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return KindNone, err
		}
		timerPath := filepath.Join(home, ".config", "systemd", "user", serviceBase+".timer")
		if _, err := os.Stat(timerPath); err == nil {
			return KindSystemdUser, nil
		}
		return KindNone, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return KindNone, err
		}
		plistPath := filepath.Join(home, "Library", "LaunchAgents", labelBase+".plist")
		if _, err := os.Stat(plistPath); err == nil {
			return KindLaunchd, nil
		}
		return KindNone, nil
	case "windows":
		exists, err := hostshell.ScheduledTaskExists(localPSRunner(powerShellExec), serviceBase)
		if err != nil {
			return KindNone, err
		}
		if exists {
			return KindWindowsTask, nil
		}
		return KindNone, nil
	default:
		return KindNone, fmt.Errorf("disk schedule not supported on GOOS %q", runtimeGOOS)
	}
}

// Install writes and enables a local daily disk prune schedule.
func Install(opts InstallOpts) error {
	if opts.ConfigPath == "" {
		return fmt.Errorf("config path is required")
	}
	if opts.GhPath == "" {
		gh, err := execLookPath("gh")
		if err != nil {
			return fmt.Errorf("gh not found on PATH: %w", err)
		}
		opts.GhPath = gh
	}
	if opts.AtTime == "" {
		opts.AtTime = DefaultAtTime
	}
	hour, minute, err := parseAtTime(opts.AtTime)
	if err != nil {
		return err
	}

	switch runtimeGOOS {
	case "linux":
		return installSystemdUser(opts, hour, minute)
	case "darwin":
		return installLaunchd(opts, hour, minute)
	case "windows":
		return installWindowsTask(opts, hour, minute)
	default:
		return fmt.Errorf("disk schedule install not supported on GOOS %q", runtimeGOOS)
	}
}

// Uninstall removes the local disk prune schedule.
func Uninstall() error {
	switch runtimeGOOS {
	case "linux":
		return uninstallSystemdUser()
	case "darwin":
		return uninstallLaunchd()
	case "windows":
		return uninstallWindowsTask()
	default:
		return fmt.Errorf("disk schedule uninstall not supported on GOOS %q", runtimeGOOS)
	}
}

// Status returns a human-readable schedule status string.
func Status() (ScheduleKind, string, error) {
	kind, err := Detect()
	if err != nil {
		return KindNone, "", err
	}
	if kind == KindNone {
		return KindNone, "not installed", nil
	}
	switch kind {
	case KindSystemdUser:
		out, err := execCombinedOutput("systemctl", "--user", "is-enabled", serviceBase+".timer")
		detail := strings.TrimSpace(string(out))
		if err != nil {
			detail += " (check failed: " + err.Error() + ")"
		}
		return kind, "installed (systemd user timer): " + detail, nil
	case KindLaunchd:
		return kind, "installed (launchd): " + labelBase + ".plist", nil
	case KindWindowsTask:
		state, err := hostshell.ScheduledTaskState(localPSRunner(powerShellExec), serviceBase)
		if err != nil {
			return kind, "installed (task): error " + err.Error(), nil
		}
		return kind, "installed (task): " + state, nil
	default:
		return kind, string(kind), nil
	}
}

func parseAtTime(at string) (hour, minute int, err error) {
	at = strings.TrimSpace(at)
	hourStr, minuteStr, ok := strings.Cut(at, ":")
	if !ok {
		return 0, 0, fmt.Errorf("invalid time %q (expected HH:MM)", at)
	}
	if hour, err = strconv.Atoi(hourStr); err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q", at)
	}
	if minute, err = strconv.Atoi(minuteStr); err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", at)
	}
	return hour, minute, nil
}

func installSystemdUser(opts InstallOpts, hour, minute int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	execStart := fmt.Sprintf("%s sr disk prune --yes -c %s", systemdQuoteArg(opts.GhPath), systemdQuoteArg(opts.ConfigPath))
	envFile := filepath.Join(home, ".gh-sr", "env")
	service := fmt.Sprintf(`[Unit]
Description=gh-sr disk prune

[Service]
Type=oneshot
EnvironmentFile=-%s
ExecStart=%s
`, systemdQuoteArg(envFile), execStart)

	timer := fmt.Sprintf(`[Unit]
Description=Daily gh-sr disk prune

[Timer]
OnCalendar=*-*-* %02d:%02d:00
Persistent=true

[Install]
WantedBy=timers.target
`, hour, minute)

	if err := os.WriteFile(filepath.Join(dir, serviceBase+".service"), []byte(service), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, serviceBase+".timer"), []byte(timer), 0o600); err != nil {
		return err
	}

	cmds := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", serviceBase + ".timer"},
		{"systemctl", "--user", "start", serviceBase + ".timer"},
	}
	for _, c := range cmds {
		if out, err := execCombinedOutput(c[0], c[1:]...); err != nil {
			return fmt.Errorf("%s: %w (%s)", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func uninstallSystemdUser() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	_ = execRun("systemctl", "--user", "disable", "--now", serviceBase+".timer")
	_ = os.Remove(filepath.Join(dir, serviceBase+".timer"))
	_ = os.Remove(filepath.Join(dir, serviceBase+".service"))
	_ = execRun("systemctl", "--user", "daemon-reload")
	return nil
}

func installLaunchd(opts InstallOpts, hour, minute int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>sr</string>
    <string>disk</string>
    <string>prune</string>
    <string>--yes</string>
    <string>-c</string>
    <string>%s</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key><integer>%d</integer>
    <key>Minute</key><integer>%d</integer>
  </dict>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, hostshell.PlistEscape(labelBase), hostshell.PlistEscape(opts.GhPath), hostshell.PlistEscape(opts.ConfigPath), hour, minute,
		hostshell.PlistEscape(filepath.Join(home, ".gh-sr", "disk-prune.log")),
		hostshell.PlistEscape(filepath.Join(home, ".gh-sr", "disk-prune.err.log")))

	plistPath := filepath.Join(dir, labelBase+".plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		return err
	}
	uid := os.Getuid()
	out, err := execCombinedOutput("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), plistPath)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallLaunchd() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plistFile := labelBase + ".plist"
	script := autostart.LaunchdBootoutScript(hostshell.PosixSingleQuote(labelBase), plistFile)
	_ = execRunInDir(home, "sh", "-c", script)
	return nil
}

func installWindowsTask(opts InstallOpts, hour, minute int) error {
	ps := fmt.Sprintf(`
$tn = %s
$gh = %s
$cfg = %s
Unregister-ScheduledTask -TaskName $tn -Confirm:$false -ErrorAction SilentlyContinue
$act = New-ScheduledTaskAction -Execute $gh -Argument ('sr disk prune --yes -c ' + [char]34 + $cfg + [char]34)
$tr = New-ScheduledTaskTrigger -Daily -At (Get-Date '%02d:%02d').TimeOfDay
$st = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName $tn -Action $act -Trigger $tr -Settings $st -Force | Out-Null
`, hostshell.PowerShellSingleQuote(serviceBase), hostshell.PowerShellSingleQuote(opts.GhPath), hostshell.PowerShellSingleQuote(opts.ConfigPath), hour, minute)
	out, err := powerShellCombinedOutput(ps)
	if err != nil {
		return fmt.Errorf("registering scheduled task: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallWindowsTask() error {
	ps := fmt.Sprintf(`Unregister-ScheduledTask -TaskName %s -Confirm:$false -ErrorAction SilentlyContinue`, hostshell.PowerShellSingleQuote(serviceBase))
	_, err := powerShellCombinedOutput(ps)
	return err
}

// systemdQuoteArg escapes a single argument for systemd unit ExecStart lines.
func systemdQuoteArg(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
