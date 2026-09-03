package agentic

import (
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

// The docker/iptables probes that ValidateAWFHygieneInner fans out across the
// inner Docker daemon of a DinD runner container. Lifted into named constants
// so the test assertions can reuse the same strings the function emits —
// guard against silent refactors of the probe shapes (the previous
// test-improver run noted that the exact-string match in prereqTestExecutor
// is a feature, not a bug, for exactly this reason).
//
// Note: the inner-Docker iptables probe has no `sudo -n` prefix because the
// DinD inner daemon runs as root.
const (
	awfHygieneAwfCmd  = `docker ps -a --filter "name=awf-" --filter "name=gh-aw" --format '{{.Names}}' 2>/dev/null | head -20`
	awfHygieneMcpgCmd = `docker ps -a --filter "name=gh-aw-mcpg-" --format '{{.Names}}' 2>/dev/null | head -20`

	// The inner-Docker variant of the iptables probe — no `sudo -n`
	// because the DinD inner daemon runs as root.
	awfHygieneIptablesInnerCmd = `iptables -L DOCKER-USER --line-numbers -n 2>/dev/null | grep -i "awf\|gh-aw" | head -20`
)

func TestValidateAWFHygieneInner(t *testing.T) {
	t.Parallel()

	const outerContainer = "gh-sr-myinstance"

	// The three inner probes are the outer ones wrapped in `docker exec "X" `.
	// strconv.Quote is used by the function so the prefix is literally
	// `docker exec "gh-sr-myinstance" ` (double-quoted, space-prefixed).
	// The iptables probe drops the `sudo -n` prefix because the DinD inner
	// daemon runs as root.
	innerAwfCmd := `docker exec "gh-sr-myinstance" ` + awfHygieneAwfCmd
	innerIptablesCmd := `docker exec "gh-sr-myinstance" ` + awfHygieneIptablesInnerCmd
	innerMcpgCmd := `docker exec "gh-sr-myinstance" ` + awfHygieneMcpgCmd

	t.Run("non-linux short-circuits", func(t *testing.T) {
		t.Parallel()
		for _, os := range []string{"darwin", "windows"} {
			os := os
			t.Run(os, func(t *testing.T) {
				t.Parallel()
				h := host.NewHost("h", config.HostConfig{OS: os})
				h.SetConn(&prereqTestExecutor{}) // unused — short-circuit
				if got := ValidateAWFHygieneInner(h, outerContainer); got != nil {
					t.Errorf("non-linux must return nil, got %#v", got)
				}
			})
		}
	})

	t.Run("clean inner Docker returns nil", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerAwfCmd:      "",
				innerIptablesCmd: "",
				innerMcpgCmd:     "",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateAWFHygieneInner(h, outerContainer); got != nil {
			t.Errorf("clean inner Docker must return nil, got %#v", got)
		}
		for _, cmd := range []string{innerAwfCmd, innerIptablesCmd, innerMcpgCmd} {
			if !exec.saw(cmd) {
				t.Errorf("expected inner probe to run: %q", cmd)
			}
		}
	})

	t.Run("orphan awf inside container returns inner warning naming the container", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerAwfCmd:      "awf-c1",
				innerIptablesCmd: "",
				innerMcpgCmd:     "",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateAWFHygieneInner(h, outerContainer)
		f := failureByName(t, failures, "awf-orphan-containers-inner")
		if f.Name == "" {
			t.Fatalf("expected awf-orphan-containers-inner failure, got %#v", failures)
		}
		if f.Severity != SeverityWarning {
			t.Errorf("Severity = %q, want warning", f.Severity)
		}
		// The Message and Remediation must name the container so an operator
		// running `gh sr doctor` knows which runner to ssh into.
		if !strings.Contains(f.Message, outerContainer) {
			t.Errorf("Message should name the runner container %q, got %q", outerContainer, f.Message)
		}
		if !strings.Contains(f.Message, "inner Docker") {
			t.Errorf("Message should distinguish inner Docker from host, got %q", f.Message)
		}
		if !strings.Contains(f.Remediation, outerContainer) {
			t.Errorf("Remediation should name the runner container, got %q", f.Remediation)
		}
		if !strings.Contains(f.Remediation, "docker exec") {
			t.Errorf("Remediation should use docker exec to enter the container, got %q", f.Remediation)
		}
		if !strings.Contains(f.Remediation, "docker rm -f") {
			t.Errorf("Remediation should pipeline cleanup, got %q", f.Remediation)
		}
		if len(failures) != 1 {
			t.Errorf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
	})

	t.Run("stale DOCKER-USER rules in inner netns returns inner warning", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerAwfCmd:      "",
				innerIptablesCmd: "1  DROP  all  --  172.30.0.5  anywhere",
				innerMcpgCmd:     "",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateAWFHygieneInner(h, outerContainer)
		f := failureByName(t, failures, "stale-docker-user-rules-inner")
		if f.Name == "" {
			t.Fatalf("expected stale-docker-user-rules-inner failure, got %#v", failures)
		}
		if !strings.Contains(f.Message, "inner netns") {
			t.Errorf("Message should mention inner netns, got %q", f.Message)
		}
		if !strings.Contains(f.Remediation, "docker exec "+outerContainer+" iptables -F DOCKER-USER") {
			t.Errorf("Remediation should flush via docker exec %s, got %q", outerContainer, f.Remediation)
		}
		if len(failures) != 1 {
			t.Errorf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
	})

	t.Run("orphan MCP gateway inside container returns inner warning", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerAwfCmd:      "",
				innerIptablesCmd: "",
				innerMcpgCmd:     "gh-aw-mcpg-1",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateAWFHygieneInner(h, outerContainer)
		f := failureByName(t, failures, "mcpg-orphan-containers-inner")
		if f.Name == "" {
			t.Fatalf("expected mcpg-orphan-containers-inner failure, got %#v", failures)
		}
		if !strings.Contains(f.Message, outerContainer) {
			t.Errorf("Message should name the runner container, got %q", f.Message)
		}
		if !strings.Contains(f.Remediation, "docker exec -it "+outerContainer) {
			t.Errorf("Remediation should show docker exec -it entrypoint, got %q", f.Remediation)
		}
		if len(failures) != 1 {
			t.Errorf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
	})

	t.Run("all three inner probes fail returns three inner warnings", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerAwfCmd:      "awf-c1",
				innerIptablesCmd: "1  DROP",
				innerMcpgCmd:     "gh-aw-mcpg-1",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateAWFHygieneInner(h, outerContainer)
		if len(failures) != 3 {
			t.Fatalf("expected 3 inner failures, got %d (%#v)", len(failures), failures)
		}
		for _, want := range []string{
			"awf-orphan-containers-inner",
			"stale-docker-user-rules-inner",
			"mcpg-orphan-containers-inner",
		} {
			f := failureByName(t, failures, want)
			if f.Name == "" {
				t.Errorf("missing inner failure %q in %#v", want, failures)
				continue
			}
			if !strings.Contains(f.Message, outerContainer) {
				t.Errorf("%s: Message should name %s, got %q", want, outerContainer, f.Message)
			}
		}
	})

	t.Run("container name with special chars is shell-quoted in commands", func(t *testing.T) {
		t.Parallel()
		// strconv.Quote should escape a name that contains a double-quote
		// (or other shell-special characters). We don't assert the exact
		// output, just that the function does not panic and that the mock
		// recorded a command beginning with the quoted form.
		const weirdName = `name"with$special`
		exec := &prereqTestExecutor{
			response: map[string]string{},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		_ = ValidateAWFHygieneInner(h, weirdName)
		// Verify all three commands were prefixed with `docker exec "..." `.
		// strconv.Quote("name\"with$special") = `"name\"with$special"`.
		const expectedPrefix = `docker exec "name\"with$special" `
		count := 0
		for _, seen := range exec.seen {
			if strings.HasPrefix(seen, expectedPrefix) {
				count++
			}
		}
		if count != 3 {
			t.Errorf("expected 3 commands prefixed with %q, got %d (seen=%v)", expectedPrefix, count, exec.seen)
		}
	})
}
