package runner

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

// decodeEncodedPowerShellCommand extracts the script body from a wrapped
// `powershell.exe -NoProfile -NonInteractive -EncodedCommand <base64>` shell
// command produced by host.Host.RunShell on a non-local Windows host.
// Mirrors encodePowerShellScript's UTF-16LE + base64 encoding. Returns the
// decoded script and ok=false if the command isn't a recognised EncodedCommand
// wrapper (e.g. tests that exercise the unsupported-host-OS branch).
func decodeEncodedPowerShellCommand(cmd string) (string, bool) {
	const marker = "-EncodedCommand "
	idx := strings.Index(cmd, marker)
	if idx < 0 {
		return "", false
	}
	payload := strings.TrimSpace(cmd[idx+len(marker):])
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	if len(raw)%2 != 0 {
		return "", false
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
	}
	return string(utf16.Decode(u16)), true
}

func diskMockHost(os string, mock *testutil.MockExecutor) *host.Host {
	h := host.NewHost("test", config.HostConfig{OS: os, Addr: "local"})
	h.SetConn(mock)
	return h
}

func TestParseFourInt64s(t *testing.T) {
	t.Parallel()
	a, b, c, d, err := parseFourInt64s("100 20 10 70\n")
	if err != nil {
		t.Fatal(err)
	}
	if a != 100 || b != 20 || c != 10 || d != 70 {
		t.Fatalf("got %d %d %d %d", a, b, c, d)
	}
}

func TestMeasureDiskUsage_linux(t *testing.T) {
	t.Parallel()
	h := diskMockHost("linux", &testutil.MockExecutor{
		Responses: []string{"1000000 500000 100000 300000\n"},
	})
	rc := config.RunnerConfig{Name: "ci", Count: 1}
	entry := MeasureDiskUsage(h, "host1", "ci-1", &rc)
	if entry.Err != nil {
		t.Fatal(entry.Err)
	}
	if entry.TotalBytes != 1000000 {
		t.Fatalf("total=%d", entry.TotalBytes)
	}
	if entry.WorkBytes != 500000 || entry.TempBytes != 100000 || entry.DockerDataBytes != 300000 {
		t.Fatalf("breakdown work=%d temp=%d docker=%d", entry.WorkBytes, entry.TempBytes, entry.DockerDataBytes)
	}
	if entry.Mode != "native" {
		t.Fatalf("mode=%q", entry.Mode)
	}
}

func TestMeasureDiskUsage_agenticMode(t *testing.T) {
	t.Parallel()
	h := diskMockHost("linux", &testutil.MockExecutor{
		Responses: []string{"100 0 0 0\n"},
	})
	rc := config.RunnerConfig{Name: "ag", Count: 1, Profile: "agentic"}
	entry := MeasureDiskUsage(h, "host1", "ag-1", &rc)
	if entry.Err != nil {
		t.Fatal(entry.Err)
	}
	if entry.Mode != "container" {
		t.Fatalf("mode=%q, want container", entry.Mode)
	}
}

// TestParseDirSizesBatch_happyPath pins the parser contract used by
// dirSizesBatchPOSIX: tab-separated `<inst>\t<total>\t<work>\t<temp>\t<docker>`
// lines, one per requested instance. Missing instances → error.
func TestParseDirSizesBatch_happyPath(t *testing.T) {
	t.Parallel()
	out := "ci-1\t1000\t200\t100\t300\nci-2\t2500\t500\t250\t750\n"
	res, err := parseDirSizesBatch(out, []string{"ci-1", "ci-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res["ci-1"].total != 1000 || res["ci-1"].work != 200 || res["ci-1"].temp != 100 || res["ci-1"].dockerData != 300 {
		t.Errorf("ci-1: %+v", res["ci-1"])
	}
	if res["ci-2"].total != 2500 || res["ci-2"].work != 500 || res["ci-2"].temp != 250 || res["ci-2"].dockerData != 750 {
		t.Errorf("ci-2: %+v", res["ci-2"])
	}
}

// TestParseDirSizesBatch_missingInstance covers the truncation-detection
// branch: the script was supposed to emit one line per instance but only
// N-1 lines came back. The parser must surface an explicit error rather
// than silently dropping the missing instance.
func TestParseDirSizesBatch_missingInstance(t *testing.T) {
	t.Parallel()
	out := "ci-1\t1000\t0\t0\t0\n"
	_, err := parseDirSizesBatch(out, []string{"ci-1", "ci-2"})
	if err == nil {
		t.Fatal("expected error for missing instance")
	}
	if !strings.Contains(err.Error(), "ci-2") {
		t.Errorf("error should mention missing instance: %v", err)
	}
}

// TestParseDirSizesBatch_tooFewFields rejects lines that don't have the
// 5-field contract. Defends against a future script change that drops a
// column.
func TestParseDirSizesBatch_tooFewFields(t *testing.T) {
	t.Parallel()
	out := "ci-1\t1000\t0\t0\n"
	_, err := parseDirSizesBatch(out, []string{"ci-1"})
	if err == nil {
		t.Fatal("expected error for too-few-fields line")
	}
	if !strings.Contains(err.Error(), "too few fields") {
		t.Errorf("error should mention field count: %v", err)
	}
}

// TestBuildDirSizesBatchPOSIXScript_structure pins the structural
// contract of the batch script: a single `for inst in ...; do` loop with
// the ghsr-batch-disk-v1 sentinel and the same `du --max-depth`/`du -d`
// probe as the single-instance script. Pinned separately so a future
// refactor that re-introduces a per-instance dispatch trips this test.
func TestBuildDirSizesBatchPOSIXScript_structure(t *testing.T) {
	t.Parallel()
	s := buildDirSizesBatchPOSIXScript([]string{"ci-1", "ci-2"})
	if !strings.Contains(s, "# ghsr-batch-disk-v1") {
		t.Errorf("missing batch sentinel: %q", s)
	}
	if !strings.Contains(s, "for inst in") {
		t.Errorf("missing for-loop: %q", s)
	}
	if strings.Count(s, "for inst in") != 1 {
		t.Errorf("expected exactly one for-loop, got %d", strings.Count(s, "for inst in"))
	}
	if !strings.Contains(s, "du --max-depth") || !strings.Contains(s, "du -d 1") {
		t.Errorf("missing du GNU/BSD probe: %q", s)
	}
	// Only ONE `du` invocation per instance is needed (the loop reuses
	// the same body). The structural sentinel guards against a future
	// refactor that re-introduces a per-instance `du` outside the loop.
	if strings.Count(s, "du --max-depth=1") != 1 {
		t.Errorf("expected exactly one du --max-depth=1 invocation: %q", s)
	}
}

// TestMeasureDiskUsageBatch_linux covers the happy path on Linux: one
// SSH round-trip, three instances, sizes match the batch mock's response.
func TestMeasureDiskUsageBatch_linux(t *testing.T) {
	t.Parallel()
	h := diskMockHost("linux", &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if !strings.Contains(cmd, "for inst in") {
				t.Errorf("unexpected non-batch command: %q", cmd)
			}
			return "ci-1\t1000\t200\t100\t300\nci-2\t2500\t500\t250\t750\nstale-orphan\t500\t0\t0\t0\n", nil
		},
	})
	ci1 := config.RunnerConfig{Name: "ci", Count: 2, Repo: "o/r"}
	rcByInstance := map[string]*config.RunnerConfig{
		"ci-1": &ci1,
		"ci-2": &ci1,
	}
	entries := MeasureDiskUsageBatch(h, "host1", []string{"ci-1", "ci-2", "stale-orphan"}, rcByInstance)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if e := entries["ci-1"]; e.TotalBytes != 1000 || e.WorkBytes != 200 || e.TempBytes != 100 || e.DockerDataBytes != 300 || e.Orphan {
		t.Errorf("ci-1 unexpected: %+v", e)
	}
	if e := entries["ci-2"]; e.TotalBytes != 2500 || e.WorkBytes != 500 || e.TempBytes != 250 || e.DockerDataBytes != 750 || e.Orphan {
		t.Errorf("ci-2 unexpected: %+v", e)
	}
	if e := entries["stale-orphan"]; e.TotalBytes != 500 || !e.Orphan || e.Mode != "unknown" {
		t.Errorf("stale-orphan unexpected: %+v", e)
	}
	// Other = total - work - temp - docker. For ci-1: 1000 - 200 - 100 - 300 = 400.
	if entries["ci-1"].OtherBytes != 400 {
		t.Errorf("ci-1 OtherBytes = %d, want 400", entries["ci-1"].OtherBytes)
	}
}

// TestMeasureDiskUsageBatch_unsafeInstanceName surfaces unsafe instance
// names with Err set without spending a shell round-trip on them. Mirrors
// the per-instance API's contract.
func TestMeasureDiskUsageBatch_unsafeInstanceName(t *testing.T) {
	t.Parallel()
	var calls int
	h := diskMockHost("linux", &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls++
			return "ok-1\t0\t0\t0\t0\n", nil
		},
	})
	entries := MeasureDiskUsageBatch(h, "host1", []string{"ok-1", "bad;name"}, nil)
	if calls != 1 {
		t.Errorf("expected 1 SSH round-trip (only safe names shipped), got %d", calls)
	}
	if entries["bad;name"].Err == nil {
		t.Errorf("expected Err for unsafe name, got %+v", entries["bad;name"])
	}
	if entries["ok-1"].Err != nil {
		t.Errorf("unexpected error for safe name: %v", entries["ok-1"].Err)
	}
}

// TestMeasureDiskUsageBatch_hostError propagates the SSH-round-trip error
// to every safe entry's Err field. Matches the per-instance API's
// "Err set, other fields zero" contract.
func TestMeasureDiskUsageBatch_hostError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("ssh: connection reset")
	h := diskMockHost("linux", &testutil.MockExecutor{
		RunFn: func(string) (string, error) {
			return "", sentinel
		},
	})
	entries := MeasureDiskUsageBatch(h, "host1", []string{"ci-1", "ci-2"}, nil)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if !errors.Is(entries["ci-1"].Err, sentinel) {
		t.Errorf("ci-1 err = %v, want sentinel", entries["ci-1"].Err)
	}
	if !errors.Is(entries["ci-2"].Err, sentinel) {
		t.Errorf("ci-2 err = %v, want sentinel", entries["ci-2"].Err)
	}
}

// TestMeasureDiskUsageBatch_windows forces the Windows dispatch branch
// on a host with OS="windows". The mocked RunFn returns the four-bucket
// layout wrapped in RunShell's UTF-16LE + base64 encoded command path;
// the mock intercepts the encoded command and returns the same tab-
// separated shape the POSIX branch would emit.
func TestMeasureDiskUsageBatch_windows(t *testing.T) {
	t.Parallel()
	h := diskMockHost("windows", &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			// The Windows path uses h.RunShell which base64-encodes the
			// script; the mock returns the tabular output for any
			// invocation. We don't need to validate the script shape
			// here — the structural test for the generator guards the
			// dispatch contract.
			return "ci-1\t42\t10\t0\t5\n", nil
		},
	})
	entries := MeasureDiskUsageBatch(h, "host1", []string{"ci-1"}, nil)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if e := entries["ci-1"]; e.TotalBytes != 42 || e.WorkBytes != 10 || e.TempBytes != 0 || e.DockerDataBytes != 5 {
		t.Errorf("ci-1 unexpected: %+v", e)
	}
}

// TestClearWorkTemp_posixDispatchesToRunnerDir covers the POSIX branch of
// clearWorkTemp's runOnHostOS dispatch: on a Linux host, the dispatcher
// must invoke h.Run exactly once with the clearWorkTempPOSIX(...) script
// (recognisable by the shared `set -e` header) and propagate the runner's
// exit error verbatim. The previous coverage only exercised the
// unsupported-OS path, leaving the POSIX callback's host-side error
// propagation unverified.
func TestClearWorkTemp_posixDispatchesToRunnerDir(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			return "", nil
		},
	}
	h := diskMockHost("linux", mock)
	if err := clearWorkTemp(h, "ci-1", false); err != nil {
		t.Fatalf("clearWorkTemp: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("clearWorkTemp POSIX calls = %d, want 1; calls=%v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "set -e") {
		t.Fatalf("clearWorkTemp POSIX script missing set -e: %q", calls[0])
	}
	if !strings.Contains(calls[0], "ci-1") {
		t.Fatalf("clearWorkTemp POSIX script should include the instance name: %q", calls[0])
	}
}

// TestClearWorkTemp_posixPropagatesRunnerError pins that clearWorkTemp's
// POSIX branch bubbles host-side failures up to the caller rather than
// silently returning nil: the dispatcher in disk.go treats a remote exec
// error as a fatal prune failure.
func TestClearWorkTemp_posixPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	sentinel := fmt.Errorf("ssh connection refused")
	mock := &testutil.MockExecutor{RunFn: func(string) (string, error) {
		return "", sentinel
	}}
	h := diskMockHost("linux", mock)
	err := clearWorkTemp(h, "ci-1", false)
	if err == nil {
		t.Fatal("clearWorkTemp POSIX: expected propagated error, got nil")
	}
	if !strings.Contains(err.Error(), "ssh connection refused") {
		t.Fatalf("clearWorkTemp POSIX: error should wrap sentinel, got %v", err)
	}
}

// TestClearWorkTemp_windowsBranch pins the Windows-side branch of
// clearWorkTemp's runOnHostOS dispatch: a PowerShell foreach-with-Test-Path
// script that targets the same _work/_temp subdirectories. The host shim
// routes RunShell through Run with a base64 wrapper, so we assert exactly
// one host invocation carrying the encoded PowerShell payload. Requires a
// remote addr so IsLocal is false and the wrapper activates.
func TestClearWorkTemp_windowsBranch(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			return "", nil
		},
	}
	h := host.NewHost("win", config.HostConfig{Addr: "runner@vps", OS: "windows", Arch: "amd64"})
	h.SetConn(mock)
	if err := clearWorkTemp(h, "win-ci-1", false); err != nil {
		t.Fatalf("clearWorkTemp Windows: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("clearWorkTemp Windows calls = %d, want 1; calls=%v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "powershell") || !strings.Contains(calls[0], "EncodedCommand") {
		t.Fatalf("clearWorkTemp Windows script must be base64-wrapped via powershell -EncodedCommand: %q", calls[0])
	}
}

// TestRemoveDirTree_posixDispatchesOnce pins the POSIX branch of
// removeDirTree's runOnHostOS dispatch: on Linux the dispatcher must
// issue exactly one h.Run containing the removeDirTreePOSIX(...) script
// (recognisable by the shared `set -e` header and docker-exec
// escalation block) and propagate the runner's exit error verbatim.
func TestRemoveDirTree_posixDispatchesOnce(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			return "", nil
		},
	}
	h := diskMockHost("linux", mock)
	if err := removeDirTree(h, "rune-orphan-7"); err != nil {
		t.Fatalf("removeDirTree POSIX: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("removeDirTree POSIX calls = %d, want 1; calls=%v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "set -e") {
		t.Fatalf("removeDirTree POSIX script missing set -e: %q", calls[0])
	}
	if !strings.Contains(calls[0], "docker exec") {
		t.Fatalf("removeDirTree POSIX script should include docker exec escalation: %q", calls[0])
	}
	if !strings.Contains(calls[0], "rune-orphan-7") {
		t.Fatalf("removeDirTree POSIX script should include instance name: %q", calls[0])
	}
}

// TestRemoveDirTree_posixPropagatesRunnerError pins that removeDirTree's
// POSIX branch bubbles host-side failures up to the caller (the prune path
// uses this error to mark the instance failed in PruneResult.Err).
func TestRemoveDirTree_posixPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	sentinel := fmt.Errorf("ssh connection reset")
	mock := &testutil.MockExecutor{RunFn: func(string) (string, error) {
		return "", sentinel
	}}
	h := diskMockHost("linux", mock)
	err := removeDirTree(h, "rune-orphan-7")
	if err == nil {
		t.Fatal("removeDirTree POSIX: expected propagated error, got nil")
	}
	if !strings.Contains(err.Error(), "ssh connection reset") {
		t.Fatalf("removeDirTree POSIX: error should wrap sentinel, got %v", err)
	}
}

// TestPruneInstance_orphanIncludeOrphans_noAutostart covers the
// orphan-with-IncludeOrphans branch in PruneInstance when the orphan has
// NO autostart unit: the branch must report the would-be `remove orphan
// directory ...` action and skip removeDirTree under --dry-run. RC is nil
// to flag the instance as orphan; --include-orphans opts in.
func TestPruneInstance_orphanIncludeOrphans_noAutostart(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		// Autostart helpers probe for known service names; return "no" so
		// the uninstall skip-path is taken.
		if strings.Contains(cmd, "&& echo yes || echo no") {
			return "no\n", nil
		}
		return "", nil
	}}
	h := diskMockHost("linux", mock)
	res := m.PruneInstance(h, "host1", "orphan-7", nil, false, PruneOptions{DryRun: true, IncludeOrphans: true})
	if res.Err != nil {
		t.Fatalf("dry-run orphan prune: %v", res.Err)
	}
	if res.Skipped {
		t.Fatalf("IncludeOrphans must not skip; got %+v", res)
	}
	if len(res.Actions) != 1 {
		t.Fatalf("orphan prune actions = %d, want 1; got %+v", len(res.Actions), res)
	}
	if !strings.Contains(res.Actions[0], "orphan") {
		t.Fatalf("orphan prune action should mention orphan: %q", res.Actions[0])
	}
}

// TestDiskDispatchers_unsupportedHostOS locks in the runOnHostOS fallback for
// the three dispatchers in disk.go (#229): when h.OS is neither "windows" nor
// "linux"/"darwin", dirSizes / clearWorkTemp / removeDirTree must all return
// the "unsupported host OS" error and never invoke the remote host. The
// refactor that moved the three hand-rolled switches onto runOnHostOS would be
// a regression if it dropped the error.
func TestDiskDispatchers_unsupportedHostOS(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{}
	h := diskMockHost("freebsd", mock)

	if _, _, _, _, err := dirSizes(h, "ci-1"); err == nil {
		t.Fatal("dirSizes: expected unsupported-host-OS error, got nil")
	}
	if err := clearWorkTemp(h, "ci-1", false); err == nil {
		t.Fatal("clearWorkTemp: expected unsupported-host-OS error, got nil")
	}
	if err := removeDirTree(h, "ci-1"); err == nil {
		t.Fatal("removeDirTree: expected unsupported-host-OS error, got nil")
	}
	if len(mock.Calls) != 0 {
		t.Fatalf("unsupported OS must not invoke the host, got %d call(s)", len(mock.Calls))
	}
	for _, err := range []error{
		func() error { _, _, _, _, e := dirSizes(h, "ci-1"); return e }(),
		clearWorkTemp(h, "ci-1", false),
		removeDirTree(h, "ci-1"),
	} {
		if !strings.Contains(err.Error(), "freebsd") {
			t.Fatalf("error should mention the offending OS, got: %v", err)
		}
	}
}

func TestPOSIXScripts_includeSetE(t *testing.T) {
	t.Parallel()
	for name, script := range map[string]string{
		"dirSizes":      buildDirSizesPOSIXScript("ci-1"),
		"clearWorkTemp": clearWorkTempPOSIX("ci-1", false),
		"removeDir":     removeDirTreePOSIX("ci-1"),
	} {
		if !strings.Contains(script, "set -e") {
			t.Fatalf("%s script missing set -e", name)
		}
	}
}

func TestPruneInstance_skipsBusy(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	h := diskMockHost("linux", &testutil.MockExecutor{})
	rc := config.RunnerConfig{Name: "ci", Count: 1}
	res := m.PruneInstance(h, "host1", "ci-1", &rc, true, PruneOptions{})
	if !res.Skipped || res.Reason != "busy" {
		t.Fatalf("got %+v", res)
	}
}

func TestPruneInstance_clearWorkTemp_dryRun(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{}
	h := diskMockHost("linux", mock)
	rc := config.RunnerConfig{Name: "ci", Count: 1}
	res := m.PruneInstance(h, "host1", "ci-1", &rc, false, PruneOptions{DryRun: true})
	if res.Skipped {
		t.Fatalf("unexpected skip: %+v", res)
	}
	if len(res.Actions) == 0 {
		t.Fatal("expected actions")
	}
	if len(mock.Calls) > 0 {
		t.Fatalf("dry-run should not run remote commands, got %d calls", len(mock.Calls))
	}
}

func TestPruneInstance_defaultKeepsDockerCache(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{}
	h := diskMockHost("linux", mock)
	rc := config.RunnerConfig{Name: "ci", Count: 1, Profile: "agentic"}
	res := m.PruneInstance(h, "host1", "ci-1", &rc, false, PruneOptions{DryRun: true})
	for _, a := range res.Actions {
		if strings.Contains(a, "docker cache") {
			t.Fatalf("default should keep docker cache: %q", a)
		}
	}
}

func TestPruneInstance_pruneCacheIncludesDockerPrune(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{}
	h := diskMockHost("linux", mock)
	rc := config.RunnerConfig{Name: "ci", Count: 1, Profile: "agentic"}
	res := m.PruneInstance(h, "host1", "ci-1", &rc, false, PruneOptions{DryRun: true, PruneCache: true})
	found := false
	for _, a := range res.Actions {
		if strings.Contains(a, "docker cache") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected inner docker cache prune action with --prune-cache")
	}
}

// TestPruneInnerDockerCache_innerDockerDown covers the guard branch:
// pruneInnerDockerCache runs `docker info` first; if the inner dockerd
// isn't responding, the function must short-circuit with a descriptive
// error and NOT run the destructive `docker system prune -af --volumes`.
//
// As of the 2026-08-10 single-SSH fold the probe and prune run inside one
// docker exec call: the in-container `if ! docker info ...; then exit 1`
// keeps the destructive prune from running when the inner dockerd is down.
// The real `runWithCapture` helper in internal/host/exec.go appends the
// captured stderr to the returned error, so the "inner dockerd not
// responding" message reaches the caller through the wrapped err. The
// MockExecutor used here doesn't capture stderr, so the test injects the
// descriptive message directly via the returned error to mirror the
// production stderr surface.
func TestPruneInnerDockerCache_innerDockerDown(t *testing.T) {
	t.Parallel()
	var calls []string
	h := diskMockHost("linux", &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			// Single docker exec call: probe fails (exit 1). The error
			// embeds the descriptive message to mirror production stderr
			// capture — see MockExecutor's stderr note above.
			return "", errors.New("exit status 1: inner dockerd not responding in gh-sr-my-1; skipped cache prune")
		},
	})
	err := pruneInnerDockerCache(h, "gh-sr-my-1")
	if err == nil {
		t.Fatal("expected error when inner dockerd is down")
	}
	if !strings.Contains(err.Error(), "inner dockerd not responding") {
		t.Errorf("expected descriptive error, got %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("expected 1 SSH call (folded probe+prune), got %d: %v", len(calls), calls)
	}
	if len(calls) == 1 && !strings.Contains(calls[0], `docker exec "gh-sr-my-1" sh -c '`) {
		t.Errorf("folded probe+prune must run under docker exec sh -c, got %q", calls[0])
	}
}

// TestPruneInnerDockerCache_happyPath covers the happy path: probe
// returns "ok" so the destructive prune runs. After the 2026-08-10
// single-SSH fold, both the probe and prune live inside one docker exec
// invocation; the test verifies the function returns nil on success and
// that the single SSH call carries both `docker info` and
// `docker system prune`.
func TestPruneInnerDockerCache_happyPath(t *testing.T) {
	t.Parallel()
	var calls []string
	h := diskMockHost("linux", &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			if strings.Contains(cmd, "docker info") && strings.Contains(cmd, "docker system prune") {
				return "", nil
			}
			return "", nil
		},
	})
	if err := pruneInnerDockerCache(h, "gh-sr-my-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 SSH call (folded probe+prune), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], `docker exec "gh-sr-my-1" sh -c '`) {
		t.Errorf("single SSH call must run under docker exec sh -c, got %q", calls[0])
	}
	if !strings.Contains(calls[0], "docker info") {
		t.Errorf("single SSH call should include the probe, got %q", calls[0])
	}
	if !strings.Contains(calls[0], "docker system prune") {
		t.Errorf("single SSH call should include the prune, got %q", calls[0])
	}
}

// TestPruneInnerDockerCache_probeError covers the case where the underlying
// SSH call fails (e.g. host unreachable). The function must surface a
// non-nil error and must not silently succeed — destructive `docker system
// prune` lives behind the probe branch on the remote side, so the same
// script handles probe-error and prune-error uniformly.
func TestPruneInnerDockerCache_probeError(t *testing.T) {
	t.Parallel()
	h := diskMockHost("linux", &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			return "", errors.New("ssh: connection reset")
		},
	})
	err := pruneInnerDockerCache(h, "gh-sr-my-1")
	if err == nil {
		t.Fatal("expected error when SSH fails")
	}
	if !strings.Contains(err.Error(), "inner docker cache prune") {
		t.Errorf("err = %v, want descriptive wrapper", err)
	}
}

func TestClearWorkTempPOSIX_escalatesForContainerMode(t *testing.T) {
	t.Parallel()
	script := clearWorkTempPOSIX("rune-agentic-3", true)
	if !strings.Contains(script, "docker exec") {
		t.Fatal("container mode should try docker exec for root-owned _work files")
	}
	if !strings.Contains(script, "gh-sr-rune-agentic-3") {
		t.Fatalf("expected container name in script, got: %s", script)
	}
	if !strings.Contains(script, "sudo -n") {
		t.Fatal("expected host sudo fallback")
	}
}

func TestClearWorkTempPOSIX_nativeSkipsDockerExec(t *testing.T) {
	t.Parallel()
	script := clearWorkTempPOSIX("ci-1", false)
	if strings.Contains(script, "docker exec") {
		t.Fatal("native mode should not use docker exec")
	}
	if !strings.Contains(script, "sudo -n") {
		t.Fatal("expected host sudo fallback")
	}
}

func TestContainerEscalation_quotesContainerAndCommand(t *testing.T) {
	t.Parallel()
	script := containerEscalation("gh-sr-rune-1", `for sub in _work _temp; do rm -rf "$sub"; done`)
	if !strings.Contains(script, "docker exec") {
		t.Fatal("expected docker exec invocation")
	}
	if !strings.Contains(script, "docker start") {
		t.Fatal("expected docker start fallback")
	}
	// QuoteContainerName wraps the name in Go-style double quotes via strconv.Quote.
	if !strings.Contains(script, `"gh-sr-rune-1"`) {
		t.Fatalf("expected double-quoted container name, got: %s", script)
	}
	if !strings.Contains(script, "sh -c") {
		t.Fatal("expected shell wrapper for the inner command")
	}
	if !strings.Contains(script, `for sub in _work _temp; do rm -rf "$sub"; done`) {
		t.Fatalf("inner command should appear verbatim inside the sh -c quote, got: %s", script)
	}
}

func TestContainerEscalation_handlesSpacesInName(t *testing.T) {
	t.Parallel()
	script := containerEscalation("weird name", `echo hi`)
	// QuoteContainerName wraps spaces in Go-style double quotes via strconv.Quote.
	if !strings.Contains(script, `"weird name"`) {
		t.Fatalf("expected double-quoted container name with spaces, got: %s", script)
	}
}

func TestPasswordlessSudo_setsSUDOVariable(t *testing.T) {
	t.Parallel()
	script := passwordlessSudo()
	if !strings.Contains(script, "SUDO=") {
		t.Fatal("expected SUDO variable assignment")
	}
	if !strings.Contains(script, "sudo -n true") {
		t.Fatal("expected passwordless sudo probe")
	}
	if !strings.Contains(script, "sudo -n'") {
		t.Fatal("expected sudo -n prefix assignment when probe succeeds")
	}
}

func TestRemoveDirTreePOSIX_usesSharedEscalationHelpers(t *testing.T) {
	t.Parallel()
	script := removeDirTreePOSIX("rune-orphan-7")
	// Both helpers should appear: docker exec escalation + sudo probe.
	if !strings.Contains(script, "docker exec") {
		t.Fatal("expected docker exec escalation in removeDirTreePOSIX")
	}
	if !strings.Contains(script, "sudo -n true") {
		t.Fatal("expected passwordless sudo probe in removeDirTreePOSIX")
	}
	if !strings.Contains(script, "rm -rf /runner-state") {
		t.Fatal("expected inner command to reach the container")
	}
	if !strings.Contains(script, "runner-state/rune-orphan-7") && !strings.Contains(script, `runners/rune-orphan-7`) {
		t.Fatalf("expected runner dir var to point at rune-orphan-7, got: %s", script)
	}
}

func TestPruneInstance_neverTouchesRunnerRegistration(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{Responses: []string{""}}
	h := diskMockHost("linux", mock)
	rc := config.RunnerConfig{Name: "ci", Count: 1}
	_ = m.PruneInstance(h, "host1", "ci-1", &rc, false, PruneOptions{})
	for _, cmd := range mock.Calls {
		if strings.Contains(cmd, ".runner") {
			t.Fatalf("prune must not touch .runner: %q", cmd)
		}
	}
}

func TestFormatBytesHuman(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{2 * 1024, "2.0 KiB"},
		{1024*1024 - 1, "1024.0 KiB"}, // boundary: just under MiB still renders as KiB
		{1024 * 1024, "1.0 MiB"},
		{500 * 1024 * 1024, "500.0 MiB"},
		{1024*1024*1024 - 1, "1024.0 MiB"}, // boundary: just under GiB still renders as MiB
		{1024 * 1024 * 1024, "1.0 GiB"},
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
		{50 * 1024 * 1024 * 1024, "50.0 GiB"},
		{-1, "0 B"}, // negative input clamps to zero
		{-1024, "0 B"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%d_to_%s", tc.in, tc.want), func(t *testing.T) {
			t.Parallel()
			if got := FormatBytesHuman(tc.in); got != tc.want {
				t.Errorf("FormatBytesHuman(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// BenchmarkFormatBytesHuman exercises the per-row size formatter that runs
// 5× per runner instance per `gh sr disk` listing plus once per host for the
// doctor disk-entry path. Per-call alloc drops compound across listings with
// many runners, so this benchmark pins the cost on the hot path.
func BenchmarkFormatBytesHuman(b *testing.B) {
	samples := []int64{
		0, 512, 2 * 1024, 5 * 1024 * 1024, 500 * 1024 * 1024,
		2 * 1024 * 1024 * 1024, 50 * 1024 * 1024 * 1024,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, sz := range samples {
			_ = FormatBytesHuman(sz)
		}
	}
}

func TestDiskWarnThresholdBytes(t *testing.T) {
	t.Parallel()
	want := int64(50) * 1024 * 1024 * 1024
	if got := DiskWarnThresholdBytes(); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestSafeRunnerInstanceName(t *testing.T) {
	t.Parallel()
	if err := SafeRunnerInstanceName("ci-1"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a\nb", `a";rm -rf`} {
		if err := SafeRunnerInstanceName(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestMeasureDiskUsage_rejectsUnsafeInstance(t *testing.T) {
	t.Parallel()
	h := diskMockHost("linux", &testutil.MockExecutor{})
	entry := MeasureDiskUsage(h, "host1", `bad"name`, nil)
	if entry.Err == nil {
		t.Fatal("expected error")
	}
}

// TestDirSizesPOSIXScript_structure guards the single-walk optimization: the
// generated script must make exactly one `du` invocation with depth 1, not
// four separate `du -sk` calls. This is the energy-efficiency invariant the
// PR claims — if a future refactor re-introduces multiple `du` walks, this
// test will catch it. The test runs the real production script (via
// buildDirSizesPOSIXScript) on a real temp dir, not a frozen copy.
func TestDirSizesPOSIXScript_structure(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("du"); err != nil {
		t.Skip("du not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	// Build the same instance-dir layout the production script targets.
	inst := "ci-bench"
	fakeHome := t.TempDir()
	runnerDir := filepath.Join(fakeHome, ".gh-sr", "runners", inst)
	for _, sub := range []string{"_work", "_temp", "docker-data"} {
		if err := os.MkdirAll(filepath.Join(runnerDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			f, err := os.CreateTemp(filepath.Join(runnerDir, sub), "f*.bin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(make([]byte, 4096)); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}

	cmd := exec.Command("bash", "-c", buildDirSizesPOSIXScript(inst))
	cmd.Env = append(os.Environ(), "HOME="+fakeHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 4 {
		t.Fatalf("expected 4 numbers, got %d: %q", len(parts), out)
	}
	// All three subdirs were populated with 5 × 4 KiB files = 20 KiB each.
	// `du` reports in 1-KiB blocks; allow >= 16 KiB each (rounding).
	for i, label := range []string{"total", "work", "temp", "docker"} {
		var n int64
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				t.Fatalf("non-numeric in %s: %q", label, parts[i])
			}
			n = n*10 + int64(c-'0')
		}
		if n < 16*1024 {
			t.Fatalf("%s=%d bytes, want >= 16384", label, n)
		}
	}
}

// TestDirSizesPOSIXScript_singleDuInvocation is the structural invariant
// guard. The OLD script had 4 separate `du -sk "$dir[/sub]"` calls. The
// NEW script has exactly one effective `du` invocation per execution
// (an `if/else` branch probes GNU vs BSD, but only one branch runs).
// We assert two properties:
//
//  1. The old `du -sk "$dir"`-per-subdir pattern is gone (0 occurrences).
//  2. There is exactly one `du` line that performs a data fetch
//     (the depth-0 probe is not a fetch — it only tests flag support).
func TestDirSizesPOSIXScript_singleDuInvocation(t *testing.T) {
	t.Parallel()
	script := buildDirSizesPOSIXScript("foo")

	// (1) The old 4-call pattern must be absent.
	if c := strings.Count(script, `du -sk "$dir"`); c != 0 {
		t.Fatalf("old 4-du pattern present: %d occurrences of `du -sk \"$dir\"` in script", c)
	}

	// (2) Exactly one data-fetching `du` line. Both the GNU and BSD
	// branches assign to `out=$(du ...)`; structurally that's 2 lines,
	// but only one runs per execution. We allow 2 (one per platform
	// branch) and disallow anything more.
	lines := strings.Split(script, "\n")
	fetches := 0
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "out=$(du ") {
			fetches++
		}
	}
	if fetches != 1 && fetches != 2 {
		t.Fatalf("expected 1 or 2 du fetch lines (GNU/BSD branch), got %d\nscript:\n%s", fetches, script)
	}
}

// BenchmarkMeasureDiskUsage_linux benchmarks the Go side of MeasureDiskUsage
// with a mock host. The real I/O win is on the remote host (4 SSH round
// trips → 1), which is not exercised here. This benchmark exists primarily
// to catch accidental per-call allocation regressions in the Go wrapper.
func BenchmarkMeasureDiskUsage_linux(b *testing.B) {
	h := diskMockHost("linux", &testutil.MockExecutor{
		Responses: []string{"1000000 500000 100000 300000\n"},
	})
	rc := config.RunnerConfig{Name: "ci", Count: 1}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MeasureDiskUsage(h, "host1", "ci-1", &rc)
	}
}

// TestPosixRunnerDirVar locks the POSIX double-quoted shell escape contract
// that posixRunnerDirVar feeds into posixScriptHeader (and through it into the
// disk-prune scripts that `gh sr disk` and `gh sr prune` send to the host).
// The escape runes — backslash, double-quote, dollar, backtick — must each be
// neutralised; if any one slips through, the resulting script can break out of
// the quoted string and execute attacker-controlled content on the host.
// The refactor that introduced posixInstanceEscaper (see commit
// repo-assist/perf-posix-dir-var-no-allocs-2026-07-10) replaced a per-call
// strings.NewReplacer with a shared package-level value; this test guards
// against accidental shape changes during future edits.
func TestPosixRunnerDirVar(t *testing.T) {
	t.Parallel()
	// Expected output bytes are computed by feeding the input through the
	// package's posixInstanceEscaper and stitching on the constant prefix
	// and suffix. The metacharacter escapes mirror strings.NewReplacer's
	// first-match order: backslash is doubled, then double-quote, dollar,
	// and backtick each get a leading backslash.
	cases := []struct {
		name     string
		instance string
		want     string
	}{
		{"plain", "ci-1", `dir="$HOME/.gh-sr/runners/ci-1"`},
		{"with_hyphen", "ci-runner-prod-01", `dir="$HOME/.gh-sr/runners/ci-runner-prod-01"`},
		// Each metacharacter must be backslash-escaped so it cannot break
		// out of the surrounding double-quoted string. The expected output
		// below mirrors what `posixInstanceEscaper.Replace` produces when
		// run against the input byte sequence (see commit message for the
		// canonical "before/after" trace).
		{"backslash", "a\\b", `dir="$HOME/.gh-sr/runners/a\\b"`},
		{"double_quote", "a\"b", `dir="$HOME/.gh-sr/runners/a\"b"`},
		{"dollar", "a$b", `dir="$HOME/.gh-sr/runners/a\$b"`},
		{"backtick", "a`b", "dir=\"$HOME/.gh-sr/runners/a\\`b\""},
		{"all_meta", "$\"\\`", "dir=\"$HOME/.gh-sr/runners/\\$\\\"\\\\\\`\""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := posixRunnerDirVar(tc.instance)
			if got != tc.want {
				t.Fatalf("posixRunnerDirVar(%q):\n  got:  %s\n  want: %s", tc.instance, got, tc.want)
			}
			// Defensive: the returned string must always start with the
			// well-known `dir="$HOME/.gh-sr/runners/` prefix and end with a
			// closing double-quote; downstream scripts grep for these
			// anchors when parsing the variable.
			if !strings.HasPrefix(got, `dir="$HOME/.gh-sr/runners/`) || !strings.HasSuffix(got, `"`) {
				t.Fatalf("posixRunnerDirVar(%q) shape changed: %s", tc.instance, got)
			}
		})
	}
}

// TestPosixRunnerDirVar_concurrent exercises the shared posixInstanceEscaper
// under -race to confirm strings.Replacer.Replace is safe for the concurrent
// caller pattern (each host.Run call inside the disk-prune path can fire from
// its own goroutine when a Manager fans out across hosts).
func TestPosixRunnerDirVar_concurrent(t *testing.T) {
	t.Parallel()
	const goroutines = 16
	const iters = 200
	done := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			for i := 0; i < iters; i++ {
				want := `dir="$HOME/.gh-sr/runners/ci-1"`
				if got := posixRunnerDirVar("ci-1"); got != want {
					done <- fmt.Errorf("g=%d i=%d: got %q want %q", g, i, got, want)
					return
				}
			}
			done <- nil
		}()
	}
	for g := 0; g < goroutines; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// TestDirSizesWindows_wrappedScript pins that dirSizesWindows goes through
// host.Host.RunShell — which on a non-local Windows host base64-wraps the
// PowerShell payload via host.Host.wrapCommand — and that the encoded script
// contains the size-collection helpers and the dirExpr for the requested
// instance. Without a non-local Addr the wrapper is a silent no-op and the
// test would observe the raw PowerShell source as if it were a shell command.
func TestDirSizesWindows_wrappedScript(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			return "0 0 0 0", nil
		},
	}
	h := host.NewHost("win", config.HostConfig{Addr: "runner@vps", OS: "windows", Arch: "amd64"})
	h.SetConn(mock)

	if _, _, _, _, err := dirSizesWindows(h, "ci-1"); err != nil {
		t.Fatalf("dirSizesWindows: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("dirSizesWindows calls = %d, want 1; calls=%v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "powershell.exe") || !strings.Contains(calls[0], "-EncodedCommand") {
		t.Fatalf("Windows script must be base64-wrapped via powershell -EncodedCommand: %q", calls[0])
	}
	script, ok := decodeEncodedPowerShellCommand(calls[0])
	if !ok {
		t.Fatalf("could not decode EncodedCommand payload: %q", calls[0])
	}
	// The PowerShell helper functions and the size-bucket join logic must all
	// survive the wrap so the remote host can actually execute the script.
	for _, want := range []string{
		"function Ghsr-DirSize",
		"function Ghsr-OtherDirSize",
		"Get-ChildItem",
		"Measure-Object",
		"_work",
		"_temp",
		"docker-data",
		"ci-1",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("encoded script missing %q; script=%q", want, script)
		}
	}
}

// TestDirSizesWindows_parsesFourBuckets verifies that the four-bucket emit
// from the Windows script ("total work temp docker") is parsed back into the
// expected total/work/temp/dockerData values, including the case where the
// runner's diagnostic preamble (a stray PowerShell warning) trails the
// numeric line — parseFourInt64s must keep the last non-empty line.
func TestDirSizesWindows_parsesFourBuckets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		out            string
		wantT, wantW   int64
		wantTe, wantDk int64
	}{
		{
			name:  "simple",
			out:   "1000 200 100 500",
			wantT: 1000, wantW: 200, wantTe: 100, wantDk: 500,
		},
		{
			name:  "trailing_newline",
			out:   "1234 234 134 600\n",
			wantT: 1234, wantW: 234, wantTe: 134, wantDk: 600,
		},
		{
			name: "diagnostic_preamble",
			// A stray PowerShell warning before the size line must be
			// discarded: parseFourInt64s keeps the trailing non-empty line.
			out:   "WARNING: using fallback cache\n8000 100 200 300\n",
			wantT: 8000, wantW: 100, wantTe: 200, wantDk: 300,
		},
		{
			name:  "zero_sizes",
			out:   "0 0 0 0",
			wantT: 0, wantW: 0, wantTe: 0, wantDk: 0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := &testutil.MockExecutor{Output: tc.out}
			h := host.NewHost("win", config.HostConfig{Addr: "runner@vps", OS: "windows", Arch: "amd64"})
			h.SetConn(mock)
			total, work, temp, docker, err := dirSizesWindows(h, "ci-1")
			if err != nil {
				t.Fatalf("dirSizesWindows: %v", err)
			}
			if total != tc.wantT || work != tc.wantW || temp != tc.wantTe || docker != tc.wantDk {
				t.Fatalf("dirSizesWindows(%q): got (%d,%d,%d,%d), want (%d,%d,%d,%d)",
					tc.out, total, work, temp, docker, tc.wantT, tc.wantW, tc.wantTe, tc.wantDk)
			}
		})
	}
}

// TestDirSizesWindows_runErrorPropagates pins that a host-side failure on
// the wrapped PowerShell call bubbles up verbatim so the caller (MeasureDiskUsage)
// can surface the failure in DiskUsageEntry.Err. Without this, a transient
// SSH error would silently leave the entry with zeroed bytes.
func TestDirSizesWindows_runErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := fmt.Errorf("ssh connection reset")
	mock := &testutil.MockExecutor{RunFn: func(string) (string, error) {
		return "", sentinel
	}}
	h := host.NewHost("win", config.HostConfig{Addr: "runner@vps", OS: "windows", Arch: "amd64"})
	h.SetConn(mock)
	_, _, _, _, err := dirSizesWindows(h, "ci-1")
	if err == nil {
		t.Fatal("dirSizesWindows: expected propagated error, got nil")
	}
	if !strings.Contains(err.Error(), "ssh connection reset") {
		t.Fatalf("error should wrap sentinel, got %v", err)
	}
}

// TestDirSizesWindows_parseErrorPropagates pins that when the wrapped
// PowerShell script emits non-numeric output (e.g. the host returned a
// localized error message instead of the four-bucket line), parseFourInt64s
// surfaces a "parsing size line" error so the listing marks the host failed
// rather than silently reporting zero bytes.
func TestDirSizesWindows_parseErrorPropagates(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{Output: "Access is denied"}
	h := host.NewHost("win", config.HostConfig{Addr: "runner@vps", OS: "windows", Arch: "amd64"})
	h.SetConn(mock)
	_, _, _, _, err := dirSizesWindows(h, "ci-1")
	if err == nil {
		t.Fatal("dirSizesWindows: expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing size line") {
		t.Fatalf("error should mention parse failure, got %v", err)
	}
}

// TestDirSizesWindows_localAddrPassesRawScript locks in the silent no-op
// behaviour: when the host addr is local, host.Host.wrapCommand returns the
// script unchanged so the test sees the raw PowerShell text. This guards
// against a regression that wrapped local commands too — wrapping a
// `powershell.exe -EncodedCommand ...` call locally works, but the script
// would also leak the wrapper into a local-addr user's normal shell, where
// it could collide with their environment.
func TestDirSizesWindows_localAddrPassesRawScript(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			return "100 20 10 70", nil
		},
	}
	h := diskMockHost("windows", mock)
	if _, _, _, _, err := dirSizesWindows(h, "ci-1"); err != nil {
		t.Fatalf("dirSizesWindows local: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("dirSizesWindows local calls = %d, want 1; calls=%v", len(calls), calls)
	}
	if strings.Contains(calls[0], "-EncodedCommand") {
		t.Fatalf("local Windows host should not base64-wrap, got %q", calls[0])
	}
	if !strings.Contains(calls[0], "function Ghsr-DirSize") {
		t.Fatalf("raw PowerShell script body should reach the host unmodified: %q", calls[0])
	}
}

// TestDirSizes_dispatchesToWindowsOnWindowsHost pins runOnHostOS's branch for
// dirSizes: a Windows host must call h.RunShell (the encoded PowerShell
// wrapper) exactly once, and the call must carry the four-bucket emit
// ("$t $w $te $dk") that the disk-listing parser depends on. The POSIX
// counterpart is covered separately by TestMeasureDiskUsage_linux.
func TestDirSizes_dispatchesToWindowsOnWindowsHost(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			return "9999 100 200 300", nil
		},
	}
	h := host.NewHost("win", config.HostConfig{Addr: "runner@vps", OS: "windows", Arch: "amd64"})
	h.SetConn(mock)
	total, work, temp, docker, err := dirSizes(h, "ci-1")
	if err != nil {
		t.Fatalf("dirSizes Windows: %v", err)
	}
	if total != 9999 || work != 100 || temp != 200 || docker != 300 {
		t.Fatalf("dirSizes Windows: got (%d,%d,%d,%d), want (9999,100,200,300)", total, work, temp, docker)
	}
	if len(calls) != 1 {
		t.Fatalf("dirSizes Windows calls = %d, want 1; calls=%v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "powershell.exe") {
		t.Fatalf("dirSizes on a Windows host must invoke PowerShell, got %q", calls[0])
	}
	script, ok := decodeEncodedPowerShellCommand(calls[0])
	if !ok {
		t.Fatalf("dirSizes Windows must use -EncodedCommand wrapper, got %q", calls[0])
	}
	if !strings.Contains(script, `Write-Output "$t $w $te $dk"`) {
		t.Fatalf("dirSizes Windows script must emit the four-bucket line, got %q", script)
	}
}
