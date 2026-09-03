package agentic

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/runner"
)

// PrereqFailure represents a single prerequisite check that failed.
type PrereqFailure struct {
	// Name is a short identifier for the failure, e.g. "sudo-iptables".
	Name string
	// Severity is "error" (blocks setup) or "warning" (non-blocking).
	Severity string
	// Message is a short human-readable description.
	Message string
	// Remediation is the exact shell command(s) to run to fix this failure.
	Remediation string
	// DocRef is an optional documentation reference, e.g. "agentic-workflows.md §5".
	DocRef string
}

// SeverityError indicates a hard failure that blocks setup.
const SeverityError = "error"

// SeverityWarning indicates a non-blocking warning.
const SeverityWarning = "warning"

// ValidateContainerPrereqs checks host prerequisites for runner_mode: container (DinD).
// The host only needs:
//   - Docker available on the host (to run the outer runner container)
//   - Support for --privileged containers (required for the inner dockerd)
//
// dnsmasq, sudoers/iptables, host.docker.internal, gh-aw tooling, and RUNNER_TEMP
// live inside the runner image and are not validated here.
func ValidateContainerPrereqs(h *host.Host) []PrereqFailure {
	var failures []PrereqFailure

	if h.OS != "linux" {
		failures = append(failures, PrereqFailure{
			Name:        "linux-required",
			Severity:    SeverityError,
			Message:     "runner_mode: container is only supported on Linux hosts",
			Remediation: "Use a Linux host with Docker. Container-mode self-hosted runners require Linux on the host.",
			DocRef:      "agentic-workflows.md §8b",
		})
		return failures
	}

	// docker CLI → daemon → --privileged chain — all three sub-probes run in
	// one SSH round-trip via dockerChainCheckCommand. Exit codes are captured
	// by parseDockerChainOutput and mapped to PrereqFailure entries via
	// dockerChainSpecs. Replaces 3 sequential h.Run calls with 1.
	out, _ := h.Run(dockerChainCheckCommand("privileged"))
	failures = append(failures, parseDockerChainOutput(out, dockerChainSpecs("privileged"))...)

	return failures
}

// awfHygieneCheck describes one AWF/gh-aw hygiene probe shared between
// ValidateAWFHygiene (host-level) and ValidateAWFHygieneInner (inner Docker
// inside a DinD runner). The two callers pass a prefix (empty for the host,
// `docker exec "X" ` for inner) and a Name suffix (empty for the host,
// `-inner` for inner) so the helper renders the correct failure shape.
type awfHygieneCheck struct {
	// Name is the base failure name; the helper appends `nameSuffix` (typically
	// empty or "-inner").
	Name string
	// Cmd is the probe shell command WITHOUT any prefix. The helper prepends
	// `pfx` so the same definition works for host-level and inner-Docker.
	Cmd string
	// Message is the fully-rendered PrereqFailure.Message for this variant.
	// The caller pre-formats any per-variant wording (e.g. outer container name).
	Message string
	// Remediation is the fully-rendered PrereqFailure.Remediation for this
	// variant. Same pre-format rule as Message.
	Remediation string
}

// runAWFHygieneChecks fans the supplied checks out across goroutines using the
// shared failureCollector. Each probe runs h.Run(pfx + check.Cmd) and, on
// non-empty TrimSpace output, appends a PrereqFailure with the suffix-applied
// name and the pre-rendered Message/Remediation/DocRef.
func runAWFHygieneChecks(h *host.Host, pfx, nameSuffix string, checks []awfHygieneCheck) []PrereqFailure {
	var collector failureCollector
	appendFailure := collector.append
	for _, c := range checks {
		c := c
		collector.spawn(func() {
			out, _ := h.Run(pfx + c.Cmd)
			if strings.TrimSpace(out) == "" {
				return
			}
			appendFailure(PrereqFailure{
				Name:        c.Name + nameSuffix,
				Severity:    SeverityWarning,
				Message:     c.Message,
				Remediation: c.Remediation,
				DocRef:      "agentic-workflows.md §12",
			})
		})
	}
	return collector.wait()
}

// awfInnerHygieneChecks returns the inner-Docker (DinD runner) AWF hygiene
// check definitions. Each remediation is pre-formatted with the outer
// container name so the operator running `gh sr doctor` knows which runner
// to ssh into. Probes drop the host-side `sudo -n` because the DinD inner
// daemon runs as root.
func awfInnerHygieneChecks(outerContainer string) []awfHygieneCheck {
	return []awfHygieneCheck{
		{
			Name:    "awf-orphan-containers",
			Cmd:     `docker ps -a --filter "name=awf-" --filter "name=gh-aw" --format '{{.Names}}' 2>/dev/null | head -20`,
			Message: fmt.Sprintf("orphan gh-aw/awf containers in inner Docker (runner container %s)", outerContainer),
			Remediation: fmt.Sprintf(`Clean up inside the runner container (outer name %s):

  docker exec -it %s bash
  docker ps -a --filter "name=awf-" --format '{{.ID}}' | xargs -r docker rm -f
  docker ps -a --filter "name=gh-aw" --format '{{.ID}}' | xargs -r docker rm -f`, outerContainer, outerContainer),
		},
		{
			Name:    "stale-docker-user-rules",
			Cmd:     `iptables -L DOCKER-USER --line-numbers -n 2>/dev/null | grep -i "awf\|gh-aw" | head -20`,
			Message: fmt.Sprintf("stale DOCKER-USER iptables rules in inner netns (runner container %s)", outerContainer),
			Remediation: fmt.Sprintf(`Flush inner AWF egress rules when no agentic job is using this runner:

  docker exec %s iptables -F DOCKER-USER`, outerContainer),
		},
		{
			Name:    "mcpg-orphan-containers",
			Cmd:     `docker ps -a --filter "name=gh-aw-mcpg-" --format '{{.Names}}' 2>/dev/null | head -20`,
			Message: fmt.Sprintf("orphan gh-aw-mcpg-* containers in inner Docker (runner container %s)", outerContainer),
			Remediation: fmt.Sprintf(`Clean up inside the runner container:

  docker exec -it %s bash
  docker ps -a --filter "name=gh-aw-mcpg-" --format '{{.ID}}' | xargs -r docker rm -f`, outerContainer),
		},
	}
}

// ValidateAWFHygieneInner runs orphan/stale AWF/gh-aw artefact checks against the
func ValidateAWFHygieneInner(h *host.Host, outerContainer string) []PrereqFailure {
	if h.OS != "linux" {
		return nil
	}
	pfx := runner.DockerExecCommand(outerContainer, "")
	return runAWFHygieneChecks(h, pfx, "-inner", awfInnerHygieneChecks(outerContainer))
}

// ValidateContainerInnerNetwork checks the network paths gh-aw depends on inside
// a container-mode runner. The MCP gateway runs in the inner host network, while
// agent/AWF child containers use the default bridge and reach the gateway via
// host.docker.internal.
func ValidateContainerInnerNetwork(h *host.Host, outerContainer, runnerName string) []PrereqFailure {
	d, _ := containerCheckDefByName("container-inner-host-docker-internal", outerContainer, runnerName, 0)
	return runContainerCheck(h, containerCheckSpec{
		name:        d.Name,
		checkCmd:    d.checkCommand(outerContainer),
		message:     d.Message,
		remediation: d.Remediation,
		docRef:      d.DocRef,
	})
}

// ValidateContainerInnerResolv checks that the runner container's /etc/resolv.conf
// points at the bundled dnsmasq (the inner docker0 gateway). entrypoint.sh repoints it
// there at startup so gh-aw's firewall — which auto-detects the agent sandbox's DNS
// servers from this very file — propagates an authoritative, AWF-exempt resolver for
// host.docker.internal to the sandbox.
//
// If a stale (pre-fix) image or a read-only resolv.conf leaves the OUTER host resolver
// in place, the sandbox resolves host.docker.internal there and intermittently gets a
// non-exempt IP. AWF's sandbox iptables only exempts the inner-bridge gateway from the
// transparent Squid redirect, so a non-exempt answer force-proxies the MCP gateway POST
// into Squid, which rejects the origin-form request (ERR_INVALID_URL → HTTP 400) and the
// agent reports "MCP server(s) failed to launch". This check surfaces that latent
// misconfiguration before a job hits it.
func ValidateContainerInnerResolv(h *host.Host, outerContainer, runnerName string) []PrereqFailure {
	d, _ := containerCheckDefByName("container-inner-resolv", outerContainer, runnerName, 0)
	return runContainerCheck(h, containerCheckSpec{
		name:        d.Name,
		checkCmd:    d.checkCommand(outerContainer),
		message:     d.Message,
		remediation: d.Remediation,
		docRef:      d.DocRef,
	})
}

// ValidateContainerAWFServiceRouting checks that the runner container has the
// AWF service-routing bypass installed in NAT PREROUTING.
//
// Without this rule, AWF agents on awf-net cannot reach workflow `services:`
// containers (postgres/redis/etc.) via host.docker.internal:<port>. Inner dockerd
// DNATs port-published traffic from the host gateway IP to the service container
// IP *before* AWF's FW_WRAPPER chain in FORWARD inspects it. AWF's
// `--allow-host-service-ports` rules match on the host gateway IP, so the post-
// DNAT packet falls through to FW_WRAPPER's catch-all REJECT (ICMP port-
// unreachable), surfacing as `Connection refused` for libpq/redis/etc.
//
// The bypass (installed by entrypoint.sh) is a single rule at the head of the
// runner container's NAT PREROUTING chain:
//
//	iptables -t nat -I PREROUTING -s 172.30.0.0/24 -m addrtype --dst-type LOCAL -j RETURN
//
// Traffic from awf-net to any local IP of the runner container then skips DNAT
// and is delivered to the userland docker-proxy, which forwards to the service.
func ValidateContainerAWFServiceRouting(h *host.Host, outerContainer, runnerName string) []PrereqFailure {
	d, _ := containerCheckDefByName("container-awf-service-routing", outerContainer, runnerName, 0)
	return runContainerCheck(h, containerCheckSpec{
		name:        d.Name,
		checkCmd:    d.checkCommand(outerContainer),
		message:     d.Message,
		remediation: d.Remediation,
		docRef:      d.DocRef,
	})
}

// ValidateContainerMTU warns when the runner container's egress interface (eth0) or the
// inner dockerd bridge (docker0) carries an MTU larger than the host's egress MTU. A
// container MTU above the host path MTU silently drops large packets when PMTUD is
// black-holed, breaking TLS handshakes — workflow downloads such as actions/setup-go
// then fail with "Client network socket disconnected before secure TLS connection was
// established" even though the host downloads fine. hostEgressMTU is the host's primary
// egress interface MTU (from runner.DetectHostEgressMTU); 0 or >= 1500 means there is
// nothing to pin, so the check is skipped (standard 1500 networks never see this).
func ValidateContainerMTU(h *host.Host, outerContainer, runnerName string, hostEgressMTU int) []PrereqFailure {
	if hostEgressMTU <= 0 || hostEgressMTU >= 1500 {
		return nil
	}
	d, _ := containerCheckDefByName("container-mtu", outerContainer, runnerName, hostEgressMTU)
	return runContainerCheck(h, containerCheckSpec{
		name:        d.Name,
		checkCmd:    d.checkCommand(outerContainer),
		message:     d.Message,
		remediation: d.Remediation,
		docRef:      d.DocRef,
	})
}

// ValidateContainerNodeNPM checks that node and npm are on PATH inside the runner
// container. gh-aw activation setup installs @actions/artifact via npm when daily AI
// credits guardrails are enabled (safe-output-artifact-client), before actions/setup-node runs.
func ValidateContainerNodeNPM(h *host.Host, outerContainer, runnerName string) []PrereqFailure {
	d, _ := containerCheckDefByName("container-node-npm", outerContainer, runnerName, 0)
	return runContainerCheck(h, containerCheckSpec{
		name:        d.Name,
		checkCmd:    d.checkCommand(outerContainer),
		message:     d.Message,
		remediation: d.Remediation,
		docRef:      d.DocRef,
	})
}

// ValidateContainerAWF checks that the gh-aw firewall CLI is available exactly
// the way compiled workflows invoke it.
func ValidateContainerAWF(h *host.Host, outerContainer, runnerName string) []PrereqFailure {
	d, _ := containerCheckDefByName("container-awf", outerContainer, runnerName, 0)
	return runContainerCheck(h, containerCheckSpec{
		name:        d.Name,
		checkCmd:    d.checkCommand(outerContainer),
		message:     d.Message,
		remediation: d.Remediation,
		docRef:      d.DocRef,
	})
}

// containerCheckDef is the single source of truth for each agentic container
// probe. The per-check wrappers (ValidateContainerInnerNetwork, etc.) and the
// fanout path (ValidateContainerAgenticFanout) both derive their shell command,
// failure metadata, and tagged output from the same definition, so a wording
// or logic change only needs to be made in one place.
type containerCheckDef struct {
	Name        string
	LoginShell  bool   // sh -lc (true) vs sh -c (false) for the per-check command
	InnerBody   string // raw shell statements (no docker-exec wrapper)
	Message     string
	Remediation string
	DocRef      string
}

// checkCommand builds the standalone `docker exec … sh -Xc '…'` command used
// by the per-check wrappers. The fanout path does not call this — it wraps
// InnerBody in `{ … } >/dev/null 2>&1; echo "#Name:$?"` blocks instead.
func (d containerCheckDef) checkCommand(outerContainer string) string {
	flag := "-c"
	if d.LoginShell {
		flag = "-lc"
	}
	return runner.DockerExecCommand(outerContainer, "sh "+flag+" '"+d.InnerBody+"'")
}

// containerCheckDefs returns the definitions for all in-scope agentic
// container probes, with Message/Remediation pre-rendered against the supplied
// outerContainer / runnerName / hostEgressMTU. The MTU probe is included only
// when hostEgressMTU falls in the pinning window (0 < MTU < 1500), matching
// ValidateContainerMTU's gate.
func containerCheckDefs(outerContainer, runnerName string, hostEgressMTU int) []containerCheckDef {
	defs := []containerCheckDef{
		{
			Name:       "container-inner-host-docker-internal",
			LoginShell: false,
			InnerBody: `set -eu
ok=0
for i in 1 2 3 4 5; do
  ip=$(docker run --rm alpine getent hosts host.docker.internal 2>/dev/null | awk "{print \$1; exit}")
  case "$ip" in
    "" | 127.* | ::1) ;;
    *) ok=1; break ;;
  esac
  sleep 1
done
[ "$ok" -eq 1 ]`,
			Message: fmt.Sprintf("host.docker.internal does not resolve to a usable non-loopback address inside runner container %s (baked dnsmasq/daemon.json DNS)", outerContainer),
			Remediation: fmt.Sprintf(`Inspect the runner container's inner Docker DNS and restart/rebuild it if stale:

  docker exec -it %s bash
  getent hosts host.docker.internal
  docker run --rm alpine sh -c 'getent hosts host.docker.internal'
  docker run --rm --network host alpine sh -c 'getent hosts host.docker.internal'
  docker run --rm --add-host=host.docker.internal:host-gateway alpine sh -c 'getent hosts host.docker.internal'

If add-host resolution is empty or loopback, restart the runner container. If it persists, run:

  gh sr rebuild %s`, outerContainer, runnerName),
			DocRef: "agentic-workflows.md §11a",
		},
		{
			Name:       "container-inner-resolv",
			LoginShell: false,
			InnerBody: `set -eu
gw=$(ip -4 -o addr show docker0 2>/dev/null | awk "{print \$4}" | cut -d/ -f1 | head -n1)
[ -n "$gw" ] || gw=10.200.0.1
grep -Eq "^nameserver[[:space:]]+$gw([[:space:]]|$)" /etc/resolv.conf`,
			Message: fmt.Sprintf("runner container %s /etc/resolv.conf is not pinned to the bundled dnsmasq (inner bridge gateway); the gh-aw agent sandbox can inherit the host resolver and intermittently fail MCP launch (host.docker.internal force-proxied into Squid)", outerContainer),
			Remediation: fmt.Sprintf(`Rebuild the runner image so entrypoint.sh repoints resolv.conf at the bundled dnsmasq:

  gh sr rebuild %s

Verify after restart (expect a single nameserver equal to the inner docker0 gateway, e.g. 10.200.0.1):

  docker exec %s cat /etc/resolv.conf
  docker exec %s ip -4 -o addr show docker0`, runnerName, outerContainer, outerContainer),
			DocRef: "agentic-workflows.md §11a",
		},
		{
			Name:       "container-awf-service-routing",
			LoginShell: false,
			InnerBody:  `iptables -t nat -S PREROUTING 2>/dev/null | grep -Fq -e "-A PREROUTING -s 172.30.0.0/24 -m addrtype --dst-type LOCAL -j RETURN"`,
			Message:    fmt.Sprintf("AWF service-routing bypass not installed in runner container %s; agentic workflows that use `services:` (postgres, redis, etc.) will see `Connection refused` on host.docker.internal", outerContainer),
			Remediation: fmt.Sprintf(`Rebuild the runner image (entrypoint.sh installs the bypass at startup):

  gh sr rebuild %s

Or apply the rule live without restart (lasts until docker daemon restart):

  docker exec %s iptables -t nat -I PREROUTING -s 172.30.0.0/24 -m addrtype --dst-type LOCAL -j RETURN

Verify:

  docker exec %s iptables -t nat -S PREROUTING | head -3`, runnerName, outerContainer, outerContainer),
			DocRef: "agentic-workflows.md §11b",
		},
		{
			Name:       "container-node-npm",
			LoginShell: true,
			InnerBody:  `command -v node >/dev/null && command -v npm >/dev/null`,
			Message:    fmt.Sprintf("node LTS/npm are not on PATH inside runner container %s", outerContainer),
			Remediation: fmt.Sprintf(`Rebuild the runner image so it includes Node.js LTS:

  gh sr rebuild %s`, runnerName),
			DocRef: "agentic-workflows.md §8",
		},
		{
			Name:       "container-awf",
			LoginShell: true,
			InnerBody: `set -eu
command -v awf >/dev/null
sudo -n -E awf --version >/dev/null`,
			Message: fmt.Sprintf("awf is not available via sudo inside runner container %s", outerContainer),
			Remediation: fmt.Sprintf(`Rebuild the runner image so it includes github/gh-aw-firewall:

  gh sr rebuild %s

For a temporary live-container unblock only:

  docker exec %s sh -lc 'curl -sSL https://raw.githubusercontent.com/github/gh-aw-firewall/main/install.sh | AWF_FORCE_BINARY=1 bash'`, runnerName, outerContainer),
			DocRef: "agentic-workflows.md §12",
		},
	}
	if hostEgressMTU > 0 && hostEgressMTU < 1500 {
		mtu := strconv.Itoa(hostEgressMTU)
		defs = append(defs, containerCheckDef{
			Name:       "container-mtu",
			LoginShell: false,
			InnerBody: "host=" + mtu + `
for ifc in eth0 docker0; do
  m=$(cat /sys/class/net/$ifc/mtu 2>/dev/null || echo 0)
  [ "$m" -le "$host" ] || exit 1
done`,
			Message: fmt.Sprintf("runner container %s has a Docker interface MTU larger than the host egress MTU (%d); large-packet TLS handshakes (e.g. actions/setup-go) can fail with \"Client network socket disconnected before secure TLS connection was established\"", outerContainer, hostEgressMTU),
			Remediation: fmt.Sprintf(`Rebuild the runner so it pins the inner/outer Docker MTU to the host egress MTU:

  gh sr rebuild %s

Verify (both must be <= %d):

  docker exec %s cat /sys/class/net/eth0/mtu
  docker exec %s cat /sys/class/net/docker0/mtu

If the host's real path MTU is below its NIC MTU (a tunnel the NIC is unaware of), set it explicitly in runners.yml and rebuild:

  container_runner_image:
    mtu: %d`, runnerName, hostEgressMTU, outerContainer, outerContainer, hostEgressMTU),
			DocRef: "agentic-workflows.md §11c",
		})
	}
	return defs
}

// containerCheckDefByName looks up a single probe definition by its Name tag.
// Used by the per-check wrappers so they share the same metadata + shell body
// as the fanout path.
func containerCheckDefByName(name, outerContainer, runnerName string, hostEgressMTU int) (containerCheckDef, bool) {
	for _, d := range containerCheckDefs(outerContainer, runnerName, hostEgressMTU) {
		if d.Name == name {
			return d, true
		}
	}
	return containerCheckDef{}, false
}

// ValidateContainerAgenticFanout runs all six agentic container prereq probes
// (InnerNetwork, InnerResolv, AWFServiceRouting, NodeNPM, AWF, MTU) against the
// outerContainer in a single `docker exec` invocation, replacing the six
// separate h.Run round-trips the per-check wrappers used to issue with one.
//
// Each probe runs in its own `{ ...; } >/dev/null 2>&1` block whose exit code
// is captured by a trailing `echo "#<Name>:$?"`. The combined shell exits 0
// unconditionally (trailing `true`) so h.Run always returns success and the
// full tagged output reaches the Go side, where it's parsed line-by-line: a
// `:1` tag emits the per-check PrereqFailure; `:0` (or no line, for the MTU
// check skipped on hosts with nothing to pin) emits nothing.
//
// This is the same win-class as PR #264/#269/#285/#301/#317 — collapse
// independent probes of the same resource into a single shell call so the
// SSH/host-exec round-trip count drops from 6 (one per probe) to 1 per
// container scanned by `gh sr doctor`. Material on agentic-fleet doctor
// runs (one round-trip per instance, regardless of how many probes fail).
//
// hostEgressMTU gates the MTU check (same rule as ValidateContainerMTU):
// 0 (unknown) or >= 1500 (standard) means there is nothing to pin, so the MTU
// block is omitted from the combined shell. Caller-visible behaviour is
// identical to calling the six per-check wrappers in order.
//
// On a nil host, non-Linux host, or empty outerContainer the function returns
// nil without making any SSH round-trip — same short-circuit as the
// per-check wrappers.
func ValidateContainerAgenticFanout(h *host.Host, outerContainer, runnerName string, hostEgressMTU int) []PrereqFailure {
	if h == nil || h.OS != "linux" || outerContainer == "" {
		return nil
	}
	cmd := containerAgenticFanoutCheckCommand(outerContainer, runnerName, hostEgressMTU)
	specs := containerAgenticFanoutSpecs(outerContainer, runnerName, hostEgressMTU)
	out, err := h.Run(cmd)
	if err != nil {
		// Transport-level failure (SSH drop, etc.) — surface one synthetic
		// failure for the fanout itself rather than silently dropping the
		// per-check warnings. Callers see this as the fanout having run
		// but produced no per-check detail.
		return []PrereqFailure{{
			Name:        "container-agentic-fanout",
			Severity:    SeverityWarning,
			Message:     fmt.Sprintf("could not run agentic container fanout against %s: %v", outerContainer, err),
			Remediation: fmt.Sprintf("Verify the runner container %s is reachable and Docker is responsive:\n\n  docker ps --filter name=%s\n  docker exec %s echo ok", outerContainer, outerContainer, outerContainer),
			DocRef:      "agentic-workflows.md §11",
		}}
	}
	return parseContainerAgenticFanoutOutput(out, specs)
}

// containerAgenticFanoutCheckCommand builds the single `docker exec` command
// that runs all in-scope agentic container probes against outerContainer. The
// MTU block is appended only when hostEgressMTU falls in the pinning window
// (0 < MTU < 1500), matching ValidateContainerMTU's gate so a fanout call
// has identical observable behaviour to calling the six per-check wrappers.
func containerAgenticFanoutCheckCommand(outerContainer, runnerName string, hostEgressMTU int) string {
	defs := containerCheckDefs(outerContainer, runnerName, hostEgressMTU)
	var inner strings.Builder
	for _, d := range defs {
		inner.WriteString("{ ")
		inner.WriteString(d.InnerBody)
		inner.WriteString("\n} >/dev/null 2>&1; echo \"#")
		inner.WriteString(d.Name)
		inner.WriteString(":$?\"\n")
	}
	inner.WriteString("true")
	return runner.DockerExecCommand(outerContainer, "sh -lc '"+inner.String()+"'")
}

// containerAgenticFanoutSpecs returns the per-check metadata used by the
// fanout parser to convert a `#<Name>:1` tag into the corresponding
// PrereqFailure. The set mirrors ValidateContainerAgenticFanout's
// containerAgenticFanoutCheckCommand body — when the MTU block is omitted,
// the parser simply never sees a `#container-mtu:...` line and the spec is
// silently ignored. Message and Remediation are pre-rendered against the
// supplied outerContainer / runnerName so the output is byte-identical to
// the per-check wrappers.
func containerAgenticFanoutSpecs(outerContainer, runnerName string, hostEgressMTU int) map[string]PrereqFailure {
	defs := containerCheckDefs(outerContainer, runnerName, hostEgressMTU)
	specs := make(map[string]PrereqFailure, len(defs))
	for _, d := range defs {
		specs[d.Name] = PrereqFailure{
			Name:        d.Name,
			Severity:    SeverityWarning,
			Message:     d.Message,
			Remediation: d.Remediation,
			DocRef:      d.DocRef,
		}
	}
	return specs
}

// parseContainerAgenticFanoutOutput walks the tagged stdout emitted by the
// combined `docker exec` fanout and emits a PrereqFailure for every
// `#<specName>:1` line it finds. Lines that don't match the `#name:N` shape
// (e.g. incidental stderr that snuck through, or shell noise from one of the
// scoped blocks) are ignored. A `:0` tag means the probe passed and is
// silently dropped. The order of returned failures matches the order of the
// tags on stdout, which in turn matches the order of the probe blocks in
// containerAgenticFanoutCheckCommand — i.e. the same submission order the
// per-check wrappers would have produced.
func parseContainerAgenticFanoutOutput(out string, specs map[string]PrereqFailure) []PrereqFailure {
	var failures []PrereqFailure
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "#") {
			continue
		}
		rest := strings.TrimPrefix(line, "#")
		name, status, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(status) != "1" {
			continue
		}
		spec, ok := specs[name]
		if !ok {
			continue
		}
		failures = append(failures, spec)
	}
	return failures
}

// dockerChainCheckCommand builds the single shell command that runs the
// three docker-chain prereq probes (CLI → daemon → <third>) and tags each
// with `#<name>:<exit>` on stdout. All three sub-probes run unconditionally
// so a single failing prerequisite surfaces every dependent failure in one
// pass — the Go parser maps each non-zero tag back to its PrereqFailure
// via the chain's specs. Replaces 3 sequential h.Run calls with 1 SSH
// round-trip on the `gh sr doctor` ValidateContainerPrereqs hot path.
//
// variant selects the third probe and its associated spec key:
//   - "privileged": --privileged support probe (used by ValidateContainerPrereqs)
//
// The first two probes (CLI version, daemon info) are shared; only the third
// differs to match the caller's intent.
func dockerChainCheckCommand(variant string) string {
	var third string
	switch variant {
	case "privileged":
		// Mirrors the original probe's dual check: docker must exit 0 AND
		// the inner shell must echo "privileged-ok". Either failing the
		// block emits a non-zero tag.
		third = `{ out=$(docker run --rm --privileged alpine sh -c "echo privileged-ok" 2>/dev/null); rc=$?; if [ "$rc" -ne 0 ] || [ "$out" != "privileged-ok" ]; then exit 1; fi; } >/dev/null 2>&1; echo "#docker-privileged:$?"`
	default:
		return ""
	}
	return `{ docker --version >/dev/null 2>&1; } >/dev/null 2>&1; echo "#docker-cli:$?"
{ docker info >/dev/null 2>&1; } >/dev/null 2>&1; echo "#docker-daemon:$?"
` + third
}

// dockerChainSpecs returns the per-probe metadata the docker-chain parser
// uses to convert a `#<name>:<non-zero>` tag into the corresponding
// PrereqFailure. The CLI/daemon specs are shared across both variants; the
// third probe spec varies. Pass the same variant string to both
// dockerChainCheckCommand and dockerChainSpecs so they line up.
func dockerChainSpecs(variant string) map[string]PrereqFailure {
	specs := map[string]PrereqFailure{
		"docker-cli": {
			Name:     "docker-cli",
			Severity: SeverityError,
			Message:  "docker CLI not found on PATH",
			Remediation: `On the host, install Docker:

  sudo apt-get update && sudo apt-get install -y docker.io
  sudo systemctl enable --now docker
  sudo usermod -aG docker $USER
  # Log out and back in for group membership to take effect`,
			DocRef: "agentic-workflows.md §3g",
		},
		"docker-daemon": {
			Name:     "docker-daemon",
			Severity: SeverityError,
			Message:  "docker daemon not running",
			Remediation: `Start the Docker daemon on the host:

  sudo systemctl start docker
  sudo systemctl enable docker  # persist across reboots`,
			DocRef: "agentic-workflows.md §3g",
		},
	}
	switch variant {
	case "privileged":
		specs["docker-privileged"] = PrereqFailure{
			Name:     "docker-privileged",
			Severity: SeverityError,
			Message:  "docker --privileged containers are not supported; required for DinD (inner dockerd)",
			Remediation: `Privileged containers may be blocked by:
  - A non-root Docker daemon with userns-remap enabled (disable it for this use-case)
  - A Kubernetes/container runtime security policy
  - Seccomp/AppArmor profile restrictions

  Verify with: docker run --rm --privileged alpine echo ok
  For Sysbox (rootless-compatible alternative): see agentic-workflows.md §12`,
			DocRef: "agentic-workflows.md §8b",
		}
	}
	return specs
}

// parseDockerChainOutput converts the stdout of dockerChainCheckCommand
// into the per-probe failure list. Each `#<name>:N` tag is mapped through
// the supplied specs; tags with N==0 (success) are dropped, non-zero tags
// produce a failure entry. Tags absent from the output (e.g. shell
// short-circuited away — never happens with dockerChainCheckCommand's
// unconditional block layout, but defensive against future tightening) are
// silently ignored. Order of returned failures matches tag order on stdout,
// which matches probe-block order in dockerChainCheckCommand.
func parseDockerChainOutput(out string, specs map[string]PrereqFailure) []PrereqFailure {
	var failures []PrereqFailure
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "#") {
			continue
		}
		rest := strings.TrimPrefix(line, "#")
		name, status, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		// Non-zero exit code → failure. Covers docker CLI missing (127),
		// daemon down (1), permission denied (1), image pull failure (125/1),
		// and any other error mode without the parser needing to know the
		// specific code.
		code, err := strconv.Atoi(strings.TrimSpace(status))
		if err != nil || code == 0 {
			continue
		}
		spec, ok := specs[name]
		if !ok {
			continue
		}
		failures = append(failures, spec)
	}
	return failures
}

func containerInnerNetworkCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-inner-host-docker-internal", outerContainer, "", 0)
	return d.checkCommand(outerContainer)
}

func containerNodeNPMCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-node-npm", outerContainer, "", 0)
	return d.checkCommand(outerContainer)
}

func containerAWFCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-awf", outerContainer, "", 0)
	return d.checkCommand(outerContainer)
}

func containerMTUCheckCommand(outerContainer string, hostEgressMTU int) string {
	d, _ := containerCheckDefByName("container-mtu", outerContainer, "", hostEgressMTU)
	return d.checkCommand(outerContainer)
}

func containerInnerResolvCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-inner-resolv", outerContainer, "", 0)
	return d.checkCommand(outerContainer)
}

func containerAWFServiceRoutingCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-awf-service-routing", outerContainer, "", 0)
	return d.checkCommand(outerContainer)
}

// containerCheckSpec captures the per-check inputs to runContainerCheck: the
// already-built docker-exec probe command, plus the failure Name, the pre-
// rendered human Message and Remediation, and the DocRef. The spec fields
// are populated from containerCheckDef (the single source of truth) so the
// per-check wrappers and the fanout path always produce identical metadata.
type containerCheckSpec struct {
	name        string
	checkCmd    string
	message     string
	remediation string
	docRef      string
}

// runContainerCheck executes one ValidateContainer* probe: short-circuits on
// nil host or non-Linux OS, runs spec.checkCmd via h.Run, and emits a single
// SeverityWarning PrereqFailure when the command errors. Used by all six
// ValidateContainer* wrappers in this file; ValidateContainerMTU keeps its
// extra hostEgressMTU guard at the wrapper level because that gate is the
// only check that depends on a numeric input.
func runContainerCheck(h *host.Host, spec containerCheckSpec) []PrereqFailure {
	if h == nil || h.OS != "linux" {
		return nil
	}
	if _, err := h.Run(spec.checkCmd); err != nil {
		return []PrereqFailure{{
			Name:        spec.name,
			Severity:    SeverityWarning,
			Message:     spec.message,
			Remediation: spec.remediation,
			DocRef:      spec.docRef,
		}}
	}
	return nil
}

// failureCollector accumulates PrereqFailure entries from concurrent goroutines
// and waits for them to finish. Use it inside any Validate* function that fans
// its checks out across independent goroutines: declare `var c failureCollector`,
// spawn each check via `c.spawn(func(){ ... })`, then return `c.wait()`.
//
// The Validate* funcs in this file (runAWFHygieneChecks for
// ValidateAWFHygieneInner) need exactly this pattern — a mutex-guarded
// failures slice plus a WaitGroup — so the boilerplate lives here instead of
// being copied. The helper is private because no caller outside this package
// should reach for the failure-append primitives directly; failures are a
// return-value contract, not an exposed accumulator.
type failureCollector struct {
	mu       sync.Mutex
	wg       sync.WaitGroup
	failures []PrereqFailure
}

// append records f in submission order under a mutex so concurrent goroutines
// can safely share one collector. Safe for use from inside a c.spawn closure.
func (c *failureCollector) append(f PrereqFailure) {
	c.mu.Lock()
	c.failures = append(c.failures, f)
	c.mu.Unlock()
}

// go spawns fn in a tracked goroutine. Pair with c.wait() to join.
func (c *failureCollector) spawn(fn func()) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		fn()
	}()
}

// wait blocks until every goroutine spawned via c.spawn has returned and returns
// the accumulated failures in submission order.
func (c *failureCollector) wait() []PrereqFailure {
	c.wg.Wait()
	return c.failures
}

// HasBlockingFailures returns true if any failure has severity "error".
func HasBlockingFailures(failures []PrereqFailure) bool {
	for _, f := range failures {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// FormatRemediation returns a formatted remediation string for a single failure.
func FormatRemediation(failure PrereqFailure) string {
	var sb strings.Builder
	sb.WriteString("\n  ")
	if failure.DocRef != "" {
		fmt.Fprintf(&sb, "[%s] ", failure.DocRef)
	}
	sb.WriteString(failure.Message)
	sb.WriteString("\n\n")
	lines := strings.Split(failure.Remediation, "\n")
	for i, line := range lines {
		if i == 0 {
			sb.WriteString("  To fix:\n")
		}
		sb.WriteString("    ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatAllRemediations returns a formatted string with all failures and their remediations.
func FormatAllRemediations(failures []PrereqFailure) string {
	if len(failures) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintln(&sb, "╔════════════════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(&sb, "║  Agentic Prerequisite Failures                                            ║")
	fmt.Fprintln(&sb, "╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(&sb, "║  %d failure(s) need to be resolved before agentic workflows can run.      ║\n", len(failures))
	fmt.Fprintln(&sb, "╚════════════════════════════════════════════════════════════════════════════╝")
	for i, f := range failures {
		sev := "FAIL"
		if f.Severity == SeverityWarning {
			sev = "WARN"
		}
		fmt.Fprintf(&sb, "\n[%d] %s: %s", i+1, sev, f.Name)
		sb.WriteString(FormatRemediation(f))
	}
	return sb.String()
}
