package agentic

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

func TestHasBlockingFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		failures []PrereqFailure
		want     bool
	}{
		{"empty", nil, false},
		{"warnings only", []PrereqFailure{
			{Name: "a", Severity: SeverityWarning},
			{Name: "b", Severity: SeverityWarning},
		}, false},
		{"one error", []PrereqFailure{
			{Name: "a", Severity: SeverityError},
		}, true},
		{"mixed warning and error", []PrereqFailure{
			{Name: "a", Severity: SeverityWarning},
			{Name: "b", Severity: SeverityError},
		}, true},
		{"all errors", []PrereqFailure{
			{Name: "a", Severity: SeverityError},
			{Name: "b", Severity: SeverityError},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := HasBlockingFailures(tc.failures)
			if got != tc.want {
				t.Errorf("HasBlockingFailures(%v) = %v, want %v", tc.failures, got, tc.want)
			}
		})
	}
}

func TestFormatRemediation(t *testing.T) {
	t.Parallel()

	t.Run("with DocRef", func(t *testing.T) {
		t.Parallel()
		f := PrereqFailure{
			Name:        "docker-cli",
			Severity:    SeverityError,
			Message:     "docker CLI not found",
			Remediation: "sudo apt-get install -y docker.io",
			DocRef:      "agentic-workflows.md §3g",
		}
		got := FormatRemediation(f)
		if !strings.Contains(got, "[agentic-workflows.md §3g]") {
			t.Errorf("expected DocRef in output, got:\n%s", got)
		}
		if !strings.Contains(got, "docker CLI not found") {
			t.Errorf("expected message in output, got:\n%s", got)
		}
		if !strings.Contains(got, "To fix:") {
			t.Errorf("expected 'To fix:' in output, got:\n%s", got)
		}
		if !strings.Contains(got, "sudo apt-get install -y docker.io") {
			t.Errorf("expected remediation command in output, got:\n%s", got)
		}
	})

	t.Run("without DocRef", func(t *testing.T) {
		t.Parallel()
		f := PrereqFailure{
			Name:        "some-check",
			Severity:    SeverityWarning,
			Message:     "something missing",
			Remediation: "run fix-it",
		}
		got := FormatRemediation(f)
		if strings.Contains(got, "[") {
			t.Errorf("expected no DocRef bracket in output, got:\n%s", got)
		}
		if !strings.Contains(got, "something missing") {
			t.Errorf("expected message in output, got:\n%s", got)
		}
		if !strings.Contains(got, "run fix-it") {
			t.Errorf("expected remediation in output, got:\n%s", got)
		}
	})

	t.Run("multiline remediation indented", func(t *testing.T) {
		t.Parallel()
		f := PrereqFailure{
			Message:     "need stuff",
			Remediation: "line1\nline2",
		}
		got := FormatRemediation(f)
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if strings.Contains(line, "line1") || strings.Contains(line, "line2") {
				if !strings.HasPrefix(line, "    ") {
					t.Errorf("remediation line not indented with 4 spaces: %q", line)
				}
			}
		}
	})
}

func TestFormatAllRemediations(t *testing.T) {
	t.Parallel()

	t.Run("empty returns empty string", func(t *testing.T) {
		t.Parallel()
		got := FormatAllRemediations(nil)
		if got != "" {
			t.Errorf("expected empty string for no failures, got %q", got)
		}
	})

	t.Run("error uses FAIL label", func(t *testing.T) {
		t.Parallel()
		failures := []PrereqFailure{
			{Name: "docker-cli", Severity: SeverityError, Message: "docker missing", Remediation: "install docker"},
		}
		got := FormatAllRemediations(failures)
		if !strings.Contains(got, "FAIL") {
			t.Errorf("expected FAIL label for error severity, got:\n%s", got)
		}
		if !strings.Contains(got, "docker-cli") {
			t.Errorf("expected failure name in output, got:\n%s", got)
		}
		if !strings.Contains(got, "1 failure") {
			t.Errorf("expected failure count in banner, got:\n%s", got)
		}
	})

	t.Run("warning uses WARN label", func(t *testing.T) {
		t.Parallel()
		failures := []PrereqFailure{
			{Name: "sudo-iptables", Severity: SeverityWarning, Message: "no passwordless sudo", Remediation: "add sudoers rule"},
		}
		got := FormatAllRemediations(failures)
		if !strings.Contains(got, "WARN") {
			t.Errorf("expected WARN label for warning severity, got:\n%s", got)
		}
	})

	t.Run("multiple failures numbered and all included", func(t *testing.T) {
		t.Parallel()
		failures := []PrereqFailure{
			{Name: "a", Severity: SeverityError, Message: "err-a", Remediation: "fix-a"},
			{Name: "b", Severity: SeverityWarning, Message: "warn-b", Remediation: "fix-b"},
		}
		got := FormatAllRemediations(failures)
		if !strings.Contains(got, "[1]") {
			t.Errorf("expected [1] in output, got:\n%s", got)
		}
		if !strings.Contains(got, "[2]") {
			t.Errorf("expected [2] in output, got:\n%s", got)
		}
		if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
			t.Errorf("expected both failure names in output, got:\n%s", got)
		}
		if !strings.Contains(got, "2 failure") {
			t.Errorf("expected '2 failure' in banner, got:\n%s", got)
		}
	})
}

type prereqTestExecutor struct {
	mu       sync.Mutex
	seen     []string
	response map[string]string
}

func (e *prereqTestExecutor) Run(cmd string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, cmd)

	if out, ok := e.response[cmd]; ok {
		return out, nil
	}
	return "", fmt.Errorf("unexpected command: %s", cmd)
}

func (e *prereqTestExecutor) Upload(string, string) error { return nil }

func (e *prereqTestExecutor) Close() error { return nil }

func (e *prereqTestExecutor) saw(cmd string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, seen := range e.seen {
		if seen == cmd {
			return true
		}
	}
	return false
}

// TestDockerChainCheckCommandEchoOutsideRedirect guards against placing the
// status-tag echo inside the `{ ... } >/dev/null 2>&1` block — that would
// discard stdout and parseDockerChainOutput would always see empty output.
func TestDockerChainCheckCommandEchoOutsideRedirect(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"privileged"} {
		variant := variant
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			cmd := dockerChainCheckCommand(variant)
			for _, line := range strings.Split(cmd, "\n") {
				line = strings.TrimSpace(line)
				if !strings.Contains(line, `echo "#docker-`) {
					continue
				}
				// Status tags must follow the block-level redirect, not sit
				// inside `{ ... } >/dev/null 2>&1` where stdout is discarded.
				if !strings.Contains(line, `} >/dev/null 2>&1; echo "#docker-`) {
					t.Errorf("probe tag echo must follow block redirect, got: %q", line)
				}
			}
		})
	}
}

// TestValidateContainerPrereqsDockerChainConsolidation pins the SSH
// round-trip count for the docker CLI → daemon → privileged chain to
// exactly 1 — the consolidated dockerChainCheckCommand replaces the three
// sequential h.Run calls that existed before this optimization. This is
// the hot-path equivalent for the container-mode runner prereq check
// (called once per container-mode runner from `gh sr doctor`).
func TestValidateContainerPrereqsDockerChainConsolidation(t *testing.T) {
	t.Parallel()

	t.Run("uses exactly one h.Run call on happy path", func(t *testing.T) {
		t.Parallel()
		chainCmd := dockerChainCheckCommand("privileged")
		exec := &prereqTestExecutor{
			response: map[string]string{
				chainCmd: "#docker-cli:0\n#docker-daemon:0\n#docker-privileged:0",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)

		failures := ValidateContainerPrereqs(h)

		if len(exec.seen) != 1 {
			t.Errorf("expected exactly 1 h.Run call on happy path, saw %d (%v)", len(exec.seen), exec.seen)
		}
		if exec.seen[0] != chainCmd {
			t.Errorf("expected single call to be dockerChainCheckCommand(\"privileged\"), got %q", exec.seen[0])
		}
		if len(failures) != 0 {
			t.Errorf("happy path should produce no failures, got %#v", failures)
		}
	})

	t.Run("non-linux still short-circuits to one linux-required failure", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{}
		h := host.NewHost("h", config.HostConfig{OS: "darwin"})
		h.SetConn(exec)
		failures := ValidateContainerPrereqs(h)
		if len(failures) != 1 || failures[0].Name != "linux-required" {
			t.Errorf("non-linux must short-circuit to linux-required, got %#v", failures)
		}
		if len(exec.seen) != 0 {
			t.Errorf("non-linux must make zero h.Run calls, saw %d (%v)", len(exec.seen), exec.seen)
		}
	})
}
