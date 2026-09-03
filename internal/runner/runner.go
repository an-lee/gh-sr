package runner

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

type Manager struct {
	GitHub *GitHubClient
	// Out receives progress messages from runner operations. If nil, os.Stdout is used.
	Out io.Writer
	// GhSrVersion is the gh-sr CLI version (e.g. Makefile -X main.version). Used for
	// container image layout revision and Docker image labels.
	GhSrVersion string
	// ContainerImageExtraApt is global extra apt packages for the gh-sr/agentic-runner
	// image (from runners.yml container_runner_image). Set by ops before container setup.
	ContainerImageExtraApt []string
	// ContainerImageBaseImage is the base (fork) runner image the gh-sr/agentic-runner
	// image derives FROM (from runners.yml container_runner_image.base_image). Empty
	// means the DefaultForkRunnerImage constant; see containerImageBaseImage().
	ContainerImageBaseImage string
	// CacheURL is the local Actions cache server base URL (trailing slash) injected
	// into container runners as CUSTOM_ACTIONS_RESULTS_URL. Empty = use GitHub's
	// cache service. Set by ops from the cache section of runners.yml.
	CacheURL string
	// ContainerMTU, when > 0, forces the inner/outer Docker MTU for container-mode runners
	// (overriding host egress MTU auto-detection). From runners.yml
	// container_runner_image.mtu; set by ops before container setup. 0 = auto-detect.
	ContainerMTU int
	// ContainerDockerdStartTimeout is seconds to wait for inner dockerd bootstrap.
	ContainerDockerdStartTimeout int
	// ContainerBootstrapMaxRetries is consecutive dockerd bootstrap failures before giving up.
	ContainerBootstrapMaxRetries int
	// ContainerStartStaggerSeconds is delay between starting container instances on one host.
	ContainerStartStaggerSeconds int
}

func (m *Manager) out() io.Writer {
	if m != nil && m.Out != nil {
		return m.Out
	}
	return os.Stdout
}

func NewManager(pat string) *Manager {
	return &Manager{
		GitHub: NewGitHubClient(pat),
	}
}

type RunnerStatus struct {
	Instance string
	Host     string
	Repo     string // DisplayTarget: owner/repo or org:<name> [group=<name>]
	Labels   string
	Mode     string
	// ContainerImage is the Docker image ref from the container config (runner_mode: container only).
	// Empty for native runners or when the container does not exist.
	ContainerImage string
	// ContainerImageRevision is the gh-sr.image-revision label on the running image (empty if missing).
	ContainerImageRevision string
	// ContainerImageBuild is a short match hint: "-", "?", "ok (hexprefix)", "stale (hexprefix)".
	ContainerImageBuild string
	Local               string // "running", "stopped", "not installed", "service error", "restarting", "failed"
	Remote              string // from GitHub API: "online", "offline", ""
	Busy                bool
}

func (m *Manager) Setup(h *host.Host, rc config.RunnerConfig) error {
	if rc.IsContainerMode() {
		return m.setupContainer(h, rc)
	}
	return m.setupNative(h, rc)
}

func (m *Manager) containerImageExtraApt() []string {
	if m == nil {
		return nil
	}
	return m.ContainerImageExtraApt
}

// containerImageBaseImage returns the configured fork base image, defaulting to
// DefaultForkRunnerImage when unset so Manager works in zero-config contexts
// (tests, native-only hosts) without ops wiring.
func (m *Manager) containerImageBaseImage() string {
	if m == nil || m.ContainerImageBaseImage == "" {
		return DefaultForkRunnerImage
	}
	return m.ContainerImageBaseImage
}

// containerMTU returns the configured MTU override for container runners (0 = auto-detect).
func (m *Manager) containerMTU() int {
	if m == nil {
		return 0
	}
	return m.ContainerMTU
}

// NeedsSetup checks whether a runner requires setup before it can start.
// Checks if any instance is missing the .runner config file (native) or container (container mode).
func (m *Manager) NeedsSetup(h *host.Host, rc config.RunnerConfig) bool {
	if rc.IsContainerMode() {
		return m.needsSetupContainer(h, rc)
	}
	for _, name := range rc.InstanceNames() {
		ok, _ := NativeRunnerConfigPresent(h, name)
		if !ok {
			return true
		}
	}
	return false
}

// EnsureSetup runs setup only if the runner is not already installed.
func (m *Manager) EnsureSetup(h *host.Host, rc config.RunnerConfig) error {
	if !m.NeedsSetup(h, rc) {
		return nil
	}
	fmt.Fprintf(m.out(), "  %s: not yet set up, running setup...\n", rc.Name)
	return m.Setup(h, rc)
}

// RebuildImage rebuilds the container runner Docker image for container-mode
// runners (agentic or not), recreates each container instance (preserving runner
// state), and starts them. Native-mode runners are a no-op (return nil).
func (m *Manager) RebuildImage(h *host.Host, rc config.RunnerConfig) error {
	if !rc.IsContainerMode() {
		fmt.Fprintf(m.out(), "  %s: skipping rebuild (not runner_mode: container)\n", rc.Name)
		return nil
	}
	return m.rebuildContainerImage(h, rc)
}

func (m *Manager) startAutostartWithDarwinFallback(h *host.Host, rc config.RunnerConfig, name string) error {
	err := autostart.Start(h, name)
	if err != nil && h.OS == "darwin" {
		fmt.Fprintf(m.out(), "  %s: warning: launchd start failed (%v); falling back to direct start (log in at the Mac GUI for launchd autostart on boot)\n", name, err)
		return m.startNative(h, rc, name)
	}
	return err
}

func (m *Manager) Start(h *host.Host, rc config.RunnerConfig) error {
	if rc.IsContainerMode() {
		stagger := m.containerStartStaggerSeconds()
		for i, name := range rc.InstanceNames() {
			if i > 0 && stagger > 0 {
				time.Sleep(time.Duration(i*stagger) * time.Second)
			}
			env := m.NewContainerEnvironment(h, rc, i, name)
			if err := env.Start(); err != nil {
				return err
			}
			fmt.Fprintf(m.out(), "  %s: container started\n", name)
			if containerLogsContainStaleRegistration(h, name) {
				fmt.Fprintf(m.out(), "  %s: registration expired on GitHub, re-creating container...\n", name)
				if err := m.recoverContainerStaleRegistration(h, rc, i, name); err != nil {
					return fmt.Errorf("recovering stale registration for %s: %w", name, err)
				}
				fmt.Fprintf(m.out(), "  %s: container re-created with fresh registration\n", name)
			}
			// Health-gate: confirm the slot is actually ready (inner dockerd up,
			// runner registered) before treating it as available. Non-fatal so a
			// slow first boot does not fail `gh sr up`; doctor surfaces persistent issues.
			if err := env.AwaitHealthy(defaultContainerHealthTimeout); err != nil {
				fmt.Fprintf(m.out(), "  %s: warning: not ready yet: %v\n", name, err)
			} else {
				fmt.Fprintf(m.out(), "  %s: ready (inner dockerd up, runner registered)\n", name)
			}
		}
		return nil
	}

	for _, name := range rc.InstanceNames() {
		// One SSH round-trip per instance that answers both "is svc.sh deployed?"
		// and "which autostart kind is installed?". Replaces the previous
		// svcShPresent + autostart.Detect pair (2 SSH per instance).
		hasSvc, kind, derr := linuxSvcAndAutostartProbe(h, name)
		if derr != nil {
			return fmt.Errorf("starting %s: %w", name, derr)
		}

		// Prefer svc.sh for Linux if it's deployed
		if hasSvc {
			dir := h.RunnerDir(name)
			// Install the systemd unit first if not already installed (.service file is the marker).
			cmd := fmt.Sprintf("cd %s && %s\nif [ ! -f .service ]; then $SUDO ./svc.sh install; fi\n$SUDO ./svc.sh start", dir, strings.TrimSpace(sudoPrelude()))
			out, err := h.Run(cmd)
			if err != nil {
				return fmt.Errorf("starting %s via svc.sh: %w", name, err)
			}
			fmt.Fprintf(m.out(), "  %s: %s\n", name, strings.TrimSpace(out))
			continue
		}

		var err error
		if kind != autostart.KindNone {
			err = m.startAutostartWithDarwinFallback(h, rc, name)
		} else {
			// Auto-install autostart for non-ephemeral runners so the runner
			// auto-restarts on crash and starts on boot.
			if !rc.Ephemeral {
				fmt.Fprintf(m.out(), "  %s: installing autostart for always-on...\n", name)
				if ierr := autostart.Install(h, name, autostart.InstallOpts{}); ierr != nil {
					fmt.Fprintf(m.out(), "  %s: warning: failed to install autostart: %v\n", name, ierr)
					// Fall back to direct start; autostart install failure is non-fatal.
				}
				// Re-detect after install attempt.
				hasSvc, kind, _ = linuxSvcAndAutostartProbe(h, name)
			}
			if kind != autostart.KindNone {
				err = m.startAutostartWithDarwinFallback(h, rc, name)
			} else {
				err = m.startNative(h, rc, name)
			}
		}
		if err != nil {
			return fmt.Errorf("starting %s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) Stop(h *host.Host, rc config.RunnerConfig) error {
	if rc.IsContainerMode() {
		for _, name := range rc.InstanceNames() {
			if err := m.stopContainer(h, name); err != nil {
				return err
			}
			fmt.Fprintf(m.out(), "  %s: container stopped\n", name)
		}
		return nil
	}

	for _, name := range rc.InstanceNames() {
		// One SSH round-trip per instance that answers both "is svc.sh deployed?"
		// and "which autostart kind is installed?". Replaces the previous
		// svcShPresent + autostart.Detect pair (2 SSH per instance).
		hasSvc, kind, derr := linuxSvcAndAutostartProbe(h, name)
		if derr != nil {
			return fmt.Errorf("stopping %s: %w", name, derr)
		}

		// Prefer svc.sh for Linux if it's deployed
		if hasSvc {
			dir := h.RunnerDir(name)
			cmd := fmt.Sprintf("cd %s && %s\n$SUDO ./svc.sh stop", dir, strings.TrimSpace(sudoPrelude()))
			out, err := h.Run(cmd)
			if err != nil {
				return fmt.Errorf("stopping %s via svc.sh: %w", name, err)
			}
			fmt.Fprintf(m.out(), "  %s: %s\n", name, strings.TrimSpace(out))
			continue
		}

		var err error
		if kind != autostart.KindNone {
			err = autostart.Stop(h, name)
			// LaunchAgent may be installed but the runner was started directly
			// (e.g. macOS SSH with no GUI login). Stop that process too.
			if h.OS == "darwin" {
				if nerr := m.stopNative(h, name); nerr != nil && err == nil {
					err = nerr
				}
			}
		} else {
			err = m.stopNative(h, name)
		}
		if err != nil {
			return fmt.Errorf("stopping %s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) Remove(h *host.Host, rc config.RunnerConfig) error {
	for _, name := range rc.InstanceNames() {
		if rc.IsContainerMode() {
			if err := m.removeContainer(h, rc, name); err != nil {
				return fmt.Errorf("removing %s: %w", name, err)
			}
		} else {
			if err := m.removeNative(h, rc, name); err != nil {
				return fmt.Errorf("removing %s: %w", name, err)
			}
		}
	}
	return nil
}

func (m *Manager) Status(h *host.Host, rc config.RunnerConfig) ([]RunnerStatus, error) {
	// Hoist loop-invariant values: EffectiveRunnerMode, DisplayTarget, the
	// labels join, and the container-image layout revision all return the same
	// result for every instance of a given RunnerConfig (effectiveLabelsCore
	// ignores the instance index; ContainerImageLayoutRevision only depends on
	// m.GhSrVersion + m.containerImageBaseImage() + m.containerImageExtraApt(),
	// which are static during one Status() call). Computing them once outside the loop saves N-1
	// effectiveLabelsCore slice builds, N-1 strings.Join calls, and N-1
	// ContainerImageLayoutRevision invocations per Status() invocation; for a
	// Count=10 RunnerConfig that is 9 wasted slice headers + 9 wasted join
	// strings + 9 wasted sha256-hashes-of-the-embedded-container-image (~30µs
	// and 150KB per call) on every per-tick refresh.
	mode := rc.EffectiveRunnerMode()
	repo := rc.DisplayTarget()
	labels := strings.Join(rc.EffectiveLabelsForInstance(h.OS, h.Arch, 0), ", ")
	host := rc.Host
	isContainer := rc.IsContainerMode()

	var expectedLayoutRev string
	if isContainer {
		expectedLayoutRev = ContainerImageLayoutRevision(m.GhSrVersion, m.containerImageBaseImage(), m.containerImageExtraApt())
	}

	names := rc.InstanceNames()
	statuses := make([]RunnerStatus, 0, len(names))

	for _, name := range names {
		s := RunnerStatus{
			Instance: name,
			Host:     host,
			Repo:     repo,
			Labels:   labels,
			Mode:     mode,
		}

		if isContainer {
			s.Local, s.ContainerImage, s.ContainerImageRevision = m.containerLocalStatusImageAndRevision(h, name)
			s.ContainerImageBuild = formatContainerImageBuild(s.Local, expectedLayoutRev, s.ContainerImageRevision)
		} else {
			// statusNativeAndVersion folds the .runner-version read into
			// the same Linux combined probe (V marker), so the per-tick
			// native path saves one SSH round-trip per instance vs the
			// previous separate statusNative + nativeRunnerVersion pair.
			s.Local, s.ContainerImageBuild = statusNativeAndVersion(h, name)
		}
		statuses = append(statuses, s)
	}

	return statuses, nil
}

func (m *Manager) Logs(h *host.Host, rc config.RunnerConfig, instanceName string) (string, error) {
	if rc.IsContainerMode() {
		return m.logsContainer(h, instanceName)
	}
	return m.logsNative(h, instanceName)
}

// expectedGitHubRunnerOS returns the expected OS label for GitHub API based on host OS.
func expectedGitHubRunnerOS(hostOS string) string {
	switch hostOS {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	default:
		return ""
	}
}

// scopeKey is the (scope, target) pair used to dedupe GitHub API calls per
// repo/org across multiple runner instances in EnrichWithGitHubStatus and
// CleanupOffline. Declared at package level so both call sites share a single
// type definition.
type scopeKey struct{ scope, target string }

func (m *Manager) EnrichWithGitHubStatus(statuses []RunnerStatus, cfg *config.Config) {
	keys := make(map[scopeKey]bool)
	for _, r := range cfg.Runners {
		keys[scopeKey{r.Scope(), r.ScopeTarget()}] = true
	}

	scopeRunners := make(map[scopeKey][]GitHubRunner, len(keys))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for key := range keys {
		wg.Add(1)
		go func(k scopeKey) {
			defer wg.Done()
			runners, err := m.GitHub.ListRunnersScoped(k.scope, k.target)
			if err != nil {
				return
			}
			mu.Lock()
			scopeRunners[k] = runners
			mu.Unlock()
		}(key)
	}
	wg.Wait()

	m.enrichFromScopeRunners(statuses, cfg, scopeRunners)
}

// enrichFromScopeRunners matches statuses against GitHub runner data already
// fetched in scopeRunners, mutating statuses[i].Remote/Busy in place. Extracted
// from EnrichWithGitHubStatus so the inner work can be benchmarked and unit-
// tested without going through the GitHub API.
//
// The rcByInstance build inlines the deterministic `name-N` construction
// instead of calling rc.InstanceNames(): the helper would allocate a
// []string slice header per RunnerConfig in addition to the N name strings,
// and the slice is consumed only as map keys. The Go compiler folds the short
// `+` chain into a single allocation per result, so the inline form costs
// the unavoidable per-name string allocation only.
func (m *Manager) enrichFromScopeRunners(statuses []RunnerStatus, cfg *config.Config, scopeRunners map[scopeKey][]GitHubRunner) {
	rcByInstance := make(map[string]*config.RunnerConfig)
	for i := range cfg.Runners {
		rc := &cfg.Runners[i]
		count := rc.Count
		if count < 1 {
			count = 1
		}
		name := rc.Name
		for j := 1; j <= count; j++ {
			rcByInstance[name+"-"+strconv.Itoa(j)] = rc
		}
	}

	for i := range statuses {
		hcfg, ok := cfg.Hosts[statuses[i].Host]
		if !ok {
			continue
		}
		rc := rcByInstance[statuses[i].Instance]
		if rc == nil {
			continue
		}
		exp := expectedGitHubRunnerOS(hcfg.OS)
		key := scopeKey{rc.Scope(), rc.ScopeTarget()}
		for _, gr := range scopeRunners[key] {
			if gr.Name != statuses[i].Instance {
				continue
			}
			if exp != "" && gr.OS != "" && !strings.EqualFold(gr.OS, exp) {
				continue
			}
			statuses[i].Remote = gr.Status
			statuses[i].Busy = gr.Busy
			break
		}
	}
}

func (m *Manager) CleanupOffline(cfg *config.Config) (int, error) {
	seen := make(map[scopeKey]bool)
	removed := 0

	for _, rc := range cfg.Runners {
		key := scopeKey{rc.Scope(), rc.ScopeTarget()}
		if seen[key] {
			continue
		}
		seen[key] = true
		runners, err := m.GitHub.ListRunnersScoped(key.scope, key.target)
		if err != nil {
			return removed, fmt.Errorf("listing runners for %s: %w", key.target, err)
		}
		for _, r := range runners {
			if r.Status == "offline" {
				if err := m.GitHub.DeleteRunnerScoped(key.scope, key.target, r.ID); err != nil {
					return removed, fmt.Errorf("deleting runner %s (id=%d): %w", r.Name, r.ID, err)
				}
				removed++
			}
		}
	}
	return removed, nil
}

func formatContainerImageBuild(local, expected, actual string) string {
	if local == "not installed" {
		return "-"
	}
	if actual == "" {
		return "?"
	}
	short := func(s string) string {
		if len(s) <= 8 {
			return s
		}
		return s[:8]
	}
	if actual == expected {
		return "ok (" + short(expected) + ")"
	}
	return "stale (" + short(actual) + ")"
}
