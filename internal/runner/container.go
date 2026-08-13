package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
)

// AgenticRunnerImageTag is the local Docker image tag built by gh sr setup.
const AgenticRunnerImageTag = "gh-sr/agentic-runner"

// Docker image labels stamped at build time (see buildAgenticRunnerImage).
const (
	dockerLabelImageRevision = "gh-sr.image-revision"
	dockerLabelCLIVersion    = "gh-sr.cli-version"
)

// Host-side marker files under the runner state bind-mount (/runner-state in container).
const (
	bootstrapFailedMarker    = "bootstrap-failed"
	dockerdStartFailuresFile = "dockerd-start-failures"
)

// ContainerImageLayoutRevision returns a short hex fingerprint of the embedded
// container image layout (Dockerfile, manifests, entrypoint, wrapper), gh-sr
// CLI version, and extra apt package list. It changes when any of those inputs change.
func ContainerImageLayoutRevision(ghSrVersion string, extraApt []string) string {
	if ghSrVersion == "" {
		ghSrVersion = "unknown"
	}
	var b strings.Builder
	b.WriteString("gh-sr-container-image/v1\x00")
	b.WriteString(ghSrVersion)
	b.WriteByte(0)
	for _, p := range containerRunnerImageExtraSorted(extraApt) {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString(agenticRunnerDockerfile)
	b.WriteString(agenticRunnerAptPackagesCore)
	b.WriteString(agenticRunnerEntrypoint)
	b.WriteString(agenticRunnerDockerWrapper)
	b.WriteString(agenticRunnerDaemonJSON)
	b.WriteString(agenticRunnerDnsmasqConf)
	b.WriteString(agenticRunnerJobStartedHook)
	b.WriteString(agenticRunnerJobCompletedHook)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:12]
}

// containerRunnerImageExtraSorted returns a sorted copy of unique non-empty package names.
func containerRunnerImageExtraSorted(extra []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, p := range extra {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// ContainerRunnerImageTag returns the Docker image reference for the container runner
// (e.g. gh-sr/agentic-runner:2.320.0 or gh-sr/agentic-runner:2.320.0-xa1b2c3d when extras are set).
func ContainerRunnerImageTag(actionsRunnerVersion string, extraApt []string) string {
	base := fmt.Sprintf("%s:%s", AgenticRunnerImageTag, actionsRunnerVersion)
	sorted := containerRunnerImageExtraSorted(extraApt)
	if len(sorted) == 0 {
		return base
	}
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	suffix := hex.EncodeToString(sum[:])[:8]
	return base + "-x" + suffix
}

// ContainerDockerName returns the deterministic Docker container name for a runner instance.
func ContainerDockerName(instanceName string) string {
	return "gh-sr-" + instanceName
}

func containerName(instanceName string) string {
	return ContainerDockerName(instanceName)
}

// DockerExecCommand returns a `docker exec <cname> <innerCmd>` shell-safe command
// string, with cname wrapped in Go-style double quotes via QuoteContainerName.
// The trailing space after the quoted name lets callers append the inner command
// directly (`DockerExecCommand(name, "sh -c '...'")` produces
// `docker exec "name" sh -c '...'`).
//
// This is the canonical helper for "run X inside a known runner container".
// It replaces the former inline `q := strconv.Quote(c); ... + "docker exec " + q + " " + ...`
// blocks across internal/agentic and internal/doctor (see issue #251) and the
// `hostshell.PosixSingleQuote(cname)` blocks in internal/runner/disk.go and
// internal/runner/environment.go (consolidated via QuoteContainerName).
func DockerExecCommand(cname, innerCmd string) string {
	return "docker exec " + QuoteContainerName(cname) + " " + innerCmd
}

// QuoteContainerName returns cname wrapped in Go-style double quotes via
// strconv.Quote, producing shell-safe output for use as a single shell argument
// to docker commands (docker exec, docker inspect, docker start, docker rm, etc.).
//
// It is the canonical helper for "container name as a docker CLI shell arg",
// mirroring DockerExecCommand's quoting style so a single regression test pins
// both call shapes. The escape behaviour was verified end-to-end: input
// `evil"; rm -rf /; "` round-trips as `docker exec "evil\"; rm -rf /; \"" echo ok`.
//
// Prefer this over hostshell.PosixSingleQuote for any docker command that takes
// the container name as a positional argument. Use hostshell.PosixSingleQuote
// only when the value must be embedded inside a single-quoted shell snippet
// (e.g. the inner argument to `sh -c '...'`).
func QuoteContainerName(cname string) string {
	return strconv.Quote(cname)
}

// ContainerReadinessReport captures the result of a single readiness probe
// against a DinD runner container. The three signals (State / InnerDockerdOK /
// Registered) together encode the contract that "a healthy DinD runner container
// is in state running/restarting, has a responsive inner dockerd, and contains
// a registered actions runner". Callers decide whether to interpret a partial
// report as a transient (polling) or terminal (one-shot) failure and format
// their own user-facing messages.
type ContainerReadinessReport struct {
	// State is the result of `docker inspect --format '{{.State.Status}}'`,
	// trimmed. Expected values: "running", "restarting", "missing", "" (host
	// unreachable or the inspect command itself failed), or any other Docker
	// state string (e.g. "paused", "exited").
	State string
	// InnerDockerdOK is true iff the inner dockerd answered `docker info`.
	// Only meaningful when State is "running" or "restarting"; false
	// otherwise.
	InnerDockerdOK bool
	// Registered is true iff /home/runner/actions-runner/.runner is present
	// inside the container (the actions runner has finished its config.sh
	// step). Only meaningful when State is "running" or "restarting";
	// false otherwise.
	Registered bool
}

// ContainerStateStatus probes the runtime state of cname via
// `docker inspect --format '{{.State.Status}}'`, returning the trimmed status
// string (e.g. "running", "restarting", "exited"). The probe uses
// `... || echo missing` so an absent container surfaces as ("missing", nil)
// rather than an error; an empty inspect result is collapsed to "missing" as
// well. err is non-nil only when the underlying h.Run call itself fails
// (typically a host-level connection error), in which case state is "missing".
//
// This is the canonical helper for "what state is this container in?" and
// consolidates the three former inline copies of the inspect probe (see issue
// #268): ProbeDinDContainerReadiness and the doctor's per-instance hygiene
// gate both switch on its return instead of re-emitting the shell string.
func ContainerStateStatus(h *host.Host, cname string) (string, error) {
	q := QuoteContainerName(cname)
	out, err := h.Run(fmt.Sprintf(`docker inspect --format '{{.State.Status}}' %s 2>/dev/null || echo missing`, q))
	if err != nil {
		return "missing", err
	}
	state := strings.TrimSpace(out)
	if state == "" {
		state = "missing"
	}
	return state, nil
}

// IsContainerAcceptingJobs reports whether a docker container state means the
// container is up enough to be accepting runner work — i.e. "running" or
// "restarting" (the latter is a transient self-heal state that the readiness
// gate treats as ready once the inner dockerd responds). It is the single
// source of truth for the `state == "running" || state == "restarting"`
// acceptance set that previously appeared inline in containerAwaitHealthy,
// ProbeDinDContainerReadiness, and the doctor checks (see issue #275). If
// Docker gains another transient-OK state, this is the only place to update.
func IsContainerAcceptingJobs(state string) bool {
	return state == "running" || state == "restarting"
}

// ProbeDinDContainerReadiness runs the standard readiness triad against cname
// on host h: outer container state + inner dockerd responsive + actions runner
// registered. It is shared by:
//   - runner.containerAwaitHealthy (polling gate during Start)
//   - doctor.checkContainerRunnerInstall (one-shot doctor report)
//
// The error return is non-nil only if the underlying `docker inspect` call
// itself failed (typically a host-level connection error). A missing container
// surfaces as State == "missing" with err == nil, because ContainerStateStatus
// absorbs the "No such object" exit code into the captured stdout.
func ProbeDinDContainerReadiness(h *host.Host, cname string) (ContainerReadinessReport, error) {
	state, err := ContainerStateStatus(h, cname)
	if err != nil {
		return ContainerReadinessReport{}, err
	}
	rep := ContainerReadinessReport{State: state}
	if !IsContainerAcceptingJobs(state) {
		return rep, nil
	}
	// Combine the two inner docker-exec probes (inner dockerd + .runner
	// registered) into a single `docker exec` SSH round-trip. Both probes
	// run against the same container and answer correlated questions, so
	// they belong in one shell invocation — saves 1 round-trip per probe,
	// which is material on containerAwaitHealthy's 2-s polling loop during
	// Start and on doctor.checkContainerRunnerInstall's per-instance check.
	// Mirrors the win-class of removeContainer (PR #264), EnsureHostDocker
	// (PR #269), and autostart.Detect (PR #285). Observable contract is
	// unchanged: "dockerd-ok" sets InnerDockerdOK, "ok" sets Registered,
	// and either token being absent leaves the corresponding bool false.
	out, _ := h.Run(DockerExecCommand(cname, `sh -c 'docker info >/dev/null 2>&1 && echo dockerd-ok || echo no
test -f /home/runner/actions-runner/.runner && echo ok || echo no'`))
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case "dockerd-ok":
			rep.InnerDockerdOK = true
		case "ok":
			rep.Registered = true
		}
	}
	return rep, nil
}

// containerStateDir returns the host-side bind-mount path for runner instance state
// (mounted at /runner-state inside the container).
//
// Cache vs. per-job scratch separation:
//   - PERSISTENT cache: /runner-state/docker-data holds the inner Docker image-layer
//     cache. It survives container restarts and per-job resets so jobs never re-pull
//     gh-aw's (large) images. The per-job reset hooks never prune images/volumes.
//   - PER-JOB scratch: the gh-aw runtime tree (/tmp/gh-aw, inside the container rootfs),
//     leftover inner containers/networks, and AWF iptables rules are wiped before and
//     after every job by /opt/gh-sr/hooks/job-{started,completed}.sh, so each job starts
//     from a pristine inner environment.
func containerStateDir(h *host.Host, instanceName string) string {
	return h.RunnerDir(instanceName)
}

// resolveAbsoluteRunnerDir returns the absolute (non-variable) path for the runner
// state directory on the host by expanding $HOME to its real value.
// h.RunnerDir returns a shell-variable string ("$HOME/...") which must not appear
// inside single quotes in Docker arguments — Docker does not perform shell expansion.
func resolveAbsoluteRunnerDir(h *host.Host, instanceName string) (string, error) {
	dir := h.RunnerDir(instanceName)
	if !strings.HasPrefix(dir, "$HOME") {
		return dir, nil
	}
	out, err := h.Run("echo $HOME")
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	home := strings.TrimSpace(out)
	return home + dir[len("$HOME"):], nil
}

// resolveStateDirOrFallback returns the absolute runner state directory for
// best-effort host-side paths where an unresolved "$HOME/..." literal is still
// safe (the shell expands $HOME on the subsequent h.Run call). Use this when
// the path is being passed into a `rm -f` / `test -f` shell command — for
// `docker create -v`, use resolveAbsoluteRunnerDir instead, since Docker does
// not perform shell expansion.
func resolveStateDirOrFallback(h *host.Host, instanceName string) string {
	if dir, err := resolveAbsoluteRunnerDir(h, instanceName); err == nil {
		return dir
	}
	return containerStateDir(h, instanceName)
}

// containerRunnerPresent returns true when the Docker container for the instance exists
// (regardless of whether it is running or stopped).
func containerRunnerPresent(h *host.Host, instanceName string) bool {
	name := containerName(instanceName)
	ok, _ := hostshell.RemoteBoolCheck(h,
		fmt.Sprintf("docker inspect --format='{{.Name}}' %s 2>/dev/null | grep -q '^/%s$'", name, name))
	return ok
}

// containersPresentOneShot returns which of the requested runner instance names have a
// Docker container present on the host (running or stopped), doing exactly ONE
// SSH round-trip regardless of how many instances are queried.
//
// This replaces the N-round-trip `containerRunnerPresent` loops in
// setupContainer and needsSetupContainer when N>=2; for a single instance the
// dedicated containerRunnerPresent is cheaper (no list parsing). The output
// is parsed as one name per line; names outside the input set are ignored
// (host-owned containers whose name happens to share the `gh-sr-` prefix are
// not relevant and won't affect the result).
//
// The `name=gh-sr-` Docker filter is a substring match; we always strip the
// prefix back off before lookup so we can reason about "is gh-sr-<n> there"
// with the same semantics containerRunnerPresent had per-instance.
func containersPresentOneShot(h *host.Host, instanceNames []string) (map[string]bool, error) {
	present := make(map[string]bool, len(instanceNames))
	for _, n := range instanceNames {
		present[n] = false
	}
	if len(instanceNames) == 0 {
		return present, nil
	}
	out, err := h.Run(`docker ps -a --filter name=gh-sr- --format '{{.Names}}' | sed 's|^gh-sr-||' | sort -u`)
	if err != nil {
		return present, err
	}
	// strings.SplitSeq avoids the upfront []string allocation that strings.Split
	// makes for the full output of `docker ps -a --filter name=gh-sr-`. Each line
	// is a sub-slice of `out` (no copy), and the rare trailing \r (CRLF) is
	// trimmed in-place by slicing one byte off the end — also 0-alloc. The
	// per-call alloc drop compounds across the NeedsSetup/Remove/Status paths
	// that call containersPresentOneShot once per host.
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		// Defensive against CRLF line endings from Windows-hosted scripts:
		// the trailing \r would otherwise make `present["ci-1\r"]` miss.
		if line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
			if line == "" {
				continue
			}
		}
		if _, ok := present[line]; ok {
			present[line] = true
		}
	}
	return present, nil
}

// setupContainer builds the gh-sr container runner image (if not already up to date)
// and creates (but does not start) each runner container.
func (m *Manager) setupContainer(h *host.Host, rc config.RunnerConfig) error {
	if h.OS != "linux" {
		return fmt.Errorf("runner_mode: container is only supported on Linux hosts")
	}

	if err := EnsureHostDocker(h, m.out(), rc.Name); err != nil {
		if errors.Is(err, ErrDockerGroupPending) {
			return err
		}
		return fmt.Errorf("%s: ensuring host Docker: %w", rc.Name, err)
	}

	// Resolve runner version, host arch, and image tag.
	version, arch, imageTag, err := m.resolveRunnerImageInputs(h)
	if err != nil {
		return err
	}

	fmt.Fprintf(m.out(), "  %s: checking container runner image %s...\n", rc.Name, imageTag)

	built, err := m.buildRunnerImageIfMissing(h, imageTag, version, arch, func() {
		fmt.Fprintf(m.out(), "  %s: building container runner image (this may take several minutes)...\n", rc.Name)
	})
	if err != nil {
		return err
	}
	if built {
		fmt.Fprintf(m.out(), "  %s: image built: %s\n", rc.Name, imageTag)
	} else {
		fmt.Fprintf(m.out(), "  %s: image already up to date\n", rc.Name)
	}

	// Batch the per-instance existence probe so a single `gh sr setup` makes
	// ONE round-trip for presence checks instead of N. Mirrors the
	// startContainer fold-in (PR #342): correlated I/O belongs in one shell.
	instanceNames := rc.InstanceNames()
	present, err := containersPresentOneShot(h, instanceNames)
	if err != nil {
		return fmt.Errorf("checking container presence: %w", err)
	}
	for i, name := range instanceNames {
		if present[name] {
			fmt.Fprintf(m.out(), "  %s: container already exists, skipping\n", name)
			continue
		}
		if err := m.createContainerInstance(h, rc, i, name, imageTag); err != nil {
			return err
		}
		fmt.Fprintf(m.out(), "  %s: container created (run `gh sr up` to start)\n", name)
	}

	return nil
}

// DetectHostEgressMTU returns the MTU of the host's primary egress interface (the one
// routing to the public internet), or 0 if it cannot be determined. It is used to pin
// the runner container's inner/outer Docker MTU to the host's real MTU so large-packet
// TLS handshakes survive a reduced host path MTU (cloud overlay networks such as GCP's
// 1460 default, VPN/WireGuard, nested virtualisation). Without this, the outer container
// and inner dockerd bridge keep Docker's 1500 default; when the host path MTU is smaller
// and PMTUD is black-holed, large packets are dropped and downloads like actions/setup-go
// fail with "Client network socket disconnected before secure TLS connection was
// established" while the host downloads fine. Linux-only (container mode is Linux-only).
func DetectHostEgressMTU(h *host.Host) int {
	if h == nil || h.OS != "linux" {
		return 0
	}
	// Resolve the egress interface (route to a public IP, then default route), then read
	// its MTU from sysfs. Failures yield empty output → 0 (caller keeps Docker's default).
	out, err := h.Run(`iface=$(ip -o route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="dev") {print $(i+1); exit}}')
[ -n "$iface" ] || iface=$(ip -o route show default 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="dev") {print $(i+1); exit}}')
[ -n "$iface" ] || exit 0
cat "/sys/class/net/$iface/mtu" 2>/dev/null`)
	if err != nil {
		return 0
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil || n < 576 || n > 9000 {
		return 0
	}
	return n
}

// dockerCreateEnvLineIf returns the indented `  -e NAME='value' \\\n` continuation
// line for a `docker create` command when emit is true, else "". Centralises the
// optional-int-env-var pattern shared by the MTU / dockerd start-timeout /
// bootstrap-retry helpers below; callers pass their own validity predicate so a
// narrow range (MTU's [576, 1500)) and a positivity check (timeout / retries) can
// share the formatting.
func dockerCreateEnvLineIf(name string, value int, emit bool) string {
	if !emit {
		return ""
	}
	return "  -e " + name + "=" + hostshell.PosixSingleQuote(strconv.Itoa(value)) + " \\\n"
}

// mtuDockerCreateArg returns the `-e GH_SR_HOST_MTU=<n>` line for the `docker create`
// command when mtu is a sub-1500 value worth pinning, or "" otherwise. The MTU is only
// ever lowered: 1500 is Docker's default (no-op) and values outside [576, 1500) are
// ignored.
func mtuDockerCreateArg(mtu int) string {
	return dockerCreateEnvLineIf("GH_SR_HOST_MTU", mtu, mtu >= 576 && mtu < 1500)
}

func dockerdStartTimeoutDockerCreateArg(seconds int) string {
	return dockerCreateEnvLineIf("GH_SR_DOCKERD_START_TIMEOUT", seconds, seconds > 0)
}

func bootstrapMaxRetriesDockerCreateArg(maxRetries int) string {
	return dockerCreateEnvLineIf("GH_SR_BOOTSTRAP_MAX_RETRIES", maxRetries, maxRetries > 0)
}

// containerRestartPolicy returns the Docker --restart policy used for every
// container-mode runner instance.
//
// Why "unless-stopped" (and not "on-failure:N" or "always"):
//
//   - "on-failure:N" (the historical choice) only restarts on a NON-ZERO exit
//     code. Clean exits — SIGTERM handled by the entrypoint's _shutdown (exit
//     0), `docker stop` after a graceful stop, the actions runner .NET process
//     terminating normally, the Docker daemon itself restarting, the host
//     rebooting — all leave the container permanently stopped. That is exactly
//     the "agentic runner just stopped, not restart" failure mode we saw on
//     x1, where every `docker stop` (e.g. a host reboot, a `gh sr down`) took
//     the runner offline and it never came back.
//
//   - "always" would also restart after an explicit `docker stop` / `gh sr
//     down`, which is surprising for operators and breaks the "down means
//     down" contract that `gh sr up` / `gh sr down` rely on.
//
//   - "unless-stopped" restarts on any non-explicit exit (crash, OOM, daemon
//     restart, host reboot) but honours `docker stop`. Native-mode runners use
//     `Restart=always` in their systemd unit (internal/autostart/generate.go),
//     so this also brings container mode in line with the native behaviour for
//     crashes and host reboots while keeping a clean down/up story.
//
// Persistent bootstrap failures (inner dockerd cannot start) are NOT handled
// here: the entrypoint caps consecutive dockerd-start failures with the
// `bootstrap-failed` marker and `exec sleep infinity` (entrypoint.sh §2b), so
// the container parks itself on persistent host-side problems instead of
// looping forever. That guard is what made the old "on-failure:N" safe to
// begin with, and it stays in place — the new policy just stops suppressing
// recovery on every other exit path.
func containerRestartPolicy() string {
	return "unless-stopped"
}

// resolveContainerMTU returns the MTU to pin for a new runner container: the explicit
// config override when set, otherwise the auto-detected host egress MTU.
func (m *Manager) resolveContainerMTU(h *host.Host) int {
	if mtu := m.containerMTU(); mtu > 0 {
		return mtu
	}
	return DetectHostEgressMTU(h)
}

// positiveIntOrDefault returns v when v > 0, else def. Centralizes the
// "use the configured value when positive, else the hard-coded default"
// rule shared by the container-timeout / -retry / -stagger accessors below.
// Using a single helper avoids drift in the positivity check (e.g. the
// previous `>= 0 && != 0` form on containerStartStaggerSeconds that was
// logically equivalent to `> 0` but inconsistent with the other accessors).
func positiveIntOrDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// nilPositiveInt returns def when m is nil, otherwise
// positiveIntOrDefault(get(), def). It centralizes the nil-safe +
// positivity-safe accessor envelope used by the container-timeout /
// -retry / -stagger accessors below. The closure-based get() is needed
// because Go evaluates method arguments eagerly: passing m.Field as a
// plain argument would dereference the nil receiver before this helper
// could run its nil check. (See issue #312.)
func (m *Manager) nilPositiveInt(get func() int, def int) int {
	if m == nil {
		return def
	}
	return positiveIntOrDefault(get(), def)
}

func (m *Manager) containerDockerdStartTimeout() int {
	return m.nilPositiveInt(func() int { return m.ContainerDockerdStartTimeout }, 90)
}

func (m *Manager) containerBootstrapMaxRetries() int {
	return m.nilPositiveInt(func() int { return m.ContainerBootstrapMaxRetries }, 5)
}

func (m *Manager) containerStartStaggerSeconds() int {
	return m.nilPositiveInt(func() int { return m.ContainerStartStaggerSeconds }, 3)
}

// createContainerInstance creates (but does not start) a single runner container
// instance. The image must already exist. It is the per-instance unit used by both
// setupContainer and ContainerEnvironment.Provision.
func (m *Manager) createContainerInstance(h *host.Host, rc config.RunnerConfig, instanceIndex int, instanceName, imageTag string) error {
	fmt.Fprintf(m.out(), "  %s: creating runner container...\n", instanceName)

	regToken, err := m.GitHub.GetRegistrationTokenScoped(rc.Scope(), rc.ScopeTarget())
	if err != nil {
		return fmt.Errorf("getting registration token for %s: %w", instanceName, err)
	}

	stateDir, err := resolveAbsoluteRunnerDir(h, instanceName)
	if err != nil {
		return fmt.Errorf("resolving state dir for %s: %w", instanceName, err)
	}
	labels := rc.EffectiveLabelsForInstance(h.OS, h.Arch, instanceIndex)

	runURL := rc.GitHubRegistrationURL()

	group := rc.Group
	if group == "" {
		group = "Default"
	}

	ephemeral := ""
	if rc.Ephemeral {
		ephemeral = "true"
	}

	// Pin the inner/outer Docker MTU to the host egress MTU when it is below 1500 so
	// large-packet TLS handshakes survive a reduced host path MTU (see DetectHostEgressMTU
	// and entrypoint.sh §2a). Empty (no env) on standard 1500 networks — a no-op.
	mtuEnv := mtuDockerCreateArg(m.resolveContainerMTU(h))
	dockerdTimeoutEnv := dockerdStartTimeoutDockerCreateArg(m.containerDockerdStartTimeout())
	bootstrapRetriesEnv := bootstrapMaxRetriesDockerCreateArg(m.containerBootstrapMaxRetries())
	restartPolicy := containerRestartPolicy()

	// Build the `docker create` command. We use `--restart unless-stopped` so any
	// non-explicit exit (crash, OOM, inner process shutdown, Docker daemon
	// restart, host reboot) brings the container back automatically — but
	// `docker stop` / `gh sr down` still keep it down. The entrypoint caps
	// consecutive inner-dockerd bootstrap failures with the `bootstrap-failed`
	// marker + `exec sleep infinity`, so persistent host-side problems park
	// the container instead of looping forever. `--privileged` is required
	// for DinD (inner dockerd needs full capabilities). Large `/dev/shm`
	// avoids Chromium/Selenium flakiness.
	cmd := fmt.Sprintf(`
mkdir -p %s
docker create \
  --name %s \
  --privileged \
  --shm-size=2g \
  --restart %s \
  -v %s:/runner-state \
  -e GH_SR_RUNNER_NAME=%s \
  -e GH_SR_RUNNER_TOKEN=%s \
  -e GH_SR_RUNNER_URL=%s \
  -e GH_SR_RUNNER_LABELS=%s \
  -e GH_SR_RUNNER_GROUP=%s \
  -e GH_SR_RUNNER_EPHEMERAL=%s \
%s%s%s  %s`,
		hostshell.PosixSingleQuote(stateDir),
		hostshell.PosixSingleQuote(containerName(instanceName)),
		hostshell.PosixSingleQuote(restartPolicy),
		hostshell.PosixSingleQuote(stateDir),
		hostshell.PosixSingleQuote(instanceName),
		hostshell.PosixSingleQuote(regToken),
		hostshell.PosixSingleQuote(runURL),
		hostshell.PosixSingleQuote(strings.Join(labels, ",")),
		hostshell.PosixSingleQuote(group),
		hostshell.PosixSingleQuote(ephemeral),
		mtuEnv,
		dockerdTimeoutEnv,
		bootstrapRetriesEnv,
		hostshell.PosixSingleQuote(imageTag),
	)

	if _, err := h.Run(cmd); err != nil {
		return fmt.Errorf("creating container %s: %w", containerName(instanceName), err)
	}
	return nil
}

// startContainer starts an existing runner container (docker start).
//
// All three pre-flight operations are chained into a single h.Run so each
// instance costs one SSH round-trip instead of three (saves 2 round-trips
// per instance per `gh sr up`):
//
//  1. rm -f of the bootstrap-failed and dockerd-start-failures state-dir
//     markers. Uses the unresolved `"$HOME/.gh-sr/runners/<inst>/..."` form
//     (double-quoted so the shell expands $HOME) — saves the separate
//     `echo $HOME` resolve that resolveAbsoluteRunnerDir would otherwise
//     make. Mirrors `containerLocalStatusOneShot` and disk.go's
//     `"$HOME/.gh-sr/runners/..."` listing pattern.
//  2. `docker update --restart=<policy>` (best-effort — failures here should
//     not block starting the container, hence the trailing `|| true`).
//  3. `docker start <name>` — the only operation whose failure surfaces.
//
// Mirrors the same per-instance chain pattern used by removeContainer
// (PR #264) and rebuildContainerImage (PR #255).
func (m *Manager) startContainer(h *host.Host, instanceName string) error {
	name := containerName(instanceName)
	policy := containerRestartPolicy()
	// Keep $HOME outside single quotes so the remote shell expands it.
	// PosixSingleQuote("$HOME/...") would freeze the literal "$HOME" path
	// (same pitfall as a single-quoted assignment). Double-quoted form
	// matches disk.go / orphans listing.
	script := fmt.Sprintf(
		`rm -f "$HOME/.gh-sr/runners/%s/%s" "$HOME/.gh-sr/runners/%s/%s"; `+
			"docker update --restart=%s %s 2>/dev/null || true; "+
			"docker start %s",
		instanceName, bootstrapFailedMarker,
		instanceName, dockerdStartFailuresFile,
		hostshell.PosixSingleQuote(policy),
		hostshell.PosixSingleQuote(name),
		name,
	)
	if _, err := h.Run(script); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}
	return nil
}

// containerLogsContainStaleRegistration reports whether the runner inside an
// existing container has hit GitHub's "registration deleted" failure. With
// --restart unless-stopped Docker can otherwise keep restarting the same stale
// local .runner/.credentials files forever.
func containerLogsContainStaleRegistration(h *host.Host, instanceName string) bool {
	cname := containerName(instanceName)
	cmd := fmt.Sprintf(
		"docker logs --tail 200 %s 2>&1 | grep -F %s >/dev/null",
		QuoteContainerName(cname),
		hostshell.PosixSingleQuote(staleRegistrationMsg),
	)
	_, err := h.Run(cmd)
	return err == nil
}

func containerInspectImage(h *host.Host, instanceName string) (string, error) {
	cname := containerName(instanceName)
	out, err := h.Run(fmt.Sprintf("docker inspect --format '{{.Config.Image}}' %s", QuoteContainerName(cname)))
	if err != nil {
		return "", err
	}
	image := strings.TrimSpace(out)
	if image == "" {
		return "", fmt.Errorf("container %s has empty image", cname)
	}
	return image, nil
}

func (m *Manager) recoverContainerStaleRegistration(h *host.Host, rc config.RunnerConfig, instanceIndex int, instanceName string) error {
	imageTag, err := containerInspectImage(h, instanceName)
	if err != nil {
		return fmt.Errorf("inspecting stale container image: %w", err)
	}
	stateDir, err := resolveAbsoluteRunnerDir(h, instanceName)
	if err != nil {
		return fmt.Errorf("resolving state dir for %s: %w", instanceName, err)
	}
	cname := containerName(instanceName)
	cleanup := fmt.Sprintf(
		"docker rm -f %s 2>/dev/null || true; rm -f %s %s %s",
		QuoteContainerName(cname),
		hostshell.PosixSingleQuote(stateDir+"/.runner"),
		hostshell.PosixSingleQuote(stateDir+"/.credentials"),
		hostshell.PosixSingleQuote(stateDir+"/.credentials_rsaparams"),
	)
	if _, err := h.Run(cleanup); err != nil {
		return fmt.Errorf("removing stale container state: %w", err)
	}
	if err := m.createContainerInstance(h, rc, instanceIndex, instanceName, imageTag); err != nil {
		return err
	}
	return m.startContainer(h, instanceName)
}

// ContainerBootstrapFailed reports whether the runner instance gave up after repeated
// inner-dockerd bootstrap failures (bootstrap-failed marker in the state bind-mount).
func ContainerBootstrapFailed(h *host.Host, instanceName string) bool {
	stateDir := resolveStateDirOrFallback(h, instanceName)
	ok, _ := hostshell.RemoteFileExists(h, stateDir+"/"+bootstrapFailedMarker)
	return ok
}

// stopContainer stops a running runner container (docker stop).
func (m *Manager) stopContainer(h *host.Host, instanceName string) error {
	name := containerName(instanceName)
	if _, err := h.Run(fmt.Sprintf("docker stop %s 2>/dev/null || true", name)); err != nil {
		return fmt.Errorf("stopping container %s: %w", name, err)
	}
	return nil
}

// removeContainer stops and removes a runner container plus its state directory.
//
// All three teardown operations are chained into a single h.Run so each
// instance costs one SSH round-trip on Linux instead of three (saves 2
// round-trips per instance per `gh sr down` / `Remove`):
//
//  1. `docker stop <name>` — stop the running container.
//  2. `docker rm -f <name>` — remove the container (best-effort; trailing
//     `|| true` so an already-removed container does not abort the chain).
//  3. `rm -rf "$HOME/.gh-sr/runners/<inst>"` — remove the host state dir.
//     Uses the unresolved `$HOME/...` form (double-quoted so the shell
//     expands `$HOME`) so we skip the previous `echo $HOME` resolve that
//     `resolveStateDirOrFallback` made. Same `$HOME`-form pattern as
//     startContainer (PR #342) and containerLocalStatusOneShot.
//
// All three are best-effort (`|| true` on the rm legs): host-level failures
// (SSH error) still surface via the wrapping h.Run error return. The
// pre-fold state-dir failure surfaced as a `warning:` line on stderr;
// folding the rm into the chained shell makes that warning silent — a
// pragmatic trade-off since `rm -rf` on a user-owned path essentially
// never fails, and the chain still returns the SSH-level error if the
// transport itself breaks.
func (m *Manager) removeContainer(h *host.Host, rc config.RunnerConfig, instanceName string) error {
	cName := containerName(instanceName)

	// Best-effort deregister from GitHub first.
	removeTok, err := m.GitHub.GetRemovalTokenScoped(rc.Scope(), rc.ScopeTarget())
	if err == nil {
		// Run inside the container if it's still alive; ignore errors.
		inner := "su - runner -c " + hostshell.PosixSingleQuote("cd /home/runner/actions-runner && ./config.sh remove --token "+removeTok)
		_, _ = h.Run(DockerExecCommand(cName, inner+" 2>/dev/null || true"))
	}

	// Single SSH round-trip: docker stop + docker rm -f + state-dir rm -rf.
	// All three are best-effort so the chain returns 0 even when the
	// container or state dir does not exist. Matches the win-class of
	// startContainer (PR #342) and rebuildContainerImage (PR #255).
	script := fmt.Sprintf(
		"docker stop %s 2>/dev/null; docker rm -f %s 2>/dev/null || true; "+
			`rm -rf "$HOME/.gh-sr/runners/%s" 2>/dev/null || true`,
		cName, cName, instanceName,
	)
	if _, err := h.Run(script); err != nil {
		return fmt.Errorf("removing container %s: %w", cName, err)
	}

	return nil
}

// parseContainerStatusInspectOutput parses one line of the form
// status|configImage|digest|imageRevision (from containerLocalStatusOneShot).
// Missing fields default to "".
func parseContainerStatusInspectOutput(out string) (local, image, imageRev string) {
	line := strings.TrimSpace(out)
	// status|configImage|digest|imageRev — strings.Cut chain is 0-alloc vs
	// strings.Split's 4-element slice + padding loop. The third Cut discards
	// the digest field (we only carry status, image, imageRev through).
	status, rest, _ := strings.Cut(line, "|")
	image, rest, _ = strings.Cut(rest, "|")
	_, imageRev, _ = strings.Cut(rest, "|")
	status = strings.TrimSpace(status)
	image = strings.TrimSpace(image)
	imageRev = strings.TrimSpace(imageRev)
	switch status {
	case "running":
		local = "running"
	case "restarting":
		local = "restarting"
	case "not installed":
		local = "not installed"
	case "failed":
		// Surface "failed" verbatim — the bootstrap-failed marker was set
		// inside the combined containerLocalStatusOneShot script.
		local = "failed"
	default:
		// exited, created, paused, etc.
		local = "stopped"
	}
	if local == "not installed" {
		return local, "", ""
	}
	return local, image, imageRev
}

// containerLocalStatusOneShot returns local status, Config.Image, and the
// gh-sr.image-revision label on the container's image in a single SSH
// round-trip. The script folds two checks that the per-tick TUI status path
// used to issue as separate `h.Run` calls:
//
//  1. The bootstrap-failed marker (ContainerBootstrapFailed).
//  2. The docker inspect of the container.
//
// The script assigns `sd="$HOME/.gh-sr/runners/<instance>"` (double-quoted so
// the remote shell expands $HOME) and does not need the separate `echo $HOME`
// resolveAbsoluteRunnerDir previously did on Linux. The only new status value
// beyond "not installed" / parsed docker state is "failed" (produced when the
// bootstrap-failed marker exists).
func (m *Manager) containerLocalStatusOneShot(h *host.Host, instanceName string) (string, string, string) {
	cid := hostshell.PosixSingleQuote(containerName(instanceName))
	// Assign sd with double quotes so $HOME expands on the remote shell.
	// PosixSingleQuote("$HOME/...") would freeze a literal "$HOME/..." path.
	script := fmt.Sprintf(
		"sd=\"$HOME/.gh-sr/runners/%s\"\n"+
			"cid=%s\n"+
			"if [ -f \"$sd/bootstrap-failed\" ]; then override=failed; else override=; fi\n"+
			"line=$(docker inspect --format '{{.State.Status}}|{{.Config.Image}}|{{.Image}}' \"$cid\" 2>/dev/null) || line=\"\"\n"+
			"if [ -z \"$line\" ]; then\n"+
			"  if [ -n \"$override\" ]; then echo \"failed|||\"; else echo \"not installed|||\"; fi\n"+
			"else\n"+
			"  digest=${line##*|}\n"+
			"  rev=\"\"\n"+
			"  if [ -n \"$digest\" ]; then\n"+
			"    rev=$(docker image inspect \"$digest\" --format '{{index .Config.Labels \"gh-sr.image-revision\"}}' 2>/dev/null || true)\n"+
			"  fi\n"+
			"  if [ -n \"$override\" ]; then printf '%%s|%%s|%%s\\n' \"$override\" \"${line#*|}\" \"$rev\"\n"+
			"  else printf '%%s|%%s\\n' \"$line\" \"$rev\"\n"+
			"  fi\n"+
			"fi\n",
		instanceName,
		cid,
	)
	out, err := h.Run(script)
	if err != nil {
		return "not installed", "", ""
	}
	return parseContainerStatusInspectOutput(out)
}

// containerLocalStatusImageAndRevision returns local status, Config.Image, and the
// gh-sr.image-revision label on the container's image (one SSH round-trip).
// The bootstrap-failed marker check and the docker inspect are folded into a
// single shell script by containerLocalStatusOneShot — previously this issued
// 2-3 SSH calls (echo $HOME + marker test + docker inspect) on every
// per-instance status refresh, which is the per-tick hot path for the TUI
// dashboard.
func (m *Manager) containerLocalStatusImageAndRevision(h *host.Host, instanceName string) (string, string, string) {
	return m.containerLocalStatusOneShot(h, instanceName)
}

// logsContainer returns recent log lines from a runner container.
func (m *Manager) logsContainer(h *host.Host, instanceName string) (string, error) {
	name := containerName(instanceName)
	out, err := h.Run(fmt.Sprintf("docker logs --tail 100 %s 2>&1 || echo 'no logs found'", name))
	if err != nil {
		return "", fmt.Errorf("fetching logs for container %s: %w", name, err)
	}
	return out, nil
}

// rebuildContainerImage tears down all containers for a runner group, removes
// the old agentic-runner image, rebuilds it from the embedded sources, recreates
// the containers, and starts them. The runner state directories (including the
// .runner registration file) are intentionally preserved so the runners do not
// re-register with GitHub on next start.
func (m *Manager) rebuildContainerImage(h *host.Host, rc config.RunnerConfig) error {
	if h.OS != "linux" {
		return fmt.Errorf("runner_mode: container is only supported on Linux hosts")
	}

	// Stop and remove containers (keep state dirs so .runner persists).
	// Chain `docker stop` and `docker rm -f` in one shell so each instance costs
	// a single SSH round-trip instead of two (saves N round-trips for an
	// N-instance rebuild).
	for _, name := range rc.InstanceNames() {
		cName := containerName(name)
		fmt.Fprintf(m.out(), "  %s: stopping container...\n", name)
		fmt.Fprintf(m.out(), "  %s: removing container...\n", name)
		_, _ = h.Run(fmt.Sprintf(
			"docker stop %s 2>/dev/null; docker rm -f %s 2>/dev/null || true",
			cName, cName,
		))
	}

	// Resolve runner version, host arch, and image tag.
	version, arch, imageTag, err := m.resolveRunnerImageInputs(h)
	if err != nil {
		return err
	}

	// Remove only this tag so we force a fresh build. Do not `docker rmi` every
	// gh-sr/agentic-runner image on the host: other runners' containers may still
	// reference those digests; removing them breaks `docker image inspect` and
	// BUILD shows "?" until those runners are rebuilt too.
	fmt.Fprintf(m.out(), "  %s: removing image %s (if present)...\n", rc.Name, imageTag)
	_, _ = h.Run(fmt.Sprintf("docker rmi -f %s 2>/dev/null || true", hostshell.PosixSingleQuote(imageTag)))

	fmt.Fprintf(m.out(), "  %s: building container runner image %s (this may take several minutes)...\n", rc.Name, imageTag)
	if err := buildAgenticRunnerImage(h, imageTag, version, arch, m.GhSrVersion, m.containerImageExtraApt()); err != nil {
		return fmt.Errorf("building container runner image: %w", err)
	}
	fmt.Fprintf(m.out(), "  %s: image built: %s\n", rc.Name, imageTag)

	// Recreate and start each container. Because state dirs still exist on the
	// host (bind-mounted at /runner-state), the entrypoint will find .runner and
	// skip config.sh, so no new registration token is consumed on start.
	if err := m.setupContainer(h, rc); err != nil {
		return err
	}
	for _, name := range rc.InstanceNames() {
		fmt.Fprintf(m.out(), "  %s: starting container...\n", name)
		if err := m.startContainer(h, name); err != nil {
			return err
		}
	}
	return nil
}

// needsSetupContainer reports whether any instance container is missing.
//
// One SSH round-trip via containersPresentOneShot; previously issued up to N
// round-trips (one per instance) for an early-exit decision.
func (m *Manager) needsSetupContainer(h *host.Host, rc config.RunnerConfig) bool {
	instanceNames := rc.InstanceNames()
	if len(instanceNames) == 0 {
		return false
	}
	present, err := containersPresentOneShot(h, instanceNames)
	if err != nil {
		// Mirror containerRunnerPresent's "absent on error" semantics so a
		// transient probe failure still surfaces a "setup needed" result
		// and the caller treats the runner as missing rather than wiping
		// state. Matches the `ok, _ := RemoteBoolCheck(...)` drop in
		// containerRunnerPresent and the `_ = ` propagation in
		// runner_orchestrator_dispatch_test.
		return true
	}
	for _, name := range instanceNames {
		if !present[name] {
			return true
		}
	}
	return false
}

// containerImageExists checks whether a Docker image with the given tag exists on the host.
func containerImageExists(h *host.Host, imageTag string) (bool, error) {
	// RemoteBoolCheck appends `&& echo yes || echo no`; on a host-execution
	// error we mirror the previous semantics and report "image absent" rather
	// than surfacing the error (the caller treats a missing image as a build
	// trigger, not a hard failure).
	ok, _ := hostshell.RemoteBoolCheck(h,
		"docker image inspect "+hostshell.PosixSingleQuote(imageTag)+" --format='{{.Id}}' 2>/dev/null | grep -q .")
	return ok, nil
}

// resolveRunnerImageInputs resolves the runner version, host arch, and image tag
// for the container runner image. It collapses the version→arch→tag preamble that
// was previously duplicated verbatim across setupContainer / rebuildContainerImage
// / ContainerEnvironment.Provision (closes the triplication flagged by #228).
// The GitHubClient caches the version response so repeat calls are cheap.
func (m *Manager) resolveRunnerImageInputs(h *host.Host) (version, arch, imageTag string, err error) {
	version, err = m.GitHub.GetLatestRunnerVersion()
	if err != nil {
		return "", "", "", fmt.Errorf("resolving runner version: %w", err)
	}
	arch = archForGitHub(h.Arch)
	imageTag = ContainerRunnerImageTag(version, m.containerImageExtraApt())
	return version, arch, imageTag, nil
}

// buildRunnerImageIfMissing checks whether the container runner image with the
// given tag already exists on h, and builds it via buildAgenticRunnerImage if not.
// Returns built=true when the image was freshly built, built=false when it was
// already present. Used by setupContainer and ContainerEnvironment.Provision.
// rebuildContainerImage intentionally skips the existence check and calls
// buildAgenticRunnerImage directly so the rebuild path always produces a fresh
// image. Error wrapping matches the historical call-site messages
// ("checking image: %w" / "building container runner image: %w") so user-visible
// error output is unchanged.
//
// onBuild, when non-nil, is invoked immediately before buildAgenticRunnerImage
// runs, so the caller can emit its own progress line (e.g. setupContainer's
// "building container runner image (this may take several minutes)..." heads-up
// for a multi-minute build). Provision passes nil to stay silent, matching its
// historical behavior.
func (m *Manager) buildRunnerImageIfMissing(h *host.Host, imageTag, version, arch string, onBuild func()) (built bool, err error) {
	exists, err := containerImageExists(h, imageTag)
	if err != nil {
		return false, fmt.Errorf("checking image: %w", err)
	}
	if exists {
		return false, nil
	}
	if onBuild != nil {
		onBuild()
	}
	if err := buildAgenticRunnerImage(h, imageTag, version, arch, m.GhSrVersion, m.containerImageExtraApt()); err != nil {
		return false, fmt.Errorf("building container runner image: %w", err)
	}
	return true, nil
}

// embedTextForRemoteWrite normalizes CRLF to LF and escapes heredoc delimiters before
// writing embedded files to a remote build context. CRLF in apt manifests breaks the
// Dockerfile grep|xargs|apt-get pipeline (package names gain a trailing \r).
func embedTextForRemoteWrite(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "GHSR_EOF", "GHSR_E0F")
}

// buildAgenticRunnerImage uploads the embedded Dockerfile+entrypoint to the host
// and builds the image via `docker build`.
func buildAgenticRunnerImage(h *host.Host, imageTag, runnerVersion, runnerArch, ghSrVersion string, extraApt []string) error {
	buildDir := "/tmp/gh-sr-agentic-runner-build"

	// Write the 8 build-context files via the shared helpers. writeRemoteHeredocFile
	// creates the parent directory on the host, and writeRemoteHeredocExecutable
	// additionally chmods the file +x — so each site collapses to a single call.
	if err := writeRemoteHeredocFile(h, buildDir+"/Dockerfile", agenticRunnerDockerfile); err != nil {
		return err
	}
	if err := writeRemoteHeredocFile(h, buildDir+"/apt-packages-core.txt", agenticRunnerAptPackagesCore); err != nil {
		return err
	}

	// apt-packages-extra.txt is empty when no extras are configured: truncate instead
	// of writing an empty heredoc (avoids a stray newline and a confusing file).
	extraSorted := containerRunnerImageExtraSorted(extraApt)
	extraPath := buildDir + "/apt-packages-extra.txt"
	if len(extraSorted) == 0 {
		if _, err := h.Run(formatEmptyRemoteFile(extraPath)); err != nil {
			return fmt.Errorf("writing %s: %w", extraPath, err)
		}
	} else {
		if err := writeRemoteHeredocFile(h, extraPath, joinExtraPackages(extraSorted)); err != nil {
			return err
		}
	}

	if err := writeRemoteHeredocExecutable(h, buildDir+"/entrypoint.sh", agenticRunnerEntrypoint); err != nil {
		return err
	}
	if err := writeRemoteHeredocExecutable(h, buildDir+"/docker-wrapper.sh", agenticRunnerDockerWrapper); err != nil {
		return err
	}

	// Write baked inner-Docker network configs (Pillar 2: deterministic DNS, single dockerd start).
	for _, f := range []struct{ name, content string }{
		{"daemon.json", agenticRunnerDaemonJSON},
		{"dnsmasq-gh-sr.conf", agenticRunnerDnsmasqConf},
	} {
		if err := writeRemoteHeredocFile(h, buildDir+"/"+f.name, f.content); err != nil {
			return err
		}
	}

	// Write per-job reset hooks into the build context (Pillar 1). The helper mkdirs
	// the parent of every path, so the explicit `mkdir -p buildDir/hooks` is gone.
	for _, hk := range []struct{ name, content string }{
		{"job-started.sh", agenticRunnerJobStartedHook},
		{"job-completed.sh", agenticRunnerJobCompletedHook},
	} {
		if err := writeRemoteHeredocExecutable(h, buildDir+"/hooks/"+hk.name, hk.content); err != nil {
			return err
		}
	}

	// Build (stamp labels so gh sr status can compare layout to this binary).
	rev := ContainerImageLayoutRevision(ghSrVersion, extraApt)
	labelRev := hostshell.PosixSingleQuote(dockerLabelImageRevision + "=" + rev)
	labelCLI := hostshell.PosixSingleQuote(dockerLabelCLIVersion + "=" + ghSrVersion)
	buildCmd := fmt.Sprintf(
		"docker build --build-arg RUNNER_VERSION=%s --build-arg RUNNER_ARCH=%s --label %s --label %s -t %s %s",
		hostshell.PosixSingleQuote(runnerVersion),
		hostshell.PosixSingleQuote(runnerArch),
		labelRev,
		labelCLI,
		hostshell.PosixSingleQuote(imageTag),
		hostshell.PosixSingleQuote(buildDir),
	)
	if _, err := h.Run(buildCmd); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}

	// Clean up build context.
	_, _ = h.Run(fmt.Sprintf("rm -rf %s", buildDir))

	return nil
}
