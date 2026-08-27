package runner

import (
	"fmt"
	"sort"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
)

// OrphanCleanupPlan describes autostart and/or directory cleanup for one instance.
type OrphanCleanupPlan struct {
	Instance  string
	Autostart bool
	Directory bool
}

// ConfiguredInstanceSet returns instance names from runners assigned to hostName.
func ConfiguredInstanceSet(runners []config.RunnerConfig, hostName string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, rc := range runners {
		if rc.Host != hostName {
			continue
		}
		for _, inst := range rc.InstanceNames() {
			set[inst] = struct{}{}
		}
	}
	return set
}

// OrphanInstances returns runner instance names on h that are not in configured
// (from runners.yml) but have a gh-sr autostart unit and/or directory under ~/.gh-sr/runners.
func OrphanInstances(h *host.Host, configured map[string]struct{}) ([]string, error) {
	dirs, err := ListRunnerInstanceDirs(h)
	if err != nil {
		return nil, err
	}
	installed, err := autostart.ListInstalled(h)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var orphans []string
	add := func(inst string) {
		if _, ok := configured[inst]; ok {
			return
		}
		if err := SafeRunnerInstanceName(inst); err != nil {
			return
		}
		if _, ok := seen[inst]; ok {
			return
		}
		seen[inst] = struct{}{}
		orphans = append(orphans, inst)
	}
	for _, inst := range dirs {
		add(inst)
	}
	for _, inst := range installed {
		add(inst)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// PlanOrphanCleanup reports what would be removed for an orphan instance.
func (m *Manager) PlanOrphanCleanup(h *host.Host, instance string) (OrphanCleanupPlan, error) {
	plan := OrphanCleanupPlan{Instance: instance}
	if err := SafeRunnerInstanceName(instance); err != nil {
		return plan, err
	}
	// Linux-only fast path: the orphan-cleanup probe (autostart kind + svc.sh
	// presence + directory existence) reads three correlated facts about the
	// same host state. Folding them into a single shell call collapses 3 SSH
	// round-trips into 1 per orphan — same win-class as runner.removeContainer
	// (PR #264), setupContainer/needsSetupContainer (PR #350), and
	// EnsureHostDocker (PR #269). For ~50 ms SSH latency and K orphans on a
	// host, this saves 2K × 50 ms = 100 ms × K per `gh sr cleanup` invocation.
	if h.OS == "linux" {
		kind, svcSh, dirExists, err := orphanLinuxPlanProbe(h, instance)
		if err != nil {
			return plan, err
		}
		plan.Autostart = kind != autostart.KindNone || svcSh
		plan.Directory = dirExists
		return plan, nil
	}
	// Non-Linux fallback: PowerShell (Windows) and launchd (Darwin) probes
	// already differ enough that batching buys little. Keep the original
	// per-instance probe ordering.
	kind, err := autostart.Detect(h, instance)
	if err != nil {
		return plan, err
	}
	plan.Autostart = kind != autostart.KindNone
	if h.OS == "linux" && svcShPresent(h, instance) {
		plan.Autostart = true
	}
	exists, err := instanceDirectoryExists(h, instance)
	if err != nil {
		return plan, err
	}
	plan.Directory = exists
	return plan, nil
}

// orphanLinuxPlanProbe issues one shell call that reports three correlated
// facts about an orphan instance on Linux: directory existence, svc.sh
// deployment, and autostart unit kind. Replaces three per-instance
// round-trips (instanceDirectoryExists + svcShPresent + autostart.Detect)
// with one. The script intentionally uses a literal "$HOME" prefix and
// instance-name interpolation (already sanitized by the caller via
// SafeRunnerInstanceName) so the remote sh performs the expansion.
//
// Returned tuple: (kind, hasSvcSh, dirExists, err).
//
// kind uses the same labels as the previous autostart.Detect call so the
// KindNone/KindSystemdUser/KindSystemdSystem contract is preserved.
func orphanLinuxPlanProbe(h *host.Host, instance string) (autostart.Kind, bool, bool, error) {
	result, err := linuxInstanceProbe(h, instance, true)
	return result.kind, result.svcSh, result.dirExists, err
}

// CleanupOrphanInstance removes gh-sr autostart and/or the runner directory for an instance
// that is not in runners.yml. When dryRun is true, nothing is removed.
func (m *Manager) CleanupOrphanInstance(h *host.Host, instance string, dryRun bool) (OrphanCleanupPlan, error) {
	plan, err := m.PlanOrphanCleanup(h, instance)
	if err != nil {
		return plan, err
	}
	if !plan.Autostart && !plan.Directory {
		return plan, nil
	}
	if dryRun {
		if plan.Autostart {
			fmt.Fprintf(m.out(), "  %s: would remove autostart\n", instance)
		}
		if plan.Directory {
			fmt.Fprintf(m.out(), "  %s: would remove orphan directory\n", instance)
		}
		return plan, nil
	}

	if plan.Autostart {
		m.removeNativeServices(h, instance)
	}
	if plan.Directory {
		if err := removeDirTree(h, instance); err != nil {
			return plan, fmt.Errorf("removing orphan directory: %w", err)
		}
		fmt.Fprintf(m.out(), "  %s: orphan directory removed\n", instance)
	}
	return plan, nil
}

func instanceDirectoryExists(h *host.Host, instance string) (bool, error) {
	if h.OS == "windows" {
		return hostshell.RemoteWindowsDirExists(h, h.RunnerDir(instance))
	}
	// dir carries a literal $HOME that the remote sh must expand; pass it raw
	// (RemoteDirExists would PosixSingleQuote it and freeze $HOME).
	dir := h.RunnerDir(instance)
	return hostshell.RemoteBoolCheck(h, "test -d "+dir)
}
