package agentic

import (
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

// The inner-Docker hygiene probes that ValidateAWFHygieneInner fans out across
// the inner Docker daemon of a DinD runner container, lifted into named
// constants so the test assertions reuse the exact strings the function emits
// (prereqTestExecutor matches commands exactly — a feature, not a bug, for
// guarding against silent probe-shape refactors).
//
// Rootless era (gh-aw v0.88+): no iptables probe (AWF no longer manipulates
// DOCKER-USER) and the orphan-container filter covers awmg-mcpg / awf- / gh-aw
// in a single docker ps.
const (
	awfHygieneOrphansCmd  = `docker ps -a --filter "name=awmg-mcpg" --filter "name=awf-" --filter "name=gh-aw" --format '{{.Names}}' 2>/dev/null | head -20`
	awfHygieneNetworksCmd = `docker network ls --filter "name=awf-net" --format '{{.Name}}' 2>/dev/null | head -20`
)

func TestValidateAWFHygieneInner(t *testing.T) {
	t.Parallel()

	const outerContainer = "gh-sr-myinstance"

	// The two inner probes are the definitions wrapped in `docker exec "X" `.
	// strconv.Quote is used by the function so the prefix is literally
	// `docker exec "gh-sr-myinstance" ` (double-quoted, space-prefixed).
	innerOrphansCmd := `docker exec "gh-sr-myinstance" ` + awfHygieneOrphansCmd
	innerNetworksCmd := `docker exec "gh-sr-myinstance" ` + awfHygieneNetworksCmd

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
				innerOrphansCmd:  "",
				innerNetworksCmd: "",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateAWFHygieneInner(h, outerContainer); got != nil {
			t.Errorf("clean inner Docker must return nil, got %#v", got)
		}
		for _, cmd := range []string{innerOrphansCmd, innerNetworksCmd} {
			if !exec.saw(cmd) {
				t.Errorf("expected inner probe to run: %q", cmd)
			}
		}
	})

	t.Run("orphan agentic container returns inner warning naming the container", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerOrphansCmd:  "awmg-mcpg-stale",
				innerNetworksCmd: "",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateAWFHygieneInner(h, outerContainer)
		f := failureByName(t, failures, "orphan-agentic-containers-inner")
		if f.Name == "" {
			t.Fatalf("expected orphan-agentic-containers-inner failure, got %#v", failures)
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
		if !strings.Contains(f.Remediation, "awmg-mcpg") {
			t.Errorf("Remediation should filter the rootless-era gateway names, got %q", f.Remediation)
		}
		if len(failures) != 1 {
			t.Errorf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
	})

	t.Run("orphan awf-net network returns inner warning", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerOrphansCmd:  "",
				innerNetworksCmd: "awf-net-old",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateAWFHygieneInner(h, outerContainer)
		f := failureByName(t, failures, "orphan-agentic-networks-inner")
		if f.Name == "" {
			t.Fatalf("expected orphan-agentic-networks-inner failure, got %#v", failures)
		}
		if f.Severity != SeverityWarning {
			t.Errorf("Severity = %q, want warning", f.Severity)
		}
		if !strings.Contains(f.Message, "awf-net") {
			t.Errorf("Message should mention awf-net, got %q", f.Message)
		}
		if !strings.Contains(f.Message, outerContainer) {
			t.Errorf("Message should name the runner container, got %q", f.Message)
		}
		if !strings.Contains(f.Remediation, `docker network prune -f --filter "name=awf-net"`) {
			t.Errorf("Remediation should prune the leftover networks, got %q", f.Remediation)
		}
		if len(failures) != 1 {
			t.Errorf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
	})

	t.Run("no iptables probe in the rootless era", func(t *testing.T) {
		t.Parallel()
		// stale-docker-user-rules was retired with the sudo/iptables sandbox:
		// rootless AWF never touches iptables, so no probe may reference it.
		exec := &prereqTestExecutor{response: map[string]string{}}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		_ = ValidateAWFHygieneInner(h, outerContainer)
		for _, seen := range exec.seen {
			if strings.Contains(seen, "iptables") {
				t.Errorf("hygiene probe must not reference iptables (rootless era), got %q", seen)
			}
		}
	})

	t.Run("both inner probes fail returns two inner warnings", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{
			response: map[string]string{
				innerOrphansCmd:  "awf-c1",
				innerNetworksCmd: "awf-net-old",
			},
		}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateAWFHygieneInner(h, outerContainer)
		if len(failures) != 2 {
			t.Fatalf("expected 2 inner failures, got %d (%#v)", len(failures), failures)
		}
		for _, want := range []string{
			"orphan-agentic-containers-inner",
			"orphan-agentic-networks-inner",
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
		// Verify both commands were prefixed with `docker exec "..." `.
		// strconv.Quote("name\"with$special") = `"name\"with$special"`.
		const expectedPrefix = `docker exec "name\"with$special" `
		count := 0
		for _, seen := range exec.seen {
			if strings.HasPrefix(seen, expectedPrefix) {
				count++
			}
		}
		if count != 2 {
			t.Errorf("expected 2 commands prefixed with %q, got %d (seen=%v)", expectedPrefix, count, exec.seen)
		}
	})
}
