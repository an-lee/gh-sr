---
title: "Commands"
weight: 50
---

# Commands

Running **`gh sr` with no subcommand** opens the interactive dashboard on a **TTY** (same as `gh sr dashboard`). If stdout is not a terminal (for example in a pipe or CI), gh sr prints a short hint to stderr and exits successfully; use `gh sr status` or `gh sr --help` in those environments.

```bash
gh sr                      # Open dashboard (TTY only; see above)
gh sr init [--force]       # Create ~/.gh-sr with template runners.yml and env file
gh sr doctor [--strict] [--fix]    # Check config, GitHub API, and host prerequisites
gh sr setup [names...]     # Install runner binary and configure on hosts
gh sr up [names...]        # Start runners
gh sr down [names...]      # Stop runners
gh sr restart [names...]   # Stop then start
gh sr status               # Show status table
gh sr hosts                # Show host resource usage (CPU, memory, disk, load, uptime)
gh sr logs <name>          # Show recent logs from a runner
gh sr cleanup              # Remove offline/ghost runners from GitHub
gh sr disk                 # Show per-instance disk usage under ~/.gh-sr/runners
gh sr disk prune           # Reclaim disk on idle runners (use --yes to execute)
gh sr disk schedule install   # Daily automatic disk prune on this machine
gh sr update [names...]    # Update runner binary (remove + setup + start)
gh sr service install [--system] [names...]   # Native: OS autostart (systemd / launchd / task)
gh sr service uninstall [names...]            # Remove gh sr-installed autostart
gh sr service status [names...]               # Autostart + unit state (container: policy note)
gh sr config path          # Print resolved config and ~/.gh-sr/env paths
gh sr config show          # Print resolved configuration (token source summarized)
gh sr config edit          # Edit resolved runners.yml in $VISUAL / $EDITOR
gh sr config edit-env      # Edit ~/.gh-sr/env in $VISUAL / $EDITOR
gh sr config validate      # Validate config (exit 0 if OK)
gh sr dashboard            # Same as bare gh sr: launch interactive TUI dashboard
```

The dashboard includes live status, per-runner actions (setup, up, down, restart, update, logs), global actions (doctor, cleanup, show/validate/edit config and env), and host/repo filters. Press `h` to open the host metrics panel (CPU, memory, disk, load, uptime) or `?` for the full key map.

**Backward compatibility:** older releases treated `gh sr config` as “print configuration”. That is now `gh sr config show`; plain `gh sr config` lists subcommands. Older scripts that expected bare `gh sr` to print usage should use `gh sr --help` instead.

## Filters

All commands accept `--host` and `--repo` flags:

```bash
gh sr doctor --host mac-mini
gh sr status --host mac-mini
gh sr logs myapp-1 --host win-pc   # disambiguate when the same instance name exists on multiple hosts
gh sr up --repo an-lee/enjoy
gh sr down enjoy-win-1
```

## Doctor

`gh sr doctor` validates local paths, configuration, GitHub API access, and host prerequisites. Checks are scoped to runners matching `--host` / `--repo` filters:

- **Native** (`runner_mode: native`): curl/tar (or PowerShell on Windows), runner install dirs, Linux passwordless sudo when needed for setup.
- **Container** (`runner_mode: container` on Linux): host Docker and `--privileged` support, each `gh-sr-<instance>` container, inner `dockerd`, and registration. On non-Linux hosts, doctor reports a FAIL if container-mode runners are in scope.

By default only **FAIL** lines produce a non-zero exit. Use **`--strict`** to fail on **WARN** as well (useful in CI). Use **`--fix`** to re-run `gh sr setup` when doctor reports failures, then always re-run doctor; the final exit code reflects the post-fix result.

## Quick start sequence

```bash
# 0. Verify config, GitHub access, and hosts (optional but recommended)
gh sr doctor

# 1. Set up runners on all hosts
gh sr setup

# 2. Start everything
gh sr up

# 3. Check status
gh sr status

# 4. Launch dashboard for live monitoring (or run `gh sr` alone on a TTY)
gh sr
# gh sr dashboard   # equivalent
```

For what each lifecycle command does on the host, see [Architecture — Lifecycle commands](architecture.md#lifecycle-commands-what-they-do).

## Host metrics

**`gh sr hosts`** collects and displays real-time resource usage from every configured host over SSH. All hosts are queried **concurrently**, so the command completes in roughly the time it takes to collect metrics from the slowest single host regardless of how many hosts you have configured:

```
HOST         CPU%   MEM              DISK              LOAD             UPTIME
──────────────────────────────────────────────────────────────────────────────
mac-mini      12%   3.2 / 16.0 GiB   45 / 500 GiB     0.42 0.38 0.31  5 days
linux-vps     55%   1.8 /  4.0 GiB    8 /  80 GiB     1.20 0.95 0.80  12 days
win-pc         8%   6.1 / 32.0 GiB   80 / 500 GiB     -               3 days
```

The same panel is available interactively in `gh sr dashboard` by pressing `h`.

Use `--host` to limit output to a single host:

```bash
gh sr hosts --host mac-mini
```

> **Note:** Load averages are not available on Windows and show as `-`.

## Disk usage and cleanup

Runner workspaces under `~/.gh-sr/runners/<instance>` accumulate job checkouts (`_work`), temp files (`_temp`), and — for container runners — a persistent inner Docker cache (`docker-data`). Per-job hooks wipe runtime state inside the container but **never** prune the image cache, so disk use can grow large over time.

```bash
gh sr disk usage                    # breakdown per host/instance
gh sr disk usage --host my-linux    # filter by host
gh sr disk prune --dry-run          # preview reclaim (default without --yes)
gh sr disk prune --yes              # clear _work/_temp on idle runners (keeps docker-data cache)
gh sr disk prune --yes --prune-cache # also reclaim inner Docker cache (slower next job)
gh sr disk prune --yes --include-orphans   # also remove dirs not in runners.yml
gh sr disk schedule install         # daily prune at 03:00 on this machine
gh sr disk schedule status
gh sr disk schedule uninstall
```

**Notes:**

- Busy runners (active job on GitHub) are always skipped.
- Default prune keeps inner Docker cache (`docker-data`) so the next container job does not re-pull image layers. Use `--prune-cache` when you need maximum disk recovery.
- `gh sr cleanup` removes **offline GitHub registrations** only — it does not free disk. Use `gh sr disk prune` for workspace cleanup.
- `gh sr doctor` warns when any instance directory exceeds 50 GiB (including orphan dirs).
- `gh sr disk schedule install` runs on **this machine** (where you run gh), not on runner hosts. Ensure `gh` is on PATH, `gh auth login` is done, and `~/.gh-sr/env` contains any tokens needed for remote hosts. On Linux headless servers, run `loginctl enable-linger $USER` so the systemd user timer runs without an interactive login. SSH keys for remote hosts must be available to the scheduled job's user environment.

