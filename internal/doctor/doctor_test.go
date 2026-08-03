package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/an-lee/gh-sr/internal/agentic"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/runner"
	"github.com/an-lee/gh-sr/internal/testutil"
)

func TestExitCode(t *testing.T) {
	t.Parallel()
	if got := ExitCode(Result{Fail: 0, Warn: 0}, false); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
	if got := ExitCode(Result{Fail: 1, Warn: 0}, false); got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}
	if got := ExitCode(Result{Fail: 0, Warn: 1}, false); got != 0 {
		t.Fatalf("strict=false: expected 0, got %d", got)
	}
	if got := ExitCode(Result{Fail: 0, Warn: 1}, true); got != 1 {
		t.Fatalf("strict=true: expected 1, got %d", got)
	}
}

func TestUniqueStringsBy(t *testing.T) {
	t.Parallel()
	t.Run("ReposKeyed", func(t *testing.T) {
		t.Parallel()
		runners := []config.RunnerConfig{
			{Repo: "z/z", Host: "a"},
			{Repo: "a/b", Host: "a"},
			{Repo: "a/b", Host: "b"},
		}
		got := uniqueStringsBy(runners, func(rc config.RunnerConfig) string { return rc.Repo })
		want := []string{"a/b", "z/z"}
		if len(got) != len(want) {
			t.Fatalf("len %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})
	t.Run("HostNamesKeyedSorted", func(t *testing.T) {
		t.Parallel()
		runners := []config.RunnerConfig{
			{Host: "beta", Repo: "o/r"},
			{Host: "alpha", Repo: "o/r"},
			{Host: "beta", Repo: "o/r2"},
		}
		got := uniqueStringsBy(runners, func(rc config.RunnerConfig) string { return rc.Host })
		want := []string{"alpha", "beta"}
		if len(got) != len(want) {
			t.Fatalf("got %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})
	t.Run("SkipsEmptyKey", func(t *testing.T) {
		t.Parallel()
		// Verify the asymmetry fix: empty host names (and empty repos/orgs)
		// are now skipped uniformly across all three call sites.
		runners := []config.RunnerConfig{
			{Host: "alpha", Repo: "o/r"},
			{Host: "", Repo: "o/r"},
			{Host: "alpha", Repo: ""},
		}
		got := uniqueStringsBy(runners, func(rc config.RunnerConfig) string { return rc.Host })
		want := []string{"alpha"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("got %v, want %v", got, want)
		}
		gotRepos := uniqueStringsBy(runners, func(rc config.RunnerConfig) string { return rc.Repo })
		if len(gotRepos) != 1 || gotRepos[0] != "o/r" {
			t.Fatalf("repos: got %v, want [o/r]", gotRepos)
		}
	})
}

func TestEnsureDoctorHostOS_LocalFillsRuntimeGOOS(t *testing.T) {
	t.Parallel()
	h := host.NewHost("local", config.HostConfig{Addr: config.LocalAddr})
	if err := ensureDoctorHostOS(h, config.LocalAddr); err != nil {
		t.Fatal(err)
	}
	if h.OS != runtime.GOOS {
		t.Fatalf("got %q want %q", h.OS, runtime.GOOS)
	}
}

func TestInstallTargetsForHost(t *testing.T) {
	t.Parallel()
	native := func(rc *config.RunnerConfig) bool { return !rc.IsContainerMode() }
	container := func(rc *config.RunnerConfig) bool { return rc.IsContainerMode() }
	agentic := func(rc *config.RunnerConfig) bool { return rc.IsAgentic() }

	t.Run("NativeMode", func(t *testing.T) {
		t.Parallel()
		runners := []config.RunnerConfig{
			{Name: "a", Host: "h1", Repo: "o/r", Count: 2},
			{Name: "b", Host: "h2", Repo: "o/r", Count: 1},
		}
		got := installTargetsForHost(runners, "h1", native)
		want := [][2]string{{"a-1", "a"}, {"a-2", "a"}}
		if len(got) != len(want) {
			t.Fatalf("len %d, want %d: %#v", len(got), len(want), got)
		}
		for i := range want {
			if got[i][0] != want[i][0] || got[i][1] != want[i][1] {
				t.Fatalf("idx %d: got %#v want %#v", i, got[i], want[i])
			}
		}
		if len(installTargetsForHost(runners, "h2", native)) != 1 {
			t.Fatalf("h2 should have one target")
		}

		mixed := []config.RunnerConfig{
			{Name: "native", Host: "hx", Repo: "o/r", Count: 1},
			{Name: "din", Host: "hx", Repo: "o/r", Count: 1, RunnerMode: config.RunnerModeContainer},
		}
		gotN := installTargetsForHost(mixed, "hx", native)
		if len(gotN) != 1 || gotN[0][0] != "native-1" {
			t.Fatalf("native targets should exclude container mode: %#v", gotN)
		}
	})

	t.Run("ContainerMode", func(t *testing.T) {
		t.Parallel()
		runners := []config.RunnerConfig{
			{Name: "n", Host: "h1", Repo: "o/r", Count: 1},
			{Name: "c", Host: "h1", Repo: "o/r", Count: 2, RunnerMode: config.RunnerModeContainer},
		}
		got := installTargetsForHost(runners, "h1", container)
		want := [][2]string{{"c-1", "c"}, {"c-2", "c"}}
		if len(got) != len(want) {
			t.Fatalf("len %d want %d: %#v", len(got), len(want), got)
		}
		for i := range want {
			if got[i][0] != want[i][0] || got[i][1] != want[i][1] {
				t.Fatalf("idx %d: got %#v want %#v", i, got[i], want[i])
			}
		}
	})

	t.Run("Agentic", func(t *testing.T) {
		t.Parallel()
		runners := []config.RunnerConfig{
			{Name: "ci", Host: "h1", Repo: "o/r", Count: 1, RunnerMode: config.RunnerModeContainer},
			{Name: "ag", Host: "h1", Repo: "o/r", Count: 1, RunnerMode: config.RunnerModeContainer, Profile: "agentic"},
		}
		got := installTargetsForHost(runners, "h1", agentic)
		if len(got) != 1 || got[0][0] != "ag-1" || got[0][1] != "ag" {
			t.Fatalf("want single agentic container instance, got %#v", got)
		}

		// profile: agentic alone implies container mode (no explicit runner_mode).
		implicit := []config.RunnerConfig{
			{Name: "aw", Host: "h1", Repo: "o/r", Count: 1, Profile: "agentic"},
		}
		gotImplicit := installTargetsForHost(implicit, "h1", agentic)
		if len(gotImplicit) != 1 || gotImplicit[0][0] != "aw-1" {
			t.Fatalf("profile: agentic should resolve to container agentic target, got %#v", gotImplicit)
		}
	})

	t.Run("PredicateReceivesPointer", func(t *testing.T) {
		t.Parallel()
		// Pins the predicate signature: callers receive a pointer so they can
		// safely call pointer-receiver methods on RunnerConfig.
		var got *config.RunnerConfig
		runners := []config.RunnerConfig{
			{Name: "a", Host: "h1", Repo: "o/r", Count: 1},
		}
		installTargetsForHost(runners, "h1", func(rc *config.RunnerConfig) bool {
			got = rc
			return true
		})
		if got == nil || got.Name != "a" {
			t.Fatalf("predicate should receive a pointer to the runner, got %#v", got)
		}
	})

	t.Run("EmptyHostReturnsNil", func(t *testing.T) {
		t.Parallel()
		runners := []config.RunnerConfig{
			{Name: "a", Host: "h1", Repo: "o/r", Count: 1},
		}
		if got := installTargetsForHost(runners, "h2", native); len(got) != 0 {
			t.Fatalf("host mismatch should return empty slice, got %#v", got)
		}
	})
}

func TestRunnersForHost(t *testing.T) {
	t.Parallel()
	runners := []config.RunnerConfig{
		{Name: "a", Host: "h1", Repo: "o/one"},
		{Name: "b", Host: "h1", Repo: "o/two"},
		{Name: "c", Host: "h2", Repo: "o/one"},
	}
	got := runnersForHost(runners, "h1")
	if len(got) != 2 {
		t.Fatalf("len %d want 2: %#v", len(got), got)
	}
	if got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("got %#v", got)
	}
	if len(runnersForHost(runners, "missing")) != 0 {
		t.Fatal("expected no runners for unknown host")
	}
}

func TestHasNativeModeRunners(t *testing.T) {
	t.Parallel()
	containerOnly := []config.RunnerConfig{
		{Name: "c", Host: "h1", Repo: "o/r", RunnerMode: config.RunnerModeContainer},
		{Name: "a", Host: "h1", Repo: "o/r", Profile: "agentic"},
	}
	if hasNativeModeRunners(containerOnly) {
		t.Fatal("container-only and profile: agentic should not require native checks")
	}
	mixed := []config.RunnerConfig{
		{Name: "n", Host: "h1", Repo: "o/r"},
		{Name: "c", Host: "h1", Repo: "o/r", RunnerMode: config.RunnerModeContainer},
	}
	if !hasNativeModeRunners(mixed) {
		t.Fatal("mixed host should require native checks")
	}
}

func TestHasContainerModeRunners(t *testing.T) {
	t.Parallel()
	nativeOnly := []config.RunnerConfig{
		{Name: "n", Host: "h1", Repo: "o/r"},
	}
	if hasContainerModeRunners(nativeOnly) {
		t.Fatal("native-only should not trigger container checks")
	}
	agenticOnly := []config.RunnerConfig{
		{Name: "a", Host: "h1", Repo: "o/r", Profile: "agentic"},
	}
	if !hasContainerModeRunners(agenticOnly) {
		t.Fatal("profile: agentic should trigger container checks")
	}
}

// TestHasContainerAgenticRunners pins the helper that gates the per-host
// inner-AWF hygiene check. It must return true if (and only if) at least
// one runner on the host uses profile: agentic — which transitively
// implies container mode via EffectiveRunnerMode — and false for every
// other combination (native, container-explicit-but-not-agentic, empty).
func TestHasContainerAgenticRunners(t *testing.T) {
	t.Parallel()
	empty := []config.RunnerConfig{}
	if hasContainerAgenticRunners(empty) {
		t.Fatal("empty runner list should not trigger agentic container checks")
	}
	nativeOnly := []config.RunnerConfig{
		{Name: "n1", Host: "h1", Repo: "o/r"},
		{Name: "n2", Host: "h2", Repo: "o/r"},
	}
	if hasContainerAgenticRunners(nativeOnly) {
		t.Fatal("native-only runners should not trigger agentic container checks")
	}
	containerExplicitOnly := []config.RunnerConfig{
		{Name: "c1", Host: "h1", Repo: "o/r", RunnerMode: config.RunnerModeContainer},
	}
	if hasContainerAgenticRunners(containerExplicitOnly) {
		t.Fatal("runner_mode: container without profile: agentic should not trigger agentic hygiene checks")
	}
	// Agentic on a different host must not affect the host-scoped decision.
	agenticOtherHost := []config.RunnerConfig{
		{Name: "a1", Host: "h2", Repo: "o/r", Profile: "agentic"},
	}
	// hasContainerAgenticRunners is host-agnostic — the per-host filtering
	// happens at the caller (installTargetsForHost). The helper itself
	// reports "does any runner anywhere use agentic container mode?", so
	// a runner on another host must still flip it to true.
	if !hasContainerAgenticRunners(agenticOtherHost) {
		t.Fatal("profile: agentic on any host should trigger agentic container checks (caller filters per-host)")
	}
	mixed := []config.RunnerConfig{
		{Name: "n1", Host: "h1", Repo: "o/r"},
		{Name: "a1", Host: "h1", Repo: "o/r", Profile: "agentic"},
	}
	if !hasContainerAgenticRunners(mixed) {
		t.Fatal("mixed list containing an agentic runner should trigger agentic container checks")
	}
}

type failIfRunExec struct {
	t *testing.T
}

func (f failIfRunExec) Run(cmd string) (string, error) {
	// The dirSizesPOSIX script now uses `du --max-depth=1` (or `du -d 1`
	// on BSD) and a `while read` parser. Match the new shape; the old
	// `du -sk` form is no longer used.
	if strings.Contains(cmd, "du --max-depth=1") || strings.Contains(cmd, "du -d 1") ||
		strings.Contains(cmd, `dir="$HOME/.gh-sr/runners/`) {
		return "0 0 0 0\n", nil
	}
	if strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`) {
		return "", nil
	}
	if strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`) {
		return "", nil
	}
	if strings.Contains(cmd, "com.github.ghsr.runner.") {
		return "", nil
	}
	f.t.Fatalf("unexpected remote command: %s", cmd)
	return "", nil
}

func (f failIfRunExec) Upload(string, string) error { return nil }
func (f failIfRunExec) Close() error                { return nil }

func TestRunHostChecks_ContainerOnlySkipsNative(t *testing.T) {
	t.Parallel()
	h := host.NewHost("mac", config.HostConfig{OS: "darwin", Addr: "user@mac"})
	h.SetConn(failIfRunExec{t: t})
	runners := []config.RunnerConfig{
		{Name: "aw", Host: "mac", Repo: "o/r", Profile: "agentic"},
	}
	var buf strings.Builder
	var r Result
	runHostChecks(&buf, "mac", h, runners, &r)
	out := buf.String()
	if strings.Contains(out, "native:") {
		t.Fatalf("container-only host should skip native checks, got:\n%s", out)
	}
}

func TestRunHostChecks_NonLinuxContainerFails(t *testing.T) {
	t.Parallel()
	h := host.NewHost("mac", config.HostConfig{OS: "darwin", Addr: "user@mac"})
	var buf strings.Builder
	var r Result
	runners := []config.RunnerConfig{
		{Name: "aw", Host: "mac", Repo: "o/r", Profile: "agentic"},
	}
	runHostChecks(&buf, "mac", h, runners, &r)
	if r.Fail != 1 {
		t.Fatalf("expected one FAIL for container mode on darwin, got %+v", r)
	}
	if !strings.Contains(buf.String(), "only supported on Linux") {
		t.Fatalf("expected linux-only container message, got:\n%s", buf.String())
	}
}

func TestFilteredHostRunners_ExcludeContainerWhenRepoFiltered(t *testing.T) {
	t.Parallel()
	filtered := []config.RunnerConfig{
		{Name: "n", Host: "h1", Repo: "o/one"},
	}
	hostRunners := runnersForHost(filtered, "h1")
	if hasContainerModeRunners(hostRunners) {
		t.Fatal("repo-filtered native-only slice should not enable container checks")
	}
	if !hasNativeModeRunners(hostRunners) {
		t.Fatal("expected native checks for filtered native runner")
	}
}

func TestRun_ConfigErrorSkipsGitHub(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cfgPath := t.TempDir() + "/missing.yml"
	envPath := t.TempDir() + "/env"
	res := Run(&buf, cfgPath, envPath, nil, assertError(t, "no config"), nil, "", "", false)
	if res.Fail < 1 {
		t.Fatalf("expected at least one FAIL, got %+v", res)
	}
	out := buf.String()
	if strings.Contains(out, "=== GitHub API ===") {
		t.Fatalf("should not reach GitHub section:\n%s", out)
	}
}

func assertError(t *testing.T, msg string) error {
	t.Helper()
	return &testErr{msg}
}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }

func TestRun_GitHubListRunnersUsesAPI(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/actions/runners") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"runners":     []any{},
		})
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		GitHub: config.GitHubConfig{},
		Hosts: map[string]config.HostConfig{
			"localh": {Addr: config.LocalAddr, OS: "linux", Arch: "amd64"},
		},
		Runners: []config.RunnerConfig{
			{Name: "only-gh", Repo: "o/r", Host: "localh", Labels: []string{"x"}},
		},
	}
	gh := runner.NewGitHubClientWithHTTP("test-token", srv.Client(), srv.URL)

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "runners.yml")
	if err := os.WriteFile(cfgPath, []byte("#"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(tmp, "env")
	if err := os.WriteFile(envPath, []byte("#"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	res := Run(&buf, cfgPath, envPath, cfg, nil, gh, "", "", false)

	out := buf.String()
	if !strings.Contains(out, "list runners OK (0 registered)") {
		t.Fatalf("expected GitHub OK line, got:\n%s", out)
	}
	// Local host probes may FAIL in CI without curl/tar; only assert GitHub section ran.
	if !strings.Contains(out, "=== GitHub API ===") {
		t.Fatalf("missing GitHub section:\n%s", out)
	}
	_ = res
}

func TestFormatOrgAPIError_permissionHint(t *testing.T) {
	t.Parallel()
	err403 := fmt.Errorf("listing runners for my-org: HTTP 403: forbidden")
	got := formatOrgAPIError("my-org", err403)
	if !strings.Contains(got, "admin:org") {
		t.Fatalf("expected admin:org hint, got: %s", got)
	}
	if !strings.Contains(got, "org my-org:") {
		t.Fatalf("expected org prefix, got: %s", got)
	}

	plain := formatOrgAPIError("my-org", fmt.Errorf("connection reset"))
	if strings.Contains(plain, "admin:org") {
		t.Fatalf("non-permission error should not get hint: %s", plain)
	}
}

func TestRun_GitHubOrgListRunnersUsesAPI(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/orgs/my-org/actions/runners") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 2,
				"runners":     []any{},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		GitHub: config.GitHubConfig{},
		Hosts: map[string]config.HostConfig{
			"localh": {Addr: config.LocalAddr, OS: "linux", Arch: "amd64"},
		},
		Runners: []config.RunnerConfig{
			{Name: "org-ci", Org: "my-org", Host: "localh", Labels: []string{"x"}},
		},
	}
	gh := runner.NewGitHubClientWithHTTP("test-token", srv.Client(), srv.URL)

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "runners.yml")
	if err := os.WriteFile(cfgPath, []byte("#"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(tmp, "env")
	if err := os.WriteFile(envPath, []byte("#"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	res := Run(&buf, cfgPath, envPath, cfg, nil, gh, "", "", false)

	out := buf.String()
	if !strings.Contains(out, "org my-org: list runners OK (0 registered)") {
		t.Fatalf("expected org GitHub OK line, got:\n%s", out)
	}
	_ = res
}

func TestRun_GitHubOrgListRunners403ShowsHint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/orgs/my-org/actions/runners") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		GitHub: config.GitHubConfig{},
		Hosts: map[string]config.HostConfig{
			"localh": {Addr: config.LocalAddr, OS: "linux", Arch: "amd64"},
		},
		Runners: []config.RunnerConfig{
			{Name: "org-ci", Org: "my-org", Host: "localh"},
		},
	}
	gh := runner.NewGitHubClientWithHTTP("test-token", srv.Client(), srv.URL)

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "runners.yml")
	if err := os.WriteFile(cfgPath, []byte("#"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(tmp, "env")
	if err := os.WriteFile(envPath, []byte("#"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	res := Run(&buf, cfgPath, envPath, cfg, nil, gh, "", "", false)

	out := buf.String()
	if !strings.Contains(out, "admin:org") {
		t.Fatalf("expected org permission hint, got:\n%s", out)
	}
	if res.Fail < 1 {
		t.Fatalf("expected FAIL for org 403, got %+v", res)
	}
}

func TestCheckRunnerDiskUsage_warnsOrphanOverThreshold(t *testing.T) {
	t.Parallel()
	h := host.NewHost("linux", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`) {
				return "orphan-1\n", nil
			}
			if strings.Contains(cmd, "du --max-depth=1") || strings.Contains(cmd, "du -d 1") {
				return fmt.Sprintf("%d 0 0 0\n", runner.DiskWarnThresholdBytes()+1), nil
			}
			return "0 0 0 0\n", nil
		},
	})
	var buf strings.Builder
	var r Result
	checkRunnerDiskUsage(&buf, "linux", h, nil, &r)
	out := buf.String()
	if !strings.Contains(out, "orphan-1 (orphan)") {
		t.Fatalf("expected orphan warning, got:\n%s", out)
	}
	if r.Warn != 1 {
		t.Fatalf("expected 1 warn, got %+v", r)
	}
}

func TestPrintAgenticFailures_SeverityError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{}
	printAgenticFailures(&buf, "h1", &r, sevWarn, "container: ", []agentic.PrereqFailure{
		{Message: "docker down", Severity: agentic.SeverityError},
	})
	if !strings.Contains(buf.String(), "FAIL  [h1          ] container: docker down") {
		t.Fatalf("missing FAIL line, got:\n%s", buf.String())
	}
	if r.Fail != 1 || r.Warn != 0 {
		t.Fatalf("counters: got %+v, want Fail=1 Warn=0", r)
	}
}

func TestPrintAgenticFailures_SeverityWarning(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{}
	printAgenticFailures(&buf, "h1", &r, sevFail, "container: ", []agentic.PrereqFailure{
		{Message: "slow", Severity: agentic.SeverityWarning},
	})
	if !strings.Contains(buf.String(), "WARN  [h1          ] container: slow") {
		t.Fatalf("missing WARN line, got:\n%s", buf.String())
	}
	if r.Fail != 0 || r.Warn != 1 {
		t.Fatalf("counters: got %+v, want Fail=0 Warn=1", r)
	}
}

func TestPrintAgenticFailures_DefaultSevFail(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{}
	printAgenticFailures(&buf, "h1", &r, sevFail, "container: ", []agentic.PrereqFailure{
		{Message: "unknown", Severity: "info"},
	})
	if !strings.Contains(buf.String(), "FAIL  [h1          ] container: unknown") {
		t.Fatalf("expected FAIL line, got:\n%s", buf.String())
	}
	if r.Fail != 1 || r.Warn != 0 {
		t.Fatalf("counters: got %+v, want Fail=1 Warn=0", r)
	}
}

func TestPrintAgenticFailures_DefaultSevWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{}
	printAgenticFailures(&buf, "h1", &r, sevWarn, "container(agent): ", []agentic.PrereqFailure{
		{Message: "unknown", Severity: "info"},
	})
	if !strings.Contains(buf.String(), "WARN  [h1          ] container(agent): unknown") {
		t.Fatalf("expected WARN line, got:\n%s", buf.String())
	}
	if r.Fail != 0 || r.Warn != 1 {
		t.Fatalf("counters: got %+v, want Fail=0 Warn=1", r)
	}
}

func TestPrintAgenticFailures_RemediationAndDocRef(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{}
	printAgenticFailures(&buf, "h1", &r, sevFail, "container: ", []agentic.PrereqFailure{
		{
			Message:     "out of disk",
			Severity:    agentic.SeverityError,
			Remediation: "rm -rf /tmp/junk\nsudo systemctl restart docker",
			DocRef:      "ops.md §7",
		},
	})
	out := buf.String()
	for _, want := range []string{
		"FAIL  [h1          ] container: out of disk",
		"       rm -rf /tmp/junk",
		"       sudo systemctl restart docker",
		"       See: ops.md §7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	if r.Fail != 1 || r.Warn != 0 {
		t.Fatalf("counters: got %+v, want Fail=1 Warn=0", r)
	}
}

func TestPrintAgenticFailures_EmptySlice(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{}
	printAgenticFailures(&buf, "h1", &r, sevFail, "container: ", nil)
	if buf.Len() != 0 {
		t.Fatalf("expected no output for empty failures, got:\n%s", buf.String())
	}
	if r.Fail != 0 || r.Warn != 0 {
		t.Fatalf("counters: got %+v, want zero", r)
	}
}

func TestPrintAgenticFailures_MixedSeverities(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	r := Result{}
	printAgenticFailures(&buf, "h1", &r, sevFail, "container(agent): ", []agentic.PrereqFailure{
		{Message: "err", Severity: agentic.SeverityError},
		{Message: "warn", Severity: agentic.SeverityWarning},
		{Message: "info", Severity: "info"},
	})
	out := buf.String()
	for _, want := range []string{
		"FAIL  [h1          ] container(agent): err",
		"WARN  [h1          ] container(agent): warn",
		"FAIL  [h1          ] container(agent): info", // defaultSev=sevFail
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	if r.Fail != 2 || r.Warn != 1 {
		t.Fatalf("counters: got %+v, want Fail=2 Warn=1", r)
	}
}

// TestCheckShellOK_OkBranch verifies checkShellOK returns true, prints the
// OK line, and leaves r.Fail untouched when trimmed stdout matches want.
func TestCheckShellOK_OkBranch(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "ok\n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	if !checkShellOK(&buf, "h1", h, &r, "true", "ok", "ready", "not ready") {
		t.Fatal("checkShellOK should return true on matching stdout")
	}
	if r.Fail != 0 {
		t.Fatalf("r.Fail should stay 0 on OK, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "ready") || strings.Contains(buf.String(), "not ready") {
		t.Fatalf("OK output should contain okMsg and not failMsg, got: %q", buf.String())
	}
}

// TestCheckShellOK_Mismatch verifies checkShellOK returns false, prints the
// FAIL line with err suffix, and increments r.Fail when trimmed stdout != want.
func TestCheckShellOK_Mismatch(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "missing\n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	if checkShellOK(&buf, "h1", h, &r, "true", "ok", "ready", "not ready") {
		t.Fatal("checkShellOK should return false on non-matching stdout")
	}
	if r.Fail != 1 {
		t.Fatalf("r.Fail should increment to 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "not ready") {
		t.Fatalf("FAIL output should contain failMsg, got: %q", buf.String())
	}
}

// TestCheckShellOK_RunError verifies checkShellOK treats a non-nil err from
// h.Run as a failure even when stdout happens to match want (defensive: err
// is a stronger signal than stdout in some failure modes, e.g. SSH closed).
func TestCheckShellOK_RunError(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn:  func(cmd string) (string, error) { return "ok", errors.New("ssh: handshake timeout") },
		RunErr: nil,
	})
	var buf bytes.Buffer
	r := Result{}
	if checkShellOK(&buf, "h1", h, &r, "true", "ok", "ready", "not ready") {
		t.Fatal("checkShellOK should return false when h.Run returns an error")
	}
	if r.Fail != 1 {
		t.Fatalf("r.Fail should increment to 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "ssh: handshake timeout") {
		t.Fatalf("FAIL output should include underlying err, got: %q", buf.String())
	}
}

// TestCheckShellOK_TrimsWhitespace verifies checkShellOK trims trailing
// newlines from h.Run output before comparing to want, matching the previous
// inline behavior at the linux/darwin call sites in checkNative.
func TestCheckShellOK_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "  ok  \n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	if !checkShellOK(&buf, "h1", h, &r, "true", "ok", "ready", "not ready") {
		t.Fatal("checkShellOK should tolerate leading/trailing whitespace")
	}
	if r.Fail != 0 {
		t.Fatalf("r.Fail should stay 0 after trim, got %d", r.Fail)
	}
}

// TestCheckNative_LinuxOk covers the linux branch of checkNative when both
// curl and tar are present. The mock returns "ok", which the conditional
// echoes from the embedded command. Asserts r.Fail stays 0 and the OK line
// names the expected dependency set.
func TestCheckNative_LinuxOk(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "ok\n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 0 {
		t.Fatalf("linux/ok: r.Fail should stay 0, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "curl and tar present") {
		t.Fatalf("linux/ok: expected OK line for curl+tar, got: %q", buf.String())
	}
}

// TestCheckNative_LinuxMissing covers the linux branch when curl or tar is
// absent (mock returns "missing"). The FAIL line must mention the missing
// dependency class so the operator knows what to install.
func TestCheckNative_LinuxMissing(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "missing\n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 1 {
		t.Fatalf("linux/missing: r.Fail should be 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "curl and tar on PATH") {
		t.Fatalf("linux/missing: expected FAIL line naming the missing deps, got: %q", buf.String())
	}
}

// TestCheckNative_DarwinOk covers the darwin branch when curl is present.
// The darwin branch uses a simpler single-binary check than linux; the mock
// only has to echo "ok".
func TestCheckNative_DarwinOk(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "darwin", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "ok\n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 0 {
		t.Fatalf("darwin/ok: r.Fail should stay 0, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "curl present") {
		t.Fatalf("darwin/ok: expected OK line for curl, got: %q", buf.String())
	}
}

// TestCheckNative_DarwinMissing covers the darwin branch when curl is absent.
// Mirrors TestCheckNative_LinuxMissing but asserts against the darwin
// failMsg wording.
func TestCheckNative_DarwinMissing(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "darwin", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "missing\n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 1 {
		t.Fatalf("darwin/missing: r.Fail should be 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "need curl") {
		t.Fatalf("darwin/missing: expected FAIL line naming the missing dep, got: %q", buf.String())
	}
}

// TestCheckNative_WindowsOk covers the windows branch when the PowerShell
// version probe returns a version string. RunShell on a local windows host
// passes through wrapCommand unchanged, so the mock receives the original
// $PSVersionTable query verbatim.
func TestCheckNative_WindowsOk(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "windows", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "7.4.1\n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 0 {
		t.Fatalf("windows/ok: r.Fail should stay 0, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "PowerShell 7.4.1") {
		t.Fatalf("windows/ok: expected OK line naming the version, got: %q", buf.String())
	}
}

// TestCheckNative_WindowsEmpty covers the windows branch when the PowerShell
// version probe returns empty output (e.g. fresh container with no profile).
// Even with a nil error, an empty trimmed version string must be treated as
// failure so the operator doesn't silently proceed with a broken runner host.
func TestCheckNative_WindowsEmpty(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "windows", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "  \n", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 1 {
		t.Fatalf("windows/empty: r.Fail should be 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "PowerShell check failed") {
		t.Fatalf("windows/empty: expected FAIL line, got: %q", buf.String())
	}
}

// TestCheckNative_WindowsError covers the windows branch when the PowerShell
// invocation itself errors (e.g. PowerShell not on PATH on the runner host).
// Mirrors the empty-output case but exercises the err != nil branch.
func TestCheckNative_WindowsError(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "windows", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "", errors.New("pwsh: not found") },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 1 {
		t.Fatalf("windows/error: r.Fail should be 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "pwsh: not found") {
		t.Fatalf("windows/error: expected FAIL line to include the underlying err, got: %q", buf.String())
	}
}

// TestCheckNative_UnknownOS covers the default branch when h.OS is set to a
// value not in {linux, darwin, windows}. The check must WARN (not FAIL)
// because an unknown OS is a soft signal — the doctor surfaces it but does
// not abort the rest of the run.
func TestCheckNative_UnknownOS(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "freebsd", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "", nil },
	})
	var buf bytes.Buffer
	r := Result{}
	checkNative(&buf, "h1", h, &r)
	if r.Fail != 0 {
		t.Fatalf("unknown-os: r.Fail should stay 0 (it's a WARN, not a FAIL), got %d", r.Fail)
	}
	if r.Warn != 1 {
		t.Fatalf("unknown-os: r.Warn should be 1, got %d", r.Warn)
	}
	if !strings.Contains(buf.String(), "unknown os") {
		t.Fatalf("unknown-os: expected WARN line, got: %q", buf.String())
	}
}

// TestCheckLinuxSudo_Root covers the early-return path when the host user is
// root (id -u returns "0"). No sudo probe is issued, no warning is emitted,
// and the function returns silently after the trimSpace check.
func TestCheckLinuxSudo_Root(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	calls := 0
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls++
			return "0\n", nil
		},
	})
	var buf bytes.Buffer
	r := Result{}
	checkLinuxSudo(&buf, "h1", h, &r)
	// Root path must issue only id -u; no follow-up sudo probe.
	if calls != 1 {
		t.Fatalf("root: expected exactly 1 SSH call (id -u), got %d", calls)
	}
	if r.Fail != 0 || r.Warn != 0 {
		t.Fatalf("root: expected no FAIL or WARN, got Fail=%d Warn=%d", r.Fail, r.Warn)
	}
	if buf.Len() != 0 {
		t.Fatalf("root: expected no output for the silent root path, got: %q", buf.String())
	}
}

// TestCheckLinuxSudo_Passwordless covers the happy path where the non-root
// user has passwordless sudo configured. The OK line must name the
// non-root user so an operator can confirm the probe matched the right
// account.
func TestCheckLinuxSudo_Passwordless(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	calls := 0
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls++
			if calls == 1 {
				return "1000\n", nil // id -u
			}
			return "ok\n", nil // sudo -n true → ok
		},
	})
	var buf bytes.Buffer
	r := Result{}
	checkLinuxSudo(&buf, "h1", h, &r)
	if r.Fail != 0 || r.Warn != 0 {
		t.Fatalf("passwordless: expected no FAIL or WARN, got Fail=%d Warn=%d", r.Fail, r.Warn)
	}
	if !strings.Contains(buf.String(), "passwordless sudo") {
		t.Fatalf("passwordless: expected OK line, got: %q", buf.String())
	}
	if calls != 2 {
		t.Fatalf("passwordless: expected exactly 2 SSH calls (id -u + sudo), got %d", calls)
	}
}

// TestCheckLinuxSudo_Missing covers the failure path where the non-root user
// does not have passwordless sudo (sudo -n true echoes "no"). A WARN must be
// emitted, the FAIL counter must stay 0 (sudo is a setup-time requirement,
// not a doctor-time blocker), and the remediation hint must point at
// gh sr setup/update.
func TestCheckLinuxSudo_Missing(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	calls := 0
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls++
			if calls == 1 {
				return "1000\n", nil // id -u
			}
			return "no\n", nil // sudo -n true → no
		},
	})
	var buf bytes.Buffer
	r := Result{}
	checkLinuxSudo(&buf, "h1", h, &r)
	if r.Warn != 1 {
		t.Fatalf("missing: r.Warn should be 1, got %d", r.Warn)
	}
	if r.Fail != 0 {
		t.Fatalf("missing: r.Fail should stay 0 (this is a WARN, not a FAIL), got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "passwordless sudo not available") {
		t.Fatalf("missing: expected WARN line, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "gh sr setup/update") {
		t.Fatalf("missing: expected WARN line to include remediation hint, got: %q", buf.String())
	}
}

// TestCheckLinuxSudo_UidError covers the error path when the id -u probe
// itself fails (e.g. SSH handshake failed before the host shell could
// respond). A WARN must be emitted and the function must return without
// attempting the sudo probe — otherwise the operator sees a misleading
// "passwordless sudo not available" instead of the real connectivity error.
func TestCheckLinuxSudo_UidError(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	calls := 0
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			calls++
			return "", errors.New("ssh: handshake failed")
		},
	})
	var buf bytes.Buffer
	r := Result{}
	checkLinuxSudo(&buf, "h1", h, &r)
	if calls != 1 {
		t.Fatalf("uid-error: expected only the id -u probe, got %d calls", calls)
	}
	if r.Warn != 1 {
		t.Fatalf("uid-error: r.Warn should be 1, got %d", r.Warn)
	}
	if !strings.Contains(buf.String(), "could not check uid") {
		t.Fatalf("uid-error: expected WARN line naming the underlying err class, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "ssh: handshake failed") {
		t.Fatalf("uid-error: expected WARN line to include the underlying err, got: %q", buf.String())
	}
}

// TestEnsureDoctorHostOS_PreSetReturns covers the early-return path: when
// h.OS is non-empty the function must NOT issue a DetectOS probe and must
// leave h.OS untouched. This pins the contract that pre-populating OS
// (e.g. from a config file) skips the round-trip.
func TestEnsureDoctorHostOS_PreSetReturns(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "user@host"})
	called := false
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			called = true
			return "darwin\n", nil
		},
	})
	if err := ensureDoctorHostOS(h, "user@host"); err != nil {
		t.Fatalf("ensureDoctorHostOS should not error when OS is pre-set, got %v", err)
	}
	if called {
		t.Fatal("ensureDoctorHostOS must not issue a probe when h.OS is pre-set")
	}
	if h.OS != "linux" {
		t.Fatalf("h.OS should remain unchanged, got %q", h.OS)
	}
}

// TestEnsureDoctorHostOS_RemoteSuccess covers the remote-addr detection
// path. When addr is non-local and OS is empty, the function must call
// host.DetectOS and assign the returned value to h.OS.
func TestEnsureDoctorHostOS_RemoteSuccess(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{Addr: "user@host"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "linux\n", nil },
	})
	if err := ensureDoctorHostOS(h, "user@host"); err != nil {
		t.Fatalf("ensureDoctorHostOS should not error on successful DetectOS, got %v", err)
	}
	if h.OS != "linux" {
		t.Fatalf("h.OS should be 'linux' after DetectOS, got %q", h.OS)
	}
}

// TestEnsureDoctorHostOS_RemoteError covers the remote-addr detection
// failure path. When DetectOS returns an error, the function must
// propagate it without mutating h.OS — leaving the field empty lets the
// caller distinguish "detection failed" from "detected as X".
func TestEnsureDoctorHostOS_RemoteError(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{Addr: "user@host"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			return "", errors.New("ssh: could not resolve hostname")
		},
	})
	err := ensureDoctorHostOS(h, "user@host")
	if err == nil {
		t.Fatal("ensureDoctorHostOS should propagate DetectOS errors")
	}
	if !strings.Contains(err.Error(), "could not resolve hostname") {
		t.Fatalf("err should include underlying cause, got %v", err)
	}
	if h.OS != "" {
		t.Fatalf("h.OS must remain empty on DetectOS failure, got %q", h.OS)
	}
}

// TestCheckNativeRunnerInstall_Empty covers the no-targets path: when no
// runners on hostName satisfy the native predicate (or the runners slice is
// empty), checkNativeRunnerInstall must return silently without touching
// r.Fail / r.Warn or emitting output. Pins that callers don't accidentally
// crash on an empty list.
func TestCheckNativeRunnerInstall_Empty(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	called := false
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			called = true
			return "yes\n", nil
		},
	})
	var buf bytes.Buffer
	r := Result{}
	checkNativeRunnerInstall(&buf, "h1", h, nil, &r)
	if called {
		t.Fatal("checkNativeRunnerInstall must not issue any probes when there are no targets")
	}
	if r.Fail != 0 || r.Warn != 0 {
		t.Fatalf("empty: counters should stay zero, got %+v", r)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty: expected no output, got %q", buf.String())
	}
}

// TestCheckNativeRunnerInstall_Installed covers the OK path: when the
// instance directory has the .runner + run.sh sentinel files (mock echoes
// 'yes'), the function must emit the OK line and leave r.Fail untouched.
func TestCheckNativeRunnerInstall_Installed(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "yes\n", nil },
	})
	runners := []config.RunnerConfig{
		{Name: "r", Host: "h1", Repo: "o/r", Count: 1},
	}
	var buf bytes.Buffer
	r := Result{}
	checkNativeRunnerInstall(&buf, "h1", h, runners, &r)
	if r.Fail != 0 {
		t.Fatalf("installed: r.Fail should stay 0, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "r-1 installed") {
		t.Fatalf("installed: expected OK line, got: %q", buf.String())
	}
}

// TestCheckNativeRunnerInstall_Missing covers the FAIL path: when the
// instance directory is incomplete (mock echoes 'no'), the function must
// emit the FAIL line with the setup remediation and increment r.Fail.
func TestCheckNativeRunnerInstall_Missing(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) { return "no\n", nil },
	})
	runners := []config.RunnerConfig{
		{Name: "r", Host: "h1", Repo: "o/r", Count: 1},
	}
	var buf bytes.Buffer
	r := Result{}
	checkNativeRunnerInstall(&buf, "h1", h, runners, &r)
	if r.Fail != 1 {
		t.Fatalf("missing: r.Fail should be 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "not installed") {
		t.Fatalf("missing: expected FAIL line, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "gh sr setup r") {
		t.Fatalf("missing: expected remediation hint, got: %q", buf.String())
	}
}

// TestCheckNativeRunnerInstall_ProbeError covers the probe-error path: when
// NativeRunnerConfigPresent returns a non-nil err (e.g. SSH handshake
// failed), the function must emit the FAIL line with the underlying error
// and increment r.Fail, instead of continuing the loop and reporting
// confusingly different state per instance.
func TestCheckNativeRunnerInstall_ProbeError(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			return "", errors.New("ssh: connection refused")
		},
	})
	runners := []config.RunnerConfig{
		{Name: "r", Host: "h1", Repo: "o/r", Count: 1},
	}
	var buf bytes.Buffer
	r := Result{}
	checkNativeRunnerInstall(&buf, "h1", h, runners, &r)
	if r.Fail != 1 {
		t.Fatalf("probe-error: r.Fail should be 1, got %d", r.Fail)
	}
	if !strings.Contains(buf.String(), "ssh: connection refused") {
		t.Fatalf("probe-error: expected FAIL line to include underlying err, got: %q", buf.String())
	}
}

// TestCheckNativeRunnerInstall_SkipsOtherHosts covers the host-filter path:
// runners on a different host must be ignored even when they appear in the
// input slice. Pins the per-host scoping that the rest of the doctor's
// host-loop relies on.
func TestCheckNativeRunnerInstall_SkipsOtherHosts(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	called := false
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			called = true
			return "yes\n", nil
		},
	})
	runners := []config.RunnerConfig{
		{Name: "other", Host: "h2", Repo: "o/r", Count: 2},
	}
	var buf bytes.Buffer
	r := Result{}
	checkNativeRunnerInstall(&buf, "h1", h, runners, &r)
	if called {
		t.Fatal("checkNativeRunnerInstall must not probe runners on other hosts")
	}
	if r.Fail != 0 || r.Warn != 0 {
		t.Fatalf("skipped: counters should stay zero, got %+v", r)
	}
}

// TestCheckContainerRunnerInstall_Empty covers the no-targets path for the
// container predicate and host filter.
func TestCheckContainerRunnerInstall_Empty(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	called := false
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			called = true
			return "", nil
		},
	})
	runners := []config.RunnerConfig{
		{Name: "native", Host: "h1", Repo: "o/r", Count: 1},
		{Name: "container-other-host", Host: "h2", Repo: "o/r", Count: 1, RunnerMode: config.RunnerModeContainer},
	}
	var buf bytes.Buffer
	r := Result{}
	checkContainerRunnerInstall(&buf, "h1", h, runners, &r)
	if called {
		t.Fatal("checkContainerRunnerInstall must not probe when no container targets match")
	}
	if r.Fail != 0 || r.Warn != 0 {
		t.Fatalf("empty: counters should stay zero, got %+v", r)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty: expected no output, got %q", buf.String())
	}
}

// TestCheckContainerRunnerInstall covers the user-facing health classifications
// and verifies terminal states stop before the inner-container probe.
func TestCheckContainerRunnerInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		bootstrapFailed  bool
		inspectOut       string
		inspectErr       error
		execOut          string
		wantFail         int
		wantWarn         int
		wantInspectCalls int
		wantExecCalls    int
		wantContains     []string
		wantNotContains  []string
	}{
		{
			name:            "bootstrap failed",
			bootstrapFailed: true,
			wantFail:        1,
			wantContains:    []string{"bootstrap failed", "gh sr up aw", "gh sr rebuild aw"},
			wantNotContains: []string{"Docker container"},
		},
		{
			name:             "container missing",
			inspectOut:       "missing\n",
			wantFail:         1,
			wantInspectCalls: 1,
			wantContains:     []string{"Docker container gh-sr-aw-1 not found", "gh sr setup aw"},
		},
		{
			name:             "inspect error",
			inspectErr:       errors.New("docker unavailable"),
			wantFail:         1,
			wantInspectCalls: 1,
			wantContains:     []string{"Docker container gh-sr-aw-1 not found", "gh sr setup aw"},
		},
		{
			name:             "not accepting jobs",
			inspectOut:       "exited\n",
			wantWarn:         1,
			wantInspectCalls: 1,
			wantContains:     []string{`state is "exited"`, "gh sr up aw"},
			wantNotContains:  []string{"inner dockerd"},
		},
		{
			name:             "inner dockerd and registration missing",
			inspectOut:       "running\n",
			execOut:          "no\nno\n",
			wantFail:         1,
			wantWarn:         1,
			wantInspectCalls: 1,
			wantExecCalls:    1,
			wantContains:     []string{"inner dockerd not responding", "missing .runner", "gh sr setup aw"},
		},
		{
			name:             "healthy",
			inspectOut:       "running\n",
			execOut:          "dockerd-ok\nok\n",
			wantInspectCalls: 1,
			wantExecCalls:    1,
			wantContains:     []string{"inner dockerd healthy", "registered (.runner present"},
			wantNotContains:  []string{"not found", "not configured"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
			inspectCalls := 0
			execCalls := 0
			h.SetConn(&testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					switch {
					case cmd == "echo $HOME":
						return "/home/test\n", nil
					case strings.Contains(cmd, "bootstrap-failed"):
						if tt.bootstrapFailed {
							return "yes\n", nil
						}
						return "no\n", nil
					case strings.Contains(cmd, "docker inspect --format"):
						inspectCalls++
						return tt.inspectOut, tt.inspectErr
					case strings.Contains(cmd, "docker exec"):
						execCalls++
						return tt.execOut, nil
					default:
						t.Fatalf("unexpected command: %s", cmd)
						return "", nil
					}
				},
			})
			runners := []config.RunnerConfig{
				{Name: "aw", Host: "h1", Repo: "o/r", Count: 1, RunnerMode: config.RunnerModeContainer},
			}
			var buf bytes.Buffer
			r := Result{}
			checkContainerRunnerInstall(&buf, "h1", h, runners, &r)

			if r.Fail != tt.wantFail || r.Warn != tt.wantWarn {
				t.Fatalf("counters: got %+v, want Fail=%d Warn=%d", r, tt.wantFail, tt.wantWarn)
			}
			if inspectCalls != tt.wantInspectCalls || execCalls != tt.wantExecCalls {
				t.Fatalf("probe calls: inspect=%d exec=%d, want inspect=%d exec=%d", inspectCalls, execCalls, tt.wantInspectCalls, tt.wantExecCalls)
			}
			out := buf.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in output:\n%s", want, out)
				}
			}
			for _, unwanted := range tt.wantNotContains {
				if strings.Contains(out, unwanted) {
					t.Errorf("unexpected %q in output:\n%s", unwanted, out)
				}
			}
		})
	}
}

// TestCheckContainerAgenticInnerHygiene_Empty covers the no-targets path for
// the agentic profile predicate and host filter.
func TestCheckContainerAgenticInnerHygiene_Empty(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
	called := false
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			called = true
			return "", nil
		},
	})
	runners := []config.RunnerConfig{
		{Name: "native", Host: "h1", Repo: "o/r", Count: 1},
		{Name: "container", Host: "h1", Repo: "o/r", Count: 1, RunnerMode: config.RunnerModeContainer},
		{Name: "agentic-other-host", Host: "h2", Repo: "o/r", Count: 1, Profile: "agentic"},
	}
	var buf bytes.Buffer
	r := Result{}
	checkContainerAgenticInnerHygiene(&buf, "h1", h, runners, &r)
	if called {
		t.Fatal("checkContainerAgenticInnerHygiene must not probe when no agentic targets match")
	}
	if r.Fail != 0 || r.Warn != 0 {
		t.Fatalf("empty: counters should stay zero, got %+v", r)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty: expected no output, got %q", buf.String())
	}
}

type agenticInnerHygieneExecutor struct {
	mu sync.Mutex

	inspectOut string
	inspectErr error
	mtuOut     string
	awfOut     string
	fanoutOut  string

	calls      []string
	unexpected []string
}

func (e *agenticInnerHygieneExecutor) Run(cmd string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, cmd)

	switch {
	case strings.Contains(cmd, "ip -o route get 1.1.1.1"):
		return e.mtuOut, nil
	case strings.Contains(cmd, "docker inspect --format"):
		return e.inspectOut, e.inspectErr
	case strings.Contains(cmd, `echo "#container-inner-host-docker-internal:$?"`):
		return e.fanoutOut, nil
	case strings.Contains(cmd, `--filter "name=awf-" --filter "name=gh-aw"`):
		return e.awfOut, nil
	case strings.Contains(cmd, "iptables -L DOCKER-USER"):
		return "", nil
	case strings.Contains(cmd, `--filter "name=gh-aw-mcpg-"`):
		return "", nil
	default:
		e.unexpected = append(e.unexpected, cmd)
		return "", fmt.Errorf("unexpected command: %s", cmd)
	}
}

func (e *agenticInnerHygieneExecutor) Upload(string, string) error { return nil }
func (e *agenticInnerHygieneExecutor) Close() error                { return nil }

func (e *agenticInnerHygieneExecutor) snapshot() (calls, unexpected []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...), append([]string(nil), e.unexpected...)
}

// TestCheckContainerAgenticInnerHygiene covers the strict running-state gate,
// clean result, both warning sources, and the host-MTU fanout gate.
func TestCheckContainerAgenticInnerHygiene(t *testing.T) {
	t.Parallel()

	const cleanFanout = "#container-inner-host-docker-internal:0\n" +
		"#container-inner-resolv:0\n" +
		"#container-awf-service-routing:0\n" +
		"#container-node-npm:0\n" +
		"#container-awf:0"

	tests := []struct {
		name            string
		inspectOut      string
		inspectErr      error
		mtuOut          string
		awfOut          string
		fanoutOut       string
		wantWarn        int
		wantExecCalls   int
		wantFanoutCalls int
		wantMTUBlock    bool
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:            "inspect error skips inner probes",
			inspectErr:      errors.New("docker unavailable"),
			mtuOut:          "1400\n",
			wantNotContains: []string{"container(agent):"},
		},
		{
			name:            "restarting skips inner probes",
			inspectOut:      "restarting\n",
			mtuOut:          "1400\n",
			wantNotContains: []string{"container(agent):"},
		},
		{
			name:            "healthy standard MTU",
			inspectOut:      "running\n",
			mtuOut:          "1500\n",
			fanoutOut:       cleanFanout,
			wantExecCalls:   4,
			wantFanoutCalls: 1,
			wantContains:    []string{"container(agent): awf installed, inner Docker clean", "gh-sr-aw-1"},
			wantNotContains: []string{"[WARN]", "[FAIL]"},
		},
		{
			name:            "inner AWF orphan warning",
			inspectOut:      "running\n",
			mtuOut:          "1500\n",
			awfOut:          "awf-c1\n",
			fanoutOut:       cleanFanout,
			wantWarn:        1,
			wantExecCalls:   4,
			wantFanoutCalls: 1,
			wantContains: []string{
				"container(agent): orphan gh-aw/awf containers in inner Docker",
				"docker exec -it gh-sr-aw-1 bash",
				"See: agentic-workflows.md §12",
			},
		},
		{
			name:            "fanout probe warning",
			inspectOut:      "running\n",
			mtuOut:          "1500\n",
			fanoutOut:       "#container-node-npm:1",
			wantWarn:        1,
			wantExecCalls:   4,
			wantFanoutCalls: 1,
			wantContains: []string{
				"container(agent): node LTS/npm are not on PATH",
				"gh sr rebuild aw",
				"See: agentic-workflows.md §8",
			},
		},
		{
			name:            "healthy reduced MTU includes probe",
			inspectOut:      "running\n",
			mtuOut:          "1400\n",
			fanoutOut:       cleanFanout + "\n#container-mtu:0",
			wantExecCalls:   4,
			wantFanoutCalls: 1,
			wantMTUBlock:    true,
			wantContains:    []string{"container(agent): awf installed, inner Docker clean"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exec := &agenticInnerHygieneExecutor{
				inspectOut: tt.inspectOut,
				inspectErr: tt.inspectErr,
				mtuOut:     tt.mtuOut,
				awfOut:     tt.awfOut,
				fanoutOut:  tt.fanoutOut,
			}
			h := host.NewHost("h1", config.HostConfig{OS: "linux", Addr: "local"})
			h.SetConn(exec)
			runners := []config.RunnerConfig{
				{Name: "aw", Host: "h1", Repo: "o/r", Count: 1, Profile: "agentic"},
			}
			var buf bytes.Buffer
			r := Result{}
			checkContainerAgenticInnerHygiene(&buf, "h1", h, runners, &r)

			if r.Fail != 0 || r.Warn != tt.wantWarn {
				t.Fatalf("counters: got %+v, want Fail=0 Warn=%d", r, tt.wantWarn)
			}
			out := buf.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in output:\n%s", want, out)
				}
			}
			for _, unwanted := range tt.wantNotContains {
				if strings.Contains(out, unwanted) {
					t.Errorf("unexpected %q in output:\n%s", unwanted, out)
				}
			}

			calls, unexpected := exec.snapshot()
			if len(unexpected) != 0 {
				t.Fatalf("unexpected commands: %q", unexpected)
			}
			var mtuCalls, inspectCalls, execCalls, fanoutCalls int
			var fanoutCommand string
			for _, cmd := range calls {
				switch {
				case strings.Contains(cmd, "ip -o route get 1.1.1.1"):
					mtuCalls++
				case strings.Contains(cmd, "docker inspect --format"):
					inspectCalls++
				}
				if strings.Contains(cmd, `docker exec "gh-sr-aw-1"`) {
					execCalls++
				}
				if strings.Contains(cmd, `echo "#container-inner-host-docker-internal:$?"`) {
					fanoutCalls++
					fanoutCommand = cmd
				}
			}
			if mtuCalls != 1 || inspectCalls != 1 || execCalls != tt.wantExecCalls || fanoutCalls != tt.wantFanoutCalls {
				t.Fatalf("probe calls: mtu=%d inspect=%d exec=%d fanout=%d; want 1, 1, %d, %d\nall calls: %q",
					mtuCalls, inspectCalls, execCalls, fanoutCalls, tt.wantExecCalls, tt.wantFanoutCalls, calls)
			}
			if tt.wantFanoutCalls != 0 {
				hasMTUBlock := strings.Contains(fanoutCommand, `echo "#container-mtu:$?"`) && strings.Contains(fanoutCommand, "host=1400")
				if hasMTUBlock != tt.wantMTUBlock {
					t.Errorf("fanout MTU block = %v, want %v\ncommand: %s", hasMTUBlock, tt.wantMTUBlock, fanoutCommand)
				}
			}
		})
	}
}
