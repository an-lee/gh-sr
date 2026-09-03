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

// awfInnerHygieneChecks returns the inner-Docker (DinD runner) agentic hygiene
// check definitions. gh-aw v0.88+ runs AWF rootless: the job leaves behind
// awmg-mcpg / awf- / gh-aw containers rather than iptables state, so only
// container leakage is probed. Each remediation is pre-formatted with the outer
// container name so the operator running `gh sr doctor` knows which runner
// to ssh into.
func awfInnerHygieneChecks(outerContainer string) []awfHygieneCheck {
	return []awfHygieneCheck{
		{
			Name:    "orphan-agentic-containers",
			Cmd:     `docker ps -a --filter "name=awmg-mcpg" --filter "name=awf-" --filter "name=gh-aw" --format '{{.Names}}' 2>/dev/null | head -20`,
			Message: fmt.Sprintf("orphan awmg-mcpg/awf/gh-aw containers in inner Docker (runner container %s); normally reaped by the job-completed hook", outerContainer),
			Remediation: fmt.Sprintf(`Clean up inside the runner container (outer name %s):

  docker exec -it %s bash
  docker ps -a --filter "name=awmg-mcpg" --filter "name=awf-" --filter "name=gh-aw" --format '{{.ID}}' | xargs -r docker rm -f`, outerContainer, outerContainer),
		},
		{
			Name:    "orphan-agentic-networks",
			Cmd:     `docker network ls --filter "name=awf-net" --format '{{.Name}}' 2>/dev/null | head -20`,
			Message: fmt.Sprintf("orphan awf-net networks in inner Docker (runner container %s)", outerContainer),
			Remediation: fmt.Sprintf(`Prune leftover job networks inside the runner container:

  docker exec %s docker network prune -f --filter "name=awf-net"`, outerContainer),
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
// a container-mode runner: the inner dockerd must map
// --add-host=host.docker.internal:host-gateway to a usable non-loopback address,
// which is how gh-aw v0.88+ job containers (agent sandbox and awmg-mcpg gateway,
// both started with that flag) reach gateway/services endpoints.
func ValidateContainerInnerNetwork(h *host.Host, outerContainer, runnerName string) []PrereqFailure {
	d, _ := containerCheckDefByName("container-inner-host-docker-internal", outerContainer, runnerName, 0, false)
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
	d, _ := containerCheckDefByName("container-mtu", outerContainer, runnerName, hostEgressMTU, false)
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
	d, _ := containerCheckDefByName("container-node-npm", outerContainer, runnerName, 0, false)
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
// ValidateContainerMTU's gate; the cache-env probe only when cacheEnabled
// (runners without a local cache keep GitHub's cache service and must not
// carry CUSTOM_ACTIONS_RESULTS_URL).
//
// gh-aw v0.88+ rootless notes: no resolv.conf/dnsmasq probe (no baked DNS), no
// iptables service-routing probe (network isolation is the inner bridge
// topology), no awf CLI/sudo probe (AWF is rootless and fetched by the job).
func containerCheckDefs(outerContainer, runnerName string, hostEgressMTU int, cacheEnabled bool) []containerCheckDef {
	defs := []containerCheckDef{
		{
			Name:       "container-inner-host-docker-internal",
			LoginShell: false,
			InnerBody: `set -eu
ok=0
for i in 1 2 3 4 5; do
  ip=$(docker run --rm --add-host=host.docker.internal:host-gateway alpine getent hosts host.docker.internal 2>/dev/null | awk "{print \$1; exit}")
  case "$ip" in
    "" | 127.* | ::1) ;;
    *) ok=1; break ;;
  esac
  sleep 1
done
[ "$ok" -eq 1 ]`,
			Message: fmt.Sprintf("inner dockerd in runner container %s does not map --add-host=host.docker.internal:host-gateway to a usable non-loopback address; gh-aw v0.88+ job containers (agent sandbox, awmg-mcpg gateway) rely on it", outerContainer),
			Remediation: fmt.Sprintf(`Inspect the runner container's inner Docker and restart/rebuild it if stale:

  docker exec -it %s bash
  docker run --rm --add-host=host.docker.internal:host-gateway alpine sh -c 'getent hosts host.docker.internal'

If resolution is empty or loopback, restart the runner container. If it persists, run:

  gh sr rebuild %s`, outerContainer, runnerName),
			DocRef: "agentic-workflows.md §11a",
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
			Name:       "container-docker-socket-user",
			LoginShell: false,
			InnerBody:  `su -s /bin/sh runner -c 'docker info >/dev/null 2>&1'`,
			Message:    fmt.Sprintf("the runner user inside runner container %s cannot talk to the inner dockerd socket; the awmg-mcpg gateway mounts that socket and needs non-root access (fork image puts runner in the docker group, gid 123)", outerContainer),
			Remediation: fmt.Sprintf(`Rebuild the runner image so the inner dockerd socket is group-accessible to the runner user:

  gh sr rebuild %s

Verify:

  docker exec %s su -s /bin/sh runner -c 'docker info >/dev/null && echo ok'`, runnerName, outerContainer),
			DocRef: "agentic-workflows.md §11a",
		},
		{
			Name:       "container-zstd",
			LoginShell: true,
			InnerBody:  `command -v zstd >/dev/null`,
			Message:    fmt.Sprintf("zstd is not on PATH inside runner container %s; actions/cache restore/save and tool-cache archives need it", outerContainer),
			Remediation: fmt.Sprintf(`Rebuild the runner image so it includes zstd:

  gh sr rebuild %s`, runnerName),
			DocRef: "agentic-workflows.md §8",
		},
	}
	if cacheEnabled {
		defs = append(defs, containerCheckDef{
			Name:       "container-cache-env",
			LoginShell: false,
			InnerBody:  `grep -q '^CUSTOM_ACTIONS_RESULTS_URL=' /home/runner/.env`,
			Message:    fmt.Sprintf("runner container %s has no CUSTOM_ACTIONS_RESULTS_URL in .env; with the cache enabled the runner must point actions/cache at the per-host cache server", outerContainer),
			Remediation: fmt.Sprintf(`Redeploy the runner so the entrypoint writes the cache URL (check that the cache is enabled and reachable on this host):

  gh sr cache deploy
  gh sr up %s`, runnerName),
			DocRef: "guides/local-cache.md",
		})
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
// as the fanout path. Pass the same hostEgressMTU/cacheEnabled inputs as the
// fanout call so gated defs resolve consistently.
func containerCheckDefByName(name, outerContainer, runnerName string, hostEgressMTU int, cacheEnabled bool) (containerCheckDef, bool) {
	for _, d := range containerCheckDefs(outerContainer, runnerName, hostEgressMTU, cacheEnabled) {
		if d.Name == name {
			return d, true
		}
	}
	return containerCheckDef{}, false
}

// ValidateContainerAgenticFanout runs all in-scope agentic container prereq
// probes (InnerNetwork, NodeNPM, DockerSocketUser, Zstd, and the gated
// CacheEnv/MTU) against the outerContainer in a single `docker exec`
// invocation, replacing the separate h.Run round-trips the per-check wrappers
// used to issue with one.
//
// Each probe runs in its own `{ ...; } >/dev/null 2>&1` block whose exit code
// is captured by a trailing `echo "#<Name>:$?"`. The combined shell exits 0
// unconditionally (trailing `true`) so h.Run always returns success and the
// full tagged output reaches the Go side, where it's parsed line-by-line: a
// `:1` tag emits the per-check PrereqFailure; `:0` (or no line, for the gated
// checks when their gate is off) emits nothing.
//
// hostEgressMTU gates the MTU check (same rule as ValidateContainerMTU):
// 0 (unknown) or >= 1500 (standard) means there is nothing to pin, so the MTU
// block is omitted. cacheEnabled=false omits the cache-env block (a runner
// without a local cache must not carry CUSTOM_ACTIONS_RESULTS_URL).
//
// On a nil host, non-Linux host, or empty outerContainer the function returns
// nil without making any SSH round-trip — same short-circuit as the
// per-check wrappers.
func ValidateContainerAgenticFanout(h *host.Host, outerContainer, runnerName string, hostEgressMTU int, cacheEnabled bool) []PrereqFailure {
	if h == nil || h.OS != "linux" || outerContainer == "" {
		return nil
	}
	cmd := containerAgenticFanoutCheckCommand(outerContainer, runnerName, hostEgressMTU, cacheEnabled)
	specs := containerAgenticFanoutSpecs(outerContainer, runnerName, hostEgressMTU, cacheEnabled)
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
// (0 < MTU < 1500), and the cache-env block only when cacheEnabled — matching
// the gating in containerCheckDefs so a fanout call has identical observable
// behaviour to calling the per-check wrappers.
func containerAgenticFanoutCheckCommand(outerContainer, runnerName string, hostEgressMTU int, cacheEnabled bool) string {
	defs := containerCheckDefs(outerContainer, runnerName, hostEgressMTU, cacheEnabled)
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
// containerAgenticFanoutCheckCommand body — when a gated block (MTU,
// cache-env) is omitted, the parser simply never sees its `#<Name>:...` line
// and the spec is silently ignored. Message and Remediation are pre-rendered
// against the supplied outerContainer / runnerName so the output is
// byte-identical to the per-check wrappers.
func containerAgenticFanoutSpecs(outerContainer, runnerName string, hostEgressMTU int, cacheEnabled bool) map[string]PrereqFailure {
	defs := containerCheckDefs(outerContainer, runnerName, hostEgressMTU, cacheEnabled)
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
	d, _ := containerCheckDefByName("container-inner-host-docker-internal", outerContainer, "", 0, false)
	return d.checkCommand(outerContainer)
}

func containerNodeNPMCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-node-npm", outerContainer, "", 0, false)
	return d.checkCommand(outerContainer)
}

func containerDockerSocketUserCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-docker-socket-user", outerContainer, "", 0, false)
	return d.checkCommand(outerContainer)
}

func containerZstdCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-zstd", outerContainer, "", 0, false)
	return d.checkCommand(outerContainer)
}

func containerCacheEnvCheckCommand(outerContainer string) string {
	d, _ := containerCheckDefByName("container-cache-env", outerContainer, "", 0, true)
	return d.checkCommand(outerContainer)
}

func containerMTUCheckCommand(outerContainer string, hostEgressMTU int) string {
	d, _ := containerCheckDefByName("container-mtu", outerContainer, "", hostEgressMTU, false)
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
