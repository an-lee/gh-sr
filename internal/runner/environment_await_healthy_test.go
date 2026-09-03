package runner

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

// fakeClock is a deterministic time source for the containerAwaitHealthy
// polling loop. nowFn returns the current fake time; sleepFn advances it
// by the requested duration without blocking. Tests install these via
// installFakeClock and restore the real clock with t.Cleanup.
//
// The fake clock MUST NOT be used in tests that run with t.Parallel(): the
// package-level nowFn / sleepFn are global, and parallel tests would race on
// them — the same constraint that t.Setenv-based seams (e.g. HOME in the
// diskschedule tests) impose.
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) nowFn() time.Time        { return f.now }
func (f *fakeClock) sleepFn(d time.Duration) { f.now = f.now.Add(d) }

// installFakeClock swaps the package-level nowFn / sleepFn for a fake clock
// anchored at start, returning the clock so tests can assert the loop
// actually iterated. Real clock is restored on t.Cleanup.
func installFakeClock(t *testing.T, start time.Time) *fakeClock {
	t.Helper()
	prevNow, prevSleep := nowFn, sleepFn
	fc := &fakeClock{now: start}
	nowFn = fc.nowFn
	sleepFn = fc.sleepFn
	t.Cleanup(func() {
		nowFn = prevNow
		sleepFn = prevSleep
	})
	return fc
}

// fakeHost returns a host backed by the supplied mock. The instance name and
// container name are pinned to "aw-1" / "gh-sr-aw-1" so assertions can match
// the exact strings containerAwaitHealthy puts into its error messages.
func fakeHost(t *testing.T, mock host.Executor) *host.Host {
	t.Helper()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Addr: "runner@vps", Arch: "amd64"})
	h.SetConn(mock)
	return h
}

// readinessMock returns a mock executor that recognises the readiness signals
// the production code probes for. stateAnswer drives the `docker inspect`
// probe; the inner-dockerd + registered signals come from the same combined
// `docker exec` script as production (echo dockerd-ok / echo ok).
//
// The returned mock also fails the test if the production code reaches a
// command we did not anticipate — that catches regressions that emit extra
// probes (e.g. an accidental per-iteration image inspect or a resurrected
// DNS gate).
func readinessMock(t *testing.T, stateAnswer, dockerd, registered string) *testutil.MockExecutor {
	t.Helper()
	return &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "docker inspect --format"):
				return stateAnswer + "\n", nil
			case strings.Contains(cmd, "docker exec") &&
				strings.Contains(cmd, "docker info") &&
				strings.Contains(cmd, "/home/runner/.runner"):
				// ProbeDinDContainerReadiness's combined inner probe.
				return dockerd + "\n" + registered + "\n", nil
			default:
				t.Errorf("readinessMock: unexpected command reached the host: %s", cmd)
				return "", nil
			}
		},
	}
}

// TestContainerAwaitHealthy_returnsNilImmediatelyWhenReady pins the success
// path: a single readiness probe reports running + dockerd-ok + registered,
// so the loop must return nil on the FIRST iteration without sleeping or
// re-probing (rootless gh-aw resolves host.docker.internal via its own
// --add-host, so there is no extra DNS gate after the triad).
func TestContainerAwaitHealthy_returnsNilImmediatelyWhenReady(t *testing.T) {
	// Not t.Parallel(): this test mutates the package-level nowFn / sleepFn
	// via installFakeClock. Parallel tests would race on those globals.

	mock := readinessMock(t, "running", "dockerd-ok", "ok")
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	fc := installFakeClock(t, start)

	if err := containerAwaitHealthy(h, "aw-1", 30*time.Second); err != nil {
		t.Fatalf("containerAwaitHealthy(ready) = %v, want nil", err)
	}
	// Sanity: the fake clock must not have advanced — the loop must have
	// returned before sleeping.
	if !fc.now.Equal(start) {
		t.Errorf("fake clock advanced from %v to %v; loop should have returned on first probe", start, fc.now)
	}
}

// TestContainerAwaitHealthy_innerDockerdNotRespondingExpiresDeadline pins
// the readiness report's InnerDockerdOK=false branch: the loop must record
// "inner dockerd not responding" as lastErr and surface it after deadline.
func TestContainerAwaitHealthy_innerDockerdNotRespondingExpiresDeadline(t *testing.T) {
	mock := readinessMock(t, "running", "no", "ok")
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	err := containerAwaitHealthy(h, "aw-1", 4*time.Second)
	if err == nil {
		t.Fatalf("containerAwaitHealthy(no-dockerd) = nil, want error")
	}
	if !strings.Contains(err.Error(), "inner dockerd not responding inside gh-sr-aw-1") {
		t.Errorf("containerAwaitHealthy(no-dockerd) = %v, want inner-dockerd message", err)
	}
}

// TestContainerAwaitHealthy_notRegisteredExpiresDeadline pins the readiness
// report's Registered=false branch: the loop must record "actions runner not
// yet registered" as lastErr and surface it after deadline. dockerd-ok is
// still true so the InnerDockerdOK branch is bypassed.
func TestContainerAwaitHealthy_notRegisteredExpiresDeadline(t *testing.T) {
	mock := readinessMock(t, "running", "dockerd-ok", "no")
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	err := containerAwaitHealthy(h, "aw-1", 4*time.Second)
	if err == nil {
		t.Fatalf("containerAwaitHealthy(no-register) = nil, want error")
	}
	if !strings.Contains(err.Error(), "actions runner not yet registered inside gh-sr-aw-1") {
		t.Errorf("containerAwaitHealthy(no-register) = %v, want not-registered message", err)
	}
}

// TestContainerAwaitHealthy_missingContainerExpiresDeadline pins the
// "missing" state branch: when the docker inspect probe returns missing, the
// loop must record "container <name> not found" and surface it after deadline.
func TestContainerAwaitHealthy_missingContainerExpiresDeadline(t *testing.T) {
	mock := readinessMock(t, "missing", "", "")
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	err := containerAwaitHealthy(h, "aw-1", 4*time.Second)
	if err == nil {
		t.Fatalf("containerAwaitHealthy(missing) = nil, want error")
	}
	if !strings.Contains(err.Error(), "container gh-sr-aw-1 not found") {
		t.Errorf("containerAwaitHealthy(missing) = %v, want not-found message", err)
	}
}

// TestContainerAwaitHealthy_emptyStateTreatedAsMissing pins the empty-state
// fallback: when the inspect probe returns "" (probe ran but produced no
// output), the loop must treat it as missing, not as a free-form state.
func TestContainerAwaitHealthy_emptyStateTreatedAsMissing(t *testing.T) {
	mock := readinessMock(t, "", "", "")
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	err := containerAwaitHealthy(h, "aw-1", 4*time.Second)
	if err == nil {
		t.Fatalf("containerAwaitHealthy(empty) = nil, want error")
	}
	if !strings.Contains(err.Error(), "container gh-sr-aw-1 not found") {
		t.Errorf("containerAwaitHealthy(empty) = %v, want not-found message", err)
	}
}

// TestContainerAwaitHealthy_exitedStateExpiresDeadline pins the default
// branch: any non-accepting state that is not "missing"/"" falls into the
// `state is %q` formatter and surfaces with the original Docker state name.
func TestContainerAwaitHealthy_exitedStateExpiresDeadline(t *testing.T) {
	mock := readinessMock(t, "exited", "", "")
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	err := containerAwaitHealthy(h, "aw-1", 4*time.Second)
	if err == nil {
		t.Fatalf("containerAwaitHealthy(exited) = nil, want error")
	}
	if !strings.Contains(err.Error(), `container gh-sr-aw-1 state is "exited"`) {
		t.Errorf("containerAwaitHealthy(exited) = %v, want state-quoted message", err)
	}
}

// TestContainerAwaitHealthy_restartingIsAccepting pins the "restarting"
// branch of IsContainerAcceptingJobs: docker reports state=restarting (a
// transient self-heal) and the inner probes are healthy, so the loop must
// return nil. Guards against a future regression that narrows the acceptance
// set to only "running" (see issue #275).
func TestContainerAwaitHealthy_restartingIsAccepting(t *testing.T) {
	mock := readinessMock(t, "restarting", "dockerd-ok", "ok")
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	if err := containerAwaitHealthy(h, "aw-1", 30*time.Second); err != nil {
		t.Fatalf("containerAwaitHealthy(restarting) = %v, want nil", err)
	}
}

// TestContainerAwaitHealthy_recoversAfterTransitions pins the multi-iteration
// happy path: the first probe sees "missing", the second sees "running" with
// inner dockerd-ok + registered, and the loop must return nil on iteration 2.
// Asserts that the polling loop actually iterates (not just returns on first
// probe) AND that lastErr from the missing iteration does not leak out.
func TestContainerAwaitHealthy_recoversAfterTransitions(t *testing.T) {
	var probeCount int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "docker inspect --format"):
				probeCount++
				if probeCount == 1 {
					return "missing\n", nil
				}
				return "running\n", nil
			case strings.Contains(cmd, "docker info") && strings.Contains(cmd, "/home/runner/.runner"):
				return "dockerd-ok\nok\n", nil
			default:
				t.Errorf("unexpected command: %s", cmd)
				return "", nil
			}
		},
	}
	h := fakeHost(t, mock)

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	if err := containerAwaitHealthy(h, "aw-1", 30*time.Second); err != nil {
		t.Fatalf("containerAwaitHealthy(recover) = %v, want nil", err)
	}
	if probeCount != 2 {
		t.Errorf("probe calls = %d, want 2 (missing then running)", probeCount)
	}
}

// TestAwaitHealthyOrchestrator_delegatesToContainerAwaitHealthy pins the
// Environment interface contract: ContainerEnvironment.AwaitHealthy must
// delegate to containerAwaitHealthy with the configured timeout and must
// surface the readiness error verbatim. We assert the deadline-expiry path
// (readiness triad never satisfied), which distinguishes a real delegation
// from a no-op stub that would return nil.
func TestAwaitHealthyOrchestrator_delegatesToContainerAwaitHealthy(t *testing.T) {
	mock := readinessMock(t, "running", "no", "ok")
	h := fakeHost(t, mock)

	// Manager is not needed by AwaitHealthy beyond what NewContainerEnvironment
	// already requires, but Out must be non-nil to avoid nil-deref in any future
	// logging additions.
	m := &Manager{Out: io.Discard}
	rc := config.RunnerConfig{Name: "aw", Repo: "o/r", Host: "h", Count: 1, RunnerMode: config.RunnerModeContainer}
	env := m.NewContainerEnvironment(h, rc, 0, "aw-1")

	start := time.Unix(1_700_000_000, 0)
	installFakeClock(t, start)

	err := env.AwaitHealthy(4 * time.Second)
	if err == nil {
		t.Fatalf("env.AwaitHealthy(no-dockerd) = nil, want error")
	}
	if !strings.Contains(err.Error(), "inner dockerd not responding inside gh-sr-aw-1") {
		t.Errorf("env.AwaitHealthy(no-dockerd) = %v, want inner-dockerd message", err)
	}
}
