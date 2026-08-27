package tui

import (
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
)

// benchFormatConfigCfg mirrors the realistic shape a FormatConfig caller
// passes: a few hosts and a handful of runners (the typical developer
// config), plus one ephemeral + one container-mode runner so the %s/%d
// branches inside FormatConfig are all exercised.
var benchFormatConfigCfg = func() *config.Config {
	hosts := map[string]config.HostConfig{
		"alpha":   {Addr: "local", OS: "linux", Arch: "amd64"},
		"bravo":   {Addr: "ssh://b@10.0.0.2", OS: "linux", Arch: "arm64"},
		"charlie": {Addr: "ssh://c@10.0.0.3", OS: "darwin", Arch: "arm64"},
	}
	runners := []config.RunnerConfig{
		{Name: "native-default", Repo: "baizhiheizi/gh-sr", Host: "alpha", Count: 1, Labels: []string{"self-hosted", "linux", "amd64"}},
		{Name: "container-ci", Repo: "baizhiheizi/gh-sr", Host: "bravo", Count: 2, RunnerMode: config.RunnerModeContainer, Labels: []string{"self-hosted", "linux", "arm64"}},
		{Name: "ephemeral-uat", Org: "my-org", Group: "uat-pool", Host: "charlie", Count: 1, Ephemeral: true, Labels: []string{"self-hosted", "darwin", "arm64"}},
		{Name: "windows-build", Repo: "baizhiheizei/win", Host: "alpha", Count: 1, Labels: []string{"self-hosted", "windows", "amd64"}},
	}
	return &config.Config{Hosts: hosts, Runners: runners}
}()

// BenchmarkFormatConfig is the sentinel for FormatConfig. The function
// is called whenever the user opens the dashboard's config panel
// (`m.scrollLines = wrapLines(FormatConfig(m.cfg), ...)`) — not the
// 5-second refresh tick, but still user-driven and a focus area for
// removing the per-render reflection + []interface{} allocations that
// fmt.Sprintf drags in.
func BenchmarkFormatConfig(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = FormatConfig(benchFormatConfigCfg)
	}
}
