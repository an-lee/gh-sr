package agentic

import (
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

// failureByName returns the first failure with the given Name, or nil if not found.
func failureByName(t *testing.T, failures []PrereqFailure, name string) PrereqFailure {
	t.Helper()
	for _, f := range failures {
		if f.Name == name {
			return f
		}
	}
	return PrereqFailure{}
}

func TestValidateContainerInnerNetwork(t *testing.T) {
	t.Parallel()

	t.Run("non-linux short-circuits", func(t *testing.T) {
		t.Parallel()
		h := host.NewHost("h", config.HostConfig{OS: "darwin"})
		h.SetConn(&prereqTestExecutor{})
		if got := ValidateContainerInnerNetwork(h, "gh-sr-runner", "agentic-1"); got != nil {
			t.Errorf("non-linux must short-circuit, got %#v", got)
		}
	})

	t.Run("successful check returns nil", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				`docker exec "gh-sr-runner" sh -c 'set -eu
ok=0
for i in 1 2 3 4 5; do
  ip=$(docker run --rm --add-host=host.docker.internal:host-gateway alpine getent hosts host.docker.internal 2>/dev/null | awk "{print \$1; exit}")
  case "$ip" in
    "" | 127.* | ::1) ;;
    *) ok=1; break ;;
  esac
  sleep 1
done
[ "$ok" -eq 1 ]'`: ``,
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateContainerInnerNetwork(h, "gh-sr-runner", "agentic-1"); got != nil {
			t.Errorf("successful check must return nil, got %#v", got)
		}
	})

	t.Run("failing check returns host-docker-internal warning", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{} // every command errors
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateContainerInnerNetwork(h, "gh-sr-runner", "agentic-1")
		if len(failures) != 1 {
			t.Fatalf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
		f := failures[0]
		if f.Name != "container-inner-host-docker-internal" {
			t.Errorf("Name = %q, want %q", f.Name, "container-inner-host-docker-internal")
		}
		if f.Severity != SeverityWarning {
			t.Errorf("Severity = %q, want warning", f.Severity)
		}
		if !strings.Contains(f.Message, "gh-sr-runner") {
			t.Errorf("Message should name the container, got %q", f.Message)
		}
		if !strings.Contains(f.Remediation, "gh sr rebuild agentic-1") {
			t.Errorf("Remediation should reference gh sr rebuild, got %q", f.Remediation)
		}
		if f.DocRef == "" {
			t.Error("DocRef should be populated")
		}
	})
}

func TestValidateContainerNodeNPM(t *testing.T) {
	t.Parallel()

	t.Run("non-linux short-circuits", func(t *testing.T) {
		t.Parallel()
		h := host.NewHost("h", config.HostConfig{OS: "darwin"})
		h.SetConn(&prereqTestExecutor{})
		if got := ValidateContainerNodeNPM(h, "gh-sr-runner", "agentic-1"); got != nil {
			t.Errorf("non-linux must short-circuit, got %#v", got)
		}
	})

	t.Run("successful check returns nil", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				`docker exec "gh-sr-runner" sh -lc 'command -v node >/dev/null && command -v npm >/dev/null'`: ``,
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateContainerNodeNPM(h, "gh-sr-runner", "agentic-1"); got != nil {
			t.Errorf("successful check must return nil, got %#v", got)
		}
	})

	t.Run("failing check returns container-node-npm warning", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateContainerNodeNPM(h, "gh-sr-runner", "agentic-1")
		f := failureByName(t, failures, "container-node-npm")
		if f.Name == "" {
			t.Fatalf("expected container-node-npm failure, got %#v", failures)
		}
		if f.Severity != SeverityWarning {
			t.Errorf("Severity = %q, want warning", f.Severity)
		}
		if !strings.Contains(f.Message, "node LTS/npm") {
			t.Errorf("Message should mention node LTS/npm, got %q", f.Message)
		}
		if !strings.Contains(f.Remediation, "gh sr rebuild") {
			t.Errorf("Remediation should mention rebuild, got %q", f.Remediation)
		}
	})
}

func TestValidateContainerPrereqs_nonLinux(t *testing.T) {
	t.Parallel()
	for _, os := range []string{"darwin", "windows"} {
		os := os
		t.Run(os, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("h", config.HostConfig{OS: os})
			h.SetConn(&prereqTestExecutor{})
			failures := ValidateContainerPrereqs(h)
			if len(failures) != 1 {
				t.Fatalf("expected 1 failure on %s, got %d (%#v)", os, len(failures), failures)
			}
			if failures[0].Name != "linux-required" {
				t.Errorf("Name = %q, want linux-required", failures[0].Name)
			}
			if failures[0].Severity != SeverityError {
				t.Errorf("Severity = %q, want error", failures[0].Severity)
			}
		})
	}
}

// TestValidateContainerPrereqs_happyPath checks the linear chain:
// docker --version -> docker info -> docker --privileged smoke test all pass.
func TestValidateContainerPrereqs_happyPath(t *testing.T) {
	t.Parallel()
	exec := &prereqTestExecutor{
		response: map[string]string{
			dockerChainCheckCommand("privileged"): "#docker-cli:0\n#docker-daemon:0\n#docker-privileged:0",
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux"})
	h.SetConn(exec)
	if got := ValidateContainerPrereqs(h); len(got) != 0 {
		t.Errorf("expected no failures on happy path, got %#v", got)
	}
}

func TestValidateContainerPrereqs_dockerCLIMissing(t *testing.T) {
	t.Parallel()
	// With dockerChain consolidation, all three probes run unconditionally
	// so a missing CLI surfaces all three dependent failures in one pass
	// (matches the containerAgenticFanout UX — see PR #322).
	exec := &prereqTestExecutor{
		response: map[string]string{
			dockerChainCheckCommand("privileged"): "#docker-cli:127\n#docker-daemon:127\n#docker-privileged:127",
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux"})
	h.SetConn(exec)
	failures := ValidateContainerPrereqs(h)
	if len(failures) != 3 {
		t.Fatalf("expected 3 failures (cli+daemon+privileged all surface when CLI missing), got %d (%#v)", len(failures), failures)
	}
	f := failureByName(t, failures, "docker-cli")
	if f.Name == "" {
		t.Fatalf("expected docker-cli failure, got %#v", failures)
	}
	if f.Severity != SeverityError {
		t.Errorf("Severity = %q, want error", f.Severity)
	}
}

func TestValidateContainerPrereqs_dockerDaemonDown(t *testing.T) {
	t.Parallel()
	exec := &prereqTestExecutor{
		response: map[string]string{
			// docker --version OK, but daemon down → daemon + privileged
			// both surface because the privileged probe depends on the
			// running daemon.
			dockerChainCheckCommand("privileged"): "#docker-cli:0\n#docker-daemon:1\n#docker-privileged:1",
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux"})
	h.SetConn(exec)
	failures := ValidateContainerPrereqs(h)
	f := failureByName(t, failures, "docker-daemon")
	if f.Name == "" {
		t.Fatalf("expected docker-daemon failure, got %#v", failures)
	}
	if f.Severity != SeverityError {
		t.Errorf("Severity = %q, want error", f.Severity)
	}
	// Privileged must NOT also surface — the docker-privileged tag was :1
	// (failure) in the mocked output, so it WILL surface; this is intended
	// parallel-failures UX. Verify it's there with the right severity.
	fp := failureByName(t, failures, "docker-privileged")
	if fp.Name == "" {
		t.Fatalf("expected docker-privileged failure (daemon down → privileged also fails), got %#v", failures)
	}
}

func TestValidateContainerPrereqs_privilegedBlocked(t *testing.T) {
	t.Parallel()
	exec := &prereqTestExecutor{
		response: map[string]string{
			dockerChainCheckCommand("privileged"): "#docker-cli:0\n#docker-daemon:0\n#docker-privileged:1",
		},
	}
	h := host.NewHost("h", config.HostConfig{OS: "linux"})
	h.SetConn(exec)
	failures := ValidateContainerPrereqs(h)
	f := failureByName(t, failures, "docker-privileged")
	if f.Name == "" {
		t.Fatalf("expected docker-privileged failure, got %#v", failures)
	}
	if f.Severity != SeverityError {
		t.Errorf("Severity = %q, want error", f.Severity)
	}
	if !strings.Contains(f.Remediation, "userns-remap") {
		t.Errorf("Remediation should mention userns-remap as a common cause, got %q", f.Remediation)
	}
}

// TestPrereqTestExecutor_RunErrorsWhenNoResponse ensures the mock surfaces
// run errors as wrapped errors — important so callers can branch on err != nil.
func TestPrereqTestExecutor_RunErrorsWhenNoResponse(t *testing.T) {
	t.Parallel()
	exec := &prereqTestExecutor{} // no responses, every Run errors
	_, err := exec.Run("anything")
	if err == nil {
		t.Fatal("expected error when no response is configured")
	}
	if !strings.Contains(err.Error(), "unexpected command") {
		t.Errorf("error should mention 'unexpected command', got %q", err)
	}
}

// TestRunContainerCheck_helpersCoverAllWrappers pins the contract that the
// ValidateContainer* wrappers in agentic.go (InnerNetwork, MTU, NodeNPM — the
// probes that also exist as standalone wrappers; docker-socket-user / zstd /
// cache-env are fanout-only) all share the same OS-gate + Run + PrereqFailure
// shape via runContainerCheck. Future drift (one wrapper re-introducing its
// own OS gate, one wrapper forgetting the severity, etc.) will fail here.
func TestRunContainerCheck_helpersCoverAllWrappers(t *testing.T) {
	t.Parallel()

	exec := &prereqTestExecutor{} // every command errors
	hostLinux := func() *host.Host {
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		return h
	}
	hostNonLinux := func() *host.Host {
		h := host.NewHost("h", config.HostConfig{OS: "darwin"})
		h.SetConn(exec)
		return h
	}

	// Each wrapper must produce exactly one failure on a Linux host when the
	// check command errors, with SeverityWarning and a non-empty DocRef.
	cases := []struct {
		name string
		run  func() []PrereqFailure
		want string
	}{
		{"InnerNetwork", func() []PrereqFailure { return ValidateContainerInnerNetwork(hostLinux(), "gh-sr-runner", "agentic-1") }, "container-inner-host-docker-internal"},
		{"MTU", func() []PrereqFailure { return ValidateContainerMTU(hostLinux(), "gh-sr-runner", "agentic-1", 1400) }, "container-mtu"},
		{"NodeNPM", func() []PrereqFailure { return ValidateContainerNodeNPM(hostLinux(), "gh-sr-runner", "agentic-1") }, "container-node-npm"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			failures := tc.run()
			if len(failures) != 1 {
				t.Fatalf("expected 1 failure, got %d (%#v)", len(failures), failures)
			}
			if failures[0].Name != tc.want {
				t.Errorf("Name = %q, want %q", failures[0].Name, tc.want)
			}
			if failures[0].Severity != SeverityWarning {
				t.Errorf("Severity = %q, want warning", failures[0].Severity)
			}
			if failures[0].DocRef == "" {
				t.Error("DocRef should be populated")
			}
		})
	}

	// Each non-MTU wrapper must short-circuit to nil on non-Linux; MTU has
	// its own pre-OS gate (hostEgressMTU) and is exercised separately above.
	nonLinuxCases := []struct {
		name string
		run  func() []PrereqFailure
	}{
		{"InnerNetwork", func() []PrereqFailure {
			return ValidateContainerInnerNetwork(hostNonLinux(), "gh-sr-runner", "agentic-1")
		}},
		{"NodeNPM", func() []PrereqFailure { return ValidateContainerNodeNPM(hostNonLinux(), "gh-sr-runner", "agentic-1") }},
	}
	for _, tc := range nonLinuxCases {
		tc := tc
		t.Run("nonLinux/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.run(); got != nil {
				t.Errorf("non-linux must short-circuit, got %#v", got)
			}
		})
	}
}

// TestRunContainerCheck_nilHostIsSafe pins the nil-host guard: callers may
// short-circuit with a nil *host.Host before any dereference, and the helper
// must not panic.
func TestRunContainerCheck_nilHostIsSafe(t *testing.T) {
	t.Parallel()
	if got := runContainerCheck(nil, containerCheckSpec{name: "anything"}); got != nil {
		t.Errorf("nil host must short-circuit, got %#v", got)
	}
}
