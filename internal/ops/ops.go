package ops

import (
	"fmt"
	"io"
	"sync"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/runner"
)

// ConnectHost opens a connection to the configured host (SSH or local).
func ConnectHost(hostName string, hcfg config.HostConfig) (*host.Host, error) {
	h := host.NewHost(hostName, hcfg)
	if err := h.Connect(); err != nil {
		return nil, err
	}
	return h, nil
}

// connectHostFn is the host-connection factory used by every orchestrator in this
// package. It points at ConnectHost by default and is swapped in tests so the
// orchestrators can be exercised against host.Executor mocks without standing up
// an SSH server. Tests that override it must restore it via t.Cleanup so they
// don't leak state into other tests.
var connectHostFn = ConnectHost

// lockedWriter serialises concurrent writes to an underlying io.Writer.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// writeHostBanner writes "<prefix> (local)..." or "<prefix> (<addr>)..." to w.
// Callers compose `prefix` with whatever verb/object they need (e.g.
// "Setting up on h1" or "Autostart for ci on h1"); the helper handles the
// local-vs-SSH suffix so the wording stays uniform across orchestrators.
func writeHostBanner(w io.Writer, prefix, addr string) {
	if config.IsLocalAddr(addr) {
		fmt.Fprintf(w, "%s (local)...\n", prefix)
	} else {
		fmt.Fprintf(w, "%s (%s)...\n", prefix, addr)
	}
}

// hostGroup bundles runners that share the same host, preserving the order
// in which they were discovered. Callers use this to issue a single SSH
// connection per host and process that host's runners sequentially on it.
type hostGroup struct {
	name    string
	runners []config.RunnerConfig
}

// groupRunnersByHost buckets runners by their Host field, preserving the
// original input order within each group and the discovery order of groups
// (so multi-host runs are deterministic).
func groupRunnersByHost(runners []config.RunnerConfig) []hostGroup {
	groups := make([]hostGroup, 0)
	groupIdx := make(map[string]int)
	for _, rc := range runners {
		if i, ok := groupIdx[rc.Host]; ok {
			groups[i].runners = append(groups[i].runners, rc)
		} else {
			groupIdx[rc.Host] = len(groups)
			groups = append(groups, hostGroup{name: rc.Host, runners: []config.RunnerConfig{rc}})
		}
	}
	return groups
}

// resolveAndFilter runs ResolveHostInfo and returns the filtered runners.
// It is the common preamble to every orchestrator in this package; each
// orchestrator then dispatches the per-runner work in its own way
// (runPerHostParallel, a custom goroutine fan-out, a sequential loop, etc.).
// Logs does not use this helper because it operates on a single resolved
// runner instance rather than the broad FilterRunners result.
func resolveAndFilter(w io.Writer, cfg *config.Config, filterHost, filterRepo string, nameArgs []string) ([]config.RunnerConfig, error) {
	if err := ResolveHostInfo(w, cfg); err != nil {
		return nil, err
	}
	return config.FilterRunners(cfg, filterHost, filterRepo, nameArgs), nil
}

// runPerHostParallel groups runners by host and executes fn for each runner concurrently
// across hosts. Within each host group runners are processed sequentially using a single
// SSH connection, reducing connection overhead from O(N_runners) to O(N_hosts).
// All host groups run in parallel, so total latency is O(SSH_latency) regardless of
// the number of hosts. Writes to w are serialised; pass nil to discard output.
func runPerHostParallel(
	w io.Writer,
	cfg *config.Config,
	runners []config.RunnerConfig,
	fn func(w io.Writer, h *host.Host, rc config.RunnerConfig) error,
) error {
	groups := groupRunnersByHost(runners)

	var out io.Writer = io.Discard
	if w != nil {
		out = &lockedWriter{w: w}
	}

	// Capture the host-connection factory in a local so the goroutines below
	// see the factory that was active when this function was called, not
	// whatever the package-level var points to later. Tests override
	// connectHostFn and rely on this capture for per-call isolation when they
	// run with t.Parallel().
	connect := connectHostFn

	errs := make([]error, len(groups))
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Add(1)
		go func(i int, g hostGroup) {
			defer wg.Done()
			hcfg := cfg.Hosts[g.name]
			h, err := connect(g.name, hcfg)
			if err != nil {
				errs[i] = err
				return
			}
			defer h.Close()
			for _, rc := range g.runners {
				if err := fn(out, h, rc); err != nil {
					errs[i] = err
					return
				}
			}
		}(i, g)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func applyContainerImageExtras(mgr *runner.Manager, cfg *config.Config) {
	if mgr == nil {
		return
	}
	if cfg == nil {
		mgr.ContainerImageBaseImage = ""
		mgr.ContainerImageExtraApt = nil
		mgr.ContainerMTU = 0
		mgr.ContainerDockerdStartTimeout = 0
		mgr.ContainerBootstrapMaxRetries = 0
		mgr.ContainerStartStaggerSeconds = 0
		mgr.Cache = nil
		return
	}
	mgr.ContainerImageBaseImage = cfg.ContainerRunnerImageBaseImage()
	mgr.ContainerImageExtraApt = cfg.ContainerRunnerImageExtraAptPackages()
	mgr.ContainerMTU = cfg.ContainerRunnerImageMTU()
	mgr.ContainerDockerdStartTimeout = cfg.ContainerRunnerImageDockerdStartTimeout()
	mgr.ContainerBootstrapMaxRetries = cfg.ContainerRunnerImageBootstrapMaxRetries()
	mgr.ContainerStartStaggerSeconds = cfg.ContainerRunnerImageStartStaggerSeconds()
	mgr.Cache = cacheSettings(cfg)
}

// Setup installs and configures runners, mirroring the gh sr setup command.
func Setup(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) error {
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return err
	}
	applyContainerImageExtras(mgr, cfg)
	hostsDone := map[string]bool{}
	for _, rc := range runners {
		if hostsDone[rc.Host] {
			continue
		}
		hostsDone[rc.Host] = true
		hcfg := cfg.Hosts[rc.Host]

		writeHostBanner(w, "Setting up on "+rc.Host, hcfg.Addr)
		if err := setupHost(w, cfg, mgr, rc); err != nil {
			return err
		}
	}

	fmt.Fprintln(w, "\nSetup complete.")
	fmt.Fprintln(w, "Start runners with: gh sr up [runner-names...] (setup registers the runner; up launches the listener.)")
	return nil
}

func setupHost(w io.Writer, cfg *config.Config, mgr *runner.Manager, rc config.RunnerConfig) error {
	h, err := connectHostFn(rc.Host, cfg.Hosts[rc.Host])
	if err != nil {
		return err
	}
	defer h.Close()
	if rc.IsContainerMode() {
		if err := ensureCacheForHost(w, h, cacheSettings(cfg)); err != nil {
			return err
		}
	}
	return mgr.Setup(h, rc)
}

// Up starts runners, automatically running setup first if needed.
//
// Runners are grouped by host so that each host requires only one SSH connection.
// All host groups are started concurrently, reducing wall-clock time from
// O(N_hosts × SSH_latency) to O(SSH_latency) for multi-host configurations.
func Up(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) error {
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return err
	}
	applyContainerImageExtras(mgr, cfg)
	cacheSet := cacheSettings(cfg)
	return runPerHostParallel(w, cfg, runners, func(w io.Writer, h *host.Host, rc config.RunnerConfig) error {
		if rc.IsContainerMode() {
			if err := ensureCacheForHost(w, h, cacheSet); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "Starting %s on %s...\n", rc.Name, rc.Host)
		if err := mgr.EnsureSetup(h, rc); err != nil {
			return err
		}
		return mgr.Start(h, rc)
	})
}

// Down stops runners.
//
// Runners are grouped by host so that each host requires only one SSH connection.
// All host groups are stopped concurrently, reducing wall-clock time from
// O(N_hosts × SSH_latency) to O(SSH_latency) for multi-host configurations.
func Down(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) error {
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return err
	}
	return runPerHostParallel(w, cfg, runners, func(w io.Writer, h *host.Host, rc config.RunnerConfig) error {
		fmt.Fprintf(w, "Stopping %s on %s...\n", rc.Name, rc.Host)
		return mgr.Stop(h, rc)
	})
}

// Restart stops then starts runners.
//
// Runners are grouped by host so that each host requires only one SSH connection.
// All host groups are restarted concurrently, reducing wall-clock time from
// O(N_hosts × SSH_latency) to O(SSH_latency) for multi-host configurations.
func Restart(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) error {
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return err
	}
	return runPerHostParallel(w, cfg, runners, func(w io.Writer, h *host.Host, rc config.RunnerConfig) error {
		fmt.Fprintf(w, "Restarting %s on %s...\n", rc.Name, rc.Host)
		_ = mgr.Stop(h, rc)
		return mgr.Start(h, rc)
	})
}

// partitionRebuildTargets splits runners into container-mode targets vs native rows (skipped by rebuild).
func partitionRebuildTargets(runners []config.RunnerConfig) (container []config.RunnerConfig, skipped []config.RunnerConfig) {
	for _, rc := range runners {
		if rc.IsContainerMode() {
			container = append(container, rc)
		} else {
			skipped = append(skipped, rc)
		}
	}
	return container, skipped
}

// RebuildImage rebuilds the container runner Docker image for container-mode
// runners (agentic or not), recreates the containers (preserving state), and
// starts them. Native-mode runners in the selection are skipped (logged); only
// container-mode runners are rebuilt.
func RebuildImage(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) error {
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return err
	}
	applyContainerImageExtras(mgr, cfg)
	containerRunners, skipped := partitionRebuildTargets(runners)
	for _, rc := range skipped {
		fmt.Fprintf(w, "Skipping %s (runner_mode: native); gh sr rebuild applies only to runner_mode: container\n", rc.Name)
	}
	if len(containerRunners) == 0 {
		fmt.Fprintln(w, "No container-mode runners to rebuild.")
		return nil
	}
	cacheSet := cacheSettings(cfg)
	return runPerHostParallel(w, cfg, containerRunners, func(w io.Writer, h *host.Host, rc config.RunnerConfig) error {
		if err := ensureCacheForHost(w, h, cacheSet); err != nil {
			return err
		}
		fmt.Fprintf(w, "Rebuilding image for %s on %s...\n", rc.Name, rc.Host)
		return mgr.RebuildImage(h, rc)
	})
}

// Update removes, sets up, and starts runners again.
//
// Runners are grouped by host so that each host requires only one SSH connection.
// All host groups are updated concurrently, reducing wall-clock time from
// O(N_hosts × SSH_latency) to O(SSH_latency) for multi-host configurations.
func Update(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) error {
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return err
	}
	applyContainerImageExtras(mgr, cfg)
	cacheSet := cacheSettings(cfg)
	if err := runPerHostParallel(w, cfg, runners, func(w io.Writer, h *host.Host, rc config.RunnerConfig) error {
		if rc.IsContainerMode() {
			if err := ensureCacheForHost(w, h, cacheSet); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "Updating %s on %s...\n", rc.Name, rc.Host)
		_ = mgr.Remove(h, rc)
		if err := mgr.Setup(h, rc); err != nil {
			return err
		}
		return mgr.Start(h, rc)
	}); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nUpdate complete.")
	return nil
}

// Remove deregisters a runner from GitHub, removes it from the host, and removes it from the local config.
func Remove(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) error {
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return err
	}
	if len(runners) == 0 {
		return fmt.Errorf("no runners matching the given filters")
	}

	cfgPath, err := config.ResolveConfigPath("")
	if err != nil {
		return err
	}

	for _, rc := range runners {
		hcfg := cfg.Hosts[rc.Host]
		writeHostBanner(w, fmt.Sprintf("Removing %s from %s", rc.Name, rc.Host), hcfg.Addr)
		if err := removeHost(w, cfg, mgr, cfgPath, rc); err != nil {
			return err
		}
	}

	fmt.Fprintln(w, "\nRemove complete.")
	return nil
}

func removeHost(w io.Writer, cfg *config.Config, mgr *runner.Manager, cfgPath string, rc config.RunnerConfig) error {
	h, err := connectHostFn(rc.Host, cfg.Hosts[rc.Host])
	if err != nil {
		return err
	}
	defer h.Close()

	if err := mgr.Remove(h, rc); err != nil {
		return err
	}

	if err := config.RemoveRunner(cfgPath, rc.Name); err != nil {
		return fmt.Errorf("removing %s from config: %w", rc.Name, err)
	}

	fmt.Fprintf(w, "  %s: removed from host and config\n", rc.Name)
	return nil
}

// CollectStatus gathers runner status rows like gh sr status.
//
// Runners are grouped by host so that each host requires only one SSH connection.
// All host groups are queried concurrently, reducing wall-clock time from
// O(N_hosts × SSH_latency) to O(SSH_latency) for multi-host configurations.
func CollectStatus(w io.Writer, cfg *config.Config, mgr *runner.Manager, filterHost, filterRepo string, nameArgs []string) ([]runner.RunnerStatus, error) {
	// Mirror the Setup/Start/Stop/Remove orchestrators: status needs the same
	// image-revision inputs (CLI version, extra apt packages) as the build, or
	// it computes a "stale" build fingerprint that disagrees with the one
	// stamped on the running image. Without this call ContainerImageExtraApt
	// is nil and every container with `container_runner_image.extra_apt_packages`
	// reports "stale" indefinitely even when the layout actually matches.
	applyContainerImageExtras(mgr, cfg)
	runners, err := resolveAndFilter(w, cfg, filterHost, filterRepo, nameArgs)
	if err != nil {
		return nil, err
	}

	// Group runners by host, preserving original order for deterministic output.
	groups := groupRunnersByHost(runners)

	type groupResult struct {
		statuses []runner.RunnerStatus
		err      error
	}
	results := make([]groupResult, len(groups))

	var wg sync.WaitGroup
	var wMu sync.Mutex // guards writes to w

	for i, g := range groups {
		wg.Add(1)
		go func(i int, g hostGroup) {
			defer wg.Done()
			hcfg := cfg.Hosts[g.name]
			h, err := connectHostFn(g.name, hcfg)
			if err != nil {
				if w != nil {
					wMu.Lock()
					fmt.Fprintf(w, "Warning: cannot connect to %s: %v\n", g.name, err)
					wMu.Unlock()
				}
				var unreachable []runner.RunnerStatus
				for _, rc := range g.runners {
					for _, name := range rc.InstanceNames() {
						unreachable = append(unreachable, runner.RunnerStatus{
							Instance:            name,
							Host:                rc.Host,
							Repo:                rc.DisplayTarget(),
							Mode:                "native",
							Local:               "unreachable",
							ContainerImageBuild: "-",
						})
					}
				}
				results[i] = groupResult{statuses: unreachable}
				return
			}
			defer h.Close()

			var statuses []runner.RunnerStatus
			for _, rc := range g.runners {
				s, err := mgr.Status(h, rc)
				if err != nil {
					results[i] = groupResult{err: err}
					return
				}
				statuses = append(statuses, s...)
			}
			results[i] = groupResult{statuses: statuses}
		}(i, g)
	}
	wg.Wait()

	var allStatuses []runner.RunnerStatus
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		allStatuses = append(allStatuses, r.statuses...)
	}

	mgr.EnrichWithGitHubStatus(allStatuses, cfg)
	return allStatuses, nil
}

// Logs returns recent log lines for a runner instance or base name.
func Logs(cfg *config.Config, mgr *runner.Manager, filterHost, target string) (string, error) {
	if err := ResolveHostInfo(nil, cfg); err != nil {
		return "", err
	}
	rc, err := cfg.FindRunnerForLogs(target, filterHost)
	if err != nil {
		return "", err
	}
	instance, err := rc.ResolveRunnerInstance(target)
	if err != nil {
		return "", err
	}
	hcfg := cfg.Hosts[rc.Host]
	h, err := connectHostFn(rc.Host, hcfg)
	if err != nil {
		return "", err
	}
	defer h.Close()
	return mgr.Logs(h, *rc, instance)
}

// CleanupOffline removes offline runners via the GitHub API.
func CleanupOffline(w io.Writer, cfg *config.Config, mgr *runner.Manager) (int, error) {
	fmt.Fprintln(w, "Cleaning up offline runners...")
	removed, err := mgr.CleanupOffline(cfg)
	if err != nil {
		return 0, err
	}
	if removed == 0 {
		fmt.Fprintln(w, "No offline runners found.")
	} else {
		fmt.Fprintf(w, "Removed %d offline runner(s).\n", removed)
	}
	return removed, nil
}
