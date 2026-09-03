package doctor

import (
	"strings"
	"testing"
)

// TestAnalyzeLockWorkflow pins the legacy-era marker rules: the three FAIL
// markers (sudo-era AWF invocation, retired sandbox profile name, host-network
// gateway layout) and the compiler_version WARN gate. Pure function, no I/O.
func TestAnalyzeLockWorkflow(t *testing.T) {
	t.Parallel()

	rootlessLock := `name: CI agent
compiler_version: v0.88.2
on:
  push:
    branches: [main]
    - name: agent
      runs-on: self-hosted
      steps: []`
	t.Run("rootless lock.yml yields no findings", func(t *testing.T) {
		t.Parallel()
		if got := AnalyzeLockWorkflow("ci.lock.yml", rootlessLock); len(got) != 0 {
			t.Errorf("rootless-era lock.yml must yield no findings, got %#v", got)
		}
	})

	t.Run("sudo -E awf marker fails", func(t *testing.T) {
		t.Parallel()
		c := strings.Replace(rootlessLock, "steps: []", "run: sudo -E awf --input-file /tmp/in", 1)
		got := AnalyzeLockWorkflow("ci.lock.yml", c)
		if len(got) != 1 || got[0].Sev != sevFail {
			t.Fatalf("expected exactly one FAIL, got %#v", got)
		}
		if !strings.Contains(got[0].Msg, `"sudo -E awf"`) {
			t.Errorf("Msg should quote the marker, got %q", got[0].Msg)
		}
		if !strings.Contains(got[0].Msg, "recompile with gh-aw") {
			t.Errorf("Msg should direct at recompiling, got %q", got[0].Msg)
		}
	})

	t.Run("docker-sudo-iptables profile marker fails", func(t *testing.T) {
		t.Parallel()
		c := strings.Replace(rootlessLock, "steps: []", "sandbox: docker-sudo-iptables", 1)
		got := AnalyzeLockWorkflow("ci.lock.yml", c)
		if len(got) != 1 || got[0].Sev != sevFail {
			t.Fatalf("expected exactly one FAIL, got %#v", got)
		}
		if !strings.Contains(got[0].Msg, "docker-sudo-iptables") {
			t.Errorf("Msg should name the retired profile, got %q", got[0].Msg)
		}
	})

	t.Run("host-network gateway marker fails", func(t *testing.T) {
		t.Parallel()
		// The retired gateway layout ran the mcpg image on --network host.
		c := strings.Replace(rootlessLock, "steps: []",
			"docker run --network host ghcr.io/github/gh-aw-mcpg:latest", 1)
		got := AnalyzeLockWorkflow("ci.lock.yml", c)
		if len(got) != 1 || got[0].Sev != sevFail {
			t.Fatalf("expected exactly one FAIL, got %#v", got)
		}
		if !strings.Contains(got[0].Msg, "--network host + gh-aw-mcpg") {
			t.Errorf("Msg should name the combined marker, got %q", got[0].Msg)
		}
	})

	t.Run("gh-aw-mcpg without --network host does not fail", func(t *testing.T) {
		t.Parallel()
		c := strings.Replace(rootlessLock, "steps: []",
			"docker run --network bridge ghcr.io/github/gh-aw-mcpg:latest", 1)
		if got := AnalyzeLockWorkflow("ci.lock.yml", c); len(got) != 0 {
			t.Errorf("bridge-network gateway is the rootless layout, got %#v", got)
		}
	})

	t.Run("old compiler_version warns", func(t *testing.T) {
		t.Parallel()
		c := strings.Replace(rootlessLock, "compiler_version: v0.88.2", "compiler_version: 0.83.1", 1)
		got := AnalyzeLockWorkflow("ci.lock.yml", c)
		if len(got) != 1 || got[0].Sev != sevWarn {
			t.Fatalf("expected exactly one WARN, got %#v", got)
		}
		if !strings.Contains(got[0].Msg, "0.83.1") {
			t.Errorf("Msg should report the observed version, got %q", got[0].Msg)
		}
	})

	t.Run("both era markers and old version stack", func(t *testing.T) {
		t.Parallel()
		c := strings.Replace(rootlessLock, "steps: []", "run: sudo -E awf --version", 1)
		c = strings.Replace(c, "compiler_version: v0.88.2", "compiler_version: 0.85.0", 1)
		got := AnalyzeLockWorkflow("ci.lock.yml", c)
		if len(got) != 2 {
			t.Fatalf("expected FAIL + WARN, got %#v", got)
		}
		if got[0].Sev != sevFail || got[1].Sev != sevWarn {
			t.Errorf("findings must be FAIL first then WARN, got %#v", got)
		}
	})
}

func TestLockCompilerVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"plain", "compiler_version: 0.88.2\nother: x", "0.88.2"},
		{"v-prefix", "compiler_version: v0.83.1", "0.83.1"},
		{"quoted", `compiler_version: "0.90.0"`, "0.90.0"},
		{"indented", "  compiler_version: 1.2.3", "1.2.3"},
		{"absent", "name: x\non: push", ""},
		{"empty value", "compiler_version:", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lockCompilerVersion(tc.content); got != tc.want {
				t.Errorf("lockCompilerVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int
	}{
		{"0.88.0", "0.88.0", 0},
		{"0.88.2", "0.88.0", 1},
		{"0.83.1", "0.88.0", -1},
		{"0.88", "0.88.0", 0},  // missing part = 0
		{"1.0.0", "0.99.9", 1}, // major dominates
		{"0.87.9", "0.88.0", -1},
	}
	for _, tc := range cases {
		if got := compareSemver(tc.a, tc.b); got != tc.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
