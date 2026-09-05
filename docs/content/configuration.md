---
title: "Configuration"
weight: 20
---

# Configuration

## Config file location

When you do **not** pass `--config` / `-c`, the config file is chosen in this order:

1. **`GH_SR_CONFIG`** — path to a YAML file (absolute or relative to the current working directory).
2. **`~/.gh-sr/runners.yml`** — default after `gh sr init`.

There is no automatic discovery of `./config/runners.yml` in the current directory; use `GH_SR_CONFIG` or `-c` if your file lives elsewhere.

If you pass `-c /path/to/runners.yml`, that path is always used (and `GH_SR_CONFIG` is ignored).

Run `gh sr config path` to see which config file and `~/.gh-sr/env` path apply in your environment.

## Authentication

**gh sr** uses the [GitHub CLI](https://cli.github.com/) only: run **`gh auth login`** on the machine where you run gh sr. Do not use `github.pat` in YAML or `GITHUB_PAT` / `GITHUB_TOKEN` for gh sr (legacy `github.pat` is rejected at load time).

See [Authentication](authentication.md) for permissions and troubleshooting.

## Secrets (`~/.gh-sr/env`)

Before the YAML file is loaded, **gh sr** applies environment variables from **`~/.gh-sr/env`** if that file exists (dotenv-style: `KEY=value`, optional `export `, `#` comments). This is optional and intended for other tooling if needed — not for GitHub API tokens used by gh sr. Create the directory and file with `gh sr init`, or run `gh sr config edit-env`.

Keep `~/.gh-sr` permissions tight (`chmod 700 ~/.gh-sr`, `chmod 600 ~/.gh-sr/env` if you create files by hand).

## Example `runners.yml`

Edit `~/.gh-sr/runners.yml` (after `gh sr init`), or set `GH_SR_CONFIG` / `-c` to another YAML file. You can open the resolved file in `$VISUAL` or `$EDITOR` with `gh sr config edit`.

```yaml
hosts:
  my-laptop:
    addr: local              # run on the local machine (no SSH)
    # os and arch are auto-detected; override if needed

  mac-mini:
    addr: user@192.168.1.50
    os: darwin
    arch: arm64

  win-pc:
    addr: user@192.168.1.51
    os: windows
    arch: amd64

  vps-1:
    addr: root@203.0.113.10
    os: linux
    arch: amd64

runners:
  - name: enjoy-local
    repo: an-lee/enjoy
    host: my-laptop
    count: 1
    labels: [self-hosted, Linux, X64]
    runner_mode: native

  - name: enjoy-mac
    repo: an-lee/enjoy
    host: mac-mini
    count: 1
    labels: [self-hosted, macOS, ARM64]

  - name: enjoy-win
    repo: an-lee/enjoy
    host: win-pc
    count: 1
    labels: [self-hosted, Windows, X64]

  - name: hangar-ci
    repo: an-lee/hangar
    host: vps-1
    count: 2
    labels: [self-hosted, Linux, X64]
    runner_mode: container

  # GitHub Agentic Workflows — profile: agentic always uses container mode (DinD).
  - name: hangar-aw
    repo: an-lee/hangar
    host: vps-1
    profile: agentic
    count: 2

  # Organization-level runner — shared across all repos in the org.
  # Create runner groups in GitHub org settings before referencing group:.
  - name: myorg-ci
    org: my-org
    group: ci-pool
    host: vps-1
    count: 4
    labels: [self-hosted, Linux, X64]
    runner_mode: container
```

See [Organization runners](guides/org-runners.md) for runner groups, workflow targeting, and migrating from per-repo runners.

## Config reference

| Field | Description |
|---|---|
| `hosts.<name>.addr` | SSH target (`user@host` or `user@ip`), or `local` to run on the machine where gh sr is running. Remote commands run as that user; on Linux, privilege expectations for `setup` / `update` follow [Linux SSH user and privileges](host-setup.md#linux-ssh-user-and-privileges). |
| `hosts.<name>.os` | `linux`, `darwin`, or `windows`. Auto-detected when `addr` is `local`. **Only these three values are accepted** — anything else (typos like `Linux` / `Darwin`, or unsupported OSes like `freebsd` / `illumos`) now surfaces an explicit `unsupported host OS %q` error from disk-prune and related paths instead of silently using the POSIX implementation. |
| `hosts.<name>.arch` | `amd64` or `arm64`. Auto-detected when `addr` is `local`. |
| `hosts.<name>.windows_ps` | Optional; **Windows hosts only.** Which executable runs remote PowerShell payloads: `powershell` (default, `powershell.exe`) or `pwsh` (`pwsh.exe`). gh sr uses `-EncodedCommand` so the user’s SSH default shell (cmd.exe or pwsh) does not break nested quoting. |
| `runners[].name` | Base name (instances become `name-1`, `name-2`, ...) |
| `runners[].repo` | GitHub `owner/repo`. Required unless `org` is set. Mutually exclusive with `org`. |
| `runners[].org` | GitHub organization name. Use instead of `repo` for org-level runners shared across all repos in the org. Mutually exclusive with `repo`. See [Organization runners](guides/org-runners.md). |
| `runners[].group` | Runner group name (org-level runners only). Passed as `--runnergroup` during registration. The group must already exist in GitHub org settings. |
| `runners[].host` | References a key under `hosts` |
| `runners[].count` | Number of parallel instances (default: 1) |
| `runners[].labels` | Labels for workflow `runs-on` matching. Include **`agentic`** when the runner should serve [GitHub Agentic Workflows](https://github.github.com/gh-aw/). With **`profile: agentic`**, the `agentic` label is added automatically if omitted. |
| `runners[].runner_mode` | `native` (default) or `container`. `container` runs each runner instance in its own privileged Docker container with an inner dockerd (DinD), fully isolating `/tmp/gh-aw`, the MCP gateway, and job networking between concurrent jobs on the same host. The image is built locally by `gh sr setup` on a fork actions-runner base and includes Docker CE, Node.js LTS, zstd, gh, and the actions runner. **`profile: agentic` always uses `container` mode** (and `runner_mode: native` + `profile: agentic` is rejected). See [Agentic Workflows](guides/agentic-workflows.md). |
| `runners[].profile` | Optional. Set to **`agentic`** for [GitHub Agentic Workflows](https://github.github.com/gh-aw/): implies `runner_mode: container`, adds the `agentic` label, and bakes the rootless-sandbox tooling into the locally built runner image. See [Host setup — GitHub Agentic Workflows](host-setup.md#github-agentic-workflows-gh-aw). |
| `runners[].ephemeral` | Optional boolean. When `true`, the runner handles one job and deregisters. Container mode uses `--restart no`; native passes `--ephemeral` to `config.sh`. |
| `runners[].awf_service_bridge` | Optional boolean, **requires `profile: agentic`**. Arms the AWF service bridge: a per-job waiter (armed by the job-started hook, killed by the job-completed hook) that joins the job's GitHub Actions `services:` containers to the AWF topology network (`awf-net`) once it appears, so the sandboxed agent reaches them by service name over native TCP. Needed because under gh-aw v0.88+ network isolation the agent has no route to runner-published service ports (the documented `services:` + `host.docker.internal` + `docker-sudo-iptables` pattern only works on GitHub-hosted VM runners). Declare `services:` without port mappings and point workflow env at the service keys (e.g. `DATABASE_HOST: postgres`). See [Agentic Workflows — service bridge](guides/agentic-workflows.md#service-bridge-services-containers-in-the-sandbox). |
| `runners[].agentic_mcp_ports` / `runners[].agentic_mcp_port_base` | **Removed.** The per-instance MCP port-label scheme is no longer used: container mode isolates the MCP gateway port per runner. `gh sr` rejects these fields with a migration message — delete them. |
| `container_runner_image.base_image` | Optional. Base image the locally built container runner image derives FROM. Empty = the built-in default (`ghcr.io/falcondev-oss/actions-runner:v2.337.0` — a fork build whose runner redirects `CUSTOM_ACTIONS_RESULTS_URL` cache traffic to the per-host cache server). Pin by digest for reproducibility. Changing it changes the image layout revision, so existing containers need `gh sr rebuild <name>`. |
| `container_runner_image.extra_apt_packages` | Optional list of additional Debian package names to install in the locally built container runner image (`runner_mode: container`). At most 256 entries; each name must match `[a-z0-9][a-z0-9+.-]*` (max 200 chars). When set, the image tag gains a `-x<8-hex>` suffix so Docker does not reuse an image built without those packages. Core packages are in the repo manifest `internal/runner/agentic-runner-image/apt-packages-core.txt`. |
| `container_runner_image.mtu` | Optional integer (576–1500). Forces the Docker network MTU for `runner_mode: container` — both the outer runner container's egress interface and the inner `dockerd` bridge. Leave unset (0) to **auto-detect** the host's egress MTU, which fixes the common reduced-MTU case (cloud overlay networks like GCP's 1460, VPN/WireGuard) where large-packet TLS handshakes otherwise fail with `Client network socket disconnected before secure TLS connection was established` (e.g. `actions/setup-go`). Set this only when the host's real path MTU is below its NIC MTU (a tunnel the NIC is unaware of) so auto-detection cannot see it. Only ever lowers the MTU; applied at container-create time, so changing it requires `gh sr rebuild <name>`. |
| `container_runner_image.dockerd_start_timeout_seconds` | Optional integer (30–300). Seconds the entrypoint waits for inner `dockerd` during bootstrap. Default **90** when unset. Increase on slow or I/O-bound hosts. Applied at container-create time (`gh sr rebuild` to change existing containers). |
| `container_runner_image.bootstrap_max_retries` | Optional integer (1–20). Consecutive inner-`dockerd` start failures before the entrypoint stops retrying and marks the runner **failed** (default **5**). Applied at container-create time. |
| `container_runner_image.start_stagger_seconds` | Optional integer (0–60). Delay multiplier between starting each container instance on the same host during `gh sr up` (default **3** — instance *i* waits *i×3* seconds). Reduces lockstep dockerd spikes when `count > 1`. |

### `cache:` — per-host local Actions cache server

Optional section tuning the [local Actions cache server](guides/local-cache.md) (one `gh-sr-cache` container per Linux host, deployed automatically by `gh sr setup` / `up` / `update` / `rebuild`). Every field is optional; the whole server is disabled with `enabled: false`.

| Field | Meaning |
|---|---|
| `cache.enabled` | Deploy and wire the local cache server (default **`true`**). `false` keeps GitHub's cache service. |
| `cache.port` | Host-side published port (default **27420** — fixed and uncommon so it does not collide with dev services or the ephemeral range). |
| `cache.bind_addr` | Host address the server binds to. Empty = the **docker0 gateway IP** (containers only); `0.0.0.0` exposes the cache API on all interfaces (doctor warns). On hosts with a default-deny INPUT firewall, allow the published port from docker0 — `gh sr doctor` prints the exact rule when blocked. |
| `cache.storage_path` | Host directory for cached data (`$HOME/...` allowed; default `~/.gh-sr/cache`). |
| `cache.retention_days` | Drop entries older than N days (0 = server default 90). |
| `cache.max_size_bytes` | Cap total cache size in bytes (0 = unbounded). |
| `cache.max_usage_percent` | Evict when filesystem usage exceeds N percent (0 = server default 90). |
| `cache.image` | Cache-server image override (default `ghcr.io/falcondev-oss/github-actions-cache-server:latest`; pin a digest for reproducible deploys). |
| `cache.management_api_key` | Management API key for `gh sr cache prune` (supports `env:VAR` refs). Empty = auto-generate and persist one in the storage dir. |
| `cache.url_override` | Replace the runner-facing cache URL verbatim (must include the scheme) — escape hatch for exotic topologies. |

**Unique runner names per registration scope:** GitHub registers each self-hosted runner by its **instance** name (`name-1`, `name-2`, …). That name must be **unique within the registration scope** — within a given `owner/repo` for repo-scoped runners, or **org-wide** for org-scoped runners. If two machines use the same base `name` and `count: 1` in the same scope, both try to register as `name-1` and only one registration remains active. Prefer distinct base names (for example `myapp-win` vs `myapp-linux`, or `myorg-ci` vs `myorg-gpu`) so every machine has its own GitHub runner record and `gh sr status` matches the right row.

## Recent behavior changes

These are user-visible changes to how `gh sr` interprets config. Internal refactors, test additions, and microbenchmarks are not listed here.

- **GitHub registration URL now has a single source of truth** (PR #142) — Both the native (`config.sh` / `config.cmd`) and container (`GH_SR_RUNNER_URL`) registration paths now go through `(*RunnerConfig).GitHubRegistrationURL()`. When both `repo` and `org` are set in a runner block, **`org` wins** in both modes — previously the two paths disagreed (native was Org-first, container was Repo-first), so the same config registered against different GitHub URLs depending on `runner_mode`. This is the same precedence used by `Scope()` / `ScopeTarget()`. If you had `org` and `repo` set together, treat that as a config error and remove the field you don't want.
- **Unsupported `hosts.*.os` values now error explicitly** (PR #141) — Disk-prune and related disk-management paths used to silently fall into the POSIX branch for any unknown `h.OS` value. They now return an explicit `unsupported host OS %q` error for values outside `{linux, darwin, windows}` (typos like `Linux` / `Darwin`, or unsupported OSes like `freebsd` / `illumos`). Fix `hosts.*.os` in `runners.yml` if you see this error.
