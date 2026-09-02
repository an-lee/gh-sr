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

// removalTokenStubServer returns an httptest server that issues the same
// removal token regardless of request shape; tests can wrap a single line
// around the body of a Manager-level test that needs GitHub to succeed
// (so the deregister step inside removeNative/removeContainer is exercised)
// or fails (so the best-effort deregister is skipped).
func removalTokenStubServer(t *testing.T, status int, token string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "stub", status)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{Token: token})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestManagerRemove_dispatchesToRemoveNativeForEachInstance pins the native
// path of Manager.Remove: every InstanceNames() entry must reach removeNative
// in order, with the configured RunnerConfig, and Manager.Remove must wrap
// any per-instance error with "removing <name>: %w". We force
// removeNative to fail on the second instance so we cover the early-exit
// branch (subsequent instances must not be touched).
func TestManagerRemove_dispatchesToRemoveNativeForEachInstance(t *testing.T) {
	t.Parallel()

	// Stub the removal token so removeNative reaches its `config.sh remove`
	// deregister call; the test only cares that the orchestrator loop visits
	// every instance, not the deregister result.
	ts := removalTokenStubServer(t, http.StatusOK, "rem")

	var visited []string
	sentinel := errors.New("rm -rf permission denied")
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			// Match the per-instance dispatch by the runner dir, not by the
			// whole chain (the chained call also includes svc.sh, autostart,
			// pid-file, deregister, and finally rm -rf).
			if strings.Contains(cmd, "rm -rf") {
				// Capture the instance name embedded in the dir to assert
				// the dispatch order.
				if i := strings.Index(cmd, "runners/"); i >= 0 {
					rest := cmd[i+len("runners/"):]
					if j := strings.IndexAny(rest, " \t'\""); j > 0 {
						visited = append(visited, rest[:j])
					}
				}
				// Fail only the second visit to cover the early-exit branch.
				if len(visited) == 2 {
					return "", sentinel
				}
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Addr: "local", Arch: "amd64"})
	h.SetConn(mock)

	m := NewManager("")
	m.GitHub = NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)

	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 3}
	err := m.Remove(h, rc)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Remove error = %v, want sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "removing ci-") {
		t.Errorf("Remove error must wrap with 'removing <name>: %%w', got: %v", err)
	}
	// Early exit: only the first two instances must have been visited.
	if len(visited) != 2 {
		t.Fatalf("visited instances = %v, want [ci-1 ci-2] (early-exit after first error)", visited)
	}
	if visited[0] != "ci-1" || visited[1] != "ci-2" {
		t.Errorf("dispatch order = %v, want [ci-1 ci-2]", visited)
	}
}

// TestManagerRemove_dispatchesToRemoveContainerForEachInstance covers the
// container branch of Manager.Remove. The underlying removeContainer is
// exercised through the orchestrator loop; the per-instance h.Run call
// must contain the chained `docker stop` + `docker rm -f` + state-dir rm.
func TestManagerRemove_dispatchesToRemoveContainerForEachInstance(t *testing.T) {
	t.Parallel()

	// Stub a 500 on the removal-token endpoint so the best-effort
	// `docker exec ... config.sh remove` deregister is skipped, leaving
	// only the chained teardown call to inspect.
	ts := removalTokenStubServer(t, http.StatusInternalServerError, "")

	var chainedCalls int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker stop") && strings.Contains(cmd, "docker rm -f") {
				chainedCalls++
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Addr: "runner@vps", Arch: "amd64"})
	h.SetConn(mock)

	m := &Manager{
		GitHub:      NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL),
		GhSrVersion: "1.2.3",
		Out:         io.Discard,
	}
	rc := config.RunnerConfig{
		Name:       "aw",
		Repo:       "o/r",
		Host:       "h",
		Count:      2,
		RunnerMode: config.RunnerModeContainer,
	}

	if err := m.Remove(h, rc); err != nil {
		t.Fatalf("Remove(container): unexpected error: %v", err)
	}
	if chainedCalls != rc.Count {
		t.Errorf("chained teardown calls = %d, want %d (one per instance)", chainedCalls, rc.Count)
	}
}

// TestManagerRemove_propagatesRemoveContainerError verifies the container
// branch surfaces the underlying removeContainer error wrapped with
// "removing <name>: %w". We force the chained teardown to fail on the
// first instance; the orchestrator must return immediately.
func TestManagerRemove_propagatesRemoveContainerError(t *testing.T) {
	t.Parallel()

	ts := removalTokenStubServer(t, http.StatusInternalServerError, "")

	sentinel := errors.New("ssh connection reset")
	var visits int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker stop") && strings.Contains(cmd, "docker rm -f") {
				visits++
				return "", sentinel
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Addr: "runner@vps", Arch: "amd64"})
	h.SetConn(mock)

	m := &Manager{
		GitHub: NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL),
		Out:    io.Discard,
	}
	rc := config.RunnerConfig{
		Name:       "aw",
		Repo:       "o/r",
		Host:       "h",
		Count:      3,
		RunnerMode: config.RunnerModeContainer,
	}

	err := m.Remove(h, rc)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Remove error = %v, want sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "removing aw-1") {
		t.Errorf("Remove error must wrap with 'removing <name>: %%w', got: %v", err)
	}
	if visits != 1 {
		t.Errorf("early-exit visits = %d, want 1 (one instance before failure)", visits)
	}
}

// TestManagerStatus_nativeIteratesAllInstances covers Manager.Status for
// the native dispatch path: one RunnerStatus per InstanceNames() entry,
// each with Local populated via statusNativeAndVersion. We pin the
// hoist-invariant behaviour (Host/Repo/Labels/Mode match EffectiveRunnerMode
// / DisplayTarget / EffectiveLabelsForInstance / Host).
func TestManagerStatus_nativeIteratesAllInstances(t *testing.T) {
	t.Parallel()

	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			// linuxInstanceProbe combined probe (svc.sh + autostart + version).
			if strings.Contains(cmd, "svc.sh") && strings.Contains(cmd, ".config/systemd/user/") {
				return "U\n1.2.3\n", nil
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(mock)

	m := NewManager("")
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 2}

	statuses, err := m.Status(h, rc)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2 (one per instance)", len(statuses))
	}
	for i, s := range statuses {
		want := "ci-" + itoa(i+1)
		if s.Instance != want {
			t.Errorf("statuses[%d].Instance = %q, want %q", i, s.Instance, want)
		}
		if s.Host != "h" {
			t.Errorf("statuses[%d].Host = %q, want %q", i, s.Host, "h")
		}
		if s.Repo != "o/r" {
			t.Errorf("statuses[%d].Repo = %q, want %q", i, s.Repo, "o/r")
		}
		if s.Mode != "native" {
			t.Errorf("statuses[%d].Mode = %q, want native", i, s.Mode)
		}
		// ContainerImage fields stay empty in native mode.
		if s.ContainerImage != "" {
			t.Errorf("statuses[%d].ContainerImage = %q, want empty in native mode", i, s.ContainerImage)
		}
	}
}

// TestManagerStatus_containerPopulatesImageAndBuild covers Manager.Status
// for the container dispatch path: each RunnerStatus carries
// Local + ContainerImage + ContainerImageRevision via containerLocalStatusImageAndRevision,
// and ContainerImageBuild is computed via formatContainerImageBuild against
// the hoisted ContainerImageLayoutRevision expected value. We verify both
// the matching ("ok (<rev>)") and stale ("stale (<rev>)") branches in one
// run by returning two different revisions for two instances.
func TestManagerStatus_containerPopulatesImageAndBuild(t *testing.T) {
	t.Parallel()

	// Pin GhSrVersion + no extra apt so ContainerImageLayoutRevision is
	// stable across the two instances (and we can assert the exact
	// expected value). Compute it once before wiring the mock so the
	// RunFn closure can drive instance 1 with the matching revision
	// → "ok (<short>)".
	m := &Manager{
		GitHub:                 NewGitHubClient(""),
		GhSrVersion:            "1.2.3",
		ContainerImageExtraApt: nil,
		Out:                    io.Discard,
	}
	expected := ContainerImageLayoutRevision(m.GhSrVersion, m.containerImageExtraApt())
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			// containerLocalStatusOneShot runs its full pipeline as one
			// h.Run command. The mock cannot execute the shell script, so
			// we simulate the script's stdout for each instance:
			//
			//   script output for instance N (override="" path):
			//     `|<configImage>|<digest>|<imageRev>\n`
			//
			// parseContainerStatusInspectOutput splits on `|` (status is
			// the empty leading field, image = configImage, imageRev =
			// the trailing field) and falls through to local="stopped"
			// because status=="". Match by container name.
			if strings.Contains(cmd, "gh-sr-aw-1") {
				return "|gh-sr/agentic-runner:v1|sha256:ok|" + expected + "\n", nil
			}
			if strings.Contains(cmd, "gh-sr-aw-2") {
				return "|gh-sr/agentic-runner:v1|sha256:stale|stale\n", nil
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "runner@vps"})
	h.SetConn(mock)

	rc := config.RunnerConfig{
		Name:       "aw",
		Repo:       "o/r",
		Host:       "h",
		Count:      2,
		RunnerMode: config.RunnerModeContainer,
	}

	// containerLocalStatusOneShot issues two `docker` calls per
	// instance (combined into a single shell script passed to h.Run):
	//   1. `docker inspect --format '{{.State.Status}}|{{.Config.Image}}|{{.Image}}' <cid>`
	//      → status|configImage|digest.
	//   2. `docker image inspect <digest> --format '{{index .Config.Labels ...}}'`
	//      → the gh-sr.image-revision label.
	// The mock simulates the script's stdout directly.

	statuses, err := m.Status(h, rc)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(statuses))
	}

	// First instance: revision matches expected → "ok (<short>)".
	if statuses[0].ContainerImage != "gh-sr/agentic-runner:v1" {
		t.Errorf("statuses[0].ContainerImage = %q, want %q",
			statuses[0].ContainerImage, "gh-sr/agentic-runner:v1")
	}
	if statuses[0].ContainerImageBuild != "ok ("+shortHead(expected)+")" {
		t.Errorf("statuses[0].ContainerImageBuild = %q, want %q",
			statuses[0].ContainerImageBuild, "ok ("+shortHead(expected)+")")
	}
	// containerLocalStatusOneShot's printf prepends an empty "override" field
	// (the bootstrap-failed marker is absent on a clean install), so
	// parseContainerStatusInspectOutput sees status="" and falls through to
	// "stopped" rather than "running". Pin the actual value to surface any
	// future change to the parser.
	if statuses[0].Local != "stopped" {
		t.Errorf("statuses[0].Local = %q, want %q (parseContainerStatusInspectOutput fall-through)",
			statuses[0].Local, "stopped")
	}
	if statuses[0].Mode != "container" {
		t.Errorf("statuses[0].Mode = %q, want container", statuses[0].Mode)
	}

	// Second instance: stale revision → "stale (<short>)".
	if statuses[1].ContainerImageBuild != "stale (stale)" {
		t.Errorf("statuses[1].ContainerImageBuild = %q, want %q",
			statuses[1].ContainerImageBuild, "stale (stale)")
	}
}

// TestManagerStatus_containerNotInstalledBuildIsDash covers the
// formatContainerImageBuild short-circuit for local == "not installed":
// the build cell renders "-" regardless of the image-revision field.
func TestManagerStatus_containerNotInstalledBuildIsDash(t *testing.T) {
	t.Parallel()

	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker inspect") {
				return "not installed|||\n", nil
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "runner@vps"})
	h.SetConn(mock)

	m := &Manager{
		GitHub:      NewGitHubClient(""),
		GhSrVersion: "1.2.3",
		Out:         io.Discard,
	}
	rc := config.RunnerConfig{
		Name:       "aw",
		Repo:       "o/r",
		Host:       "h",
		Count:      1,
		RunnerMode: config.RunnerModeContainer,
	}

	statuses, err := m.Status(h, rc)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	if statuses[0].Local != "not installed" {
		t.Errorf("statuses[0].Local = %q, want not installed", statuses[0].Local)
	}
	if statuses[0].ContainerImageBuild != "-" {
		t.Errorf("statuses[0].ContainerImageBuild = %q, want \"-\" (formatContainerImageBuild short-circuit)",
			statuses[0].ContainerImageBuild)
	}
}

// TestManagerLogs_dispatchesToLogsNative covers Manager.Logs native dispatch.
// We assert that the underlying tail -50 <dir>/runner.log command is issued
// and its stdout is returned verbatim.
func TestManagerLogs_dispatchesToLogsNative(t *testing.T) {
	t.Parallel()

	const want = "2026-07-18 [INFO] test log line\n"
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "tail -50") && strings.Contains(cmd, "runner.log") {
				return want, nil
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(mock)

	m := NewManager("")
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 1}

	got, err := m.Logs(h, rc, "ci-1")
	if err != nil {
		t.Fatalf("Logs error: %v", err)
	}
	if got != want {
		t.Errorf("Logs = %q, want %q", got, want)
	}
}

// TestManagerLogs_dispatchesToLogsContainer covers Manager.Logs container
// dispatch. We assert the `docker logs --tail 100 <name>` command is
// issued for the requested instance.
func TestManagerLogs_dispatchesToLogsContainer(t *testing.T) {
	t.Parallel()

	const want = "container log line\n"
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker logs --tail 100") && strings.Contains(cmd, "gh-sr-aw-1") {
				return want, nil
			}
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "runner@vps"})
	h.SetConn(mock)

	m := NewManager("")
	rc := config.RunnerConfig{
		Name:       "aw",
		Repo:       "o/r",
		Host:       "h",
		Count:      1,
		RunnerMode: config.RunnerModeContainer,
	}

	got, err := m.Logs(h, rc, "aw-1")
	if err != nil {
		t.Fatalf("Logs error: %v", err)
	}
	if got != want {
		t.Errorf("Logs = %q, want %q", got, want)
	}
}

// TestManagerLogs_propagatesContainerLogsError covers the error wrapping
// contract on the container dispatch path. logsContainer wraps the
// underlying h.Run error with "fetching logs for container <name>: %w".
func TestManagerLogs_propagatesContainerLogsError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("docker daemon unreachable")
	mock := &testutil.MockExecutor{
		RunErr: sentinel,
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "runner@vps"})
	h.SetConn(mock)

	m := NewManager("")
	rc := config.RunnerConfig{
		Name:       "aw",
		Repo:       "o/r",
		Host:       "h",
		Count:      1,
		RunnerMode: config.RunnerModeContainer,
	}

	_, err := m.Logs(h, rc, "aw-1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Logs error = %v, want sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "fetching logs for container") {
		t.Errorf("Logs error must wrap with 'fetching logs for container <name>: %%w', got: %v", err)
	}
}

// itoa avoids pulling strconv into this test file for a single use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// shortHead mirrors formatContainerImageBuild's 8-char prefix.
func shortHead(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// Compile-time guard: ensure Manager.Out wiring still works when the
// orchestrator writes through it. Not strictly an orchestrator test, but
// it pins the io.Writer fallback used by Remove/Status/Logs when callers
// leave Manager.Out nil.
func TestManagerOut_nilDefaultsToStdout(t *testing.T) {
	t.Parallel()
	m := &Manager{}
	if m.out() == nil {
		t.Fatalf("Manager{}.out() = nil, want non-nil (os.Stdout fallback)")
	}
	var buf bytes.Buffer
	m.Out = &buf
	if m.out() != &buf {
		t.Fatalf("Manager{Out: &buf}.out() did not return &buf")
	}
	// nil receiver still returns os.Stdout (m != nil guard in out()).
	if (*Manager)(nil).out() == nil {
		t.Fatalf("(*Manager)(nil).out() = nil, want os.Stdout fallback")
	}
}
