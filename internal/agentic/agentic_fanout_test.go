package agentic

import (
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

// allAgenticFanoutClean returns the canonical mock for the agentic fanout
// happy path: the combined `docker exec` command returns a `:0` tag for every
// in-scope probe (or all but MTU when hostEgressMTU is out of the pinning
// window). Use this from any test that wants the fanout to report zero
// failures.
func allAgenticFanoutClean(hostEgressMTU int, cacheEnabled bool) *prereqTestExecutor {
	cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", hostEgressMTU, cacheEnabled)
	return &prereqTestExecutor{response: map[string]string{cmd: agenticFanoutHappyPathOutput(hostEgressMTU, cacheEnabled)}}
}

// agenticFanoutProbeOrder lists the always-on probe names in the exact order
// their blocks appear in containerAgenticFanoutCheckCommand. The gated
// container-cache-env / container-mtu blocks are appended by the helpers
// below when their gates are open.
func agenticFanoutProbeOrder() []string {
	return []string{
		"container-inner-host-docker-internal",
		"container-node-npm",
		"container-docker-socket-user",
		"container-zstd",
	}
}

// agenticFanoutHappyPathOutput renders the tagged output a clean runner
// container would emit from the combined `docker exec` in
// containerAgenticFanoutCheckCommand. Mirrors the probe block order in the
// command body, with one `:0` line per emitted block (cache-env only when
// cacheEnabled, MTU only when in the pinning window).
func agenticFanoutHappyPathOutput(hostEgressMTU int, cacheEnabled bool) string {
	lines := make([]string, 0, 6)
	for _, name := range agenticFanoutProbeOrder() {
		lines = append(lines, "#"+name+":0")
	}
	if cacheEnabled {
		lines = append(lines, "#container-cache-env:0")
	}
	if hostEgressMTU > 0 && hostEgressMTU < 1500 {
		lines = append(lines, "#container-mtu:0")
	}
	return strings.Join(lines, "\n")
}

// agenticFanoutOutputWithFailures returns the combined stdout the fanout
// would emit when only the supplied probe names fail — exit 1 instead of 0 —
// and every other in-scope probe exits 0. Useful for asserting that the
// parser surfaces a single failing probe's metadata without dragging in
// unrelated failures.
func agenticFanoutOutputWithFailures(hostEgressMTU int, cacheEnabled bool, failingNames ...string) string {
	failing := map[string]struct{}{}
	for _, n := range failingNames {
		failing[n] = struct{}{}
	}
	var lines []string
	for _, name := range agenticFanoutProbeOrder() {
		status := "0"
		if _, ok := failing[name]; ok {
			status = "1"
		}
		lines = append(lines, "#"+name+":"+status)
	}
	if cacheEnabled {
		status := "0"
		if _, ok := failing["container-cache-env"]; ok {
			status = "1"
		}
		lines = append(lines, "#container-cache-env:"+status)
	}
	if hostEgressMTU > 0 && hostEgressMTU < 1500 {
		status := "0"
		if _, ok := failing["container-mtu"]; ok {
			status = "1"
		}
		lines = append(lines, "#container-mtu:"+status)
	}
	return strings.Join(lines, "\n")
}

func TestValidateContainerAgenticFanout(t *testing.T) {
	t.Parallel()

	t.Run("nil host short-circuits", func(t *testing.T) {
		t.Parallel()
		if got := ValidateContainerAgenticFanout(nil, "gh-sr-runner", "agentic-1", 1400, false); got != nil {
			t.Errorf("nil host must short-circuit, got %#v", got)
		}
	})

	t.Run("non-linux short-circuits", func(t *testing.T) {
		t.Parallel()
		for _, os := range []string{"darwin", "windows"} {
			os := os
			t.Run(os, func(t *testing.T) {
				t.Parallel()
				exec := &prereqTestExecutor{} // no commands expected
				h := host.NewHost("h", config.HostConfig{OS: os})
				h.SetConn(exec)
				if got := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 1400, false); got != nil {
					t.Errorf("non-linux %q must short-circuit, got %#v", os, got)
				}
				if len(exec.seen) != 0 {
					t.Errorf("non-linux %q must make zero h.Run calls, saw %d (%v)", os, len(exec.seen), exec.seen)
				}
			})
		}
	})

	t.Run("empty outerContainer short-circuits", func(t *testing.T) {
		t.Parallel()
		exec := &prereqTestExecutor{}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateContainerAgenticFanout(h, "", "agentic-1", 1400, false); got != nil {
			t.Errorf("empty outerContainer must short-circuit, got %#v", got)
		}
		if len(exec.seen) != 0 {
			t.Errorf("empty outerContainer must make zero h.Run calls, saw %d", len(exec.seen))
		}
	})

	t.Run("happy path with MTU returns nil", func(t *testing.T) {
		t.Parallel()
		exec := allAgenticFanoutClean(1400, false)
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 1400, false); got != nil {
			t.Errorf("clean fanout must return nil, got %#v", got)
		}
	})

	t.Run("happy path with cache enabled returns nil", func(t *testing.T) {
		t.Parallel()
		exec := allAgenticFanoutClean(1400, true)
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 1400, true); got != nil {
			t.Errorf("clean fanout (cache on) must return nil, got %#v", got)
		}
	})

	t.Run("happy path with MTU skipped returns nil", func(t *testing.T) {
		t.Parallel()
		// hostEgressMTU >= 1500 ⇒ MTU block omitted, no `#container-mtu` line
		// emitted, no MTU failure.
		for _, mtu := range []int{0, 1500, 9000} {
			mtu := mtu
			t.Run("mtu"+itoaForTest(mtu), func(t *testing.T) {
				t.Parallel()
				exec := allAgenticFanoutClean(mtu, false)
				h := host.NewHost("h", config.HostConfig{OS: "linux"})
				h.SetConn(exec)
				if got := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", mtu, false); got != nil {
					t.Errorf("clean fanout (MTU %d skipped) must return nil, got %#v", mtu, got)
				}
			})
		}
	})

	t.Run("single failing probe surfaces only its failure", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 0, false)
		exec := &prereqTestExecutor{response: map[string]string{
			cmd: agenticFanoutOutputWithFailures(0, false, "container-node-npm"),
		}}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 0, false)
		if len(failures) != 1 {
			t.Fatalf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
		f := failures[0]
		if f.Name != "container-node-npm" {
			t.Errorf("Name = %q, want %q", f.Name, "container-node-npm")
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

	t.Run("cache-env failure only surfaces when cache enabled", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 0, true)
		exec := &prereqTestExecutor{response: map[string]string{
			cmd: agenticFanoutOutputWithFailures(0, true, "container-cache-env"),
		}}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 0, true)
		if len(failures) != 1 {
			t.Fatalf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
		if failures[0].Name != "container-cache-env" {
			t.Errorf("Name = %q, want %q", failures[0].Name, "container-cache-env")
		}
		if !strings.Contains(failures[0].Remediation, "gh sr cache deploy") {
			t.Errorf("cache-env Remediation should point at gh sr cache deploy, got %q", failures[0].Remediation)
		}

		// Same stdout tag emitted when cacheEnabled=false is a stale tag the
		// parser must ignore: the probe is not part of the command at all.
		exec2 := &prereqTestExecutor{response: map[string]string{
			containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 0, false): "#container-cache-env:1",
		}}
		h2 := host.NewHost("h2", config.HostConfig{OS: "linux"})
		h2.SetConn(exec2)
		if got := ValidateContainerAgenticFanout(h2, "gh-sr-runner", "agentic-1", 0, false); got != nil {
			t.Errorf("cache-env tag without cache enabled must be ignored, got %#v", got)
		}
	})

	t.Run("multiple failing probes surface all with correct severity", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 1400, false)
		exec := &prereqTestExecutor{response: map[string]string{
			cmd: agenticFanoutOutputWithFailures(1400, false, "container-docker-socket-user", "container-zstd"),
		}}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 1400, false)
		if len(failures) != 2 {
			t.Fatalf("expected 2 failures, got %d (%#v)", len(failures), failures)
		}
		// Order must match the order of the tags on stdout, which mirrors
		// the order of the probe blocks in containerAgenticFanoutCheckCommand.
		wantOrder := []string{"container-docker-socket-user", "container-zstd"}
		for i, want := range wantOrder {
			if failures[i].Name != want {
				t.Errorf("failures[%d].Name = %q, want %q", i, failures[i].Name, want)
			}
			if failures[i].Severity != SeverityWarning {
				t.Errorf("failures[%d].Severity = %q, want warning", i, failures[i].Severity)
			}
			if !strings.Contains(failures[i].Message, "gh-sr-runner") {
				t.Errorf("failures[%d].Message should name the container, got %q", i, failures[i].Message)
			}
		}
	})

	t.Run("MTU failure surfaces when hostEgressMTU is in window", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 1400, false)
		exec := &prereqTestExecutor{response: map[string]string{
			cmd: agenticFanoutOutputWithFailures(1400, false, "container-mtu"),
		}}
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 1400, false)
		if len(failures) != 1 {
			t.Fatalf("expected 1 failure, got %d (%#v)", len(failures), failures)
		}
		if failures[0].Name != "container-mtu" {
			t.Errorf("Name = %q, want %q", failures[0].Name, "container-mtu")
		}
		if !strings.Contains(failures[0].Message, "1400") {
			t.Errorf("MTU Message should reference the host egress MTU, got %q", failures[0].Message)
		}
	})

	t.Run("uses exactly one h.Run call per container on happy path", func(t *testing.T) {
		t.Parallel()
		// This is the round-trip-count regression guard: the whole point of
		// the fanout is collapsing N h.Run calls into one. Drift in either
		// direction (extra calls leaking back in, or the call shape
		// changing) must fail this test.
		exec := allAgenticFanoutClean(1400, false)
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 1400, false); got != nil {
			t.Fatalf("happy path must return nil, got %#v", got)
		}
		if len(exec.seen) != 1 {
			t.Errorf("expected exactly 1 h.Run call, saw %d: %v", len(exec.seen), exec.seen)
		}
	})

	t.Run("uses exactly one h.Run call when MTU is skipped", func(t *testing.T) {
		t.Parallel()
		exec := allAgenticFanoutClean(0, false)
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		if got := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 0, false); got != nil {
			t.Fatalf("happy path with MTU skipped must return nil, got %#v", got)
		}
		if len(exec.seen) != 1 {
			t.Errorf("expected exactly 1 h.Run call (MTU omitted), saw %d", len(exec.seen))
		}
	})

	t.Run("transport failure surfaces fanout-level warning", func(t *testing.T) {
		t.Parallel()
		// Mock executor returns an error for any unmatched command (default
		// behaviour of prereqTestExecutor) — simulates an SSH drop mid-run.
		exec := &prereqTestExecutor{} // every command errors
		h := host.NewHost("h", config.HostConfig{OS: "linux"})
		h.SetConn(exec)
		failures := ValidateContainerAgenticFanout(h, "gh-sr-runner", "agentic-1", 1400, false)
		if len(failures) != 1 {
			t.Fatalf("expected 1 fanout-level failure, got %d (%#v)", len(failures), failures)
		}
		if failures[0].Name != "container-agentic-fanout" {
			t.Errorf("Name = %q, want %q", failures[0].Name, "container-agentic-fanout")
		}
		if failures[0].Severity != SeverityWarning {
			t.Errorf("Severity = %q, want warning", failures[0].Severity)
		}
		if !strings.Contains(failures[0].Message, "gh-sr-runner") {
			t.Errorf("Message should name the container, got %q", failures[0].Message)
		}
	})
}

// itoaForTest is a tiny fmt-free helper so the table-driven subtest names
// above can name themselves after the MTU value without importing strconv
// just for one call site. Kept local to the test file.
func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestParseContainerAgenticFanoutOutput(t *testing.T) {
	t.Parallel()

	specs := containerAgenticFanoutSpecs("gh-sr-runner", "agentic-1", 1400, false)

	t.Run("empty input yields no failures", func(t *testing.T) {
		t.Parallel()
		if got := parseContainerAgenticFanoutOutput("", specs); got != nil {
			t.Errorf("empty input must yield no failures, got %#v", got)
		}
	})

	t.Run("only :0 lines yield no failures", func(t *testing.T) {
		t.Parallel()
		in := "#container-inner-host-docker-internal:0\n#container-zstd:0"
		if got := parseContainerAgenticFanoutOutput(in, specs); got != nil {
			t.Errorf(":0 lines must yield no failures, got %#v", got)
		}
	})

	t.Run(":1 line emits the matching PrereqFailure", func(t *testing.T) {
		t.Parallel()
		in := "#container-zstd:1"
		got := parseContainerAgenticFanoutOutput(in, specs)
		if len(got) != 1 || got[0].Name != "container-zstd" {
			t.Errorf("expected single container-zstd failure, got %#v", got)
		}
	})

	t.Run("unknown :1 tag is silently dropped", func(t *testing.T) {
		t.Parallel()
		in := "#some-other-probe:1\n#container-zstd:1"
		got := parseContainerAgenticFanoutOutput(in, specs)
		if len(got) != 1 || got[0].Name != "container-zstd" {
			t.Errorf("expected single container-zstd failure, got %#v", got)
		}
	})

	t.Run("retired sudo-era tags are ignored", func(t *testing.T) {
		t.Parallel()
		// lock.yml-era probe names from the pre-rootless image must not map
		// onto any spec: they are no longer part of the command body, and a
		// stray tag in stdout (e.g. from a stale hook) must not fabricate a
		// failure.
		in := "#container-inner-resolv:1\n#container-awf-service-routing:1\n#container-awf:1"
		if got := parseContainerAgenticFanoutOutput(in, specs); got != nil {
			t.Errorf("retired probe tags must yield no failures, got %#v", got)
		}
	})

	t.Run("non-tag lines are ignored", func(t *testing.T) {
		t.Parallel()
		in := "incidental stderr noise\n#container-zstd:1\nanother stray line"
		got := parseContainerAgenticFanoutOutput(in, specs)
		if len(got) != 1 || got[0].Name != "container-zstd" {
			t.Errorf("expected single container-zstd failure, got %#v", got)
		}
	})

	t.Run("submission order matches stdout order", func(t *testing.T) {
		t.Parallel()
		// Socket-user emits before zstd in the command body, so a stdout
		// with both failing must surface in that order. Guards against a
		// future map-iteration-order refactor of parseContainerAgenticFanoutOutput.
		in := "#container-docker-socket-user:1\n#container-zstd:1"
		got := parseContainerAgenticFanoutOutput(in, specs)
		if len(got) != 2 {
			t.Fatalf("expected 2 failures, got %d", len(got))
		}
		if got[0].Name != "container-docker-socket-user" || got[1].Name != "container-zstd" {
			t.Errorf("order = [%q, %q], want [container-docker-socket-user, container-zstd]", got[0].Name, got[1].Name)
		}
	})
}

// TestValidateContainerAgenticFanout_MTUOmissionReachesShell pins that the
// MTU block is conditionally emitted into the combined `docker exec` body,
// not just skipped at the parser level. If a future refactor moves the MTU
// gating out of containerAgenticFanoutCheckCommand, this test catches it.
func TestValidateContainerAgenticFanout_MTUOmissionReachesShell(t *testing.T) {
	t.Parallel()

	t.Run("MTU block included for hostEgressMTU in (0, 1500)", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 1400, false)
		if !strings.Contains(cmd, "#container-mtu:$?") {
			t.Errorf("MTU block should appear in combined shell for MTU=1400, got: %s", cmd)
		}
		if !strings.Contains(cmd, "host=1400") {
			t.Errorf("MTU block should embed the literal hostEgressMTU, got: %s", cmd)
		}
	})

	t.Run("MTU block omitted for hostEgressMTU=0", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 0, false)
		if strings.Contains(cmd, "#container-mtu:$?") {
			t.Errorf("MTU block should be omitted for MTU=0, got: %s", cmd)
		}
	})

	t.Run("MTU block omitted for hostEgressMTU>=1500", func(t *testing.T) {
		t.Parallel()
		for _, mtu := range []int{1500, 9000} {
			mtu := mtu
			cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", mtu, false)
			if strings.Contains(cmd, "#container-mtu:$?") {
				t.Errorf("MTU block should be omitted for MTU=%d, got: %s", mtu, cmd)
			}
		}
	})
}

// TestValidateContainerAgenticFanout_CacheEnvGatingReachesShell pins that the
// cache-env block is emitted into the combined `docker exec` body exactly
// when cacheEnabled is true — same shape guarantee as the MTU test above.
func TestValidateContainerAgenticFanout_CacheEnvGatingReachesShell(t *testing.T) {
	t.Parallel()

	t.Run("cache-env block included when cacheEnabled", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 0, true)
		if !strings.Contains(cmd, "#container-cache-env:$?") {
			t.Errorf("cache-env block should appear in combined shell when cache is enabled, got: %s", cmd)
		}
		if !strings.Contains(cmd, "CUSTOM_ACTIONS_RESULTS_URL") {
			t.Errorf("cache-env block should probe CUSTOM_ACTIONS_RESULTS_URL, got: %s", cmd)
		}
	})

	t.Run("cache-env block omitted when cache disabled", func(t *testing.T) {
		t.Parallel()
		cmd := containerAgenticFanoutCheckCommand("gh-sr-runner", "agentic-1", 0, false)
		if strings.Contains(cmd, "#container-cache-env:$?") {
			t.Errorf("cache-env block should be omitted when cache is disabled, got: %s", cmd)
		}
		if strings.Contains(cmd, "CUSTOM_ACTIONS_RESULTS_URL") {
			t.Errorf("disabled cache must not probe CUSTOM_ACTIONS_RESULTS_URL, got: %s", cmd)
		}
	})
}
