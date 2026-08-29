package runner

import (
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

func TestOrphanInstances(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, `ls -1 "$HOME/.gh-sr/runners"`):
				return "active-1\nold-1\n", nil
			case strings.Contains(cmd, "for f in \"$HOME/.config/systemd/user/ghsr-runner-\""):
				return "ghsr-runner-old-2\n", nil
			default:
				return "", nil
			}
		},
	}
	h := host.NewHost("linux", config.HostConfig{OS: "linux"})
	h.SetConn(mock)

	configured := map[string]struct{}{"active-1": {}}
	orphans, err := OrphanInstances(h, configured)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 2 || orphans[0] != "old-1" || orphans[1] != "old-2" {
		t.Fatalf("got %v", orphans)
	}
}

func TestConfiguredInstanceSet(t *testing.T) {
	t.Parallel()
	runners := []config.RunnerConfig{
		{Name: "ci", Host: "h1", Repo: "o/r", Count: 2},
		{Name: "web", Host: "h2", Repo: "o/r", Count: 1},
	}
	set := ConfiguredInstanceSet(runners, "h1")
	if len(set) != 2 {
		t.Fatalf("got %d entries", len(set))
	}
	if _, ok := set["ci-1"]; !ok {
		t.Fatal("missing ci-1")
	}
	if _, ok := set["ci-2"]; !ok {
		t.Fatal("missing ci-2")
	}
}

func TestCleanupOrphanInstanceDryRun(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "echo D") && strings.Contains(cmd, "echo S"):
				// Combined Linux orphan-plan probe: dir present, no svc.sh, user systemd unit.
				return "D\nU\n", nil
			default:
				return "", nil
			}
		},
	}
	h := host.NewHost("linux", config.HostConfig{OS: "linux"})
	h.SetConn(mock)
	m := NewManager("")
	plan, err := m.CleanupOrphanInstance(h, "old-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Autostart || !plan.Directory {
		t.Fatalf("plan = %+v", plan)
	}
}

// TestInstanceDirectoryExists_Windows pins the Windows branch of
// instanceDirectoryExists: it must call hostshell.RemoteWindowsDirExists with
// h.RunnerDir(instance) as the path. The Linux branch is exercised end-to-end
// via TestServiceCleanup_OrphanWithDirectoryInOps (test -d probe). The
// Windows-only helper landed in the same commit that closed duplicate-code
// #426 (refactor(hostshell): deduplicate Windows PowerShell scheduled-task
// probes), so we add a focused unit test to lock the wiring.
//
// Addr is set to "local" so config.IsLocalAddr reports true and
// host.Host.wrapCommand is a no-op — the mock sees the literal PowerShell
// script that RemoteWindowsDirExists emitted.
func TestInstanceDirectoryExists_Windows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		instance string
		reply    string
		want     bool
	}{
		{"dir present", "r1", "yes\r\n", true},
		{"dir absent", "gone", "no\r\n", false},
		{"dir absent whitespace", "gone", "   \r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if !strings.Contains(cmd, "Test-Path") || !strings.Contains(cmd, "-PathType Container") {
						t.Errorf("expected Test-Path -PathType Container probe, got: %q", cmd)
					}
					return tc.reply, nil
				},
			}
			h := host.NewHost("windows", config.HostConfig{OS: "windows", Addr: "local"})
			h.SetConn(mock)
			got, err := instanceDirectoryExists(h, tc.instance)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOrphanLinuxPlanProbe pins the combined-probe parsing for the Linux
// orphan-plan path: D (dir), S (svc.sh), U (user systemd unit), Y (system
// systemd unit) markers are mapped to the matching flags/kind. Asserts a
// single SSH round-trip per call (the whole point of the consolidation).
func TestOrphanLinuxPlanProbe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		out      string
		wantKind autostart.Kind
		wantSvc  bool
		wantDir  bool
	}{
		{"nothing", "", autostart.KindNone, false, false},
		{"dir only", "D\n", autostart.KindNone, false, true},
		{"svc only", "S\n", autostart.KindNone, true, false},
		{"dir+svc", "S\nD\n", autostart.KindNone, true, true},
		{"user only", "U\n", autostart.KindSystemdUser, false, false},
		{"system only", "Y\n", autostart.KindSystemdSystem, false, false},
		{"all three", "D\nS\nU\n", autostart.KindSystemdUser, true, true},
		{"crlf", "D\r\nS\r\nY\r\n", autostart.KindSystemdSystem, true, true},
		{"all three system", "D\nS\nY\n", autostart.KindSystemdSystem, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			mock := &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "echo D") && strings.Contains(cmd, "echo S") {
						calls++
						if !strings.Contains(cmd, "[ -d $HOME/") {
							t.Errorf("directory probe must allow remote $HOME expansion: %q", cmd)
						}
						return tc.out, nil
					}
					return "", nil
				},
			}
			h := host.NewHost("linux", config.HostConfig{OS: "linux"})
			h.SetConn(mock)
			kind, svc, dir, err := orphanLinuxPlanProbe(h, "old-1")
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Errorf("got %d SSH calls; want 1", calls)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %q; want %q", kind, tc.wantKind)
			}
			if svc != tc.wantSvc {
				t.Errorf("svc = %v; want %v", svc, tc.wantSvc)
			}
			if dir != tc.wantDir {
				t.Errorf("dir = %v; want %v", dir, tc.wantDir)
			}
		})
	}
}
