package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerConfig_InstanceNames(t *testing.T) {
	t.Parallel()
	rc := RunnerConfig{Name: "ci", Count: 0}
	names := rc.InstanceNames()
	if len(names) != 1 || names[0] != "ci-1" {
		t.Fatalf("count<1: got %v", names)
	}
	rc.Count = 3
	names = rc.InstanceNames()
	want := []string{"ci-1", "ci-2", "ci-3"}
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, names[i], want[i])
		}
	}
}

func TestLoad_rejectsGitHubPat(t *testing.T) {
	t.Setenv("GH_SR_TEST_PAT", "secret-from-env")
	dir := t.TempDir()
	path := filepath.Join(dir, "runners.yml")
	content := `
github:
  pat: env:GH_SR_TEST_PAT
hosts:
  h1:
    addr: a@b
    os: linux
    arch: amd64
runners:
  - name: r1
    repo: o/r
    host: h1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for legacy github.pat")
	}
	if !strings.Contains(err.Error(), "github.pat") {
		t.Errorf("error should mention github.pat: %v", err)
	}
}

func TestLoad_applyDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runners.yml")
	content := `
hosts:
  h1:
    addr: a@b
    os: linux
    arch: amd64
runners:
  - name: r1
    repo: o/r
    host: h1
    count: 0
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners[0].Count != 1 {
		t.Errorf("count default: got %d", cfg.Runners[0].Count)
	}
}

func TestValidate_errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		frag string
	}{
		{
			name: "github_pat_legacy",
			cfg: Config{
				GitHub:  GitHubConfig{PAT: "ghp_removed"},
				Hosts:   map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
			},
			frag: "github.pat",
		},
		{
			name: "no_hosts",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
			},
			frag: "at least one host",
		},
		{
			name: "host_addr",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{"h": {Addr: "", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
			},
			frag: `host "h"`,
		},
		{
			name: "host_os_invalid",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{"h": {Addr: "a@b", OS: "freebsd", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
			},
			frag: "os must be linux",
		},
		{
			name: "host_arch_invalid",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "riscv64"}},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
			},
			frag: "arch must be amd64",
		},
		{
			name: "no_runners",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
				Runners: nil,
			},
			frag: "at least one runner",
		},
		{
			name: "runner_name",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: "", Repo: "o/r", Host: "h"}},
			},
			frag: "runner name is required",
		},
		{
			name: "runner_repo_or_org",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: "r", Repo: "", Host: "h"}},
			},
			frag: "repo or org is required",
		},
		{
			name: "runner_host_missing",
			cfg: Config{
				GitHub:  GitHubConfig{},
				Hosts:   map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "unknown"}},
			},
			frag: "not found in hosts",
		},
		{
			name: "windows_ps_on_linux",
			cfg: Config{
				GitHub: GitHubConfig{},
				Hosts: map[string]HostConfig{
					"h": {Addr: "a@b", OS: "linux", Arch: "amd64", WindowsPS: "pwsh"},
				},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
			},
			frag: "windows_ps is only valid",
		},
		{
			name: "windows_ps_invalid_value",
			cfg: Config{
				GitHub: GitHubConfig{},
				Hosts: map[string]HostConfig{
					"w": {Addr: "a@b", OS: "windows", Arch: "amd64", WindowsPS: "bash"},
				},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "w"}},
			},
			frag: "windows_ps must be powershell or pwsh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.frag) {
				t.Errorf("error %q should contain %q", err.Error(), tc.frag)
			}
		})
	}
}

func TestDefaultLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		os, arch string
		want     []string
	}{
		{"linux", "amd64", []string{"self-hosted", "Linux", "X64"}},
		{"linux", "arm64", []string{"self-hosted", "Linux", "ARM64"}},
		{"darwin", "amd64", []string{"self-hosted", "macOS", "X64"}},
		{"darwin", "arm64", []string{"self-hosted", "macOS", "ARM64"}},
		{"windows", "amd64", []string{"self-hosted", "Windows", "X64"}},
	}
	for _, tc := range cases {
		got := DefaultLabels(tc.os, tc.arch)
		if len(got) != len(tc.want) {
			t.Errorf("DefaultLabels(%s,%s): got %v want %v", tc.os, tc.arch, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("DefaultLabels(%s,%s)[%d]: got %q want %q", tc.os, tc.arch, i, got[i], tc.want[i])
			}
		}
	}
}

func TestEffectiveLabels(t *testing.T) {
	t.Parallel()
	rc := RunnerConfig{Name: "r", Labels: []string{"custom"}}
	if got := rc.EffectiveLabels("linux", "amd64"); len(got) != 1 || got[0] != "custom" {
		t.Errorf("explicit labels should be used: got %v", got)
	}
	rc2 := RunnerConfig{Name: "r"}
	got := rc2.EffectiveLabels("linux", "amd64")
	if len(got) != 3 || got[0] != "self-hosted" || got[1] != "Linux" || got[2] != "X64" {
		t.Errorf("empty labels should be auto-generated: got %v", got)
	}
}

func TestValidate_emptyOSArchRemoteHost(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub:  GitHubConfig{},
		Hosts:   map[string]HostConfig{"h": {Addr: "user@host"}},
		Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty os/arch on remote host should be valid (auto-detected at runtime): %v", err)
	}
}

func TestNeedsDetection(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Hosts: map[string]HostConfig{
			"h1": {Addr: "a@b", OS: "linux", Arch: "amd64"},
		},
	}
	if cfg.NeedsDetection() {
		t.Error("all hosts have os/arch, should not need detection")
	}
	cfg.Hosts["h2"] = HostConfig{Addr: "c@d"}
	if !cfg.NeedsDetection() {
		t.Error("h2 has no os/arch, should need detection")
	}
	cfg.Hosts["local"] = HostConfig{Addr: "local"}
	if !cfg.NeedsDetection() {
		t.Error("local host should be skipped, h2 still needs detection")
	}
}

func TestValidate_windows_ps_pwsh(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub: GitHubConfig{},
		Hosts: map[string]HostConfig{
			"w": {Addr: "a@b", OS: "windows", Arch: "amd64", WindowsPS: "pwsh"},
		},
		Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "w"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("windows_ps: pwsh should be valid, got: %v", err)
	}
}

func TestIsLocalAddr(t *testing.T) {
	t.Parallel()
	if !IsLocalAddr("local") {
		t.Error("\"local\" should be local")
	}
	if !IsLocalAddr("Local") {
		t.Error("\"Local\" should be local (case-insensitive)")
	}
	if IsLocalAddr("user@host") {
		t.Error("\"user@host\" should not be local")
	}
	if IsLocalAddr("") {
		t.Error("empty string should not be local")
	}
}

func TestValidate_localHost(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub: GitHubConfig{},
		Hosts:  map[string]HostConfig{"laptop": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{
			{Name: "r", Repo: "o/r", Host: "laptop"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("local host should be valid: %v", err)
	}
}

func TestApplyDefaults_localHostAutoDetect(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub: GitHubConfig{},
		Hosts:  map[string]HostConfig{"laptop": {Addr: "local"}},
		Runners: []RunnerConfig{
			{Name: "r", Repo: "o/r", Host: "laptop"},
		},
	}
	cfg.applyDefaults()

	h := cfg.Hosts["laptop"]
	if h.OS == "" {
		t.Error("OS should be auto-detected for local host")
	}
	if h.Arch == "" {
		t.Error("Arch should be auto-detected for local host")
	}
}

func TestLoad_localHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runners.yml")
	content := `
hosts:
  laptop:
    addr: local
runners:
  - name: r1
    repo: o/r
    host: laptop
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Hosts["laptop"]
	if h.OS == "" {
		t.Error("OS should be auto-detected")
	}
	if h.Arch == "" {
		t.Error("Arch should be auto-detected")
	}
}

func TestConfig_queries(t *testing.T) {
	cfg := &Config{
		GitHub: GitHubConfig{},
		Hosts: map[string]HostConfig{
			"h1": {Addr: "a@b", OS: "linux", Arch: "amd64"},
			"h2": {Addr: "c@d", OS: "darwin", Arch: "arm64"},
		},
		Runners: []RunnerConfig{
			{Name: "alpha", Repo: "o/a", Host: "h1", Count: 2},
			{Name: "beta", Repo: "o/b", Host: "h2", Count: 1},
			{Name: "gamma", Repo: "o/a", Host: "h1", Count: 1},
		},
	}

	h1 := cfg.RunnersForHost("h1")
	if len(h1) != 2 {
		t.Fatalf("RunnersForHost h1: got %d", len(h1))
	}
	br := cfg.RunnersForRepo("o/a")
	if len(br) != 2 {
		t.Fatalf("RunnersForRepo o/a: got %d", len(br))
	}
	repos := cfg.UniqueRepos()
	if len(repos) != 2 {
		t.Fatalf("UniqueRepos: got %v", repos)
	}

	rc, ok := cfg.FindRunner("alpha")
	if !ok || rc.Name != "alpha" {
		t.Fatalf("FindRunner alpha: ok=%v", ok)
	}
	rc, ok = cfg.FindRunner("alpha-2")
	if !ok || rc.Name != "alpha" {
		t.Fatalf("FindRunner alpha-2: ok=%v name=%v", ok, rc)
	}
	rc, ok = cfg.FindRunner("nope")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestFindRunnerForLogs_and_ResolveRunnerInstance(t *testing.T) {
	cfg := &Config{
		GitHub: GitHubConfig{},
		Hosts: map[string]HostConfig{
			"h1": {Addr: "a@b", OS: "linux", Arch: "amd64"},
			"h2": {Addr: "c@d", OS: "windows", Arch: "amd64"},
		},
		Runners: []RunnerConfig{
			{Name: "dup", Repo: "o/r", Host: "h1", Count: 1},
			{Name: "dup", Repo: "o/r", Host: "h2", Count: 1},
		},
	}

	_, err := cfg.FindRunnerForLogs("dup-1", "")
	if err == nil {
		t.Fatal("expected ambiguous without --host")
	}

	rc, err := cfg.FindRunnerForLogs("dup-1", "h2")
	if err != nil || rc.Host != "h2" {
		t.Fatalf("FindRunnerForLogs with host: err=%v host=%v", err, rc)
	}

	inst, err := rc.ResolveRunnerInstance("dup-1")
	if err != nil || inst != "dup-1" {
		t.Fatalf("ResolveRunnerInstance dup-1: %v %q", err, inst)
	}
	inst, err = rc.ResolveRunnerInstance("dup")
	if err != nil || inst != "dup-1" {
		t.Fatalf("ResolveRunnerInstance dup: %v %q", err, inst)
	}

	solo := &Config{
		GitHub:  GitHubConfig{},
		Hosts:   map[string]HostConfig{"h1": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{Name: "solo", Repo: "o/r", Host: "h1", Count: 2}},
	}
	_, err = solo.FindRunnerForLogs("solo", "")
	if err != nil {
		t.Fatal(err)
	}
	soloRC := &solo.Runners[0]
	_, err = soloRC.ResolveRunnerInstance("solo")
	if err == nil {
		t.Fatal("expected error for multi-instance base name")
	}
}

func TestRunnerConfig_Scope(t *testing.T) {
	t.Parallel()
	rc := RunnerConfig{Repo: "o/r"}
	if rc.Scope() != "repo" {
		t.Errorf("repo scope: got %q", rc.Scope())
	}
	if rc.ScopeTarget() != "o/r" {
		t.Errorf("repo target: got %q", rc.ScopeTarget())
	}
	rcOrg := RunnerConfig{Org: "my-org"}
	if rcOrg.Scope() != "org" {
		t.Errorf("org scope: got %q", rcOrg.Scope())
	}
	if rcOrg.ScopeTarget() != "my-org" {
		t.Errorf("org target: got %q", rcOrg.ScopeTarget())
	}
}

func TestValidate_orgRunner(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub: GitHubConfig{},
		Hosts:  map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{
			Name: "r", Org: "my-org", Host: "h",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("org runner should be valid: %v", err)
	}
}

func TestValidate_orgAndRepoBothSet(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub: GitHubConfig{},
		Hosts:  map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{
			Name: "r", Repo: "o/r", Org: "my-org", Host: "h",
		}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected error about both: %v", err)
	}
}

func TestValidate_groupRequiresOrg(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub: GitHubConfig{},
		Hosts:  map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{
			Name: "r", Repo: "o/r", Host: "h", Group: "grp",
		}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "group requires org") {
		t.Fatalf("expected error about group: %v", err)
	}
}

func TestValidate_ephemeralRunner(t *testing.T) {
	t.Parallel()
	cfg := Config{
		GitHub: GitHubConfig{},
		Hosts:  map[string]HostConfig{"h": {Addr: "a@b", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{
			Name: "r", Repo: "o/r", Host: "h", Ephemeral: true,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ephemeral runner should be valid: %v", err)
	}
}

func TestUniqueOrgs(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Runners: []RunnerConfig{
			{Name: "a", Repo: "o/r", Host: "h"},
			{Name: "b", Org: "my-org", Host: "h"},
			{Name: "c", Org: "my-org", Host: "h"},
			{Name: "d", Org: "other-org", Host: "h"},
		},
	}
	orgs := cfg.UniqueOrgs()
	if len(orgs) != 2 {
		t.Fatalf("expected 2 orgs, got %v", orgs)
	}
}

func TestUniqueRepos_skipOrgOnly(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Runners: []RunnerConfig{
			{Name: "a", Repo: "o/r", Host: "h"},
			{Name: "b", Org: "my-org", Host: "h"},
		},
	}
	repos := cfg.UniqueRepos()
	if len(repos) != 1 || repos[0] != "o/r" {
		t.Fatalf("expected [o/r], got %v", repos)
	}
}

func TestApplyEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	content := `
# comment
export FOO=bar
EMPTY=
SKIP
BAZ="quoted"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOO", "")
	t.Setenv("BAZ", "")
	if err := ApplyEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FOO") != "bar" {
		t.Errorf("FOO: got %q", os.Getenv("FOO"))
	}
	if os.Getenv("BAZ") != "quoted" {
		t.Errorf("BAZ: got %q", os.Getenv("BAZ"))
	}
}

func TestApplyEnvFile_missing(t *testing.T) {
	t.Parallel()
	if err := ApplyEnvFile(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConfigPath_wmConfig(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.yml")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv(EnvVarConfigPath, other)

	got, err := ResolveConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != other {
		t.Errorf("GH_SR_CONFIG: want %q got %q", other, got)
	}
}

func TestResolveConfigPath_ignoresCwdLocal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	local := filepath.Join(dir, "config", "runners.yml")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv(EnvVarConfigPath, "")

	got, err := ResolveConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".gh-sr", "runners.yml")
	if got != want {
		t.Errorf("cwd config/runners.yml must not be auto-used: want %q got %q", want, got)
	}
}

func TestResolveConfigPath_explicit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "custom.yml")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv(EnvVarConfigPath, filepath.Join(dir, "ignored.yml"))
	if err := os.WriteFile(filepath.Join(dir, "ignored.yml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveConfigPath("custom.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Errorf("explicit flag wins: got %q want %q", got, p)
	}
}

func TestFilterRunners(t *testing.T) {
	cfg := &Config{
		Runners: []RunnerConfig{
			{Name: "a", Repo: "o/1", Host: "h1", Count: 2},
			{Name: "b", Repo: "o/2", Host: "h2", Count: 1},
			{Name: "c", Repo: "o/1", Host: "h1", Count: 1},
		},
	}

	all := FilterRunners(cfg, "", "", nil)
	if len(all) != 3 {
		t.Fatalf("no filter: got %d", len(all))
	}

	byHost := FilterRunners(cfg, "h1", "", nil)
	if len(byHost) != 2 {
		t.Fatalf("host h1: got %d", len(byHost))
	}

	byRepo := FilterRunners(cfg, "", "o/1", nil)
	if len(byRepo) != 2 {
		t.Fatalf("repo o/1: got %d", len(byRepo))
	}

	combo := FilterRunners(cfg, "h1", "o/1", nil)
	if len(combo) != 2 {
		t.Fatalf("host+repo: got %d", len(combo))
	}

	byName := FilterRunners(cfg, "", "", []string{"b"})
	if len(byName) != 1 || byName[0].Name != "b" {
		t.Fatalf("name b: got %v", byName)
	}

	byInst := FilterRunners(cfg, "", "", []string{"a-2"})
	if len(byInst) != 1 || byInst[0].Name != "a" {
		t.Fatalf("instance a-2: got %v", byInst)
	}

	stack := FilterRunners(cfg, "h1", "o/1", []string{"c"})
	if len(stack) != 1 || stack[0].Name != "c" {
		t.Fatalf("stacked filters: got %v", stack)
	}
}

func TestRunnerConfig_RunnerMode_defaults(t *testing.T) {
	t.Parallel()
	rc := RunnerConfig{}
	if rc.EffectiveRunnerMode() != RunnerModeNative {
		t.Fatalf("empty runner_mode should default to native, got %q", rc.EffectiveRunnerMode())
	}
	if rc.IsContainerMode() {
		t.Fatal("empty runner_mode should not be container mode")
	}
}

func TestRunnerConfig_RunnerMode_container(t *testing.T) {
	t.Parallel()
	rc := RunnerConfig{RunnerMode: RunnerModeContainer}
	if rc.EffectiveRunnerMode() != RunnerModeContainer {
		t.Fatalf("runner_mode: container should return container, got %q", rc.EffectiveRunnerMode())
	}
	if !rc.IsContainerMode() {
		t.Fatal("IsContainerMode() should return true for container mode")
	}
}

func TestValidate_runnerMode_invalid(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h", RunnerMode: "docker"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid runner_mode")
	}
}

func TestValidate_runnerMode_container_nonAgentic_valid(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Hosts: map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{
			Name:       "r",
			Repo:       "o/r",
			Host:       "h",
			RunnerMode: RunnerModeContainer,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("container mode without profile should validate: %v", err)
	}
}

func TestValidate_runnerMode_container_valid(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Hosts: map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{
			Name: "r", Repo: "o/r", Host: "h", Count: 2,
			RunnerMode: RunnerModeContainer,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error for valid container-mode config: %v", err)
	}
}

func TestValidate_runnerNameTokens(t *testing.T) {
	t.Parallel()

	t.Run("valid slug characters", func(t *testing.T) {
		t.Parallel()
		cfg := Config{
			Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
			Runners: []RunnerConfig{{Name: "ci_runner.1-prod", Repo: "o/r", Host: "h"}},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error for valid runner name: %v", err)
		}
	})

	invalid := []string{
		"ci runner",
		"../ci",
		"ci;rm",
		"ci$(touch-pwned)",
		"ci`touch-pwned`",
	}
	for _, name := range invalid {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: name, Repo: "o/r", Host: "h"}},
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error for unsafe runner name")
			}
			if !strings.Contains(err.Error(), "runner name") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidate_runnerNameAutostartCollisions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		first  string
		second string
	}{
		{first: "ci.foo", second: "ci-foo"},
		{first: "ci_runner", second: "ci-runner"},
		{first: "ci--runner", second: "ci-runner"},
		{first: "ci-", second: "ci"},
	} {
		t.Run(tc.first+"_"+tc.second, func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				Hosts: map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{
					{Name: tc.first, Repo: "o/one", Host: "h"},
					{Name: tc.second, Repo: "o/two", Host: "h"},
				},
			}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "autostart name sanitization") {
				t.Fatalf("expected autostart collision error, got %v", err)
			}
		})
	}
}

func TestValidate_duplicateRunnerInstanceNames(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Hosts: map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{
			{Name: "agentic", Repo: "o/one", Host: "h", RunnerMode: RunnerModeContainer},
			{Name: "agentic", Repo: "o/two", Host: "h", RunnerMode: RunnerModeContainer},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected duplicate instance name error")
	}
	if !strings.Contains(err.Error(), `runner instance "agentic-1" is defined more than once`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_containerRunnerImage_extraApt_ok(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h", RunnerMode: RunnerModeContainer}},
		ContainerRunnerImage: ContainerRunnerImageConfig{
			ExtraAptPackages: []string{"ffmpeg", "  libyaml-0-2  "},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_containerRunnerImage_extraApt_invalidName(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
		ContainerRunnerImage: ContainerRunnerImageConfig{
			ExtraAptPackages: []string{"Bad_Package"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid package name")
	}
}

func TestValidate_containerRunnerImage_extraApt_emptyEntry(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
		ContainerRunnerImage: ContainerRunnerImageConfig{
			ExtraAptPackages: []string{"curl", "  "},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty package entry")
	}
}

func TestValidate_containerRunnerImage_extraApt_tooMany(t *testing.T) {
	t.Parallel()
	pkgs := make([]string, maxContainerRunnerExtraAptPackages+1)
	for i := range pkgs {
		pkgs[i] = fmt.Sprintf("p%d", i)
	}
	cfg := Config{
		Hosts:                map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners:              []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h"}},
		ContainerRunnerImage: ContainerRunnerImageConfig{ExtraAptPackages: pkgs},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when extra_apt_packages exceeds max")
	}
}

func TestValidate_containerRunnerImage_mtu(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mtu     int
		wantErr bool
	}{
		{"zero auto-detect", 0, false},
		{"min valid", 576, false},
		{"typical reduced", 1460, false},
		{"max valid", 1500, false},
		{"below min", 575, true},
		{"above max", 1501, true},
		{"negative", -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
				Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h", RunnerMode: RunnerModeContainer}},
				ContainerRunnerImage: ContainerRunnerImageConfig{
					MTU: tc.mtu,
				},
			}
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("mtu=%d: expected error, got nil", tc.mtu)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("mtu=%d: unexpected error: %v", tc.mtu, err)
			}
		})
	}
}

func TestConfig_ContainerRunnerImageMTU(t *testing.T) {
	t.Parallel()
	if got := (&Config{ContainerRunnerImage: ContainerRunnerImageConfig{MTU: 1460}}).ContainerRunnerImageMTU(); got != 1460 {
		t.Fatalf("ContainerRunnerImageMTU = %d, want 1460", got)
	}
	if got := (&Config{}).ContainerRunnerImageMTU(); got != 0 {
		t.Fatalf("ContainerRunnerImageMTU (unset) = %d, want 0", got)
	}
	var nilCfg *Config
	if got := nilCfg.ContainerRunnerImageMTU(); got != 0 {
		t.Fatalf("ContainerRunnerImageMTU (nil) = %d, want 0", got)
	}
}

func TestConfig_ContainerRunnerImageBootstrapSettings(t *testing.T) {
	t.Parallel()
	if got := (&Config{}).ContainerRunnerImageDockerdStartTimeout(); got != defaultContainerDockerdStartTimeout {
		t.Fatalf("default dockerd timeout = %d, want %d", got, defaultContainerDockerdStartTimeout)
	}
	if got := (&Config{ContainerRunnerImage: ContainerRunnerImageConfig{DockerdStartTimeout: 120}}).ContainerRunnerImageDockerdStartTimeout(); got != 120 {
		t.Fatalf("dockerd timeout = %d, want 120", got)
	}
	if got := (&Config{}).ContainerRunnerImageBootstrapMaxRetries(); got != defaultContainerBootstrapMaxRetries {
		t.Fatalf("default bootstrap retries = %d, want %d", got, defaultContainerBootstrapMaxRetries)
	}
	if got := (&Config{ContainerRunnerImage: ContainerRunnerImageConfig{StartStaggerSeconds: 5}}).ContainerRunnerImageStartStaggerSeconds(); got != 5 {
		t.Fatalf("start stagger = %d, want 5", got)
	}
}

func TestConfig_validateContainerRunnerImageBootstrapFields(t *testing.T) {
	t.Parallel()
	base := Config{
		Hosts:   map[string]HostConfig{"h": {Addr: "local", OS: "linux", Arch: "amd64"}},
		Runners: []RunnerConfig{{Name: "r", Repo: "o/r", Host: "h", RunnerMode: RunnerModeContainer}},
	}
	cases := []struct {
		name string
		img  ContainerRunnerImageConfig
		ok   bool
	}{
		{"valid defaults", ContainerRunnerImageConfig{}, true},
		{"valid custom", ContainerRunnerImageConfig{DockerdStartTimeout: 120, BootstrapMaxRetries: 10, StartStaggerSeconds: 5}, true},
		{"dockerd timeout low", ContainerRunnerImageConfig{DockerdStartTimeout: 29}, false},
		{"dockerd timeout high", ContainerRunnerImageConfig{DockerdStartTimeout: 301}, false},
		{"bootstrap retries low", ContainerRunnerImageConfig{BootstrapMaxRetries: 0}, true},
		{"bootstrap retries high", ContainerRunnerImageConfig{BootstrapMaxRetries: 21}, false},
		{"stagger high", ContainerRunnerImageConfig{StartStaggerSeconds: 61}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			cfg.ContainerRunnerImage = tc.img
			err := cfg.Validate()
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConfig_ContainerRunnerImageExtraAptPackages_copy(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		ContainerRunnerImage: ContainerRunnerImageConfig{
			ExtraAptPackages: []string{"a", "b"},
		},
	}
	out := cfg.ContainerRunnerImageExtraAptPackages()
	out[0] = "z"
	if cfg.ContainerRunnerImage.ExtraAptPackages[0] != "a" {
		t.Fatal("ContainerRunnerImageExtraAptPackages should return a copy")
	}
}

func TestRunnerConfig_GitHubRegistrationURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rc   RunnerConfig
		want string
	}{
		{
			name: "repo only",
			rc:   RunnerConfig{Repo: "owner/repo"},
			want: "https://github.com/owner/repo",
		},
		{
			name: "org only",
			rc:   RunnerConfig{Org: "my-org"},
			want: "https://github.com/my-org",
		},
		{
			// Precedence must match Scope / ScopeTarget: Org wins when both
			// are set, so a user with a misconfigured dual-set RunnerConfig
			// still registers against the org the rest of the package uses
			// for API calls.
			name: "both set: org takes precedence",
			rc:   RunnerConfig{Repo: "owner/repo", Org: "my-org"},
			want: "https://github.com/my-org",
		},
		{
			// Empty / unset RunnerConfig returns the empty-prefix URL. The
			// runner config flow is expected to short-circuit before this
			// ever reaches the network, but the helper should not panic.
			name: "both empty",
			rc:   RunnerConfig{},
			want: "https://github.com/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rc.GitHubRegistrationURL(); got != tc.want {
				t.Errorf("GitHubRegistrationURL(%+v): got %q, want %q", tc.rc, got, tc.want)
			}
		})
	}
}

func TestRunnerConfig_DisplayTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rc   RunnerConfig
		want string
	}{
		{name: "repo", rc: RunnerConfig{Repo: "o/r"}, want: "o/r"},
		{name: "org", rc: RunnerConfig{Org: "my-org"}, want: "org:my-org"},
		{name: "org with group", rc: RunnerConfig{Org: "my-org", Group: "ci-pool"}, want: "org:my-org group=ci-pool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rc.DisplayTarget(); got != tc.want {
				t.Errorf("DisplayTarget(): got %q, want %q", got, tc.want)
			}
		})
	}
}
