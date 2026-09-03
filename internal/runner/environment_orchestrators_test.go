package runner

import (
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

// combinedGitHubStub returns an httptest server that handles both the
// "releases/latest" URL (GetLatestRunnerVersion) and the
// "actions/runners/registration-token" URL (GetRegistrationTokenScoped).
// This matches the production GitHubClient which uses a single apiBase URL
// for every endpoint. Tests that want to exercise a failure branch flip the
// corresponding status to a non-OK value.
func combinedGitHubStub(t *testing.T, versionStatus int, version string, regTokenStatus int, regToken string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/actions/runner/releases/latest"):
			if versionStatus != http.StatusOK {
				http.Error(w, "stub", versionStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(releaseResponse{TagName: version})
		case strings.HasSuffix(r.URL.Path, "/actions/runners/registration-token"):
			if regTokenStatus != http.StatusOK {
				http.Error(w, "stub", regTokenStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(tokenResponse{Token: regToken})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// envTestRig wires a Linux host backed by the supplied MockExecutor, a GitHub
// stub returning v1.0.0 for the version + the supplied status for the
// registration token. The Manager's Out is discarded so the tests can assert
// h.Run side effects without polluting test output.
func envTestRig(t *testing.T, mock host.Executor, regTokStatus int) *ContainerEnvironment {
	t.Helper()
	ts := combinedGitHubStub(t, http.StatusOK, "v1.0.0", regTokStatus, "reg")

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
		Count:      1,
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}
	return m.NewContainerEnvironment(h, rc, 0, "aw-1")
}

// TestContainerEnvironmentProvision_nonLinuxReturnsError pins the early
// guard: ContainerEnvironment.Provision refuses non-Linux hosts with a clear
// "container is only supported on Linux hosts" message and does NOT shell
// out to the host at all (no docker inspect, no docker create).
func TestContainerEnvironmentProvision_nonLinuxReturnsError(t *testing.T) {
	t.Parallel()

	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			t.Errorf("Provision(non-linux) must not shell out, got: %s", cmd)
			return "", nil
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "darwin", Addr: "local", Arch: "amd64"})
	h.SetConn(mock)

	m := &Manager{Out: io.Discard}
	rc := config.RunnerConfig{Name: "aw", Repo: "o/r", Host: "h", Count: 1, Profile: "agentic", RunnerMode: config.RunnerModeContainer}
	env := m.NewContainerEnvironment(h, rc, 0, "aw-1")

	err := env.Provision()
	if err == nil {
		t.Fatalf("Provision(non-linux) = nil, want error")
	}
	if !strings.Contains(err.Error(), "container is only supported on Linux hosts") {
		t.Errorf("Provision(non-linux) error = %q, want Linux-only message", err)
	}
}

// TestContainerEnvironmentProvision_alreadyPresentShortCircuits pins the
// container-already-present short-circuit: when the docker inspect probe
// returns yes, Provision must skip version resolve, image build, and
// container create (no further h.Run calls reach the mock).
func TestContainerEnvironmentProvision_alreadyPresentShortCircuits(t *testing.T) {
	t.Parallel()

	var probeOnly int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker inspect --format=") && strings.Contains(cmd, "gh-sr-aw-1") {
				probeOnly++
				return "yes\n", nil
			}
			t.Errorf("Provision(present) must short-circuit after probe, got: %s", cmd)
			return "", nil
		},
	}
	env := envTestRig(t, mock, http.StatusOK)

	if err := env.Provision(); err != nil {
		t.Fatalf("Provision(present) error: %v", err)
	}
	if probeOnly != 1 {
		t.Errorf("probe calls = %d, want 1 (presence check only)", probeOnly)
	}
}

// TestContainerEnvironmentProvision_propagatesBuildError pins the
// buildRunnerImageIfMissing failure branch: when the image-exists probe
// returns "no" and the image-build sequence fails, Provision must surface
// the wrapped build error verbatim.
func TestContainerEnvironmentProvision_propagatesBuildError(t *testing.T) {
	t.Parallel()

	buildSentinel := errors.New("docker build exploded")
	var imageProbeDone bool
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "docker inspect --format=") && strings.Contains(cmd, "gh-sr-aw-1"):
				return "no\n", nil // container not present → proceed
			case strings.Contains(cmd, "docker image inspect") && !imageProbeDone:
				imageProbeDone = true
				return "no\n", nil // image not present → must build
			case strings.Contains(cmd, "docker build") || strings.Contains(cmd, "GHSR_EOF"):
				return "", buildSentinel
			default:
				return "", nil
			}
		},
	}
	env := envTestRig(t, mock, http.StatusOK)

	err := env.Provision()
	if !errors.Is(err, buildSentinel) {
		t.Fatalf("Provision(build-error) = %v, want sentinel %v", err, buildSentinel)
	}
	if !strings.Contains(err.Error(), "building container runner image") {
		t.Errorf("Provision(build-error) must wrap with 'building container runner image: %%w', got: %v", err)
	}
}

// TestContainerEnvironmentProvision_happyPathIssuesDockerCreate pins the
// full Provision flow: probe (no) → image probe (no) → image build (ok) →
// docker create. We assert the create call is issued with the expected
// container name.
func TestContainerEnvironmentProvision_happyPathIssuesDockerCreate(t *testing.T) {
	t.Parallel()

	var sawCreate bool
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "docker inspect --format=") && strings.Contains(cmd, "gh-sr-aw-1"):
				return "no\n", nil
			case strings.Contains(cmd, "docker image inspect"):
				return "no\n", nil
			case strings.Contains(cmd, "docker create") && strings.Contains(cmd, "--name"):
				sawCreate = true
				return "", nil
			default:
				return "", nil
			}
		},
	}
	env := envTestRig(t, mock, http.StatusOK)

	if err := env.Provision(); err != nil {
		t.Fatalf("Provision(happy) error: %v", err)
	}
	if !sawCreate {
		t.Errorf("Provision(happy) did not issue docker create")
	}
}

// TestContainerEnvironmentStart_delegatesToStartContainer pins the Start
// contract: the wrapped `docker start gh-sr-<name>` script (chained with
// rm -f bootstrap markers) must reach the host, and a successful run must
// not wrap an error.
func TestContainerEnvironmentStart_delegatesToStartContainer(t *testing.T) {
	t.Parallel()

	var sawStart bool
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker start") && strings.Contains(cmd, "gh-sr-aw-1") {
				sawStart = true
			}
			return "", nil
		},
	}
	env := envTestRig(t, mock, http.StatusOK)

	if err := env.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if !sawStart {
		t.Errorf("Start did not issue docker start for gh-sr-aw-1")
	}
}

// TestContainerEnvironmentStart_propagatesStartContainerError pins the
// error-wrapping contract: when startContainer's h.Run fails, Start must
// surface it wrapped with "starting container <name>: %w".
func TestContainerEnvironmentStart_propagatesStartContainerError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ssh connection reset")
	mock := &testutil.MockExecutor{
		RunErr: sentinel,
	}
	env := envTestRig(t, mock, http.StatusOK)

	err := env.Start()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Start error = %v, want sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "starting container") {
		t.Errorf("Start error must wrap with 'starting container <name>: %%w', got: %v", err)
	}
}

// TestContainerEnvironmentReset_runsJobCompletedHook pins the Reset contract:
// the h.Run command must be `docker exec "gh-sr-<name>" /opt/gh-sr/hooks/job-completed.sh 2>/dev/null || true`.
// We assert the container name + the inner hook script, and confirm a
// successful run does not wrap an error.
func TestContainerEnvironmentReset_runsJobCompletedHook(t *testing.T) {
	t.Parallel()

	var sawReset bool
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker exec") &&
				strings.Contains(cmd, "gh-sr-aw-1") &&
				strings.Contains(cmd, "/opt/gh-sr/hooks/job-completed.sh") {
				sawReset = true
			}
			return "", nil
		},
	}
	env := envTestRig(t, mock, http.StatusOK)

	if err := env.Reset(); err != nil {
		t.Fatalf("Reset error: %v", err)
	}
	if !sawReset {
		t.Errorf("Reset did not issue the job-completed.sh docker exec hook")
	}
}

// TestContainerEnvironmentReset_propagatesHRunError pins the error contract:
// when the underlying h.Run fails, Reset must surface the error directly
// (no extra wrapping — Reset is a thin pass-through to h.Run).
func TestContainerEnvironmentReset_propagatesHRunError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("docker daemon unreachable")
	mock := &testutil.MockExecutor{
		RunErr: sentinel,
	}
	env := envTestRig(t, mock, http.StatusOK)

	err := env.Reset()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Reset error = %v, want sentinel %v", err, sentinel)
	}
}

// TestContainerEnvironmentDestroy_delegatesToRemoveContainer pins the
// Destroy contract: the chained `docker stop ... docker rm -f ... rm -rf
// "$HOME/.gh-sr/runners/<name>"` script must reach the host and succeed.
//
// We pin the orchestrator delegation, not removeContainer's internals
// (covered by TestManagerRemove_dispatchesToRemoveContainerForEachInstance
// and friends in runner_remove_status_logs_test.go).
func TestContainerEnvironmentDestroy_delegatesToRemoveContainer(t *testing.T) {
	t.Parallel()

	var sawChainedTeardown bool
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker stop") &&
				strings.Contains(cmd, "docker rm -f") &&
				strings.Contains(cmd, "gh-sr-aw-1") {
				sawChainedTeardown = true
			}
			return "", nil
		},
	}
	// 500 on registration-token endpoint → the best-effort deregister
	// inside removeContainer is skipped, leaving only the chained
	// teardown call to inspect.
	env := envTestRig(t, mock, http.StatusInternalServerError)

	if err := env.Destroy(); err != nil {
		t.Fatalf("Destroy error: %v", err)
	}
	if !sawChainedTeardown {
		t.Errorf("Destroy did not issue the chained docker stop + docker rm -f teardown")
	}
}

// TestContainerEnvironmentDestroy_propagatesRemoveContainerError pins the
// error-wrapping contract: when the chained teardown fails, Destroy must
// surface the error wrapped with "removing container <name>: %w".
func TestContainerEnvironmentDestroy_propagatesRemoveContainerError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ssh connection reset")
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker stop") {
				return "", sentinel
			}
			return "", nil
		},
	}
	env := envTestRig(t, mock, http.StatusInternalServerError)

	err := env.Destroy()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Destroy error = %v, want sentinel %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "removing container") {
		t.Errorf("Destroy error must wrap with 'removing container <name>: %%w', got: %v", err)
	}
}
