package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

func Test_archForGitHub(t *testing.T) {
	t.Parallel()
	if archForGitHub("amd64") != "x64" {
		t.Errorf("amd64 -> x64")
	}
	if archForGitHub("arm64") != "arm64" {
		t.Errorf("arm64")
	}
	if archForGitHub("riscv") != "riscv" {
		t.Errorf("passthrough: got %q", archForGitHub("riscv"))
	}
}

func Test_runnerTarballURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ver, os, arch, wantSub string
	}{
		{"2.320.0", "linux", "x64", "actions-runner-linux-x64-2.320.0.tar.gz"},
		{"2.320.0", "darwin", "arm64", "actions-runner-osx-arm64-2.320.0.tar.gz"},
		{"2.320.0", "windows", "x64", "actions-runner-win-x64-2.320.0.zip"},
		{"2.320.0", "windows", "arm64", "actions-runner-win-arm64-2.320.0.zip"},
	}
	for _, tc := range cases {
		u := runnerTarballURL(tc.ver, tc.os, tc.arch)
		if !strings.Contains(u, tc.wantSub) {
			t.Errorf("%s/%s/%s: got %q want substring %q", tc.ver, tc.os, tc.arch, u, tc.wantSub)
		}
		if !strings.HasPrefix(u, "https://github.com/actions/runner/releases/download/v") {
			t.Errorf("prefix: %q", u)
		}
	}
	if runnerTarballURL("1", "freebsd", "amd64") != "" {
		t.Errorf("unsupported OS should return empty")
	}
}

func Test_windowsNativeInstallScript_usesPowerShellExpressions(t *testing.T) {
	t.Parallel()
	h := host.NewHost("win", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64"})

	script := windowsNativeInstallScript(
		h,
		"unwx-1",
		"2.333.1",
		"https://github.com/actions/runner/releases/download/v2.333.1/actions-runner-win-x64-2.333.1.zip",
	)

	if !strings.Contains(script, "$runnerDir = Join-Path (Join-Path $env:USERPROFILE '.gh-sr\\runners') 'unwx-1'") {
		t.Fatalf("runner dir should be built from PowerShell expressions: %q", script)
	}
	if !strings.Contains(script, "$zip = Join-Path $env:TEMP 'actions-runner-2.333.1.zip'") {
		t.Fatalf("zip path should use Join-Path with $env:TEMP: %q", script)
	}
	if strings.Contains(script, "'$env:USERPROFILE") || strings.Contains(script, "'$env:TEMP") {
		t.Fatalf("script should not quote env expressions literally: %q", script)
	}
}

func Test_windowsNativeConfigScript_usesRunnerDirVariable(t *testing.T) {
	t.Parallel()
	h := host.NewHost("win", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64"})
	rc := config.RunnerConfig{
		Repo:   "an-lee/gh-sr",
		Labels: []string{"windows", "native"},
	}

	script := windowsNativeConfigScript(h, rc, "unwx-1", "token-value", 0)

	if !strings.Contains(script, "$runnerDir = Join-Path (Join-Path $env:USERPROFILE '.gh-sr\\runners') 'unwx-1'") {
		t.Fatalf("runner dir should be assigned from a PowerShell expression: %q", script)
	}
	if !strings.Contains(script, "Set-Location -Path $runnerDir; & .\\config.cmd --unattended") {
		t.Fatalf("script should configure from the resolved runner dir: %q", script)
	}
	if strings.Contains(script, "cd '$env:USERPROFILE") {
		t.Fatalf("script should not cd into a literalized env path: %q", script)
	}
}

// Test_nativeShellConfigScript mirrors Test_windowsNativeConfigScript_usesRunnerDirVariable
// for the POSIX sibling helper. The flag set is identical (--unattended --url --token
// --name --labels --work '_work' --replace, plus --runnergroup/--ephemeral when set);
// the only differences are the cd path (uses h.RunnerDir, no PowerShell variable
// indirection) and the executable prefix (`./config.sh` vs `.\\config.cmd`).
// See issue #313.
func Test_nativeShellConfigScript(t *testing.T) {
	t.Parallel()

	h := host.NewHost("lin", config.HostConfig{Addr: "u@h", OS: "linux", Arch: "amd64"})

	t.Run("base flags + cd into runner dir", func(t *testing.T) {
		t.Parallel()
		rc := config.RunnerConfig{Repo: "an-lee/gh-sr", Labels: []string{"linux", "native"}}
		script := nativeShellConfigScript(h, rc, "unlx-1", "token-value", 0)

		wantDir := h.RunnerDir("unlx-1")
		if !strings.HasPrefix(script, "cd "+wantDir+" && ") {
			t.Fatalf("script should cd into the runner dir first; got %q", script)
		}
		if !strings.Contains(script, "./config.sh --unattended --url '") {
			t.Fatalf("script should invoke ./config.sh with --unattended; got %q", script)
		}
		for _, want := range []string{
			"--token 'token-value'",
			"--name 'unlx-1'",
			"--labels 'linux,native'",
			"--work '_work'",
			"--replace",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("script missing %q in: %q", want, script)
			}
		}
	})

	t.Run("group and ephemeral are appended when set", func(t *testing.T) {
		t.Parallel()
		rc := config.RunnerConfig{
			Repo:      "an-lee/gh-sr",
			Labels:    []string{"linux"},
			Group:     "rg-east",
			Ephemeral: true,
		}
		script := nativeShellConfigScript(h, rc, "unlx-1", "tok", 0)

		if !strings.Contains(script, " --runnergroup 'rg-east'") {
			t.Errorf("script missing --runnergroup; got %q", script)
		}
		if !strings.HasSuffix(script, " --ephemeral") {
			t.Errorf("script should end with --ephemeral; got %q", script)
		}
	})

	t.Run("no group / not ephemeral omits those flags", func(t *testing.T) {
		t.Parallel()
		rc := config.RunnerConfig{Repo: "an-lee/gh-sr", Labels: []string{"linux"}}
		script := nativeShellConfigScript(h, rc, "unlx-1", "tok", 0)

		if strings.Contains(script, "--runnergroup") {
			t.Errorf("script should not include --runnergroup when rc.Group is empty; got %q", script)
		}
		if strings.Contains(script, "--ephemeral") {
			t.Errorf("script should not include --ephemeral when rc.Ephemeral is false; got %q", script)
		}
	})

	t.Run("local-host path is used verbatim (no SSH expansion)", func(t *testing.T) {
		t.Parallel()
		h2 := host.NewHost("local", config.HostConfig{Addr: config.LocalAddr, OS: "linux", Arch: "amd64"})
		rc := config.RunnerConfig{Repo: "an-lee/gh-sr", Labels: []string{"self-hosted"}}
		script := nativeShellConfigScript(h2, rc, "self-1", "tok", 0)
		if !strings.Contains(script, h2.RunnerDir("self-1")) {
			t.Fatalf("script should resolve runner dir from local host; got %q", script)
		}
	})
}

func Test_windowsStartNative_usesCimProcessCreateForMergedLogs(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{config.LocalAddr, "u@h"} {
		h := host.NewHost("win", config.HostConfig{Addr: addr, OS: "windows", Arch: "amd64"})
		script := windowsNativeStartScript(h, "x-1")
		if !strings.Contains(script, "Invoke-CimMethod") || !strings.Contains(script, "Win32_Process") {
			t.Fatalf("addr=%s: expected Invoke-CimMethod Win32_Process.Create: %q", addr, script)
		}
		if !strings.Contains(script, "cmd.exe") || !strings.Contains(script, "run.cmd") || !strings.Contains(script, "2>&1") {
			t.Fatalf("addr=%s: expected cmd.exe run.cmd shell redirection to runner.log: %q", addr, script)
		}
		if strings.Contains(script, "Start-Process") {
			t.Fatalf("addr=%s: Start-Process is tied to the SSH job on Win32-OpenSSH; use CIM instead: %q", addr, script)
		}
		if !strings.Contains(script, "ReturnValue") {
			t.Fatalf("addr=%s: should check Win32_Process.Create ReturnValue: %q", addr, script)
		}
		if strings.Contains(script, "CreateFlags=0x08000000") {
			t.Fatalf("addr=%s: must not pass CreateFlags=0x08000000 to Win32_Process.Create: WMI rejects it with ERROR_INVALID_PARAMETER (21) and the start fails: %q", addr, script)
		}
		if !strings.Contains(script, "ShowWindow=0") {
			t.Fatalf("addr=%s: should set ShowWindow=0 on Win32_ProcessStartup to keep the conhost window hidden: %q", addr, script)
		}
		if !strings.Contains(script, "Win32_ProcessStartup") {
			t.Fatalf("addr=%s: should use Win32_ProcessStartup: %q", addr, script)
		}
		if !strings.Contains(script, "ProcessStartupInformation") {
			t.Fatalf("addr=%s: should pass ProcessStartupInformation to Win32_Process.Create: %q", addr, script)
		}
	}
}

func Test_staleRegistrationMsg(t *testing.T) {
	t.Parallel()
	logLine := `Failed to create a session. The runner registration has been deleted from the server, please re-configure.`
	if !strings.Contains(logLine, staleRegistrationMsg) {
		t.Fatalf("staleRegistrationMsg %q not found in typical log line", staleRegistrationMsg)
	}
}

func Test_staleRegistrationScript_bothOSShapesContainMessage(t *testing.T) {
	t.Parallel()
	// The unifier preserves the property the duplicate-code detector flagged as a
	// three-edit blast radius: every OS shape must search runner.log for the same
	// staleRegistrationMsg substring, so a future detection-rule change has a single
	// source of truth (the constant) rather than two parallel text snippets.
	cases := []struct {
		os string
	}{
		{"windows"},
		{"linux"},
		{"darwin"},
	}
	for _, tc := range cases {
		t.Run(tc.os, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("h-"+tc.os, config.HostConfig{Addr: "u@h", OS: tc.os, Arch: "amd64"})
			script := staleRegistrationScript(h, "ci-1")
			if !strings.Contains(script, staleRegistrationMsg) {
				t.Errorf("%s: staleRegistrationScript must reference staleRegistrationMsg %q: %q",
					tc.os, staleRegistrationMsg, script)
			}
		})
	}
}

func Test_staleRegistrationScript_windowsUsesPowerShellPrimitives(t *testing.T) {
	t.Parallel()
	h := host.NewHost("win", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64"})
	script := staleRegistrationScript(h, "ci-1")
	if !strings.Contains(script, "$runnerDir = ") {
		t.Errorf("windows snippet should declare $runnerDir: %q", script)
	}
	if !strings.Contains(script, "Start-Sleep") {
		t.Errorf("windows snippet should warm up via Start-Sleep: %q", script)
	}
	if !strings.Contains(script, "Select-String") {
		t.Errorf("windows snippet should grep via Select-String: %q", script)
	}
	if !strings.Contains(script, "Write-Host 'stale'") {
		t.Errorf("windows snippet should write 'stale' on match: %q", script)
	}
}

func Test_staleRegistrationScript_posixUsesPOSIXPrimitives(t *testing.T) {
	t.Parallel()
	h := host.NewHost("lin", config.HostConfig{Addr: "u@h", OS: "linux", Arch: "amd64"})
	script := staleRegistrationScript(h, "ci-1")
	if !strings.Contains(script, "sleep ") {
		t.Errorf("posix snippet should warm up via sleep: %q", script)
	}
	if !strings.Contains(script, "kill -0") {
		t.Errorf("posix snippet should liveness-check via kill -0: %q", script)
	}
	if !strings.Contains(script, "grep -q") {
		t.Errorf("posix snippet should grep via grep -q: %q", script)
	}
	if !strings.Contains(script, "echo stale") {
		t.Errorf("posix snippet should echo 'stale' on match: %q", script)
	}
}

func Test_staleRegistrationWarmup_isFiveSeconds(t *testing.T) {
	t.Parallel()
	// Pinned so a future tuning change is deliberate. The probe sleeps this long
	// before checking runner.log; shorter risks false-positive "ok", longer slows
	// every fresh start by the difference.
	if staleRegistrationWarmup != 5*time.Second {
		t.Errorf("staleRegistrationWarmup = %v, want 5s", staleRegistrationWarmup)
	}
}

func Test_windowsRunnerDirAssignment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		instanceName string
		wantSuffix   string // expected trailing expression from h.RunnerDirPS(...)
	}{
		{
			name:         "simple instance name",
			instanceName: "r1",
			wantSuffix:   "Join-Path (Join-Path $env:USERPROFILE '.gh-sr\\runners') 'r1'",
		},
		{
			name:         "hyphenated instance name",
			instanceName: "runner-1",
			wantSuffix:   "Join-Path (Join-Path $env:USERPROFILE '.gh-sr\\runners') 'runner-1'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &host.Host{HostConfig: config.HostConfig{OS: "windows"}}
			got := windowsRunnerDirAssignment(h, tc.instanceName)
			want := "$runnerDir = " + tc.wantSuffix
			if got != want {
				t.Errorf("windowsRunnerDirAssignment(windows, %q): got %q, want %q",
					tc.instanceName, got, want)
			}
			if !strings.HasPrefix(got, "$runnerDir = ") {
				t.Errorf("result must start with %q, got %q", "$runnerDir = ", got)
			}
		})
	}
}

func Test_windowsDeleteRunnerConfig_removesCredentialFiles(t *testing.T) {
	t.Parallel()
	h := host.NewHost("win", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64"})
	script := windowsDeleteRunnerConfig(h, "runner-x")

	for _, file := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if !strings.Contains(script, "Remove-Item") || !strings.Contains(script, file) {
			t.Errorf("script should Remove-Item %s: %q", file, script)
		}
	}
	if !strings.Contains(script, "-Force") || !strings.Contains(script, "-EA SilentlyContinue") {
		t.Errorf("script should use -Force and -EA SilentlyContinue: %q", script)
	}
}

func Test_staleRegistrationScript_windowsShapePreservesLegacyContract(t *testing.T) {
	t.Parallel()
	// This test replaces the prior Test_windowsCheckStaleRegistration_containsPatternAndSleep:
	// the Windows branch of staleRegistrationScript is the same shape, so the contract
	// (Start-Sleep + Select-String grep for staleRegistrationMsg) must still hold.
	h := host.NewHost("win", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64"})
	script := staleRegistrationScript(h, "x-1")
	if !strings.Contains(script, "Start-Sleep") {
		t.Fatalf("should wait before checking: %q", script)
	}
	if !strings.Contains(script, staleRegistrationMsg) {
		t.Fatalf("should search for stale registration message: %q", script)
	}
	if !strings.Contains(script, "Select-String") {
		t.Fatalf("should use Select-String to search runner.log: %q", script)
	}
}

func TestNativeRunnerConfigPresent_local(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-style paths and sh -c probe")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	inst := "ghsr-native-probe-" + filepath.Base(t.TempDir())

	base := filepath.Join(home, ".gh-sr", "runners", inst)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	if err := os.WriteFile(filepath.Join(base, ".runner"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// run.sh is required by the NeedsSetup check
	if err := os.WriteFile(filepath.Join(base, "run.sh"), []byte("#!/bin/sh\necho hi"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := host.NewHost("local-test", config.HostConfig{Addr: config.LocalAddr, OS: "linux", Arch: "amd64"})
	if err := h.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	ok, err := NativeRunnerConfigPresent(h, inst)
	if err != nil || !ok {
		t.Fatalf("expected installed: ok=%v err=%v", ok, err)
	}

	okMissing, errMissing := NativeRunnerConfigPresent(h, inst+"-not-there")
	if errMissing != nil || okMissing {
		t.Fatalf("expected not installed: ok=%v err=%v", okMissing, errMissing)
	}
}

// TestLinuxSvcAndAutostartProbe pins the combined-probe shape: one SSH
// round-trip answers both "is svc.sh deployed?" and "which autostart kind is
// installed?". Each sub-case asserts `calls == 1` to enforce the
// 1-SSH-round-trip contract that the helper exists to deliver.
func TestLinuxSvcAndAutostartProbe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		out       string
		wantSvc   bool
		wantKind  autostart.Kind
		wantCalls int
	}{
		{"nothing", "", false, autostart.KindNone, 1},
		{"svc-only", "S\n", true, autostart.KindNone, 1},
		{"user-only", "U\n", false, autostart.KindSystemdUser, 1},
		{"system-only", "Y\n", false, autostart.KindSystemdSystem, 1},
		{"svc-and-user", "S\nU\n", true, autostart.KindSystemdUser, 1},
		{"crlf", "S\r\nU\r\n", true, autostart.KindSystemdUser, 1},
		{"svc-and-system", "S\nY\n", true, autostart.KindSystemdSystem, 1},
		{"user-and-system-keeps-user", "U\n", false, autostart.KindSystemdUser, 1},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			mock := &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					calls++
					// Sanity: combined probe must include both the svc.sh check
					// and the autostart candidate paths.
					if strings.Contains(cmd, "echo D") {
						t.Errorf("native probe should not include directory check: %q", cmd)
					}
					if !strings.Contains(cmd, "svc.sh") {
						t.Errorf("combined probe missing svc.sh check: %q", cmd)
					}
					if !strings.Contains(cmd, ".config/systemd/user/") {
						t.Errorf("combined probe missing user unit path: %q", cmd)
					}
					if !strings.Contains(cmd, "/etc/systemd/system/") {
						t.Errorf("combined probe missing system unit path: %q", cmd)
					}
					return tc.out, nil
				},
			}
			h := host.NewHost("linux", config.HostConfig{OS: "linux", Addr: "local"})
			h.SetConn(mock)

			gotSvc, gotKind, err := linuxSvcAndAutostartProbe(h, "ci-1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotSvc != tc.wantSvc {
				t.Errorf("svcShPresent: got %v want %v", gotSvc, tc.wantSvc)
			}
			if gotKind != tc.wantKind {
				t.Errorf("autostart.Kind: got %q want %q", gotKind, tc.wantKind)
			}
			if calls != tc.wantCalls {
				t.Errorf("SSH round-trips: got %d want %d (combined probe must be exactly 1)", calls, tc.wantCalls)
			}
		})
	}
}

// TestLinuxSvcAndAutostartProbe_unsupportedOS confirms non-Linux hosts fall
// through to autostart.Detect (one SSH round-trip), preserving the prior
// behaviour for Darwin and Windows.
func TestLinuxSvcAndAutostartProbe_unsupportedOS(t *testing.T) {
	t.Parallel()

	t.Run("darwin", func(t *testing.T) {
		t.Parallel()
		calls := 0
		mock := &testutil.MockExecutor{
			RunFn: func(cmd string) (string, error) {
				calls++
				// launchd probe — should NOT include svc.sh
				if strings.Contains(cmd, "svc.sh") {
					t.Errorf("darwin probe should not check svc.sh: %q", cmd)
				}
				return "", nil
			},
		}
		h := host.NewHost("darwin", config.HostConfig{OS: "darwin", Addr: "local"})
		h.SetConn(mock)
		hasSvc, kind, err := linuxSvcAndAutostartProbe(h, "ci-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hasSvc {
			t.Errorf("darwin should always report svc.sh=false, got true")
		}
		if kind != autostart.KindNone {
			t.Errorf("kind: got %q want None", kind)
		}
		if calls != 1 {
			t.Errorf("darwin path must be 1 SSH round-trip (autostart.Detect), got %d", calls)
		}
	})

	t.Run("windows", func(t *testing.T) {
		t.Parallel()
		mock := &testutil.MockExecutor{
			RunFn: func(cmd string) (string, error) {
				// Detect's PowerShell probe wrapped via h.wrapCommand ends up
				// routed through h.Run under the hood.
				return "no", nil
			},
		}
		h := host.NewHost("win", config.HostConfig{OS: "windows", Addr: "local"})
		h.SetConn(mock)
		hasSvc, kind, err := linuxSvcAndAutostartProbe(h, "ci-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hasSvc {
			t.Errorf("windows should always report svc.sh=false, got true")
		}
		if kind != autostart.KindNone {
			t.Errorf("kind: got %q want None", kind)
		}
	})
}
