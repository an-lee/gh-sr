package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/diskschedule"
	"github.com/an-lee/gh-sr/internal/doctor"
	"github.com/an-lee/gh-sr/internal/editor"
	"github.com/an-lee/gh-sr/internal/ops"
	"github.com/an-lee/gh-sr/internal/runner"
	"github.com/an-lee/gh-sr/internal/tui"
)

var version = "dev"

var (
	cfgFile    string
	filterHost string
	filterRepo string
)

// linuxSetupPrivilegesHelp is appended to root/setup/update Long text (non-interactive SSH + sudo behavior on Linux).
const linuxSetupPrivilegesHelp = `

Linux hosts: gh sr setup and update may run package installs and similar steps on the remote host. For a non-root
SSH user, gh sr uses sudo when the sudo binary exists on the remote PATH; SSH is non-interactive, so passwordless
sudo (or SSH as root) is usually required for those steps to succeed. For native mode, pre-install curl/tar and
runner OS dependencies if you cannot use sudo. See the README section "Linux SSH user and privileges".`

// serviceLongHelp documents autostart behavior for the service subcommands.
const serviceLongHelp = `

Native runners do not survive host reboot until OS autostart is installed (gh sr service install). After install,
gh sr up and gh sr down start and stop the same supervisor (systemd, launchd, or a Windows scheduled task).

Linux user units (default) require loginctl enable-linger <user> on many headless servers so systemd --user
starts at boot without an interactive login. Use --system on Linux only for a system-wide unit in
/etc/systemd/system (needs passwordless sudo or root SSH).`

func main() {
	root := &cobra.Command{
		Use:   "sr",
		Short: "Manage self-hosted GitHub Actions runners across multiple hosts",
		Long: `Self-hosted runner manager for GitHub (gh sr) manages self-hosted GitHub Actions runners on any combination
of Linux, macOS, and Windows hosts — all from your laptop over SSH.

Define your hosts and runners in ~/.gh-sr/runners.yml (or set GH_SR_CONFIG / --config),
then use unified commands to setup, start, stop, and monitor everything.

With no subcommand, gh sr opens the interactive dashboard on a terminal; use gh sr --help for all commands.` + linuxSetupPrivilegesHelp,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown argument %q — use a subcommand or gh sr --help", args[0])
			}
			return runDashboard()
		},
	}

	root.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file path (empty = auto; use \"gh sr config path\" to print the resolved file)")
	root.PersistentFlags().StringVar(&filterHost, "host", "", "filter by host name")
	root.PersistentFlags().StringVar(&filterRepo, "repo", "", "filter by repo (owner/repo)")

	root.AddCommand(
		initCmd(),
		addCmd(),
		doctorCmd(),
		setupCmd(),
		upCmd(),
		downCmd(),
		restartCmd(),
		rebuildCmd(),
		statusCmd(),
		logsCmd(),
		cleanupCmd(),
		diskCmd(),
		cacheCmd(),
		updateCmd(),
		removeCmd(),
		serviceCmd(),
		configCmd(),
		dashboardCmd(),
		hostsCmd(),
		versionCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func runDashboard() error {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, tui.NonTTYHint)
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfgPath, err := config.ResolveConfigPath(cfgFile)
	if err != nil {
		return err
	}
	envPath, err := config.EnvFilePath()
	if err != nil {
		return err
	}
	return tui.RunDashboard(cfg, tui.DashboardOpts{
		ConfigPath:  cfgPath,
		EnvPath:     envPath,
		FilterHost:  filterHost,
		FilterRepo:  filterRepo,
		GhSrVersion: version,
	})
}

func loadConfig() (*config.Config, error) {
	if err := config.BootstrapEnv(); err != nil {
		return nil, err
	}
	path, err := config.ResolveConfigPath(cfgFile)
	if err != nil {
		return nil, err
	}
	return config.LoadFromPath(path)
}

func newManager(cfg *config.Config, w io.Writer) (*runner.Manager, error) {
	tok, err := config.ResolveToken(cfg)
	if err != nil {
		return nil, err
	}
	m := runner.NewManager(tok)
	m.Out = w
	m.GhSrVersion = version
	return m, nil
}

// runnerCommandContext is the common preamble of most top-level gh sr
// subcommands: load the resolved config and build a runner manager wired to
// the command's stdout. Returns an error on either failure so callers can
// bubble it up directly.
func runnerCommandContext(cmd *cobra.Command) (*config.Config, *runner.Manager, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	mgr, err := newManager(cfg, cmd.OutOrStdout())
	if err != nil {
		return nil, nil, err
	}
	return cfg, mgr, nil
}

// runRunnerCmd wraps an ops function with the standard
// (w, cfg, mgr, filterHost, filterRepo, nameArgs) signature into a cobra
// RunE that loads the config and manager first, then forwards to the ops
// function. The package-level filterHost / filterRepo flags are applied
// automatically; only the ops call itself varies between subcommands.
func runRunnerCmd(op func(io.Writer, *config.Config, *runner.Manager, string, string, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		cfg, mgr, err := runnerCommandContext(cmd)
		if err != nil {
			return err
		}
		return op(cmd.OutOrStdout(), cfg, mgr, filterHost, filterRepo, args)
	}
}

func initCmd() *cobra.Command {
	var force bool
	var quick bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create ~/.gh-sr with template runners.yml and env file",
		Long: `Create ~/.gh-sr directory with a template runners.yml and env file.

Use --quick for an interactive setup that asks for a repo and host address,
auto-detects everything else, and writes a working config ready for gh sr up.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := config.SrDir()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			runnersPath := filepath.Join(dir, "runners.yml")

			envPath, err := config.EnvFilePath()
			if err != nil {
				return err
			}
			if _, err := os.Stat(envPath); os.IsNotExist(err) {
				if err := os.WriteFile(envPath, []byte(config.EnvFileTemplate), 0o600); err != nil {
					return err
				}
				fmt.Printf("Wrote %s\n", envPath)
			}

			if quick {
				return runQuickInit(runnersPath, force)
			}

			if _, err := os.Stat(runnersPath); err == nil && !force {
				fmt.Printf("Already exists (use --force to overwrite): %s\n", runnersPath)
			} else {
				if err := os.WriteFile(runnersPath, config.RunnersYMLTemplate, 0o600); err != nil {
					return err
				}
				fmt.Printf("Wrote %s\n", runnersPath)
			}
			fmt.Println("\nNext: edit ~/.gh-sr/runners.yml, then run `gh sr doctor` and `gh sr status`.")
			fmt.Println("Authentication: run `gh auth login`.")
			fmt.Println("Tip: use `gh sr init --quick` for interactive setup.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing ~/.gh-sr/runners.yml")
	cmd.Flags().BoolVar(&quick, "quick", false, "interactive setup: prompts for repo and host, auto-detects everything else")
	return cmd
}

func runQuickInit(runnersPath string, force bool) error {
	scanner := bufio.NewScanner(os.Stdin)
	prompt := func(label, defaultVal string) string {
		if defaultVal != "" {
			fmt.Printf("%s [%s]: ", label, defaultVal)
		} else {
			fmt.Printf("%s: ", label)
		}
		if scanner.Scan() {
			v := strings.TrimSpace(scanner.Text())
			if v != "" {
				return v
			}
		}
		return defaultVal
	}

	fmt.Println("=== gh sr quick setup ===")
	fmt.Println("This will create a working config. OS, arch, and labels are auto-detected.")
	fmt.Println()

	repo := prompt("GitHub repo (owner/repo)", "")
	if repo == "" {
		return fmt.Errorf("repo is required")
	}
	if !strings.Contains(repo, "/") {
		return fmt.Errorf("repo must be in owner/repo format")
	}

	addr := prompt("SSH address of runner host (user@host, or 'local' for this machine)", "local")

	hostName := "runner-host"
	if config.IsLocalAddr(addr) {
		hostName = "local"
	} else {
		parts := strings.SplitN(addr, "@", 2)
		if len(parts) == 2 {
			h := strings.Split(parts[1], ":")[0]
			h = strings.ReplaceAll(h, ".", "-")
			if h != "" {
				hostName = h
			}
		}
	}

	baseName := strings.ReplaceAll(strings.Split(repo, "/")[1], ".", "-")
	runnerName := baseName + "-" + hostName

	countStr := prompt("Number of runner instances", "1")
	count := 1
	if _, err := fmt.Sscanf(countStr, "%d", &count); err != nil || count < 1 {
		count = 1
	}

	profileStr := prompt(`Runner profile ("agentic" for GitHub Agentic Workflows, or empty)`, "")
	profileLine := ""
	if profileStr == "agentic" {
		profileLine = "    profile: agentic\n"
	}

	seed := fmt.Sprintf(`github: {}
hosts:
  %s:
    addr: %s
runners:
  - name: %s
    repo: %s
    host: %s
    count: %d
%s`, hostName, addr, runnerName, repo, hostName, count, profileLine)

	if _, err := os.Stat(runnersPath); err == nil && !force {
		fmt.Printf("\n%s already exists. Overwrite? [y/N]: ", runnersPath)
		if scanner.Scan() {
			if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
				fmt.Println("Aborted.")
				return nil
			}
		}
	}

	if err := os.WriteFile(runnersPath, []byte(seed), 0o600); err != nil {
		return err
	}
	fmt.Printf("\nWrote %s\n", runnersPath)

	// Validate the generated config loads.
	if _, err := config.Load(runnersPath); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}

	fmt.Println()
	fmt.Println("Config generated! Summary:")
	fmt.Printf("  Host:   %s (%s)\n", hostName, addr)
	fmt.Printf("  Runner: %s -> %s (x%d)\n", runnerName, repo, count)
	fmt.Println("  Labels: auto-generated from host os/arch")
	if profileStr == "agentic" {
		fmt.Println("  Profile: agentic (GitHub Agentic Workflows)")
	}
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  gh sr doctor   # verify config, GitHub access, and host connectivity")
	fmt.Println("  gh sr up       # setup + start runners (all in one)")
	fmt.Println()
	fmt.Println("Authentication: run `gh auth login`.")

	return nil
}

func addCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a host or runner to the config",
	}
	cmd.AddCommand(addHostCmd(), addRunnerCmd())
	return cmd
}

func addHostCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "host <name> <addr>",
		Short: "Add a host entry (os/arch auto-detected over SSH)",
		Long:  "Adds a host to runners.yml. Only name and SSH address are required; os and arch are auto-detected at runtime.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolveConfigPath(cfgFile)
			if err != nil {
				return err
			}
			name, addr := args[0], args[1]
			if err := config.AddHost(cfgPath, name, addr, "", ""); err != nil {
				return err
			}
			fmt.Printf("Added host %q (%s) to %s\n", name, addr, cfgPath)
			return nil
		},
	}
}

func addRunnerCmd() *cobra.Command {
	var (
		repo      string
		org       string
		group     string
		host      string
		count     int
		labels    []string
		ephemeral bool
		profile   string
	)
	cmd := &cobra.Command{
		Use:   "runner <name>",
		Short: "Add a runner entry (labels auto-generated if omitted)",
		Long: `Adds a runner to runners.yml. Labels are auto-generated from host os/arch if not specified.

Use --org instead of --repo for organization-level runners, and --group
to assign the runner to a runner group. Use --profile agentic for GitHub
Agentic Workflows (gh-aw) runners; agentic runners always use container
(Docker-in-Docker) isolation, so no runner_mode or MCP port config is needed.

Agentic runners run the gh-aw v0.88+ rootless sandbox (no sudo, no iptables
rules; the AWF firewall runs unprivileged on the inner Docker bridge) on a
fork runner image, and point actions/cache at a per-host local cache server
(deployed automatically; see gh sr cache and guides/local-cache.md).

Workflows must be compiled with gh-aw >= v0.88 (gh aw compile). Point them
at this runner with runs-on: self-hosted (or runs-on-slim / safe-outputs
.runs-on in gh-aw workflow front matter).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolveConfigPath(cfgFile)
			if err != nil {
				return err
			}
			name := args[0]
			if repo == "" && org == "" {
				return fmt.Errorf("--repo or --org is required")
			}
			if repo != "" && org != "" {
				return fmt.Errorf("specify --repo or --org, not both")
			}
			if host == "" {
				return fmt.Errorf("--host is required")
			}
			if profile != "" && profile != "agentic" {
				return fmt.Errorf("--profile must be empty or \"agentic\"")
			}
			opts := config.AddRunnerOpts{
				Name:      name,
				Repo:      repo,
				Org:       org,
				Group:     group,
				Host:      host,
				Count:     count,
				Labels:    labels,
				Ephemeral: ephemeral,
				Profile:   profile,
			}
			if err := config.AddRunnerFull(cfgPath, opts); err != nil {
				return err
			}
			target := repo
			if org != "" {
				target = "org:" + org
			}
			fmt.Printf("Added runner %q (%s, host=%s) to %s\n", name, target, host, cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub owner/repo (required unless --org is set)")
	cmd.Flags().StringVar(&org, "org", "", "GitHub organization (for org-level runners)")
	cmd.Flags().StringVar(&group, "group", "", "runner group name (org-level runners only)")
	cmd.Flags().StringVar(&host, "host", "", "host name from config (required)")
	cmd.Flags().IntVar(&count, "count", 1, "number of parallel instances")
	cmd.Flags().StringSliceVar(&labels, "labels", nil, "runner labels (auto-generated if empty)")
	cmd.Flags().BoolVar(&ephemeral, "ephemeral", false, "register as ephemeral (one job then deregister)")
	cmd.Flags().StringVar(&profile, "profile", "", `runner profile: "agentic" for GitHub Agentic Workflows (gh-aw); runs in container (DinD) mode automatically`)
	return cmd
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect or edit configuration",
	}
	cmd.AddCommand(
		configPathCmd(),
		configShowCmd(),
		configEditCmd(),
		configEditEnvCmd(),
		configValidateCmd(),
	)
	return cmd
}

func doctorCmd() *cobra.Command {
	var strict bool
	var fix bool
	var checkLockfiles bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check config, GitHub API access, and host prerequisites",
		Long:  "Validates local paths, configuration, GitHub API access, and SSH targets. For runner_mode: native, checks host runner dirs. profile: agentic always uses container mode: checks outer Docker and --privileged, each gh-sr-<instance> container, inner dockerd, .runner inside the container, agentic rootless-sandbox hygiene (orphan awmg-mcpg/awf containers, host-gateway alias, node/zstd, docker socket access), and the local Actions cache server when enabled. See README \"Host setup\" for steps gh sr cannot automate.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := config.BootstrapEnv(); err != nil {
				return err
			}
			cfgPath, err := config.ResolveConfigPath(cfgFile)
			if err != nil {
				return err
			}
			envPath, err := config.EnvFilePath()
			if err != nil {
				return err
			}

			cfg, cfgErr := config.LoadFromPath(cfgPath)
			var gh *runner.GitHubClient
			if cfg != nil {
				tok, tokErr := config.ResolveToken(cfg)
				if tokErr == nil {
					gh = runner.NewGitHubClient(tok)
				}
			}

			w := cmd.OutOrStdout()
			res := doctor.Run(w, cfgPath, envPath, cfg, cfgErr, gh, filterHost, filterRepo, strict, checkLockfiles)
			if fix && res.Fail > 0 && cfg != nil && cfgErr == nil {
				fmt.Fprintln(w, "\n--- Running `gh sr setup` to attempt fixes ---")
				mgr, err := newManager(cfg, w)
				if err != nil {
					fmt.Fprintf(w, "\ncannot create runner manager: %v\n", err)
				} else if err := ops.Setup(w, cfg, mgr, filterHost, filterRepo, nil); err != nil {
					fmt.Fprintf(w, "\nfix attempt completed with errors: %v\n", err)
				} else {
					fmt.Fprintln(w, "\nfix attempt completed successfully.")
				}
				fmt.Fprintln(w, "\n--- Re-running doctor after fix ---")
				res = doctor.Run(w, cfgPath, envPath, cfg, cfgErr, gh, filterHost, filterRepo, strict, checkLockfiles)
			}
			if code := doctor.ExitCode(res, strict); code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "non-zero exit if any check is WARN (default: only FAIL fails the run)")
	cmd.Flags().BoolVar(&fix, "fix", false, "automatically attempt to fix failures by re-running setup, then re-run doctor")
	cmd.Flags().BoolVar(&checkLockfiles, "check-lockfiles", false, "also scan each scoped repo's compiled gh-aw workflows (*.lock.yml) for retired sudo/iptables-era artifacts; FAIL means the workflow must be recompiled with gh-aw >= v0.88 (requires network)")
	return cmd
}

func configPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print resolved config and env file paths",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.ResolveConfigPath(cfgFile)
			if err != nil {
				return err
			}
			envPath, err := config.EnvFilePath()
			if err != nil {
				return err
			}
			envStatus := "not present"
			if _, err := os.Stat(envPath); err == nil {
				envStatus = "present"
			}
			fmt.Printf("Config file: %s\n", path)
			fmt.Printf("Env file:    %s (%s)\n", envPath, envStatus)
			return nil
		},
	}
}

func configShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print resolved configuration (token source summarized)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			tui.PrintConfig(cfg)
			return nil
		},
	}
}

func configEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open the resolved config file in $VISUAL or $EDITOR",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.ResolveConfigPath(cfgFile)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return fmt.Errorf("config file does not exist: %s\nRun `gh sr init` to create it", path)
			}
			if err := editor.Open(path); err != nil {
				return err
			}
			if _, err := loadConfig(); err != nil {
				return fmt.Errorf("config invalid after edit: %w", err)
			}
			fmt.Println("Config is valid.")
			return nil
		},
	}
}

func configEditEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit-env",
		Short: "Open ~/.gh-sr/env in $VISUAL or $EDITOR",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := config.SrDir()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			envPath, err := config.EnvFilePath()
			if err != nil {
				return err
			}
			if _, err := os.Stat(envPath); os.IsNotExist(err) {
				if err := os.WriteFile(envPath, []byte(config.EnvFileTemplate), 0o600); err != nil {
					return err
				}
			}
			if err := editor.Open(envPath); err != nil {
				return err
			}
			if _, err := loadConfig(); err != nil {
				return fmt.Errorf("config invalid after editing env: %w", err)
			}
			fmt.Println("Config is valid.")
			return nil
		},
	}
}

func configValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the resolved config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := loadConfig()
			if err != nil {
				return err
			}
			fmt.Println("OK")
			return nil
		},
	}
}

// --- Commands ---

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup [runner-names...]",
		Short: "Install runner prerequisites and configure runners on hosts",
		Long:  "Installs and configures runners on remote hosts over SSH." + linuxSetupPrivilegesHelp,
		RunE:  runRunnerCmd(ops.Setup),
	}
}

func upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up [runner-names...]",
		Short: "Start runners (all, or filtered by name/host/repo)",
		RunE:  runRunnerCmd(ops.Up),
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down [runner-names...]",
		Short: "Stop runners",
		RunE:  runRunnerCmd(ops.Down),
	}
}

func restartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart [runner-names...]",
		Short: "Restart runners (stop then start)",
		RunE:  runRunnerCmd(ops.Restart),
	}
}

// cacheCmd groups the per-host local Actions cache server management commands.
// The server is deployed automatically before container runner setup/start
// (when enabled); these commands inspect or uninstall it explicitly.
func cacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage the per-host local Actions cache server",
		Long: `Deploy and manage a local GitHub Actions cache server (falcondev-oss
github-actions-cache-server, one gh-sr-cache container per Linux host) so
container runners serve actions/cache traffic from the host instead of GitHub's
cache service. Artifacts and other non-cache traffic still pass through to
GitHub, so safe-outputs keep working across hosts.

The server is deployed automatically before container runner setup/start when
cache.enabled is not false in runners.yml; use these commands to inspect it,
prune stored entries, or uninstall it. gh sr remove never deletes the cache —
gh sr cache remove is the only uninstall path.`,
	}
	cmd.AddCommand(cacheStatusCmd(), cacheDeployCmd(), cachePruneCmd(), cacheRemoveCmd())
	return cmd
}

func cacheStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cache server state, health, and storage per host",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return ops.CacheStatus(cmd.OutOrStdout(), cfg, filterHost)
		},
	}
}

func cacheDeployCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deploy",
		Short: "Deploy (or start) the cache server container on hosts",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return ops.CacheDeploy(cmd.OutOrStdout(), cfg, filterHost)
		},
	}
}

func cachePruneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Delete all cache entries via the management API (best-effort)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return ops.CachePrune(cmd.OutOrStdout(), cfg, filterHost)
		},
	}
}

func cacheRemoveCmd() *cobra.Command {
	var purgeData bool
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Stop and remove the cache server container",
		Long: `Stops and removes the gh-sr-cache container. With --purge-data, also deletes
the storage directory (all cached data plus the generated management key).
Runner removal (gh sr remove) never touches the cache.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return ops.CacheRemove(cmd.OutOrStdout(), cfg, filterHost, purgeData)
		},
	}
	cmd.Flags().BoolVar(&purgeData, "purge-data", false, "also delete the cache storage directory (all cached data)")
	return cmd
}

func rebuildCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild [runner-names...]",
		Short: "Rebuild the container runner image and restart containers (container-mode only)",
		Long: `Rebuilds the gh-sr/agentic-runner Docker image from the embedded sources
(same image for all runner_mode: container runners, agentic or not), recreates
the runner containers, and starts them. The image includes global
container_runner_image.extra_apt_packages from runners.yml when set (see the
agentic workflows guide).

Changing container_runner_image.base_image, tag, or the embedded image
sources changes the image fingerprint; rebuild is what picks the new image up
(doctor flags a stale fingerprint). Runner state (the .runner registration
file, work directories, and Docker layer cache inside the container) is
preserved across the rebuild, so runners stay registered with GitHub and do
not consume a new registration token.

Runners with runner_mode: native are skipped (no error); only runner_mode: container rows are rebuilt.`,
		RunE: runRunnerCmd(ops.RebuildImage),
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status of all runners",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, mgr, err := runnerCommandContext(cmd)
			if err != nil {
				return err
			}
			allStatuses, err := ops.CollectStatus(cmd.OutOrStdout(), cfg, mgr, filterHost, filterRepo, args)
			if err != nil {
				return err
			}
			tui.PrintStatusTable(allStatuses)
			return nil
		},
	}
}

func logsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <runner-name>",
		Short: "Show recent logs from a runner",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, mgr, err := runnerCommandContext(cmd)
			if err != nil {
				return err
			}
			output, err := ops.Logs(cfg, mgr, filterHost, args[0])
			if err != nil {
				return err
			}
			fmt.Println(output)
			return nil
		},
	}
}

func cleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup",
		Short: "Remove offline/ghost runners from GitHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, mgr, err := runnerCommandContext(cmd)
			if err != nil {
				return err
			}
			_, err = ops.CleanupOffline(cmd.OutOrStdout(), cfg, mgr)
			return err
		},
	}
}

func diskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disk",
		Short: "Report and prune runner workspace disk usage",
		Long: `Inspect and reclaim disk space under ~/.gh-sr/runners on runner hosts.

Per-instance state includes _work (job checkouts), _temp, and docker-data (inner
Docker image cache for container/agentic runners). Prune clears _work/_temp by
default and keeps docker-data; use disk prune --prune-cache for deeper reclaim.
Prune skips busy runners.`,
	}
	usage := diskUsageCmd()
	cmd.AddCommand(usage, diskPruneCmd(), diskScheduleCmd())
	cmd.RunE = usage.RunE
	return cmd
}

func diskUsageCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "usage [runner-names...]",
		Aliases: []string{"show"},
		Short:   "Show per-instance disk usage under ~/.gh-sr/runners",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, mgr, err := runnerCommandContext(cmd)
			if err != nil {
				return err
			}
			entries, err := ops.CollectDiskUsage(cmd.OutOrStdout(), cfg, mgr, filterHost, filterRepo, args)
			if err != nil {
				return err
			}
			ops.PrintDiskUsageTable(cmd.OutOrStdout(), entries)
			return nil
		},
	}
}

func diskPruneCmd() *cobra.Command {
	var (
		dryRun         bool
		yes            bool
		pruneCache     bool
		includeOrphans bool
		force          bool
	)
	cmd := &cobra.Command{
		Use:   "prune [runner-names...]",
		Short: "Reclaim disk on idle runners (_work and _temp; cache kept by default)",
		Long: `Clears _work and _temp on idle runners. Inner Docker image cache (docker-data)
is preserved by default so the next job does not re-pull gh-aw images. Use
--prune-cache for a deeper reclaim that also runs inner docker system prune.

Busy runners are always skipped. Orphan directories (not in runners.yml) are
removed only with --include-orphans.

Examples:
  gh sr disk prune --dry-run
  gh sr disk prune --yes
  gh sr disk prune --yes --prune-cache
  gh sr disk prune --yes --host my-linux-box`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, mgr, err := runnerCommandContext(cmd)
			if err != nil {
				return err
			}
			opts := ops.DiskPruneOptions{
				DryRun:         dryRun || !yes,
				PruneCache:     pruneCache,
				IncludeOrphans: includeOrphans,
				Force:          force,
			}
			_, err = ops.PruneDisk(cmd.OutOrStdout(), cfg, mgr, filterHost, filterRepo, args, opts)
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without executing (default unless --yes)")
	cmd.Flags().BoolVar(&yes, "yes", false, "execute prune (required to make changes)")
	cmd.Flags().BoolVar(&pruneCache, "prune-cache", false, "also prune inner Docker cache (docker-data); default keeps cache for faster next job")
	cmd.Flags().BoolVar(&includeOrphans, "include-orphans", false, "remove instance directories not in runners.yml")
	cmd.Flags().BoolVar(&force, "force", false, "prune when GitHub runner status is unknown")
	return cmd
}

func diskScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Install a daily disk prune timer on this machine",
		Long: `Installs a local daily timer that runs 'gh sr disk prune --yes' against the
resolved config. Linux uses a systemd user timer; macOS uses a LaunchAgent;
Windows uses a Scheduled Task. On Linux headless servers you may need
loginctl enable-linger for the timer to run without an interactive login.`,
	}
	var atTime string
	install := &cobra.Command{
		Use:   "install",
		Short: "Install daily disk prune schedule",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolveConfigPath(cfgFile)
			if err != nil {
				return err
			}
			if err := diskschedule.Install(diskschedule.InstallOpts{
				ConfigPath: cfgPath,
				AtTime:     atTime,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed daily disk prune schedule (config: %s, time: %s).\n", cfgPath, atTimeOrDefault(atTime))
			return nil
		},
	}
	install.Flags().StringVar(&atTime, "at", diskschedule.DefaultAtTime, "local daily time HH:MM")
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove disk prune schedule",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := diskschedule.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Disk prune schedule removed.")
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show disk prune schedule state",
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, detail, err := diskschedule.Status()
			if err != nil {
				return err
			}
			if kind == diskschedule.KindNone {
				fmt.Fprintln(cmd.OutOrStdout(), detail)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", kind, detail)
			return nil
		},
	}
	cmd.AddCommand(install, uninstall, status)
	return cmd
}

func atTimeOrDefault(at string) string {
	if strings.TrimSpace(at) == "" {
		return diskschedule.DefaultAtTime
	}
	return at
}

func updateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [runner-names...]",
		Short: "Update runner binary on hosts (remove + setup + start)",
		Long:  "Removes each runner, runs setup again, then starts it. Re-runs the same remote install paths as gh sr setup." + linuxSetupPrivilegesHelp,
		RunE:  runRunnerCmd(ops.Update),
	}
}

func removeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [runner-names...]",
		Short: "Remove runners from hosts and config (deregisters from GitHub)",
		Long: `Removes each runner from its host, deregisters it from GitHub, and removes
the runner entry from ~/.gh-sr/runners.yml. Unlike gh sr update, this does not
re-setup the runner afterward.

Use --host and/or --repo to filter which runners to remove.` + linuxSetupPrivilegesHelp,
		RunE: runRunnerCmd(ops.Remove),
	}
}

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install or manage OS autostart for native runners",
		Long:  "Manage boot-time autostart for native-mode self-hosted runners." + serviceLongHelp,
	}
	var system bool
	install := &cobra.Command{
		Use:   "install [runner-names...]",
		Short: "Install autostart for native runners (all or filtered)",
		Long:  "Writes systemd user units (Linux), LaunchAgents (macOS), or a logon scheduled task (Windows), then enables and starts them." + serviceLongHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return ops.ServiceInstall(cmd.OutOrStdout(), cfg, filterHost, filterRepo, args, system)
		},
	}
	install.Flags().BoolVar(&system, "system", false, "Linux only: install systemd unit under /etc/systemd/system (passwordless sudo or root SSH)")
	uninstall := &cobra.Command{
		Use:   "uninstall [runner-names...]",
		Short: "Remove autostart definitions installed by gh sr",
		Long:  "Stops and removes systemd units, LaunchAgents, or scheduled tasks created by gh sr service install." + serviceLongHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return ops.ServiceUninstall(cmd.OutOrStdout(), cfg, filterHost, filterRepo, args)
		},
	}
	status := &cobra.Command{
		Use:   "status [runner-names...]",
		Short: "Show autostart state per runner instance",
		Long:  "Reports whether gh sr autostart is installed and the service state." + serviceLongHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return ops.ServiceStatus(cmd.OutOrStdout(), cfg, filterHost, filterRepo, args)
		},
	}
	cleanup := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove orphan runner services and directories not in runners.yml",
		Long:  "Finds gh-sr autostart units and runner directories under ~/.gh-sr/runners that are not listed in runners.yml — for example after a rename or manual config edit — and removes them." + serviceLongHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			return ops.ServiceCleanup(cmd.OutOrStdout(), cfg, filterHost, dryRun)
		},
	}
	cleanup.Flags().Bool("dry-run", false, "Report orphan services and directories without removing them")
	cmd.AddCommand(install, uninstall, status, cleanup)
	return cmd
}

func hostsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hosts",
		Short: "Show host resource usage (CPU, memory, disk, load, uptime)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			metrics := ops.CollectHostMetrics(cmd.OutOrStdout(), cfg, filterHost)
			tui.PrintHostMetricsTable(metrics)
			return nil
		},
	}
}

func dashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Launch interactive TUI dashboard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDashboard()
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print gh sr version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version)
		},
	}
}
