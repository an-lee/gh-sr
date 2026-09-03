package agentic

import (
	"strings"
	"testing"
)

func TestContainerInnerNetworkCheckCommand(t *testing.T) {
	t.Parallel()

	cmd := containerInnerNetworkCheckCommand("gh-sr-rune-agentic-1")

	// Rootless era: the probe itself must verify the --add-host mapping works
	// the way gh-aw v0.88+ job containers rely on — launching a container with
	// the same flag and rejecting loopback answers.
	for _, want := range []string{
		"docker exec",
		"gh-sr-rune-agentic-1",
		"docker run --rm --add-host=host.docker.internal:host-gateway alpine getent hosts host.docker.internal",
		"127.*",
		"ok=1",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("expected command to contain %q, got:\n%s", want, cmd)
		}
	}
	if !strings.Contains(cmd, "--add-host") {
		t.Fatalf("inner-network check must exercise the --add-host mapping, got:\n%s", cmd)
	}
	if strings.Contains(cmd, "resolv.conf") || strings.Contains(cmd, "iptables") {
		t.Fatalf("inner-network check must not depend on retired baked-DNS/iptables probes, got:\n%s", cmd)
	}
}

func TestContainerDockerSocketUserCheckCommand(t *testing.T) {
	t.Parallel()

	cmd := containerDockerSocketUserCheckCommand("gh-sr-rune-agentic-1")

	for _, want := range []string{
		"docker exec",
		"gh-sr-rune-agentic-1",
		// InnerBody is PosixSingleQuote-embedded: its own single quotes are
		// escaped ('\'''), which is what keeps the outer shell from mangling
		// the probe.
		`su -s /bin/sh runner -c '\''docker info >/dev/null 2>&1'\'''`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("expected command to contain %q, got:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "sudo") {
		t.Fatalf("socket-user check must not invoke sudo (rootless AWF era), got:\n%s", cmd)
	}
}

func TestContainerZstdCheckCommand(t *testing.T) {
	t.Parallel()

	cmd := containerZstdCheckCommand("gh-sr-rune-agentic-1")

	for _, want := range []string{
		"docker exec",
		"gh-sr-rune-agentic-1",
		"command -v zstd",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("expected command to contain %q, got:\n%s", want, cmd)
		}
	}
}

func TestContainerCacheEnvCheckCommand(t *testing.T) {
	t.Parallel()

	cmd := containerCacheEnvCheckCommand("gh-sr-rune-agentic-1")

	for _, want := range []string{
		"docker exec",
		"gh-sr-rune-agentic-1",
		`grep -q '\''^CUSTOM_ACTIONS_RESULTS_URL='\'' /home/runner/.env`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("expected command to contain %q, got:\n%s", want, cmd)
		}
	}
}

func TestContainerMTUCheckCommand(t *testing.T) {
	t.Parallel()

	cmd := containerMTUCheckCommand("gh-sr-rune-agentic-1", 1460)

	// Must docker exec into the runner container and compare both Docker interfaces'
	// MTUs (eth0, docker0) against the host egress MTU, failing (exit 1) if any exceeds it.
	for _, want := range []string{
		"docker exec",
		"gh-sr-rune-agentic-1",
		"host=1460",
		"eth0 docker0",
		"/sys/class/net/$ifc/mtu",
		"exit 1",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("expected command to contain %q, got:\n%s", want, cmd)
		}
	}
}

func TestValidateContainerMTU_skipsWhenNothingToPin(t *testing.T) {
	t.Parallel()
	// hostEgressMTU 0 (unknown) or >= 1500 (standard) means there is nothing to validate,
	// so the check must short-circuit without running any command.
	for _, mtu := range []int{0, 1500, 1600} {
		if got := ValidateContainerMTU(nil, "gh-sr-x", "x", mtu); got != nil {
			t.Fatalf("ValidateContainerMTU(hostEgressMTU=%d) = %v, want nil (skipped)", mtu, got)
		}
	}
}
