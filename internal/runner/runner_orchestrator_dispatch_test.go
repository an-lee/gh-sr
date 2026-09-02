package runner

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

// probeTailYes is the tail NativeRunnerConfigPresent and containerRunnerPresent
// append on Linux/POSIX hosts via RemoteBoolCheck: " && echo yes || echo no".
// The mock recognises this suffix so per-instance presence/absence can be
// answered without each test hand-coding the probe prefix.
const probeTailYes = "&& echo yes || echo no"

// needsSetupMockHost builds a Linux host backed by a fresh MockExecutor so
// each test can register independent per-cmd responses.
func needsSetupMockHost(t *testing.T, mock *testutil.MockExecutor) *host.Host {
	t.Helper()
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	return h
}

// answerInstancePresence returns a MockExecutor.RunFn that replies "yes" when
// the cmd references any of present and "no" for any other; it recognises
// both the native probe (test -d .../run.sh/.runner) and the container
// probes (docker inspect per-instance legacy, and the one-shot
// `docker ps -a --filter name=gh-sr- --format '{{.Names}}' | sed ...`
// added when container-presence was collapsed into a single round-trip).
func answerInstancePresence(present map[string]bool) func(string) (string, error) {
	return func(cmd string) (string, error) {
		// Container one-shot probe (containersPresentOneShot):
		// `docker ps -a --filter name=gh-sr- --format '{{.Names}}' | sed ...`
		// Returns one stripped instance name per line for every present entry.
		if strings.Contains(cmd, "docker ps -a --filter name=gh-sr-") {
			var lines []string
			for inst, ok := range present {
				if ok {
					lines = append(lines, inst)
				}
			}
			sort.Strings(lines)
			return strings.Join(lines, "\n") + "\n", nil
		}
		// Container probe: docker inspect --format='{{.Name}}' gh-sr-<name>
		for inst, ok := range present {
			if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "gh-sr-"+inst+" ") {
				if ok {
					return "yes\n", nil
				}
				return "no\n", nil
			}
		}
		// Native probe: test -d $HOME/.gh-sr/runners/<name> && test -f run.sh && test -f .runner
		for inst, ok := range present {
			if strings.Contains(cmd, ".gh-sr/runners/"+inst+" ") && strings.Contains(cmd, probeTailYes) {
				if ok {
					return "yes\n", nil
				}
				return "no\n", nil
			}
			// Also catch the case where inst is the trailing token.
			if strings.Contains(cmd, ".gh-sr/runners/"+inst+"/") && strings.Contains(cmd, probeTailYes) {
				if ok {
					return "yes\n", nil
				}
				return "no\n", nil
			}
		}
		return "no\n", nil
	}
}

// TestNeedsSetup_nativeAllPresent_returnsFalse covers the steady-state
// contract: when every instance for a native rc has a fully-installed runner
// directory (run.sh and .runner present), NeedsSetup must return false so
// EnsureSetup skips Setup without re-running the registration.
func TestNeedsSetup_nativeAllPresent_returnsFalse(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunFn: answerInstancePresence(map[string]bool{
		"ci-1": true,
		"ci-2": true,
	})}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 2}

	if m.NeedsSetup(h, rc) {
		t.Fatalf("NeedsSetup = true, want false (all instances present); calls=%v", mock.Calls)
	}
}

// TestNeedsSetup_nativeMissing_returnsTrue covers the inverse: when any
// instance is missing, NeedsSetup must return true so EnsureSetup triggers
// Setup. This is the most common state during `gh sr up` on a fresh host.
func TestNeedsSetup_nativeMissing_returnsTrue(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunFn: answerInstancePresence(map[string]bool{
		"ci-1": true,
		"ci-2": false,
	})}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 2}

	if !m.NeedsSetup(h, rc) {
		t.Fatalf("NeedsSetup = false, want true (ci-2 missing); calls=%v", mock.Calls)
	}
}

// TestNeedsSetup_nativeCountClampedToAtLeastOne covers the Count=0 edge:
// InstanceNames() falls back to ["<name>-1"], so NeedsSetup must evaluate
// (and return the correct presence value for) that single instance rather
// than vacuously returning false on an empty slice.
func TestNeedsSetup_nativeCountClampedToAtLeastOne(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunFn: answerInstancePresence(map[string]bool{
		"ci-1": false,
	})}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 0}

	if !m.NeedsSetup(h, rc) {
		t.Fatalf("NeedsSetup = false, want true (Count=0 still triggers single ci-1 install); calls=%v", mock.Calls)
	}
}

// TestNeedsSetup_nativePropagatesCheckSentinelError checks that when the
// native probe surfaces a connection error (not just "no"), NeedsSetup
// still treats the instance as not-installed (matching the existing
// `ok, _ := NativeRunnerConfigPresent(...)` swallow-and-continue pattern).
func TestNeedsSetup_nativePropagatesCheckSentinelError(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunErr: io.ErrUnexpectedEOF}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 1}

	if !m.NeedsSetup(h, rc) {
		t.Fatalf("NeedsSetup = false, want true (probe error must be treated as missing install)")
	}
}

// TestNeedsSetup_containerAllPresent_returnsFalse covers the container
// dispatch path: when the instance's Docker container exists, NeedsSetup
// must not trigger Setup. This pins the !IsContainerMode vs IsContainerMode
// branch in NeedsSetup that the native-mode tests do not exercise.
func TestNeedsSetup_containerAllPresent_returnsFalse(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunFn: answerInstancePresence(map[string]bool{
		"rune-1": true,
		"rune-2": true,
	})}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{
		Name:       "rune",
		Repo:       "o/r",
		Host:       "h",
		Count:      2,
		RunnerMode: config.RunnerModeContainer,
	}

	if m.NeedsSetup(h, rc) {
		t.Fatalf("NeedsSetup = true, want false (all containers present); calls=%v", mock.Calls)
	}
}

// TestNeedsSetup_containerMissing_returnsTrue covers the container-mode
// analogue of native-missing: any container whose Docker container does
// not exist must trigger Setup.
func TestNeedsSetup_containerMissing_returnsTrue(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunFn: answerInstancePresence(map[string]bool{
		"rune-1": true,
		"rune-2": false,
	})}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{
		Name:       "rune",
		Repo:       "o/r",
		Host:       "h",
		Count:      2,
		RunnerMode: config.RunnerModeContainer,
	}

	if !m.NeedsSetup(h, rc) {
		t.Fatalf("NeedsSetup = false, want true (rune-2 container missing); calls=%v", mock.Calls)
	}
}

// TestRebuildImage_skipsNativeRunner covers the native-mode short-circuit:
// RebuildImage must NOT call rebuildContainerImage and must log a clear
// "skipping rebuild (not runner_mode: container)" message. The mock
// host's Run function explodes if it gets called, which would surface any
// accidental rebuild attempt.
func TestRebuildImage_skipsNativeRunner(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	var buf bytes.Buffer
	m.Out = &buf
	mock := &testutil.MockExecutor{
		RunFn: func(string) (string, error) {
			t.Fatalf("RebuildImage on native runner must not invoke the host, got call")
			return "", nil
		},
	}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{Name: "ci", Repo: "o/r", Host: "h", Count: 1}

	if err := m.RebuildImage(h, rc); err != nil {
		t.Fatalf("RebuildImage native: unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "skipping rebuild") {
		t.Fatalf("expected 'skipping rebuild' message, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "ci") {
		t.Fatalf("expected instance name in message, got %q", buf.String())
	}
}

// TestRebuildImage_containerModeDispatchesToRebuildContainerImage covers
// the container-mode branch: RebuildImage must reach rebuildContainerImage.
// We force rebuildContainerImage to fail fast by returning 500 from the
// GitHub release endpoint, so the test does not have to mock the entire
// docker image build pipeline — only the dispatch is under test.
func TestRebuildImage_containerModeDispatchesToRebuildContainerImage(t *testing.T) {
	t.Parallel()

	sawContainerProbe := false
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			// The first stop+rm chain in rebuildContainerImage is the
			// earliest host-side call after the GitHub version resolve,
			// so spotting it confirms the dispatch reached
			// rebuildContainerImage.
			if strings.Contains(cmd, "docker stop") && strings.Contains(cmd, "docker rm -f") {
				sawContainerProbe = true
			}
			return "", nil
		},
	}
	h := needsSetupMockHost(t, mock)

	// Stub the GitHub client so GetLatestRunnerVersion returns an error
	// and rebuildContainerImage exits after the per-instance stop+rm
	// chain (the dispatch is what we care about, not the full rebuild).
	stub := newFailingGitHubVersionClient(t)

	m := &Manager{GitHub: stub, Out: io.Discard}
	rc := config.RunnerConfig{
		Name:       "rune",
		Repo:       "o/r",
		Host:       "h",
		Count:      1,
		RunnerMode: config.RunnerModeContainer,
	}

	_ = m.RebuildImage(h, rc)
	if !sawContainerProbe {
		t.Fatalf("RebuildImage(container) did not invoke the docker stop+rm teardown; calls=%v", mock.Calls)
	}
}

// newFailingGitHubVersionClient returns a GitHubClient whose release-version
// lookup is wired to an httptest server that 500s. Everything else (and any
// other client method) is the zero-value default, but RebuildImage →
// rebuildContainerImage only requires GetLatestRunnerVersion before the
// first host call, so this is sufficient.
func newFailingGitHubVersionClient(t *testing.T) *GitHubClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "stub", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	return NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)
}
