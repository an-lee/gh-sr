package runner

import (
	"strconv"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

// makeEnrichCfg builds a synthetic config + scopeRunners fixture for benchmarking
// EnrichWithGitHubStatus without hitting the GitHub API. The fixture mirrors the
// production pattern of one scope per repo with a few runners per scope.
func makeEnrichCfg(numRepos, countPerRepo int) (*config.Config, map[scopeKey][]GitHubRunner) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{},
		Hosts: map[string]config.HostConfig{
			"h": {Addr: "a@b", OS: "linux", Arch: "amd64"},
		},
	}
	scopeRunners := make(map[scopeKey][]GitHubRunner)
	for i := 0; i < numRepos; i++ {
		name := "runner-" + strconv.Itoa(i)
		repo := "o/r" + strconv.Itoa(i)
		cfg.Runners = append(cfg.Runners, config.RunnerConfig{
			Name:  name,
			Repo:  repo,
			Host:  "h",
			Count: countPerRepo,
		})
		key := scopeKey{"repo", repo}
		gh := make([]GitHubRunner, countPerRepo)
		for j := 1; j <= countPerRepo; j++ {
			inst := name + "-" + strconv.Itoa(j)
			gh[j-1] = GitHubRunner{Name: inst, Status: "online", OS: "Linux"}
		}
		scopeRunners[key] = gh
	}
	return cfg, scopeRunners
}

// BenchmarkEnrichFromScopeRunners measures the inner work of EnrichWithGitHubStatus
// (rcByInstance build + status-to-GitHub-runner matching) without the GitHub API
// round trip. This is the alloc hotspot targeted by the inline-rc-by-instance
// refactor: the per-TUI-refresh path runs this once every 5 seconds while the
// dashboard is open.
func BenchmarkEnrichFromScopeRunners(b *testing.B) {
	cfg, scopeRunners := makeEnrichCfg(20, 10) // 20 repos × 10 instances = 200 statuses
	m := &Manager{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Copy statuses so the in-place mutation doesn't compound across iterations.
		statuses := make([]RunnerStatus, 0, 200)
		for _, s := range makeEnrichStatuses(cfg) {
			statuses = append(statuses, s)
		}
		m.enrichFromScopeRunners(statuses, cfg, scopeRunners)
	}
}

// BenchmarkEnrichFromScopeRunners_Small: 5 repos × 2 instances. Mirrors a small
// personal config where the per-refresh alloc cost is most noticeable.
func BenchmarkEnrichFromScopeRunners_Small(b *testing.B) {
	cfg, scopeRunners := makeEnrichCfg(5, 2)
	m := &Manager{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		statuses := make([]RunnerStatus, 0, 10)
		for _, s := range makeEnrichStatuses(cfg) {
			statuses = append(statuses, s)
		}
		m.enrichFromScopeRunners(statuses, cfg, scopeRunners)
	}
}

// makeEnrichStatuses builds a fresh []RunnerStatus slice mirroring the entries
// produced by makeEnrichCfg. Kept separate from makeEnrichCfg so the benchmark
// can copy statuses per iteration without re-running the full fixture build.
func makeEnrichStatuses(cfg *config.Config) []RunnerStatus {
	var out []RunnerStatus
	for _, rc := range cfg.Runners {
		repoDisplay := rc.DisplayTarget()
		count := rc.Count
		if count < 1 {
			count = 1
		}
		for j := 1; j <= count; j++ {
			out = append(out, RunnerStatus{
				Instance: rc.Name + "-" + strconv.Itoa(j),
				Host:     rc.Host,
				Repo:     repoDisplay,
				Mode:     "docker",
			})
		}
	}
	return out
}

// BenchmarkContainerLocalStatusImageAndRevision measures the in-process cost
// of the combined container status script. The test (not the benchmark) is
// the authoritative energy measurement: SSH round trips per call drop from
// 2-3 (echo $HOME + bootstrap-failed marker + docker inspect) to exactly 1.
// This benchmark captures the per-call CPU/alloc cost of formatting the
// combined shell script and parsing its output, so regressions in the script
// builder or the parser surface here.
func BenchmarkContainerLocalStatusImageAndRevision(b *testing.B) {
	h := host.NewHost("test", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	mock := &testutil.MockExecutor{Output: "running|gh-sr/agentic-runner:2.320.0|sha256:abc|deadbeef"}
	h.SetConn(mock)
	m := &Manager{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = m.containerLocalStatusImageAndRevision(h, "ci-1")
	}
}

// BenchmarkManager_Status measures the per-RunnerConfig alloc cost of the
// inner work of Manager.Status (mode + repo + labels + name construction),
// with the container SSH round trip replaced by a mock that returns a
// well-formed response. This isolates the Go-side work the optimization
// targets — hoisting mode, repo, and labels-join out of the per-instance
// loop. The hot path is `internal/ops/ops.go` refreshAllOnce, which calls
// Manager.Status once per host per 5s tick; for a 5-host × 10-instance
// config that's 50 inner iterations per tick.
func BenchmarkManager_Status(b *testing.B) {
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	// Mock the container status script (single h.Run call) with a running
	// container response — covers both the bootstrap-failed marker and the
	// docker inspect branches.
	mock := &testutil.MockExecutor{
		Output: "running|gh-sr/agentic-runner:2.320.0|sha256:abc|deadbeef",
	}
	h.SetConn(mock)
	m := NewManager("dev")
	rc := config.RunnerConfig{
		Name:       "ci",
		Repo:       "o/r",
		Host:       "h",
		Count:      10,
		Labels:     []string{"self-hosted", "Linux", "X64"},
		RunnerMode: "container",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := m.Status(h, rc)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSplitNonEmptyLines measures the line-splitting helper used by
// ListRunnerInstanceDirs to parse the per-host `ls -1 ~/.gh-sr/runners` (or
// PowerShell Get-ChildItem) output. The function is package-private; the
// benchmark lives in the same package to keep access simple.
func BenchmarkSplitNonEmptyLines(b *testing.B) {
	raw := "alpha\nbeta\ngamma\n\ndelta\n   \nepsilon\n"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = splitNonEmptyLines(raw)
	}
}
