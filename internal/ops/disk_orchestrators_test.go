package ops

import (
	"bytes"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/runner"
	"github.com/an-lee/gh-sr/internal/testutil"
)

// newDiskMockExecutor returns a MockExecutor wired to satisfy both
// CollectDiskUsage and PruneDisk on a single host.
//
// The mock answers:
//
//   - the `du` POSIX script (MeasureDiskUsage → dirSizesPOSIX) by detecting
//     the "du --max-depth" or "du -d 1" probes. The output format is the
//     four-int64s layout the dirSizes parser expects: "total work temp docker".
//   - the `ls -1 "$HOME/.gh-sr/runners"` listing (ListRunnerInstanceDirs) by
//     returning the configured diskDirs.
//   - the clearWorkTempPOSIX script (`find "$p" ... clear_one`) by returning
//     empty (a successful no-op).
//
// Other commands return empty + nil so the orchestrator proceeds without
// surprising output.
type diskMockOpts struct {
	RunFn func(cmd string) (string, error)

	totalBytes int64
	workBytes  int64
	tempBytes  int64
	dockerBts  int64

	diskDirs []string
}

// batchInstancesFromScript extracts the list of instance names from a
// `for inst in ...; do` block in the batch disk-usage script. Used by
// the test mocks to emit one tab-separated "instance total work temp
// docker" line per walked instance — matching the batch script's output
// shape so the Go-side parser can be exercised end-to-end.
func batchInstancesFromScript(cmd string) []string {
	const marker = "for inst in"
	idx := strings.Index(cmd, marker)
	if idx < 0 {
		return nil
	}
	rest := cmd[idx+len(marker):]
	end := strings.Index(rest, "; do")
	if end < 0 {
		return nil
	}
	tokens := strings.Fields(rest[:end])
	var out []string
	for _, tok := range tokens {
		// Strip surrounding single or double quotes (the script's
		// `strconv.Quote` produces double-quoted tokens).
		if len(tok) >= 2 {
			first, last := tok[0], tok[len(tok)-1]
			if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
				out = append(out, tok[1:len(tok)-1])
				continue
			}
		}
		out = append(out, tok)
	}
	return out
}

// batchOutputFor emits the tab-separated batch-line shape that
// dirSizesBatchPOSIX's Go-side parser expects. One line per instance,
// all populated with the configured sizes.
func batchOutputFor(insts []string, total, work, temp, docker int64) string {
	var b strings.Builder
	for _, inst := range insts {
		fmt.Fprintf(&b, "%s\t%d\t%d\t%d\t%d\n", inst, total, work, temp, docker)
	}
	return b.String()
}

func newDiskMock(d diskMockOpts) *testutil.MockExecutor {
	if d.RunFn == nil {
		d.RunFn = func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "for inst in"):
				// Batch disk-usage script (dirSizesBatchPOSIX).
				return batchOutputFor(batchInstancesFromScript(cmd), d.totalBytes, d.workBytes, d.tempBytes, d.dockerBts), nil
			case strings.Contains(cmd, "du --max-depth") || strings.Contains(cmd, "du -d 1"):
				return fmt.Sprintf("%d %d %d %d\n", d.totalBytes, d.workBytes, d.tempBytes, d.dockerBts), nil
			case strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`):
				return strings.Join(d.diskDirs, "\n") + "\n", nil
			case strings.Contains(cmd, "clear_one"):
				return "", nil
			case strings.Contains(cmd, "rm -rf"):
				return "", nil
			default:
				return "", nil
			}
		}
	}
	return &testutil.MockExecutor{RunFn: d.RunFn}
}

// newDiskGitHubHTTPServer returns an httptest.Server answering the "list
// runners" endpoint with empty pages. CollectDiskUsage's status enrichment
// must not panic on nil mgr.GitHub; passing a real client keeps the call
// path the same as production.
func newDiskGitHubHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := newCollectStatusHTTPServer(t, nil)
	return ts
}

// newDiskGitHubKnownHTTPServer returns an httptest.Server that answers the
// "list runners" endpoint with a single registered runner for each
// repo-named host. PruneDisk's safety branch ("GitHub status unknown")
// treats a runner as "known" only when it appears in the GitHub actions
// runners list with a non-empty Remote. We return Remote="online" for
// every configured (repo, name, instance) so the orchestrator skips the
// safety branch and proceeds to the actual prune path.
func newDiskGitHubKnownHTTPServer(t *testing.T, runners []config.RunnerConfig) *httptest.Server {
	t.Helper()
	rows := make(map[string][]runner.GitHubRunner)
	for _, rc := range runners {
		for _, inst := range rc.InstanceNames() {
			key := "repo|" + rc.Repo
			rows[key] = append(rows[key], runner.GitHubRunner{
				Name:   inst,
				OS:     "linux",
				Status: "online",
				Busy:   false,
			})
		}
	}
	return newCollectStatusHTTPServer(t, rows)
}

// TestCollectDiskUsage_EmptyRunners covers the no-match filter case:
// FilterRunners returns no runners, so CollectDiskUsage returns
// ("no runners matching the given filters", nil). No connect attempt, no
// host call, no GitHub hit.
func TestCollectDiskUsage_EmptyRunners(t *testing.T) {
	t.Parallel()

	installMockConnectHost(t, map[string]host.Executor{})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "org/repo", Count: 1},
	}

	var buf bytes.Buffer
	_, err := CollectDiskUsage(&buf, cfg, mgr, "", "no-such-repo", nil)
	if err == nil {
		t.Fatal("expected error for empty filter")
	}
	if !strings.Contains(err.Error(), "no runners matching") {
		t.Errorf("got %v; want 'no runners matching'", err)
	}
	if got := buf.String(); got != "" {
		t.Errorf("expected no output, got %q", got)
	}
}

// TestCollectDiskUsage_ConnectError covers the per-host connect failure:
// the orchestrator must emit "Warning: cannot connect to <host>" on the
// writer AND return an aggregated error naming the failed host. No
// per-instance disk calls happen because connect failed.
func TestCollectDiskUsage_ConnectError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ssh dial timeout")
	installFailingConnectHost(t, sentinel)

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	_, err := CollectDiskUsage(&buf, cfg, mgr, "", "", nil)
	if err == nil {
		t.Fatal("expected error for connect failure")
	}
	if !strings.Contains(err.Error(), "cannot connect to host(s)") || !strings.Contains(err.Error(), "h1") {
		t.Errorf("got %v; want 'cannot connect to host(s): h1'", err)
	}
	if !strings.Contains(buf.String(), "Warning: cannot connect to h1") {
		t.Errorf("expected warning line; got:\n%s", buf.String())
	}
}

// TestCollectDiskUsage_SingleHostSingleRunner covers the happy path: one
// local host, one runner, one instance. The mock returns a fixed
// "total work temp docker" line; the orchestrator wires it into a single
// DiskUsageEntry with Host=h1, Instance=ci-1, Mode=native (from the
// RunnerConfig), and Orphan=false.
func TestCollectDiskUsage_SingleHostSingleRunner(t *testing.T) {
	t.Parallel()

	installMockConnectHost(t, map[string]host.Executor{
		"h1": newDiskMock(diskMockOpts{
			totalBytes: 10 * 1024 * 1024,
			workBytes:  4 * 1024 * 1024,
			tempBytes:  2 * 1024 * 1024,
			dockerBts:  1 * 1024 * 1024,
		}),
	})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	entries, err := CollectDiskUsage(&buf, cfg, mgr, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Host != "h1" {
		t.Errorf("Host = %q, want %q", e.Host, "h1")
	}
	if e.Instance != "ci-1" {
		t.Errorf("Instance = %q, want %q", e.Instance, "ci-1")
	}
	if e.Orphan {
		t.Errorf("Orphan = true, want false")
	}
	if e.TotalBytes != 10*1024*1024 {
		t.Errorf("TotalBytes = %d, want %d", e.TotalBytes, 10*1024*1024)
	}
	if e.WorkBytes != 4*1024*1024 {
		t.Errorf("WorkBytes = %d, want %d", e.WorkBytes, 4*1024*1024)
	}
	if e.TempBytes != 2*1024*1024 {
		t.Errorf("TempBytes = %d, want %d", e.TempBytes, 2*1024*1024)
	}
	if e.DockerDataBytes != 1*1024*1024 {
		t.Errorf("DockerDataBytes = %d, want %d", e.DockerDataBytes, 1*1024*1024)
	}
	// Other = total - work - temp - docker = 3MiB.
	if e.OtherBytes != 3*1024*1024 {
		t.Errorf("OtherBytes = %d, want %d", e.OtherBytes, 3*1024*1024)
	}
}

// TestCollectDiskUsage_MultiHostFanOut covers the parallel-host fan-out:
// two hosts (h1, h2) each get their own goroutine. Both succeed, both
// contribute entries, and the result is sorted (host, instance) ascending.
//
// Distinct mock responses per host let us verify both ran (parallel
// execution is allowed but ordering is not asserted here — that's
// guaranteed by the sort).
func TestCollectDiskUsage_MultiHostFanOut(t *testing.T) {
	t.Parallel()

	makeH := func(total int64) *testutil.MockExecutor {
		return newDiskMock(diskMockOpts{totalBytes: total})
	}

	installMockConnectHost(t, map[string]host.Executor{
		"h1": makeH(100),
		"h2": makeH(200),
	})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1", "h2")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci1", Host: "h1", Repo: "o/r", Count: 1},
		{Name: "ci2", Host: "h2", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	entries, err := CollectDiskUsage(&buf, cfg, mgr, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	// Sort order: host then instance. Both hosts each have 1 instance.
	if entries[0].Host != "h1" {
		t.Errorf("entries[0].Host = %q, want h1 (sort order)", entries[0].Host)
	}
	if entries[1].Host != "h2" {
		t.Errorf("entries[1].Host = %q, want h2 (sort order)", entries[1].Host)
	}
	if entries[0].TotalBytes != 100 {
		t.Errorf("entries[0].TotalBytes = %d, want 100", entries[0].TotalBytes)
	}
	if entries[1].TotalBytes != 200 {
		t.Errorf("entries[1].TotalBytes = %d, want 200", entries[1].TotalBytes)
	}
}

// TestCollectDiskUsage_OrphanDetection covers the orphan branch: the host
// reports a directory on disk that is NOT in the configured runner set.
// The orchestrator should include it as an entry with Orphan=true and
// Mode="unknown" (because rc is nil).
func TestCollectDiskUsage_OrphanDetection(t *testing.T) {
	t.Parallel()

	installMockConnectHost(t, map[string]host.Executor{
		"h1": newDiskMock(diskMockOpts{
			totalBytes: 500,
			diskDirs:   []string{"ci-1", "stale-orphan"},
		}),
	})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	entries, err := CollectDiskUsage(&buf, cfg, mgr, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (ci-1 + stale-orphan), got %d: %+v", len(entries), entries)
	}
	// Sort: ci-1 before stale-orphan.
	if entries[0].Instance != "ci-1" || entries[0].Orphan {
		t.Errorf("entries[0] = %+v; want ci-1 Orphan=false", entries[0])
	}
	if entries[1].Instance != "stale-orphan" || !entries[1].Orphan {
		t.Errorf("entries[1] = %+v; want stale-orphan Orphan=true", entries[1])
	}
	if entries[1].Mode != "unknown" {
		t.Errorf("entries[1].Mode = %q, want \"unknown\"", entries[1].Mode)
	}
}

// TestCollectDiskUsage_FilterByHost covers the filter narrowing: filterHost="h1"
// narrows the runner set to h1 only. h2 is registered in the mock map but
// must NEVER be connected.
func TestCollectDiskUsage_FilterByHost(t *testing.T) {
	t.Parallel()

	h2Calls := 0
	h2Exec := &testutil.MockExecutor{
		RunFn: func(string) (string, error) {
			h2Calls++
			return "0 0 0 0\n", nil
		},
	}

	installMockConnectHost(t, map[string]host.Executor{
		"h1": newDiskMock(diskMockOpts{totalBytes: 100}),
		"h2": h2Exec,
	})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1", "h2")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci1", Host: "h1", Repo: "o/r", Count: 1},
		{Name: "ci2", Host: "h2", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	entries, err := CollectDiskUsage(&buf, cfg, mgr, "h1", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (h1 only), got %d: %+v", len(entries), entries)
	}
	if entries[0].Host != "h1" {
		t.Errorf("entries[0].Host = %q, want h1", entries[0].Host)
	}
	if h2Calls != 0 {
		t.Errorf("h2 should not be connected; got %d Run calls", h2Calls)
	}
}

// TestPruneDisk_EmptyRunners covers the no-match filter case: FilterRunners
// returns no runners, so PruneDisk returns ("no runners matching the given
// filters", nil). No connect attempt.
func TestPruneDisk_EmptyRunners(t *testing.T) {
	t.Parallel()

	installMockConnectHost(t, map[string]host.Executor{})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "org/repo", Count: 1},
	}

	var buf bytes.Buffer
	_, err := PruneDisk(&buf, cfg, mgr, "", "no-such-repo", nil, DiskPruneOptions{})
	if err == nil {
		t.Fatal("expected error for empty filter")
	}
	if !strings.Contains(err.Error(), "no runners matching") {
		t.Errorf("got %v; want 'no runners matching'", err)
	}
}

// TestPruneDisk_DryRunNoShellEffects covers DryRun: the orchestrator must
// emit the prune-action line ("clear _work/_temp...") for each instance but
// must NOT actually invoke the clear shell script. The mock counts
// clear_one commands and we assert zero calls.
func TestPruneDisk_DryRunNoShellEffects(t *testing.T) {
	t.Parallel()

	var clearCalls int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "clear_one") {
				clearCalls++
			}
			if strings.Contains(cmd, "du --max-depth") || strings.Contains(cmd, "du -d 1") {
				return "0 0 0 0\n", nil
			}
			return "", nil
		},
	}

	installMockConnectHost(t, map[string]host.Executor{"h1": mock})

	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}
	ts := newDiskGitHubKnownHTTPServer(t, cfg.Runners)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}

	var buf bytes.Buffer
	results, err := PruneDisk(&buf, cfg, mgr, "", "", nil, DiskPruneOptions{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if results[0].Instance != "ci-1" {
		t.Errorf("results[0].Instance = %q, want %q", results[0].Instance, "ci-1")
	}
	if len(results[0].Actions) == 0 {
		t.Errorf("expected at least one action line in dry-run; got %+v", results[0])
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("expected [dry-run] prefix in output; got:\n%s", buf.String())
	}
	if clearCalls != 0 {
		t.Errorf("dry-run should not invoke clear_one; got %d calls", clearCalls)
	}
}

// TestPruneDisk_SingleHostSingleRunner covers the happy path: one local
// host, one runner, DryRun=false. The orchestrator invokes the clear
// script and returns a PruneResult with the "clear _work/_temp" action.
//
// Note: we use the newDiskMock default branch which returns empty for
// `clear_one`, simulating successful no-op removal.
func TestPruneDisk_SingleHostSingleRunner(t *testing.T) {
	t.Parallel()

	var clearCalls int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "clear_one") {
				clearCalls++
			}
			if strings.Contains(cmd, "du --max-depth") || strings.Contains(cmd, "du -d 1") {
				return "0 0 0 0\n", nil
			}
			return "", nil
		},
	}

	installMockConnectHost(t, map[string]host.Executor{"h1": mock})

	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}
	ts := newDiskGitHubKnownHTTPServer(t, cfg.Runners)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}

	var buf bytes.Buffer
	results, err := PruneDisk(&buf, cfg, mgr, "", "", nil, DiskPruneOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if results[0].Instance != "ci-1" {
		t.Errorf("results[0].Instance = %q, want %q", results[0].Instance, "ci-1")
	}
	if results[0].Skipped {
		t.Errorf("results[0].Skipped = true, want false; reason=%q", results[0].Reason)
	}
	if clearCalls != 1 {
		t.Errorf("expected 1 clear_one call, got %d", clearCalls)
	}
}

// TestPruneDisk_ConnectError covers the connect-failure path: the
// orchestrator must emit "Warning: cannot connect to <host>" AND return
// an aggregated error naming the failed host. No prune work attempted.
func TestPruneDisk_ConnectError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ssh dial timeout")
	installFailingConnectHost(t, sentinel)

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	_, err := PruneDisk(&buf, cfg, mgr, "", "", nil, DiskPruneOptions{})
	if err == nil {
		t.Fatal("expected error for connect failure")
	}
	if !strings.Contains(err.Error(), "cannot connect to host(s)") || !strings.Contains(err.Error(), "h1") {
		t.Errorf("got %v; want 'cannot connect to host(s): h1'", err)
	}
	if !strings.Contains(buf.String(), "Warning: cannot connect to h1") {
		t.Errorf("expected warning line; got:\n%s", buf.String())
	}
}

// TestPruneDisk_GitHubUnknownSkips covers the safety branch: when an
// instance is configured but GitHub did NOT report its status (i.e., the
// runner is not in the GitHub actions runners list), PruneDisk MUST skip
// it with reason "GitHub status unknown (use --force)" — unless Force is
// true. This is a guard against deleting a runner that is actually doing
// work but whose status we haven't yet observed.
//
// We simulate this by providing an empty GitHubHTTPServer (no rows), so
// statuses come back with Remote="" → githubKnown is false.
func TestPruneDisk_GitHubUnknownSkips(t *testing.T) {
	t.Parallel()

	var clearCalls int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "clear_one") {
				clearCalls++
			}
			return "", nil
		},
	}

	installMockConnectHost(t, map[string]host.Executor{"h1": mock})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	results, err := PruneDisk(&buf, cfg, mgr, "", "", nil, DiskPruneOptions{Force: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if !results[0].Skipped {
		t.Errorf("expected Skipped=true; got %+v", results[0])
	}
	if !strings.Contains(results[0].Reason, "GitHub status unknown") {
		t.Errorf("expected reason to mention GitHub unknown; got %q", results[0].Reason)
	}
	if clearCalls != 0 {
		t.Errorf("skipped instance must not call clear_one; got %d", clearCalls)
	}
}

// TestPruneDisk_ForceOverridesUnknown covers the force flag: with Force=true,
// the GitHub-unknown skip is bypassed and the clear script runs.
func TestPruneDisk_ForceOverridesUnknown(t *testing.T) {
	t.Parallel()

	var clearCalls int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "clear_one") {
				clearCalls++
			}
			return "", nil
		},
	}

	installMockConnectHost(t, map[string]host.Executor{"h1": mock})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}

	var buf bytes.Buffer
	results, err := PruneDisk(&buf, cfg, mgr, "", "", nil, DiskPruneOptions{Force: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if results[0].Skipped {
		t.Errorf("Force=true should bypass the GitHub-unknown skip; got Skipped=true reason=%q", results[0].Reason)
	}
	if clearCalls != 1 {
		t.Errorf("expected 1 clear_one call under Force; got %d", clearCalls)
	}
}

// TestPruneDisk_IncludeOrphans covers the orphan-pruning branch:
// IncludeOrphans=true makes the orchestrator list on-disk dirs and prune
// the ones not in the configured set. The mock returns ["ci-1", "orphan-1"];
// "ci-1" is in config so it's the regular prune path, "orphan-1" is the
// orphan → remove-directory action.
func TestPruneDisk_IncludeOrphans(t *testing.T) {
	t.Parallel()

	var removeCalls int
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`):
				return "ci-1\norphan-1\n", nil
			case strings.Contains(cmd, "clear_one"):
				return "", nil
			case strings.Contains(cmd, "rm -rf") && strings.Contains(cmd, "$dir"):
				// removeDirTreePOSIX for the orphan
				removeCalls++
				return "", nil
			}
			return "", nil
		},
	}

	installMockConnectHost(t, map[string]host.Executor{"h1": mock})

	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 1},
	}
	ts := newDiskGitHubKnownHTTPServer(t, cfg.Runners)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}

	var buf bytes.Buffer
	results, err := PruneDisk(&buf, cfg, mgr, "", "", nil, DiskPruneOptions{IncludeOrphans: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (ci-1 + orphan-1), got %d: %+v", len(results), results)
	}
	// Both must be non-skipped, non-error.
	for i, r := range results {
		if r.Skipped {
			t.Errorf("results[%d].Skipped = true; reason=%q", i, r.Reason)
		}
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v", i, r.Err)
		}
	}
	// One of the two results must be the orphan prune (action: "remove orphan directory").
	sawOrphanAction := false
	for _, r := range results {
		for _, a := range r.Actions {
			if strings.Contains(a, "remove orphan directory") {
				sawOrphanAction = true
				break
			}
		}
	}
	if !sawOrphanAction {
		t.Errorf("expected an 'remove orphan directory' action; got %+v", results)
	}
	if removeCalls < 1 {
		t.Errorf("expected at least 1 rm -rf call for orphan removal; got %d", removeCalls)
	}
}

// TestPruneDisk_MultiHostFanOut covers the parallel-host fan-out: two
// hosts, both succeed, results aggregated. Sanity-check that both hosts'
// clear_one scripts run (one call each).
func TestPruneDisk_MultiHostFanOut(t *testing.T) {
	t.Parallel()

	makeMock := func(counter *int) *testutil.MockExecutor {
		return &testutil.MockExecutor{
			RunFn: func(cmd string) (string, error) {
				if strings.Contains(cmd, "clear_one") {
					*counter++
				}
				return "", nil
			},
		}
	}
	var h1Calls, h2Calls int
	installMockConnectHost(t, map[string]host.Executor{
		"h1": makeMock(&h1Calls),
		"h2": makeMock(&h2Calls),
	})

	cfg := cfgWithLocalHost("h1", "h2")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci1", Host: "h1", Repo: "o/r", Count: 1},
		{Name: "ci2", Host: "h2", Repo: "o/r", Count: 1},
	}
	ts := newDiskGitHubKnownHTTPServer(t, cfg.Runners)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}

	var buf bytes.Buffer
	results, err := PruneDisk(&buf, cfg, mgr, "", "", nil, DiskPruneOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(results), results)
	}
	if h1Calls != 1 || h2Calls != 1 {
		t.Errorf("expected 1 clear_one per host; got h1=%d h2=%d", h1Calls, h2Calls)
	}
}

// Compile-time guard: keep diskMockOpts visible if a future refactor
// removes its only constructor's only caller.
var _ = diskMockOpts{}

// TestCollectDiskUsage_SingleHostBatchSshRoundTrip pins the post-batch SSH
// round-trip count for `gh sr disk usage`. After the batched
// MeasureDiskUsageBatch fold, a single host with K instances must pay
// exactly 2 SSH round-trips for the disk listing:
//
//  1. ListRunnerInstanceDirs (the runner-tree watcher; unchanged)
//  2. The batched dirSizesBatchPOSIX script (one round-trip walks all
//     instances and emits one `<inst>\t<total>\t<work>\t<temp>\t<docker>`
//     line per instance)
//
// Pre-fold the K-instance path paid 1 + K round-trips (1 listing + K
// `du --max-depth` calls). A future refactor that re-introduces the
// per-instance `du` will trip this test and force a maintainer review —
// the per-tick disk-usage metrics will be affected.
func TestCollectDiskUsage_SingleHostBatchSshRoundTrip(t *testing.T) {
	t.Parallel()

	var (
		listingCalls int
		batchCalls   int
		duCalls      int
	)
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "for inst in"):
				batchCalls++
				// Emit one line per requested instance with a recognisable
				// size so we can assert the per-instance mapping worked.
				insts := batchInstancesFromScript(cmd)
				return batchOutputFor(insts, 1024, 256, 64, 16), nil
			case strings.Contains(cmd, "du --max-depth") || strings.Contains(cmd, "du -d 1"):
				duCalls++
				return "0 0 0 0\n", nil
			case strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`):
				listingCalls++
				return "ci-1\nci-2\nci-3\n", nil
			default:
				return "", nil
			}
		},
	}
	installMockConnectHost(t, map[string]host.Executor{"h1": mock})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 3},
	}

	var buf bytes.Buffer
	entries, err := CollectDiskUsage(&buf, cfg, mgr, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
	}
	if listingCalls != 1 {
		t.Errorf("listingCalls = %d, want 1 (ListRunnerInstanceDirs is unchanged)", listingCalls)
	}
	if batchCalls != 1 {
		t.Errorf("batchCalls = %d, want 1 (MeasureDiskUsageBatch must be one SSH round-trip per host)", batchCalls)
	}
	if duCalls != 0 {
		t.Errorf("duCalls = %d, want 0 (per-instance du must be folded into the batch script)", duCalls)
	}
	for _, e := range entries {
		if e.TotalBytes != 1024 {
			t.Errorf("entry %s: TotalBytes = %d, want 1024", e.Instance, e.TotalBytes)
		}
	}
}

// TestCollectDiskUsage_MultiHostBatchSshRoundTrip pins the count across
// multiple hosts: each host pays exactly 2 SSH round-trips regardless of
// how many instances it owns. Pre-fold this would have been 1 + K_i
// per host i — for a 3-host 9-instance fleet, 12 round-trips. Post-fold
// stays at 6.
func TestCollectDiskUsage_MultiHostBatchSshRoundTrip(t *testing.T) {
	t.Parallel()

	makeH := func(dirSuffix string) (*testutil.MockExecutor, *int, *int) {
		var list, batch int
		exec := &testutil.MockExecutor{
			RunFn: func(cmd string) (string, error) {
				switch {
				case strings.Contains(cmd, "for inst in"):
					batch++
					insts := batchInstancesFromScript(cmd)
					return batchOutputFor(insts, 100, 0, 0, 0), nil
				case strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`):
					list++
					// Match the configured instances exactly (RunnerConfig.Name
					// is the prefix). The directory layout for `Runners[0]=ci<dirSuffix>,
					// Count=3` is `ci<dirSuffix>-1`, `ci<dirSuffix>-2`, `ci<dirSuffix>-3`.
					return fmt.Sprintf("ci%s-1\nci%s-2\nci%s-3\n", dirSuffix, dirSuffix, dirSuffix), nil
				default:
					return "", nil
				}
			},
		}
		return exec, &list, &batch
	}

	ex1, l1, b1 := makeH("1")
	ex2, l2, b2 := makeH("2")
	ex3, l3, b3 := makeH("3")

	installMockConnectHost(t, map[string]host.Executor{
		"h1": ex1,
		"h2": ex2,
		"h3": ex3,
	})

	ts := newDiskGitHubHTTPServer(t)
	mgr := &runner.Manager{GitHub: runner.NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	cfg := cfgWithLocalHost("h1", "h2", "h3")
	cfg.Runners = []config.RunnerConfig{
		{Name: "ci1", Host: "h1", Repo: "o/r", Count: 3},
		{Name: "ci2", Host: "h2", Repo: "o/r", Count: 3},
		{Name: "ci3", Host: "h3", Repo: "o/r", Count: 3},
	}

	var buf bytes.Buffer
	entries, err := CollectDiskUsage(&buf, cfg, mgr, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 9 {
		t.Fatalf("expected 9 entries, got %d: %+v", len(entries), entries)
	}
	if *l1+*l2+*l3 != 3 {
		t.Errorf("listing calls = %d, want 3 (one per host)", *l1+*l2+*l3)
	}
	if *b1+*b2+*b3 != 3 {
		t.Errorf("batch calls = %d, want 3 (one per host)", *b1+*b2+*b3)
	}
	// Each host should have exactly one of each.
	if *l1 != 1 || *l2 != 1 || *l3 != 1 {
		t.Errorf("per-host listing counts: h1=%d h2=%d h3=%d, want 1 each", *l1, *l2, *l3)
	}
	if *b1 != 1 || *b2 != 1 || *b3 != 1 {
		t.Errorf("per-host batch counts: h1=%d h2=%d h3=%d, want 1 each", *b1, *b2, *b3)
	}
}
