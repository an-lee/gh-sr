package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

// setupNativeWindowsHost wires a Windows host backed by a fresh MockExecutor.
// Addr is set to a non-local value so h.RunShell encodes PowerShell payloads
// via host.Host.wrapCommand (the same gateway used by the production SSH path
// on Windows). The decode helper in disk_test.go mirrors the encoder so tests
// can assert PowerShell script content directly.
func setupNativeWindowsHost(t *testing.T, mock *testutil.MockExecutor) *host.Host {
	t.Helper()
	h := host.NewHost("win", config.HostConfig{Addr: "runner@vps", OS: "windows", Arch: "amd64"})
	h.SetConn(mock)
	return h
}

// setupNativeWindowsGitHubServer mirrors setupNativeGitHubServer but configures
// a Manager that defaults to a Windows-shaped HostConfig. Tests can still
// override h.OS via their own NewHost call; this helper exists so the
// GitHub-wiring boilerplate doesn't duplicate per test.
func setupNativeWindowsGitHubServer(t *testing.T, version string, versionStatus int) (*Manager, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/actions/runner/releases/latest"):
			if versionStatus != http.StatusOK {
				http.Error(w, "boom", versionStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(releaseResponse{TagName: "v" + version})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runners/registration-token"):
			_ = json.NewEncoder(w).Encode(tokenResponse{Token: "TEST-TOKEN"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runners/remove-token"):
			_ = json.NewEncoder(w).Encode(tokenResponse{Token: "REMOVE-TOKEN"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	m := NewManager("")
	m.GitHub = NewGitHubClientWithHTTP("p", ts.Client(), ts.URL)
	return m, ts
}

// findWindowsInstallConfigPayloads separates the wrapped PowerShell bodies
// the mock recorded into install / config / presence categories so tests can
// assert each script independently rather than grepping the call log for
// substrings. Categories are MOST-SPECIFIC first: the presence probe is the
// only caller that emits BOTH `-PathType Container` and `Write-Output 'no'`
// (used as the unique fingerprint here — `Write-Output 'no'` appears nowhere
// else in any of the Windows scripts).
func findWindowsInstallConfigPayloads(t *testing.T, calls []string) (install, config1, presence []string) {
	t.Helper()
	for _, c := range calls {
		// Skip non-wrapped calls (e.g. uploader surfs through h.Upload, not h.Run).
		if !strings.Contains(c, "EncodedCommand") {
			continue
		}
		script, ok := decodeEncodedPowerShellCommand(c)
		if !ok {
			continue
		}
		switch {
		// Presence probe: only the runner-config-present check writes 'no'.
		// No other Windows script in this package uses Write-Output.
		case strings.Contains(script, "Write-Output 'no'"):
			presence = append(presence, script)
		case strings.Contains(script, "Invoke-WebRequest") && strings.Contains(script, "Expand-Archive"):
			install = append(install, script)
		case strings.Contains(script, ".\\config.cmd"):
			config1 = append(config1, script)
		}
	}
	return install, config1, presence
}

// findWindowsStaleRemovalStart finds the wrapped PowerShell commands the mock
// recorded for the stale-recovery cycle: windowsDeleteRunnerConfig (credential
// scrub), handleStaleRegistration's re-setup, and the start script. Used by
// TestHandleStaleRegistration_windowsRemovesCredentialsAndReconfigures and
// TestStartNativeOnce_windowsDetectsStaleAndRecovers.
//
// Categories are checked in MOST-SPECIFIC order so a script that matches
// multiple categories (e.g. the Windows stop script contains both `taskkill.exe`
// and `Remove-Item ... .runner_pid`) is assigned to the more-specific bucket.
func findWindowsStaleRemovalStart(calls []string) (scrubReRuns, starts, staleChecks, stopChecks []string) {
	for _, c := range calls {
		if !strings.Contains(c, "EncodedCommand") {
			continue
		}
		script, ok := decodeEncodedPowerShellCommand(c)
		if !ok {
			continue
		}
		switch {
		// taskkill.exe is unique to stopNative's Windows branch.
		case strings.Contains(script, "taskkill.exe"):
			stopChecks = append(stopChecks, script)
		// scrub: windowsDeleteRunnerConfig removes all three credential
		// files; .credentials_rsaparams is the unique marker (start/stop
		// never touch that file).
		case strings.Contains(script, ".credentials_rsaparams"):
			scrubReRuns = append(scrubReRuns, script)
		case strings.Contains(script, "Invoke-CimMethod") && strings.Contains(script, "Win32_Process"):
			starts = append(starts, script)
		case strings.Contains(script, "Start-Sleep") && strings.Contains(script, "Select-String"):
			staleChecks = append(staleChecks, script)
		}
	}
	return
}

// TestSetupNative_windowsDownloadsAndConfiguresRunner covers the happy path of
// setupNative on Windows for a fresh host: the presence probe reports
// "no" → the install script (Invoke-WebRequest + Expand-Archive) is issued
// via RunShell → the config script (.\\config.cmd --unattended) is issued via
// RunShell, both wrapped in powershell -EncodedCommand because the host
// address is non-local. The .runner-version marker file is written via
// WriteRemoteBytes, no systemd binaries are deployed, and the install/config
// log lines are emitted through Manager.Out.
func TestSetupNative_windowsDownloadsAndConfiguresRunner(t *testing.T) {
	t.Parallel()

	m, _ := setupNativeWindowsGitHubServer(t, "2.320.0", http.StatusOK)
	var buf bytes.Buffer
	m.Out = &buf

	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			// No special-case handling needed — the wrapping/categorising
			// happens via findWindowsInstallConfigPayloads. Returning empty
			// (nil error) mirrors "script ran successfully".
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	if err := m.setupNative(h, rc); err != nil {
		t.Fatalf("setupNative: %v", err)
	}

	install, config1, presence := findWindowsInstallConfigPayloads(t, mock.Calls)
	if len(presence) != 1 {
		t.Fatalf("expected 1 wrapped presence probe, got %d; calls=%v", len(presence), mock.Calls)
	}
	if len(install) != 1 {
		t.Fatalf("expected 1 wrapped install script, got %d; calls=%v", len(install), mock.Calls)
	}
	if len(config1) != 1 {
		t.Fatalf("expected 1 wrapped config script, got %d; calls=%v", len(config1), mock.Calls)
	}
	// Install script must target the Windows zip (zip= vs tarball): the URL
	// embed + Expand-Archive on a .zip proves the install branch picked
	// the windows tarball.
	if !strings.Contains(install[0], "actions-runner-win-x64-2.320.0.zip") {
		t.Errorf("install script must embed the win-x64 tarball URL; script=%q", install[0])
	}
	if !strings.Contains(install[0], "Expand-Archive") {
		t.Errorf("install script must expand the zip; script=%q", install[0])
	}
	// Config script must invoke .\\config.cmd with --unattended and the
	// test token from the stub GitHub server.
	if !strings.Contains(config1[0], ".\\config.cmd") || !strings.Contains(config1[0], "--unattended") {
		t.Errorf("config script must invoke .\\config.cmd --unattended; script=%q", config1[0])
	}
	if !strings.Contains(config1[0], "TEST-TOKEN") {
		t.Errorf("config script must carry the registration token; script=%q", config1[0])
	}
	// .runner-version marker must be written via WriteRemoteBytes. On Windows,
	// WriteRemoteBytes uses [Convert]::FromBase64String + [IO.File]::WriteAllBytes
	// (PowerShell, wrapped in the EncodedCommand base64). Matching the wrapped
	// call requires decoding the script and checking for the .runner-version
	// remote path + WriteAllBytes.
	var sawVersionWrite bool
	for _, c := range mock.Calls {
		if !strings.Contains(c, "EncodedCommand") {
			continue
		}
		script, ok := decodeEncodedPowerShellCommand(c)
		if !ok {
			continue
		}
		if strings.Contains(script, ".runner-version") && strings.Contains(script, "WriteAllBytes") {
			sawVersionWrite = true
			break
		}
	}
	if !sawVersionWrite {
		t.Errorf(".runner-version marker was not written via WriteRemoteBytes; calls=%v", mock.Calls)
	}
	// Linux-only systemd markers must not be issued on Windows.
	for _, c := range mock.Calls {
		if strings.Contains(c, "svc.sh install") || strings.Contains(c, "systemctl") {
			t.Errorf("Linux-only systemd call issued on Windows host: %q", c)
		}
	}
	if !strings.Contains(buf.String(), "downloading runner package") {
		t.Errorf("missing Windows install log line: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "registering runner with GitHub") {
		t.Errorf("missing register log line: %q", buf.String())
	}
}

// TestSetupNative_windowsSurfacesInstallError covers the install-error branch
// on Windows: when the install script (Invoke-WebRequest + Expand-Archive)
// fails, setupNative must wrap the error as "installing runner on Windows: %w"
// and abort the per-instance loop (the config script must NOT fire for the
// failed instance). This protects against partial installs where the runner
// zip is downloaded but extraction fails.
func TestSetupNative_windowsSurfacesInstallError(t *testing.T) {
	t.Parallel()

	m, _ := setupNativeWindowsGitHubServer(t, "2.320.0", http.StatusOK)
	var buf bytes.Buffer
	m.Out = &buf

	sentinel := errors.New("Expand-Archive: access denied")
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			// Only the install script fails; everything else succeeds.
			if strings.Contains(script, "Expand-Archive") {
				return "", sentinel
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	err := m.setupNative(h, rc)
	if err == nil {
		t.Fatalf("expected setupNative install error, got nil; calls=%v", mock.Calls)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("setupNative error = %v, want wraps sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "installing runner on Windows") {
		t.Errorf("error must wrap with 'installing runner on Windows: %%w': %v", err)
	}
	install, config1, _ := findWindowsInstallConfigPayloads(t, mock.Calls)
	if len(install) != 1 {
		t.Errorf("expected 1 install attempt before error, got %d", len(install))
	}
	if len(config1) != 0 {
		t.Errorf("config script must NOT fire after install error; config=%v", config1)
	}
}

// TestSetupNative_windowsSurfacesConfigError covers the config-error branch
// on Windows: install succeeds but config.cmd --unattended fails. setupNative
// must wrap the error with "configuring runner on Windows: %w" so the user
// sees the underlying PowerShell error rather than a generic Go stack.
func TestSetupNative_windowsSurfacesConfigError(t *testing.T) {
	t.Parallel()

	m, _ := setupNativeWindowsGitHubServer(t, "2.320.0", http.StatusOK)
	var buf bytes.Buffer
	m.Out = &buf

	sentinel := errors.New("config.cmd: invalid token")
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			// Only the config script fails.
			if strings.Contains(script, ".\\config.cmd") {
				return "", sentinel
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	err := m.setupNative(h, rc)
	if err == nil {
		t.Fatalf("expected config error, got nil; calls=%v", mock.Calls)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want wraps sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "configuring runner on Windows") {
		t.Errorf("error must wrap with 'configuring runner on Windows: %%w': %v", err)
	}
}

// TestStartNativeOnce_windowsCallsStartScript covers the steady-state Windows
// branch of startNativeOnce: when the presence probe reports "yes" (already
// installed), no setupNative is issued; only the Win32_Process start script
// runs. With retryOnStale=false the staleRegistration check is also skipped
// (matching the Linux contract pinned by TestStartNativeOnce_runsStartCmdWhenInstalled).
func TestStartNativeOnce_windowsCallsStartScript(t *testing.T) {
	t.Parallel()

	m, _ := setupNativeWindowsGitHubServer(t, "2.320.0", http.StatusOK)
	var buf bytes.Buffer
	m.Out = &buf

	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			switch {
			// Presence probe: report "yes" so startNativeOnce skips setupNative
			// (matching the steady-state branch pinned by the Linux test).
			// `Write-Output 'yes'` is unique to the probe — the start script
			// uses `Write-Host`, not `Write-Output`.
			case strings.Contains(script, "Write-Output 'yes'"):
				return "yes\n", nil
			// Start script returns the canonical "started PID N" message.
			case strings.Contains(script, "Invoke-CimMethod") && strings.Contains(script, "Win32_Process"):
				return "started PID 9999\n", nil
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	if err := m.startNativeOnce(h, rc, "ci-1", false); err != nil {
		t.Fatalf("startNativeOnce: %v", err)
	}

	_, _, presence := findWindowsInstallConfigPayloads(t, mock.Calls)
	if len(presence) != 1 {
		t.Errorf("expected 1 wrapped presence probe, got %d; calls=%v", len(presence), mock.Calls)
	}
	// Count start / stale-check invocations from the same call log — they share
	// the wrap pipeline with the install/config/presence scripts above.
	startCount := 0
	staleCount := 0
	for _, c := range mock.Calls {
		if !strings.Contains(c, "EncodedCommand") {
			continue
		}
		script, _ := decodeEncodedPowerShellCommand(c)
		switch {
		case strings.Contains(script, "Invoke-CimMethod") && strings.Contains(script, "Win32_Process"):
			startCount++
		case strings.Contains(script, "Start-Sleep") && strings.Contains(script, "Select-String"):
			staleCount++
		}
	}
	if startCount != 1 {
		t.Errorf("expected 1 wrapped start script, got %d; calls=%v", startCount, mock.Calls)
	}
	if staleCount != 0 {
		t.Errorf("retryOnStale=false must skip the stale check; got %d", staleCount)
	}
	if !strings.Contains(buf.String(), "started PID 9999") {
		t.Errorf("missing start log; buf=%q", buf.String())
	}
	// setupNative's curl/curl-fSL line must NEVER be issued on a Windows
	// host (no apt, no systemd, just .zip + Expand-Archive).
	for _, c := range mock.Calls {
		if strings.Contains(c, "curl -fSL") || strings.Contains(c, "svc.sh install") {
			t.Errorf("Windows start must not invoke Linux-only setup commands: %q", c)
		}
	}
}

// TestStartNativeOnce_windowsDetectsStaleAndRecovers covers the Windows
// stale-recovery contract: when the post-start staleRegistrationScript emits
// "stale" (initial probe only — the retry must NOT probe again to avoid an
// infinite loop), handleStaleRegistration runs windowsDeleteRunnerConfig to
// scrub .runner/.credentials, then re-runs setupNative (install + config) and
// re-runs the start script. Mirrors the Linux contract pinned by
// TestHandleStaleRegistration_clearsCredentialsAndReconfigures.
func TestStartNativeOnce_windowsDetectsStaleAndRecovers(t *testing.T) {
	t.Parallel()

	m, _ := setupNativeWindowsGitHubServer(t, "2.320.0", http.StatusOK)
	var buf bytes.Buffer
	m.Out = &buf

	const instance = "ci-1"
	staleChecks := 0
	probes := 0
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			switch {
			// Presence probe: report "yes" so startNativeOnce skips the initial
			// setupNative. Only the recovery path (re-setup) gets a probe that
			// says "no" so it actually re-installs. `Write-Output` is unique to
			// the probe (start script uses Write-Host).
			case strings.Contains(script, "Write-Output 'yes'") || strings.Contains(script, "Write-Output 'no'"):
				probes++
				if probes <= 1 {
					// First probe (initial startNativeOnce): already installed.
					return "yes\n", nil
				}
				// Subsequent probes (re-setup + post-recovery startNativeOnce):
				// report "no" so the re-setup path actually runs. The
				// post-recovery startNativeOnce uses retryOnStale=false so it
				// doesn't probe again after this.
				return "no\n", nil
			// Stale check: emit "stale" first call → trigger recovery; emit
			// "ok" on subsequent calls so a runaway retry loop fails loudly.
			case strings.Contains(script, "Start-Sleep") && strings.Contains(script, "Select-String"):
				staleChecks++
				if staleChecks == 1 {
					return "stale\n", nil
				}
				return "ok\n", nil
			// Start script: succeed silently so the post-stale retry actually
			// runs (which is what we want to verify).
			case strings.Contains(script, "Invoke-CimMethod") && strings.Contains(script, "Win32_Process"):
				return "started PID 1234\n", nil
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	if err := m.startNativeOnce(h, rc, instance, true); err != nil {
		t.Fatalf("startNativeOnce: %v", err)
	}

	// Exactly one stale check — the retry must skip it (otherwise we loop).
	if staleChecks != 1 {
		t.Errorf("stale check count = %d, want 1 (retry must skip stale check); calls=%v", staleChecks, mock.Calls)
	}
	scrubs, starts, _, _ := findWindowsStaleRemovalStart(mock.Calls)
	if len(scrubs) != 1 {
		t.Errorf("expected 1 windowsDeleteRunnerConfig invocation, got %d; calls=%v", len(scrubs), mock.Calls)
	}
	if !strings.Contains(scrubs[0], "Remove-Item") || !strings.Contains(scrubs[0], ".runner") {
		t.Errorf("scrub script must Remove-Item .runner; script=%q", scrubs[0])
	}
	if len(starts) != 2 {
		t.Errorf("expected 2 start script invocations (initial + post-stale retry), got %d; calls=%v", len(starts), mock.Calls)
	}
	// Re-setup + the post-scrub startNativeOnce (whose probe reports "no")
	// both trigger setupNative. So we expect 2 install + 2 config scripts
	// across the cycle (one each per setupNative invocation).
	install, config1, _ := findWindowsInstallConfigPayloads(t, mock.Calls)
	if len(install) != 2 {
		t.Errorf("expected 2 install scripts across the cycle (handleStaleRegistration re-setup + post-scrub startNativeOnce), got %d; calls=%v", len(install), mock.Calls)
	}
	if len(config1) != 2 {
		t.Errorf("expected 2 config scripts across the cycle (handleStaleRegistration re-setup + post-scrub startNativeOnce), got %d; calls=%v", len(config1), mock.Calls)
	}
	if !strings.Contains(buf.String(), "registration expired on GitHub, re-configuring") {
		t.Errorf("missing stale-recovery log line; buf=%q", buf.String())
	}
}

// TestHandleStaleRegistration_windowsDeletesConfigFiles covers the Windows
// stale-recovery entry-point directly: handleStaleRegistration must scrub
// .runner/.credentials/.credentials_rsaparams via windowsDeleteRunnerConfig
// (3 Remove-Item lines), then re-run setupNative and startNativeOnce with
// retryOnStale=false. The mock simulates the wincred scrub succeeding so the
// subsequent setupNative probe sees "no" and re-installs.
func TestHandleStaleRegistration_windowsDeletesConfigFiles(t *testing.T) {
	t.Parallel()

	m, _ := setupNativeWindowsGitHubServer(t, "2.320.0", http.StatusOK)
	var buf bytes.Buffer
	m.Out = &buf

	const instance = "ci-1"
	presenceChecks := 0
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			switch {
			// Presence probe: handleStaleRegistration's contract is "scrub
			// credentials then re-setup and start". We simulate the mock
			// filesystem by reporting "no" on the post-scrub probes so the
			// re-setup path actually issues install + config. `Write-Output`
			// is unique to the probe (start script uses Write-Host).
			case strings.Contains(script, "Write-Output 'yes'") || strings.Contains(script, "Write-Output 'no'"):
				presenceChecks++
				// Probes after the scrub see the .runner gone → "no".
				return "no\n", nil
			case strings.Contains(script, "Invoke-CimMethod") && strings.Contains(script, "Win32_Process"):
				return "started PID 4242\n", nil
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	if err := m.handleStaleRegistration(h, rc, instance); err != nil {
		t.Fatalf("handleStaleRegistration: %v", err)
	}

	scrubs, starts, _, _ := findWindowsStaleRemovalStart(mock.Calls)
	if len(scrubs) != 1 {
		t.Fatalf("expected 1 windowsDeleteRunnerConfig invocation, got %d; calls=%v", len(scrubs), mock.Calls)
	}
	// The scrub must target all three credential files — match the helper's contract.
	for _, file := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if !strings.Contains(scrubs[0], file) {
			t.Errorf("scrub script must reference %q; script=%q", file, scrubs[0])
		}
	}
	if len(starts) != 1 {
		// handleStaleRegistration calls startNativeOnce(ret retryOnStale=false).
		t.Errorf("expected 1 start script invocation (post-reset start), got %d; calls=%v", len(starts), mock.Calls)
	}
	install, config1, _ := findWindowsInstallConfigPayloads(t, mock.Calls)
	if len(install) != 2 || len(config1) != 2 {
		t.Errorf("expected 2 install + 2 config (one setup in handleStaleRegistration + one from post-scrub startNativeOnce); install=%d config=%d; calls=%v",
			len(install), len(config1), mock.Calls)
	}
	// 3 probes total: one from handleStaleRegistration's setupNative (line 285),
	// then two from startNativeOnce(retryOnStale=false) — its own line-409 probe
	// plus the inner setupNative line-285 probe. The mock reports "no" for all
	// of them so the install/config paths fire.
	if presenceChecks != 3 {
		t.Errorf("presence probe count = %d, want 3; calls=%v", presenceChecks, mock.Calls)
	}
}

// TestStopNative_windowsCallsTaskkillAndEmitsStopped covers the Windows
// branch of stopNative: the runner is running (pid file present, alive),
// so stopNative must call taskkill /PID <pid> /T /F and emit "stopped".
// The pid file is removed as part of the script.
func TestStopNative_windowsCallsTaskkillAndEmitsStopped(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			// taskkill returns a 0 exit code (success) so the script prints "stopped".
			if strings.Contains(script, "taskkill.exe") {
				return "stopped\n", nil
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	m := &Manager{Out: &buf}

	if err := m.stopNative(h, "ci-1"); err != nil {
		t.Fatalf("stopNative: %v", err)
	}
	if !strings.Contains(buf.String(), "stopped") {
		t.Errorf("missing 'stopped' log line; buf=%q", buf.String())
	}
	_, _, _, stops := findWindowsStaleRemovalStart(mock.Calls)
	if len(stops) != 1 {
		t.Fatalf("expected 1 wrapped stop script, got %d; calls=%v", len(stops), mock.Calls)
	}
	for _, want := range []string{"taskkill.exe", "/T", "/F", ".runner_pid"} {
		if !strings.Contains(stops[0], want) {
			t.Errorf("stop script missing %q; script=%q", want, stops[0])
		}
	}
}

// TestStopNative_windowsReportsNotRunning covers the "pid file absent or
// stale" Windows branch: when the .runner_pid file does not exist, stopNative
// must short-circuit with "not running" and skip taskkill entirely. This
// protects against double-stops of an already-gone runner and avoids
// taskill on a stale PID string.
func TestStopNative_windowsReportsNotRunning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			if strings.Contains(script, "taskkill.exe") {
				return "not running\n", nil
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	m := &Manager{Out: &buf}

	if err := m.stopNative(h, "ci-1"); err != nil {
		t.Fatalf("stopNative: %v", err)
	}
	if !strings.Contains(buf.String(), "not running") {
		t.Errorf("missing 'not running' log line; buf=%q", buf.String())
	}
	_, _, _, stops := findWindowsStaleRemovalStart(mock.Calls)
	if len(stops) != 1 {
		t.Errorf("expected 1 wrapped stop script, got %d; calls=%v", len(stops), mock.Calls)
	}
	// The stop script must read the pid file before invoking taskkill — the
	// "not running" branch returns BEFORE taskkill fires. The mock emits the
	// "not running" stdout itself, so we can only assert the script never
	// gets a successful kill result here. The behaviour is reflected through
	// the captured buf string.
}

// TestRemoveNative_windowsRunsConfigRemoveAndDeletesDir covers the Windows
// branch of removeNative: stopNative succeeds, config.cmd remove is invoked
// via RunShell with the removal token from the GitHub scope, and the runner
// directory is removed via Remove-Item -Recurse -Force (also wrapped in
// PowerShell because Windows goes through RunShell for file ops).
func TestRemoveNative_windowsRunsConfigRemoveAndDeletesDir(t *testing.T) {
	t.Parallel()

	m, _ := setupNativeWindowsGitHubServer(t, "2.320.0", http.StatusOK)
	var buf bytes.Buffer
	m.Out = &buf

	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			// taskkill returns "stopped" so removeNative's stop step succeeds.
			if strings.Contains(script, "taskkill.exe") {
				return "stopped\n", nil
			}
			// autostart.Detect returns "no" (none installed) so removeNativeServices
			// skips the systemd/launchd uninstall path (Windows uses Task Scheduler
			// but the helper reports KindNone here).
			if strings.Contains(script, "ScheduledTask") || strings.Contains(script, "Unregister") {
				return "no\n", nil
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	if err := m.removeNative(h, rc, "ci-1"); err != nil {
		t.Fatalf("removeNative: %v", err)
	}

	// Find the .\\config.cmd remove call (uses the removal token from the
	// GitHub scope endpoint we stubbed).
	var config1 []string
	var dirRemove []string
	for _, c := range mock.Calls {
		if !strings.Contains(c, "EncodedCommand") {
			continue
		}
		script, ok := decodeEncodedPowerShellCommand(c)
		if !ok {
			continue
		}
		switch {
		case strings.Contains(script, ".\\config.cmd") && strings.Contains(script, "remove"):
			config1 = append(config1, script)
		case strings.Contains(script, "Remove-Item") && strings.Contains(script, "-Recurse"):
			dirRemove = append(dirRemove, script)
		}
	}
	if len(config1) != 1 {
		t.Fatalf("expected 1 config.cmd remove call, got %d; calls=%v", len(config1), mock.Calls)
	}
	if !strings.Contains(config1[0], "REMOVE-TOKEN") {
		t.Errorf("config.cmd remove script must carry the removal token; script=%q", config1[0])
	}
	if !strings.Contains(config1[0], ".\\config.cmd") || !strings.Contains(config1[0], "remove") {
		t.Errorf("config.cmd remove script shape unexpected; script=%q", config1[0])
	}
	// The directory removal goes through RunShell on Windows (Remove-Item -Recurse
	// -Force piped through the wrapper). Must be exactly one invocation.
	if len(dirRemove) != 1 {
		t.Errorf("expected 1 Remove-Item -Recurse call, got %d; calls=%v", len(dirRemove), mock.Calls)
	}
	if !strings.Contains(dirRemove[0], "$runnerDir") {
		t.Errorf("dir removal script must target $runnerDir; script=%q", dirRemove[0])
	}
	if !strings.Contains(buf.String(), "deregistered") {
		t.Errorf("missing deregistered log line; buf=%q", buf.String())
	}
	if !strings.Contains(buf.String(), "runner directory removed") {
		t.Errorf("missing directory-removed log line; buf=%q", buf.String())
	}
	if !strings.Contains(buf.String(), "removed") {
		t.Errorf("missing final 'removed' log line; buf=%q", buf.String())
	}
}

// TestRemoveNative_windowsPropagatesRemoveTokenError covers the deregister
// branch when the removal-token endpoint fails: removeNativeServices and
// stopNative still run (services and process still need cleanup), the directory
// removal still runs (the runner instance is going away either way), but the
// deregister step is skipped. The warning log line "could not get removal
// token" must surface so the operator knows the runner wasn't unregistered on
// the GitHub side.
func TestRemoveNative_windowsPropagatesRemoveTokenError(t *testing.T) {
	t.Parallel()

	// Stub GitHub: registration succeeds (needed by re-setup) but removal
	// token endpoint returns 500.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runners/remove-token") {
			http.Error(w, "no removal token", http.StatusInternalServerError)
			return
		}
		// Registration path is not actually exercised here, but be defensive.
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(ts.Close)

	var buf bytes.Buffer
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "EncodedCommand") {
				return "", nil
			}
			script, ok := decodeEncodedPowerShellCommand(cmd)
			if !ok {
				return "", nil
			}
			if strings.Contains(script, "taskkill.exe") {
				return "stopped\n", nil
			}
			if strings.Contains(script, "ScheduledTask") || strings.Contains(script, "Unregister") {
				return "no\n", nil
			}
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	m := &Manager{
		GitHub: NewGitHubClientWithHTTP("p", ts.Client(), ts.URL),
		Out:    &buf,
	}
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "win", Count: 1}

	if err := m.removeNative(h, rc, "ci-1"); err != nil {
		t.Fatalf("removeNative: token-endpoint failure must NOT abort remove; got %v", err)
	}

	// No config.cmd remove must be issued when the token endpoint fails —
	// removeNative skips that block entirely (not even wrapped).
	var sawConfigRemove bool
	for _, c := range mock.Calls {
		if !strings.Contains(c, "EncodedCommand") {
			continue
		}
		script, _ := decodeEncodedPowerShellCommand(c)
		if strings.Contains(script, ".\\config.cmd") && strings.Contains(script, "remove") {
			sawConfigRemove = true
		}
	}
	if sawConfigRemove {
		t.Errorf("config.cmd remove must NOT fire when token endpoint fails; calls=%v", mock.Calls)
	}
	if !strings.Contains(buf.String(), "could not get removal token") {
		t.Errorf("missing 'could not get removal token' warning; buf=%q", buf.String())
	}
	if !strings.Contains(buf.String(), "runner directory removed") {
		t.Errorf("directory removal must still run; buf=%q", buf.String())
	}
	// "deregistered" line is gated behind a successful token fetch.
	if strings.Contains(buf.String(), "deregistered") {
		t.Errorf("'deregistered' must NOT log when token fetch failed; buf=%q", buf.String())
	}
}

// TestRemoveNativeDirectory_windowsDeletesViaPowerShell covers the Windows
// branch of removeNativeDirectory directly: when OS=windows, the helper must
// issue Remove-Item -Recurse -Force $runnerDir via RunShell (the wrapper
// encodes it as powershell -EncodedCommand). The Linux branch uses rm -rf via
// h.Run, so this is a distinct seam to pin.
func TestRemoveNativeDirectory_windowsDeletesViaPowerShell(t *testing.T) {
	t.Parallel()

	var calls []string
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			return "", nil
		},
	}
	h := setupNativeWindowsHost(t, mock)
	m := NewManager("")

	if err := m.removeNativeDirectory(h, "ci-1"); err != nil {
		t.Fatalf("removeNativeDirectory: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d; calls=%v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "powershell.exe") || !strings.Contains(calls[0], "-EncodedCommand") {
		t.Fatalf("Windows dir removal must go through powershell -EncodedCommand: %q", calls[0])
	}
	script, ok := decodeEncodedPowerShellCommand(calls[0])
	if !ok {
		t.Fatalf("could not decode EncodedCommand payload: %q", calls[0])
	}
	for _, want := range []string{"Remove-Item", "-Recurse", "-Force", "$runnerDir"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q; script=%q", want, script)
		}
	}
}
