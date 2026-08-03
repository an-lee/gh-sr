package runner

import (
	"fmt"
	"strings"
	"time"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
)

// NativeRunnerConfigPresent reports whether the remote instance directory contains
// a fully-configured runner: the directory exists, run.sh/run.cmd is present, and .runner exists.
// This prevents EnsureSetup from skipping setup when only the .runner file exists but
// the runner binaries (run.sh) or directory are missing.
func NativeRunnerConfigPresent(h *host.Host, instanceName string) (bool, error) {
	dir := h.RunnerDir(instanceName)
	if h.OS == "windows" {
		out, err := h.RunShell(fmt.Sprintf(
			"%s; if ((Test-Path $runnerDir -PathType Container) -and (Test-Path (Join-Path $runnerDir 'run.cmd')) -and (Test-Path (Join-Path $runnerDir '.runner'))) { Write-Output 'yes' } else { Write-Output 'no' }",
			windowsRunnerDirAssignment(h, instanceName),
		))
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(out) == "yes", nil
	}
	// dir is "$HOME/.gh-sr/runners/<instance>": the $HOME prefix is a literal
	// shell variable the remote sh expands. Do NOT PosixSingleQuote it (that
	// would freeze "$HOME" as a literal and break the probe); the instance-name
	// segment is already sanitized by SanitizeInstance, so passing it raw
	// matches the long-standing convention (see resolveStateDirOrFallback).
	return hostshell.RemoteBoolCheck(h, "test -d "+dir+" && test -f "+dir+"/run.sh && test -f "+dir+"/.runner")
}

// svcShPresent reports whether svc.sh is deployed in the runner directory.
func svcShPresent(h *host.Host, instanceName string) bool {
	dir := h.RunnerDir(instanceName)
	if h.OS == "windows" {
		return false
	}
	// dir carries a literal $HOME (see NativeRunnerConfigPresent); pass it
	// raw so the shell expands $HOME, matching the original inline probe.
	ok, _ := hostshell.RemoteBoolCheck(h, "test -f "+dir+"/svc.sh")
	return ok
}

// linuxSvcAndAutostartProbe runs a single SSH round-trip on Linux that returns
// whether svc.sh is deployed in the runner directory and the autostart kind
// for the instance. It replaces the previous pattern of calling
// `svcShPresent(h, name)` followed by `autostart.Detect(h, name)` (two separate
// SSH round-trips per instance) on the Start / Stop / removeNativeServices
// hot paths. Same win-class as orphanLinuxPlanProbe (PR #358), the systemd
// probe consolidation in autostart.Detect (PR #285), and the per-instance
// container presence one-shot (PR #350).
//
// Markers emitted by the shell on separate lines:
//
//	S - $DIR/svc.sh is present
//	U - user-level systemd unit is installed ($HOME/.config/systemd/user/...service)
//	Y - system-level systemd unit is installed (/etc/systemd/system/...service)
//
// The markers are independent: an instance with both svc.sh and a user unit
// emits both S and U. The Go-side parser maps the markers back to the
// (bool, Kind) tuple the call sites expect.
//
// On Windows and macOS, where svc.sh is never used, the helper delegates to
// autostart.Detect only (one SSH round-trip, same as before).
func linuxSvcAndAutostartProbe(h *host.Host, instanceName string) (svcSh bool, kind autostart.Kind, err error) {
	if h.OS != "linux" {
		k, derr := autostart.Detect(h, instanceName)
		return false, k, derr
	}
	result, err := linuxInstanceProbe(h, instanceName, false)
	return result.svcSh, result.kind, err
}

// posixSingleQuote and writeRemoteBytes were moved to internal/hostshell.
// Call sites use hostshell.PosixSingleQuote and hostshell.WriteRemoteBytes directly.

func runnerTarballURL(version, osName, arch string) string {
	switch osName {
	case "darwin":
		return fmt.Sprintf(
			"https://github.com/actions/runner/releases/download/v%s/actions-runner-osx-%s-%s.tar.gz",
			version, arch, version,
		)
	case "linux":
		return fmt.Sprintf(
			"https://github.com/actions/runner/releases/download/v%s/actions-runner-linux-%s-%s.tar.gz",
			version, arch, version,
		)
	case "windows":
		a := arch
		if a == "amd64" {
			a = "x64"
		}
		return fmt.Sprintf(
			"https://github.com/actions/runner/releases/download/v%s/actions-runner-win-%s-%s.zip",
			version, a, version,
		)
	}
	return ""
}

func archForGitHub(arch string) string {
	switch arch {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	}
	return arch
}

// powerShellSingleQuoted was moved to internal/hostshell.
// Call sites use hostshell.PowerShellSingleQuote directly.

// windowsRunnerDirAssignment returns the PowerShell snippet that assigns
// $runnerDir to the per-instance runner directory on Windows.
// The variable name is fixed: every Windows script in this package expects
// $runnerDir, so the helper has no second-variable form.
func windowsRunnerDirAssignment(h *host.Host, instanceName string) string {
	return "$runnerDir = " + h.RunnerDirPS(instanceName)
}

func windowsNativeInstallScript(h *host.Host, instanceName, version, url string) string {
	zipName := fmt.Sprintf("actions-runner-%s.zip", version)
	return strings.Join([]string{
		windowsRunnerDirAssignment(h, instanceName),
		"New-Item -ItemType Directory -Force -Path $runnerDir | Out-Null",
		fmt.Sprintf("$zip = Join-Path %s %s", h.TempDirPS(), hostshell.PowerShellSingleQuote(zipName)),
		"[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12",
		fmt.Sprintf("if (-not (Test-Path $zip)) { Invoke-WebRequest -Uri %s -OutFile $zip }", hostshell.PowerShellSingleQuote(url)),
		fmt.Sprintf("try { Expand-Archive -Path $zip -DestinationPath $runnerDir -Force } catch { Remove-Item -Force $zip -EA SilentlyContinue; Invoke-WebRequest -Uri %s -OutFile $zip; Expand-Archive -Path $zip -DestinationPath $runnerDir -Force }", hostshell.PowerShellSingleQuote(url)),
	}, "; ")
}

func windowsNativeConfigScript(h *host.Host, rc config.RunnerConfig, instanceName, regToken string, instanceIndex int) string {
	labels := strings.Join(rc.EffectiveLabelsForInstance(h.OS, h.Arch, instanceIndex), ",")
	cmd := fmt.Sprintf(
		"& .\\config.cmd --unattended --url %s --token %s --name %s --labels %s --work '_work' --replace",
		hostshell.PowerShellSingleQuote(rc.GitHubRegistrationURL()),
		hostshell.PowerShellSingleQuote(regToken),
		hostshell.PowerShellSingleQuote(instanceName),
		hostshell.PowerShellSingleQuote(labels),
	)
	if rc.Group != "" {
		cmd += fmt.Sprintf(" --runnergroup %s", hostshell.PowerShellSingleQuote(rc.Group))
	}
	if rc.Ephemeral {
		cmd += " --ephemeral"
	}
	return strings.Join([]string{
		windowsRunnerDirAssignment(h, instanceName),
		"Set-Location -Path $runnerDir",
		cmd,
	}, "; ")
}

// nativeShellConfigScript is the POSIX (sh/bash) sibling of
// windowsNativeConfigScript. It returns a single-line `cd <dir> && ./config.sh ...`
// command string ready for h.Run. The flag set mirrors windowsNativeConfigScript
// (--unattended --url --token --name --labels --work '_work' --replace, plus
// --runnergroup when set and --ephemeral when ephemeral). The only cross-OS
// differences are quoting (`'%s'` vs hostshell.PowerShellSingleQuote) and the
// executable prefix (`./config.sh` vs `.\\config.cmd`).
//
// See issue #313 — this is option 3 (mirror the Windows helper) from the
// detector's recommendations. Option 1 (extract registrationFlags(rc) and
// share the flag list) is a follow-up once this helper exists.
func nativeShellConfigScript(h *host.Host, rc config.RunnerConfig, instanceName, regToken string, instanceIndex int) string {
	labels := strings.Join(rc.EffectiveLabelsForInstance(h.OS, h.Arch, instanceIndex), ",")
	dir := h.RunnerDir(instanceName)
	cmd := fmt.Sprintf(
		"cd %s && ./config.sh --unattended --url '%s' --token '%s' --name '%s' --labels '%s' --work '_work' --replace",
		dir, rc.GitHubRegistrationURL(), regToken, instanceName, labels,
	)
	if rc.Group != "" {
		cmd += fmt.Sprintf(" --runnergroup '%s'", rc.Group)
	}
	if rc.Ephemeral {
		cmd += " --ephemeral"
	}
	return cmd
}

// staleRegistrationMsg is the substring the Actions runner writes to its log when the
// server-side registration has been deleted (auto-pruned after inactivity).
const staleRegistrationMsg = "runner registration has been deleted from the server"

// staleRegistrationWarmup is the time the post-start probe waits for the runner
// process to either stay alive (healthy) or exit, before it greps runner.log for the
// stale-registration message. Centralised so a future tuning change is one line, not
// a search-and-replace across the Windows and POSIX probe snippets.
const staleRegistrationWarmup = 5 * time.Second

// staleRegistrationScript returns a single-line shell snippet (PowerShell on Windows,
// POSIX sh on Linux/macOS) that waits staleRegistrationWarmup, then reports whether
// the runner's registration has been auto-pruned on the GitHub side. The snippet
// writes "stale" to stdout if the runner process exited AND runner.log contains
// staleRegistrationMsg, otherwise "ok". Callers compare stdout to "stale" to decide
// whether to re-configure.
//
// Co-locating both OS shapes in one helper means a future change to the detection
// rule (different sleep, different log-line pattern, different "alive" check) is one
// edit instead of a two-edit hazard across OSes.
func staleRegistrationScript(h *host.Host, instanceName string) string {
	if h.OS == "windows" {
		return fmt.Sprintf(
			"%s; $pidFile = Join-Path $runnerDir '.runner_pid'; "+
				"$logFile = Join-Path $runnerDir 'runner.log'; "+
				"Start-Sleep -Seconds %d; "+
				"$pid = Get-Content $pidFile -EA SilentlyContinue; "+
				"$alive = $false; if ($pid) { try { Get-Process -Id $pid -EA Stop | Out-Null; $alive = $true } catch {} }; "+
				"if ($alive) { Write-Host 'ok' } "+
				"elseif ((Test-Path $logFile) -and (Select-String -Path $logFile -Pattern %s -Quiet)) { Write-Host 'stale' } "+
				"else { Write-Host 'ok' }",
			windowsRunnerDirAssignment(h, instanceName),
			int(staleRegistrationWarmup.Seconds()),
			hostshell.PowerShellSingleQuote(staleRegistrationMsg),
		)
	}
	dir := h.RunnerDir(instanceName)
	return fmt.Sprintf(
		`sleep %d && cd %s && pid=$(cat .runner_pid 2>/dev/null) && `+
			`if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then echo ok; `+
			`elif grep -q %q runner.log 2>/dev/null; then echo stale; `+
			`else echo ok; fi`,
		int(staleRegistrationWarmup.Seconds()),
		dir, staleRegistrationMsg,
	)
}

func windowsNativeStartScript(h *host.Host, instanceName string) string {
	// Win32-OpenSSH tears down the session job on disconnect, killing Start-Process children.
	// Win32_Process.Create starts outside that job so the listener survives after gh sr closes SSH.
	// ShowWindow=0 sets STARTF_USESHOWWINDOW + wShowWindow=SW_HIDE so no visible conhost window
	// pops up. CREATE_NO_WINDOW (0x08000000) cannot be set here: WMI's Win32_Process.Create
	// validates CreateFlags against a small allow-list and rejects 0x08000000 with
	// ERROR_INVALID_PARAMETER (21), which fails the whole start with no fallback.
	parts := []string{
		windowsRunnerDirAssignment(h, instanceName) + "; ",
		`$pidFile = Join-Path $runnerDir '.runner_pid'; `,
		`$logFile = Join-Path $runnerDir 'runner.log'; `,
		`if (Test-Path $pidFile) { $existingPid = Get-Content $pidFile; try { Get-Process -Id $existingPid -EA Stop | Out-Null; Write-Host 'already running'; exit 0 } catch {} }; `,
		`$cmdArg = 'cd /d "' + $runnerDir + '" && run.cmd > "' + $logFile + '" 2>&1'; `,
		`$fullLine = 'cmd.exe /c ' + $cmdArg; `,
		`$si = New-CimInstance -CimClass (Get-CimClass -ClassName Win32_ProcessStartup) -Property @{ShowWindow=0} -ClientOnly; `,
		`$cim = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $fullLine; CurrentDirectory = $runnerDir; ProcessStartupInformation = $si }; `,
		`if ($cim.ReturnValue -ne 0) { Write-Host ('Win32_Process.Create failed: ' + $cim.ReturnValue); exit 1 }; `,
		`$cim.ProcessId | Out-File -FilePath $pidFile -NoNewline; `,
		`Write-Host ('started PID ' + $cim.ProcessId)`,
	}
	return strings.Join(parts, "")
}

// windowsDeleteRunnerConfig removes the .runner file so setupNative will re-configure.
func windowsDeleteRunnerConfig(h *host.Host, instanceName string) string {
	return fmt.Sprintf(
		"%s; Remove-Item -Force (Join-Path $runnerDir '.runner') -EA SilentlyContinue; "+
			"Remove-Item -Force (Join-Path $runnerDir '.credentials') -EA SilentlyContinue; "+
			"Remove-Item -Force (Join-Path $runnerDir '.credentials_rsaparams') -EA SilentlyContinue",
		windowsRunnerDirAssignment(h, instanceName),
	)
}

func (m *Manager) setupNative(h *host.Host, rc config.RunnerConfig) error {
	version, err := m.GitHub.GetLatestRunnerVersion()
	if err != nil {
		return err
	}

	ghArch := archForGitHub(h.Arch)
	url := runnerTarballURL(version, h.OS, ghArch)
	if url == "" {
		return fmt.Errorf("unsupported OS/arch: %s/%s", h.OS, h.Arch)
	}

	for i, name := range rc.InstanceNames() {
		dir := h.RunnerDir(name)

		installed, _ := NativeRunnerConfigPresent(h, name)
		if installed {
			fmt.Fprintf(m.out(), "  %s: already installed, skipping\n", name)
			continue
		}

		fmt.Fprintf(m.out(), "  %s: installing runner v%s...\n", name, version)

		if h.OS == "linux" {
			installDepsCmd := sudoPrelude() + `
				if ! command -v curl >/dev/null 2>&1 || ! command -v tar >/dev/null 2>&1; then
					if command -v apt-get >/dev/null 2>&1; then $SUDO apt-get update && $SUDO apt-get install -y curl tar;
					elif command -v yum >/dev/null 2>&1; then $SUDO yum install -y curl tar;
					elif command -v apk >/dev/null 2>&1; then $SUDO apk add curl tar;
					fi
				fi
			`
			if _, err := h.Run(installDepsCmd); err != nil {
				fmt.Fprintf(m.out(), "  %s: warning: failed to ensure curl/tar are installed: %v\n", name, err)
			}
		}

		if h.OS == "windows" {
			fmt.Fprintf(m.out(), "  %s: downloading runner package...\n", name)
			if _, err := h.RunShell(windowsNativeInstallScript(h, name, version, url)); err != nil {
				return fmt.Errorf("installing runner on Windows: %w", err)
			}
		} else {
			tarball := fmt.Sprintf("%s/ghsr-runner-%s-%s.tar.gz", h.TempDir(), name, version)
			fmt.Fprintf(m.out(), "  %s: downloading runner v%s...\n", name, version)
			if _, err := h.Run(fmt.Sprintf("mkdir -p %s", dir)); err != nil {
				return fmt.Errorf("creating runner directory: %w", err)
			}
			cmds := fmt.Sprintf(
				"cd %s && tarball=%s && rm -f \"$tarball\" && "+
					"curl -fSL --retry 3 --retry-delay 2 -o \"$tarball\" '%s'",
				dir, tarball, url,
			)
			if _, err := h.Run(cmds); err != nil {
				return fmt.Errorf("downloading runner: %w", err)
			}
			fmt.Fprintf(m.out(), "  %s: extracting runner...\n", name)
			cmds = fmt.Sprintf("cd %s && tarball=%s && tar xzf \"$tarball\" && rm -f \"$tarball\"", dir, tarball)
			if _, err := h.Run(cmds); err != nil {
				return fmt.Errorf("extracting runner: %w", err)
			}
		}

		hostshell.WriteRemoteBytes(h, dir+"/.runner-version", []byte(version))

		if h.OS == "linux" {
			fmt.Fprintf(m.out(), "  %s: installing runner dependencies...\n", name)
			depsCmd := fmt.Sprintf(
				"cd %s && %s && $SUDO ./bin/installdependencies.sh",
				dir, strings.TrimSpace(sudoPrelude()),
			)
			if _, err := h.Run(depsCmd); err != nil {
				fmt.Fprintf(m.out(), "  %s: warning: failed to install runner dependencies: %v\n", name, err)
			}
		}

		regToken, err := m.GitHub.GetRegistrationTokenScoped(rc.Scope(), rc.ScopeTarget())
		if err != nil {
			return err
		}

		fmt.Fprintf(m.out(), "  %s: registering runner with GitHub...\n", name)
		if h.OS == "windows" {
			if _, err := h.RunShell(windowsNativeConfigScript(h, rc, name, regToken, i)); err != nil {
				return fmt.Errorf("configuring runner on Windows: %w", err)
			}
		} else {
			if _, err := h.Run(nativeShellConfigScript(h, rc, name, regToken, i)); err != nil {
				return fmt.Errorf("configuring runner: %w", err)
			}
		}

		fmt.Fprintf(m.out(), "  %s: configured\n", name)

		// Deploy svc.sh and runsvc.sh for systemd-based service management on Linux.
		if h.OS == "linux" {
			// Deploy svc.sh to the runner directory
			if err := hostshell.WriteRemoteBytes(h, dir+"/svc.sh", []byte(SVCShContent)); err != nil {
				fmt.Fprintf(m.out(), "  %s: warning: failed to deploy svc.sh: %v\n", name, err)
			} else {
				h.Run(fmt.Sprintf("chmod +x %s/svc.sh", dir))
			}

			// Deploy the systemd service template
			if err := hostshell.WriteRemoteBytes(h, dir+"/bin/actions.runner.service.template", []byte(ServiceTemplateContent)); err != nil {
				fmt.Fprintf(m.out(), "  %s: warning: failed to deploy service template: %v\n", name, err)
			}

			// Copy runsvc.sh from the runner package to the runner root (runsvc.sh handles signals gracefully)
			if _, err := h.Run(fmt.Sprintf("cp %s/bin/runsvc.sh %s/runsvc.sh && chmod +x %s/runsvc.sh", dir, dir, dir)); err != nil {
				fmt.Fprintf(m.out(), "  %s: warning: failed to copy runsvc.sh: %v\n", name, err)
			}

			// Install svc.sh as a systemd service (requires root)
			installSvcCmd := fmt.Sprintf("cd %s && %s\n$SUDO ./svc.sh install", dir, strings.TrimSpace(sudoPrelude()))
			if _, err := h.Run(installSvcCmd); err != nil {
				fmt.Fprintf(m.out(), "  %s: warning: failed to install svc.sh service: %v\n", name, err)
			}
		}
	}

	// Note: profile: agentic always runs in container mode (see EffectiveRunnerMode),
	// so setupNative is only reached for non-agentic native runners. The former
	// native-agentic host mutations (dnsmasq, sudoers, /opt/hostedtoolcache bind mount,
	// gh-aw install, image pre-pull) lived inside the runner image instead and have
	// been removed from the host path.
	return nil
}

func (m *Manager) startNative(h *host.Host, rc config.RunnerConfig, instanceName string) error {
	return m.startNativeOnce(h, rc, instanceName, true)
}

// startNativeOnce starts the runner and optionally detects a stale registration.
// When retryOnStale is true and the runner exits immediately with the stale-registration
// message, it clears local credentials, re-runs setupNative, and retries once.
func (m *Manager) startNativeOnce(h *host.Host, rc config.RunnerConfig, instanceName string, retryOnStale bool) error {
	dir := h.RunnerDir(instanceName)

	ok, err := NativeRunnerConfigPresent(h, instanceName)
	if err != nil {
		return fmt.Errorf("checking runner install at %s: %w", dir, err)
	}
	if !ok {
		fmt.Fprintf(m.out(), "  %s: not installed, running setup (this takes a few minutes)...\n", instanceName)
		if setupErr := m.setupNative(h, rc); setupErr != nil {
			return fmt.Errorf("auto-setup for %s: %w", instanceName, setupErr)
		}
	}

	if h.OS == "windows" {
		out, err := h.RunShell(windowsNativeStartScript(h, instanceName))
		if err != nil {
			return err
		}
		fmt.Fprintf(m.out(), "  %s: %s\n", instanceName, strings.TrimSpace(out))

		if retryOnStale {
			fmt.Fprintf(m.out(), "  %s: waiting for runner to initialize...\n", instanceName)
			checkOut, _ := h.RunShell(staleRegistrationScript(h, instanceName))
			if strings.TrimSpace(checkOut) == "stale" {
				return m.handleStaleRegistration(h, rc, instanceName)
			}
		}
		return nil
	}

	// Unix (Linux / macOS)
	cmd := fmt.Sprintf(
		`cd %s; pid_file=".runner_pid"; `+
			`if [ -f "$pid_file" ] && kill -0 "$(cat "$pid_file")" 2>/dev/null; then echo "already running"; exit 0; fi; `+
			`nohup ./run.sh > runner.log 2>&1 & { echo $!; } > "$pid_file"; echo "started PID $!"`,
		dir,
	)
	out, err := h.Run(cmd)
	if err != nil {
		return err
	}
	fmt.Fprintf(m.out(), "  %s: %s\n", instanceName, strings.TrimSpace(out))

	if retryOnStale {
		fmt.Fprintf(m.out(), "  %s: waiting for runner to initialize...\n", instanceName)
		checkOut, _ := h.Run(staleRegistrationScript(h, instanceName))
		if strings.TrimSpace(checkOut) == "stale" {
			return m.handleStaleRegistration(h, rc, instanceName)
		}
	}
	return nil
}

func (m *Manager) handleStaleRegistration(h *host.Host, rc config.RunnerConfig, instanceName string) error {
	fmt.Fprintf(m.out(), "  %s: registration expired on GitHub, re-configuring...\n", instanceName)
	dir := h.RunnerDir(instanceName)

	if h.OS == "windows" {
		h.RunShell(windowsDeleteRunnerConfig(h, instanceName))
	} else {
		h.Run(fmt.Sprintf("rm -f %s/.runner %s/.credentials %s/.credentials_rsaparams", dir, dir, dir))
	}

	if err := m.setupNative(h, rc); err != nil {
		return fmt.Errorf("re-configuring after stale registration: %w", err)
	}

	return m.startNativeOnce(h, rc, instanceName, false)
}

func (m *Manager) stopNative(h *host.Host, instanceName string) error {
	dir := h.RunnerDir(instanceName)

	if h.OS == "windows" {
		cmd := fmt.Sprintf(
			"%s; $pidFile = Join-Path $runnerDir '.runner_pid'; "+
				"if (-not (Test-Path $pidFile)) { Write-Host 'not running'; exit 0 }; "+
				"$p = (Get-Content $pidFile | Select-Object -First 1).Trim(); "+
				"$null = & taskkill.exe /PID $p /T /F 2>&1; "+
				"if ($LASTEXITCODE -eq 0) { Write-Host 'stopped' } else { Write-Host 'not running' }; "+
				"Remove-Item $pidFile -Force -EA SilentlyContinue",
			windowsRunnerDirAssignment(h, instanceName),
		)
		out, err := h.RunShell(cmd)
		if err != nil {
			return err
		}
		fmt.Fprintf(m.out(), "  %s: %s\n", instanceName, strings.TrimSpace(out))
		return nil
	}

	cmd := fmt.Sprintf(
		`cd %s && pid_file=".runner_pid" && `+
			`if [ ! -f "$pid_file" ]; then echo "not running"; exit 0; fi && `+
			`pid=$(cat "$pid_file") && `+
			`if ! kill -0 "$pid" 2>/dev/null; then echo "not running"; rm -f "$pid_file"; exit 0; fi && `+
			`kill "$pid" && `+
			`for i in $(seq 1 10); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done && `+
			`kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null; `+
			`rm -f "$pid_file" && echo "stopped"`,
		dir,
	)
	out, err := h.Run(cmd)
	if err != nil {
		return err
	}
	fmt.Fprintf(m.out(), "  %s: %s\n", instanceName, strings.TrimSpace(out))
	return nil
}

func (m *Manager) removeNative(h *host.Host, rc config.RunnerConfig, instanceName string) error {
	dir := h.RunnerDir(instanceName)

	m.removeNativeServices(h, instanceName)
	_ = m.stopNative(h, instanceName)

	removeToken, err := m.GitHub.GetRemovalTokenScoped(rc.Scope(), rc.ScopeTarget())
	if err != nil {
		fmt.Fprintf(m.out(), "  %s: warning: could not get removal token: %v\n", instanceName, err)
	} else {
		if h.OS == "windows" {
			cmd := fmt.Sprintf(
				"%s; Set-Location -Path $runnerDir; & .\\config.cmd remove --token %s",
				windowsRunnerDirAssignment(h, instanceName),
				hostshell.PowerShellSingleQuote(removeToken),
			)
			h.RunShell(cmd)
		} else {
			cmd := fmt.Sprintf("cd %s && ./config.sh remove --token '%s'", dir, removeToken)
			h.Run(cmd)
		}
		fmt.Fprintf(m.out(), "  %s: deregistered\n", instanceName)
	}

	if err := m.removeNativeDirectory(h, instanceName); err != nil {
		return fmt.Errorf("removing runner directory %s: %w", dir, err)
	}
	fmt.Fprintf(m.out(), "  %s: runner directory removed\n", instanceName)

	fmt.Fprintf(m.out(), "  %s: removed\n", instanceName)
	return nil
}

func (m *Manager) removeNativeServices(h *host.Host, instanceName string) {
	dir := h.RunnerDir(instanceName)

	// Combined probe: one SSH round-trip answers both "is svc.sh deployed?" and
	// "which autostart kind is installed?". Replaces the previous
	// svcShPresent + autostart.Detect pair (2 SSH per call).
	hasSvc, kind, _ := linuxSvcAndAutostartProbe(h, instanceName)
	if hasSvc {
		uninstallSvcCmd := fmt.Sprintf("cd %s && %s\n$SUDO ./svc.sh uninstall 2>/dev/null || true", dir, strings.TrimSpace(sudoPrelude()))
		h.Run(uninstallSvcCmd) // Ignore errors - we're removing anyway
		fmt.Fprintf(m.out(), "  %s: svc.sh service removed\n", instanceName)
	}

	if err := autostart.Uninstall(h, instanceName); err != nil {
		fmt.Fprintf(m.out(), "  %s: warning: failed to remove autostart: %v\n", instanceName, err)
	} else if kind != autostart.KindNone {
		fmt.Fprintf(m.out(), "  %s: autostart removed\n", instanceName)
	}
}

func (m *Manager) removeNativeDirectory(h *host.Host, instanceName string) error {
	if h.OS == "windows" {
		_, err := h.RunShell(fmt.Sprintf("%s; Remove-Item -Recurse -Force $runnerDir", windowsRunnerDirAssignment(h, instanceName)))
		return err
	}
	dir := h.RunnerDir(instanceName)
	_, err := h.Run(fmt.Sprintf("rm -rf %s", hostshell.PosixSingleQuote(dir)))
	return err
}

func (m *Manager) statusNative(h *host.Host, instanceName string) string {
	local, _ := statusNativeAndVersion(h, instanceName)
	return local
}

// statusNativeAndVersion returns the local runner state and the
// .runner-version value for a native (non-container) instance in one shot.
// On Linux, the combined probe (linuxInstanceProbe) folds the version read
// into the same SSH round-trip that already reports svc.sh + autostart
// kind (V marker), so all three responses arrive in one round-trip —
// strictly N→1 vs the previous "statusNative + nativeRunnerVersion" pair
// that always paid an extra round-trip for the version (whether the
// runner was svc.sh-managed, autostart-managed, or direct-started).
// Same win-class as the svc.sh + autostart fold (PR #361),
// orphanLinuxPlanProbe (PR #358), the systemd probe consolidation in
// autostart.Detect (PR #285), and the per-instance container presence
// one-shot (PR #350). On macOS/Windows the shape is unchanged (separate
// autostart probe + version probe) — the fold only applies to Linux
// because that is where the combined probe is in play.
//
// Returns "-" for the version when the .runner-version file is missing
// or unreadable, mirroring the prior nativeRunnerVersion contract so the
// TUI BUILD cell renders the same dash.
func statusNativeAndVersion(h *host.Host, instanceName string) (string, string) {
	dir := h.RunnerDir(instanceName)

	if h.OS == "linux" {
		result, err := linuxInstanceProbe(h, instanceName, false)
		if err == nil {
			local := statusNativeFromProbe(h, instanceName, dir, result)
			version := result.version
			if version == "" {
				version = "-"
			}
			return local, version
		}
		// Probe error: fall through to the non-Linux two-probe path so a
		// transient probe failure does not blank the Status row.
	}

	// Non-Linux / Linux probe-error fallback: the
	// statusNativeOneshotNonLinux shape is unchanged from the previous
	// statusNative + nativeRunnerVersion pair. Fold does not apply
	// because the combined probe is Linux-only.
	return statusNativeOneshotNonLinux(h, instanceName, dir), nativeRunnerVersion(h, instanceName)
}

// statusNativeOneshotNonLinux preserves the pre-fold statusNative state
// machine verbatim for non-Linux hosts (macOS launchd, Windows
// scheduled-task) and as the fallback when the Linux combined probe
// errors. Mirrors the original statusNative (svc.sh / autostart.Detect
// + ServiceActiveState / IsServiceActive / PID-file branches) without
// any of the new combined-probe machinery — same SSH round-trip count
// as before so there is no behaviour change on macOS/Windows or on a
// probe-error fallback for Linux.
func statusNativeOneshotNonLinux(h *host.Host, instanceName, dir string) string {
	hasSvc, kind, _ := linuxSvcAndAutostartProbe(h, instanceName)

	if hasSvc {
		out, err := h.Run(fmt.Sprintf(
			`svc_file="%s/.service"; `+
				`if [ -f "$svc_file" ]; then `+
				`svc=$(cat "$svc_file"); `+
				`systemctl is-active "$svc" 2>/dev/null || echo inactive; `+
				`fi`,
			dir,
		))
		if err == nil {
			state := strings.TrimSpace(out)
			if state == "active" {
				return "running"
			}
			if state != "" {
				return "stopped"
			}
		}
	}

	if kind != autostart.KindNone {
		if state, serr := autostart.ServiceActiveState(h, instanceName, kind); serr == nil {
			switch state {
			case "active":
				return "running"
			case "failed", "activating":
				return "service error"
			}
		}
		active, err := autostart.IsServiceActive(h, instanceName, kind)
		if err == nil && active {
			return "running"
		}
	}

	return statusNativePIDFile(h, instanceName, dir)
}

// statusNativeFromProbe reproduces the statusNative branch logic using a
// pre-fetched linuxInstanceProbeResult. Same win-class as statusNative but
// avoids re-issuing the combined probe when the caller already has one in
// hand. The branches match statusNative 1:1 so a change to the state
// machine must update both call sites in lockstep (the legacy
// `statusNative` above is the surviving entry point for any caller that
// only wants the local string).
func statusNativeFromProbe(h *host.Host, instanceName, dir string, result linuxInstanceProbeResult) string {
	// For svc.sh-managed runners on Linux: read the service name from the .service
	// marker file written by "svc.sh install" and query systemctl directly.
	if result.svcSh {
		out, err := h.Run(fmt.Sprintf(
			`svc_file="%s/.service"; `+
				`if [ -f "$svc_file" ]; then `+
				`svc=$(cat "$svc_file"); `+
				`systemctl is-active "$svc" 2>/dev/null || echo inactive; `+
				`fi`,
			dir,
		))
		if err == nil {
			state := strings.TrimSpace(out)
			if state == "active" {
				return "running"
			}
			if state != "" {
				return "stopped"
			}
		}
	}

	if result.kind != autostart.KindNone {
		if state, serr := autostart.ServiceActiveState(h, instanceName, result.kind); serr == nil {
			switch state {
			case "active":
				return "running"
			case "failed", "activating":
				return "service error"
			}
		}
		active, err := autostart.IsServiceActive(h, instanceName, result.kind)
		if err == nil && active {
			return "running"
		}
		// Autostart may be installed but the runner was started directly (e.g. macOS
		// with no GUI session for launchd). Fall through to the PID file check.
	}

	return statusNativePIDFile(h, instanceName, dir)
}

// statusNativePIDFile contains the Linux PID-file / Windows Get-Process
// tail of the statusNative state machine. Extracted so
// statusNativeAndVersion (Linux path) and the surviving statusNative
// (non-Linux / fallback path) share a single implementation.
//
// On Linux (returns "running"|"stopped"|"not installed"|"unknown") the
// single SSH call below is the second (and last) round-trip in the
// statusNativeAndVersion path; the first was the combined
// linuxInstanceProbe. On Windows it is the only round-trip and returns
// the same string the previous inline implementation did.
func statusNativePIDFile(h *host.Host, instanceName, dir string) string {
	if h.OS == "windows" {
		cmd := fmt.Sprintf(
			"%s; if (-not (Test-Path (Join-Path $runnerDir '.runner'))) { Write-Host 'not installed'; exit 0 }; "+
				"$pidFile = Join-Path $runnerDir '.runner_pid'; "+
				"if (-not (Test-Path $pidFile)) { Write-Host 'stopped'; exit 0 }; "+
				"$p = Get-Content $pidFile; "+
				"try { Get-Process -Id $p -EA Stop | Out-Null; Write-Host 'running' } catch { Write-Host 'stopped' }",
			windowsRunnerDirAssignment(h, instanceName),
		)
		out, _ := h.RunShell(cmd)
		return strings.TrimSpace(out)
	}

	cmd := fmt.Sprintf(
		`d="%s" && `+
			`if [ ! -f "$d/.runner" ]; then echo "not installed"; exit 0; fi && `+
			`pid_file="$d/.runner_pid" && `+
			`if [ ! -f "$pid_file" ]; then echo "stopped"; exit 0; fi && `+
			`pid=$(cat "$pid_file") && `+
			`if kill -0 "$pid" 2>/dev/null; then echo "running"; else echo "stopped"; fi`,
		dir,
	)
	out, err := h.Run(cmd)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(out)
}

func nativeRunnerVersion(h *host.Host, instanceName string) string {
	dir := h.RunnerDir(instanceName)
	if h.OS == "windows" {
		ps := fmt.Sprintf(`if (Test-Path (Join-Path %s '.runner-version')) { Get-Content (Join-Path %s '.runner-version') -EA Stop }`, h.RunnerDirPS(instanceName), h.RunnerDirPS(instanceName))
		out, err := h.RunShell(ps)
		if err != nil {
			return "-"
		}
		v := strings.TrimSpace(out)
		if v == "" {
			return "-"
		}
		return v
	}
	out, err := h.Run(fmt.Sprintf("cat %s/.runner-version 2>/dev/null", dir))
	if err != nil || strings.TrimSpace(out) == "" {
		return "-"
	}
	return strings.TrimSpace(out)
}

func (m *Manager) logsNative(h *host.Host, instanceName string) (string, error) {
	dir := h.RunnerDir(instanceName)

	if h.OS == "windows" {
		cmd := fmt.Sprintf(
			"%s; $logFile = Join-Path $runnerDir 'runner.log'; "+
				"$diagDir = Join-Path $runnerDir '_diag'; "+
				"$tailMain = { if (Test-Path $logFile) { $lines = @(Get-Content -LiteralPath $logFile -Tail 50 -EA SilentlyContinue); "+
				"$j = [string]::Join([Environment]::NewLine, $lines); "+
				"if ($lines.Count -gt 0 -and ($j -match '\\S')) { $j } else { $null } } else { $null } }; "+
				"$main = & $tailMain; "+
				"if ($null -ne $main) { Write-Output $main } "+
				"elseif (Test-Path $diagDir) { "+
				"$latest = Get-ChildItem -Path $diagDir -Filter *.log -File -EA SilentlyContinue | Sort-Object LastWriteTime -Descending | Select-Object -First 1; "+
				"if ($latest) { Write-Output ('--- _diag/' + $latest.Name + ' (last 50 lines) ---'); Get-Content -LiteralPath $latest.FullName -Tail 50 } "+
				"else { Write-Output 'no logs found' } } "+
				"else { Write-Output 'no logs found' }",
			windowsRunnerDirAssignment(h, instanceName),
		)
		return h.RunShell(cmd)
	}

	cmd := fmt.Sprintf("tail -50 %s/runner.log 2>/dev/null || echo 'no logs found'", dir)
	return h.Run(cmd)
}
