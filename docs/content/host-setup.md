---
title: "Host Setup"
weight: 40
---

# Host setup (manual steps)

**gh sr** automates runner installation and lifecycle over SSH, but some host preparation is still manual. After you edit config, run **`gh sr doctor`** from your laptop to verify config paths, GitHub API access, SSH connectivity, and per-host tools (Docker vs native). By default the command exits with a non-zero status only when a check is **FAIL**; use **`gh sr doctor --strict`** if you also want **WARN** lines to fail (for example in CI).

## Linux SSH user and privileges

`gh sr setup` and `gh sr update` run remote commands as the SSH user in `hosts.*.addr`. On **Linux**, when a step needs **root** (package installs, GitHub’s `installdependencies.sh` for native runners, or Docker’s install script when Docker is missing), **gh sr** uses **`sudo -n`** only — non-interactive `sudo` that never prompts for a password.

**Why `sudo -n`:** Remote automation uses SSH sessions **without a PTY** (no interactive terminal), the same way `ssh` runs a remote command by default. Interactive `sudo` would try to read a password from a TTY and fails with errors such as _“a terminal is required to read the password”_. So **gh sr** requires **SSH as root** or **passwordless sudo** (`sudo -n true` must succeed on the host) for those install steps.

**If privilege checks fail**, remote stderr may include:

```text
gh sr: remote Linux commands need root SSH or passwordless sudo (non-interactive); SSH has no TTY for sudo passwords. Use NOPASSWD, connect as root, or install software manually. Run: gh sr doctor
```

Run **`gh sr doctor`** on your laptop to check Linux hosts: it prints **“linux: non-root user has passwordless sudo”** when `sudo -n` works, or **“linux: passwordless sudo not available…”** when it does not. Use **`gh sr doctor --strict`** if you want that warning to fail the command.

### Setting up the SSH user on Linux

- **Dedicated user + SSH keys** — Create a user on the runner host, authorize your public key in `~/.ssh/authorized_keys`, and set `hosts.*.addr` to `user@host` (or hostname).
- **`root@host` in `hosts.*.addr`** — Avoids sudo entirely for **gh sr**’s install paths. Only use if your security policy allows SSH as root; prefer key-based auth, disable password login, and restrict network access.
- **Passwordless sudo (`NOPASSWD`)** — On the host, use `visudo` or a drop-in under `/etc/sudoers.d/`. A broad rule such as `runner ALL=(ALL) NOPASSWD: ALL` is common for a **dedicated CI user**. **Command-scoped NOPASSWD** is hard to maintain for Docker’s `get.docker.com` script and `installdependencies.sh` because they run many different commands; if you cannot grant broad NOPASSWD, use **`root@host`** or **pre-install** software so **gh sr** does not need those scripts.
- **Pre-install to limit elevation** — Install **Docker** before `gh sr setup` and add the SSH user to the **`docker`** group so `docker` works without `sudo`. For **native** mode, ensure **`curl`** and **`tar`** are on `PATH` and install distro packages the runner needs in advance; that reduces how often `installdependencies.sh` must run with sudo.

**Verify from your laptop** (same `user@host` as in `hosts.*.addr`):

```bash
ssh -o BatchMode=yes user@host true
ssh -o BatchMode=yes user@host 'sudo -n true'
```

The first command checks non-interactive SSH (see also [All remote hosts](#all-remote-hosts)). The second must exit **0** if you rely on a non-root user with sudo for automated installs; if it fails, configure `NOPASSWD` for that user or use `root@host`.

- **Container mode on Linux** — `runner_mode: container` and `profile: agentic` require Docker on the host and support for `--privileged` containers (Docker-in-Docker). Install Docker before `gh sr setup` when possible and add the SSH user to the **`docker`** group so `docker` works without `sudo`. Container mode is **Linux-only**; configuring container or agentic runners on macOS or Windows fails at setup and doctor reports a FAIL.
- **Native mode** — You can avoid `sudo` if `curl` and `tar` are present and OS packages the runner needs are already installed; otherwise **gh sr** may print warnings and the runner might be incomplete.

**gh sr** does not deeply verify sudoers rules; failures show up as remote command errors or warnings.

### Container mode host requirements (Linux)

`runner_mode: container` and `profile: agentic` run each instance as an outer privileged container (`gh-sr-<instance>`) with its own inner `dockerd`. The host only needs:

- Docker CLI and daemon available to the SSH user (typically via the `docker` group)
- Ability to run short-lived **`docker run --privileged`** containers (required for DinD)

gh sr builds the `gh-sr/agentic-runner` image locally during setup from a fork runner base. Docker CE, Node.js LTS, zstd, and the per-job reset hooks live **inside** the image — not on the host; gh-aw and AWF are not preinstalled (jobs install their own tooling), and `host.docker.internal` needs no baking (inner job containers pass `--add-host=host.docker.internal:host-gateway`).


**Verify from inside a running runner container:**

```bash
docker exec gh-sr-<instance> docker info
```

`gh sr doctor` validates container-mode runners on **Linux** hosts: outer Docker, `--privileged` support, each `gh-sr-<instance>` container, inner `dockerd`, registration, and (for `profile: agentic`) the rootless-sandbox checks — node/npm and zstd on PATH, docker-socket access for the runner user, the inner `host.docker.internal` alias, local-cache env wiring, MTU pinning, and inner-Docker hygiene. On **non-Linux** hosts with container-mode runners configured, doctor reports a FAIL because container mode is unsupported there.

## GitHub Agentic Workflows (gh-aw)

> **For a complete guide** to agentic workflows on self-hosted runners -- architecture, configuration, troubleshooting, and what `profile: agentic` automates -- see [Agentic Workflows]({{< ref "agentic-workflows" >}}).

The easiest way to set up a runner for [GitHub Agentic Workflows](https://github.github.com/gh-aw/guides/self-hosted-runners/) is `profile: agentic`:

```yaml
runners:
  - name: aw-runner
    repo: owner/repo
    host: vps-1
    profile: agentic
    count: 2
```

`profile: agentic` always runs in **container mode** (privileged Docker-in-Docker): each instance gets its own inner `dockerd`, network namespace, MCP gateway port, and `/tmp/gh-aw`, and every job runs from a pristine inner state. The host only needs Docker (with privileged-container support); gh-aw, AWF, `host.docker.internal` DNS, the tool cache, and language runtimes are all baked into the image `gh sr` builds. You can also add it from the CLI:

```bash
gh sr add runner aw-runner --repo owner/repo --host vps-1 --profile agentic
```

For **organization-level runners** with runner groups, use `org` and `group` instead of `repo`:

```yaml
runners:
  - name: org-runner
    org: my-org
    group: my-runner-group
    host: vps-1
    profile: agentic
```

For **ephemeral runners** (clean-slate per job, good for security isolation):

```yaml
runners:
  - name: aw-ephemeral
    repo: owner/repo
    host: vps-1
    profile: agentic
    ephemeral: true
```

### How it works

Because agentic runners are container-isolated, the host needs only Docker with privileged-container support. Everything else is inside the image, which derives from a fork actions-runner base image:

- **`host.docker.internal` DNS** — gh-aw v0.88+ job containers are started with `--add-host=host.docker.internal:host-gateway`, so the alias resolves via the inner Docker bridge automatically. No host `/etc/hosts`, dnsmasq, or `daemon.json` changes are required. **Do NOT** map `host.docker.internal` to `127.0.0.1` anywhere.
- **No `sudo` / iptables** — the gh-aw sandbox runs **rootless** (default `docker` runtime profile): the AFW firewall, Squid proxy, and MCP gateway run as unprivileged containers on the inner bridge topology. The image removes all passwordless-sudo grants.
- **Docker socket access** — the `runner` user is in the base image's `docker` group so the `awmg-mcpg` gateway can mount and use `/var/run/docker.sock`.
- **Tool cache (`/home/runner/.toolcache`)** — provided inside the image where gh-aw's agent containers expect it. Lives outside `/opt/*` so gh-aw's compiled AWF invocation actually bind-mounts it into the agent container (gh-aw deliberately skips any `RUNNER_TOOL_CACHE` whose path matches `/opt/*` to avoid leaking `/opt/gh-aw` and similar control-plane paths). A compatibility symlink at `/opt/hostedtoolcache → /home/runner/.toolcache` is also baked into the image so legacy actions that hardcode the legacy path (notably `ruby/setup-ruby@v1`, which ignores `$RUNNER_TOOL_CACHE` when it doesn't detect a self-hosted runner) still find a writable tool cache and do not regress to `EACCES`.
- **`RUNNER_TEMP`** — set to a non-`/tmp` path inside the container so it never collides with `/tmp/gh-aw`.
- **Local Actions cache** — a per-host `gh-sr-cache` container is deployed automatically (`cache.enabled`, default on) and `CUSTOM_ACTIONS_RESULTS_URL` is wired into the runner. See [Local Actions cache](guides/local-cache.md).

`gh sr doctor` validates the host Docker daemon, `--privileged` support, and — for each instance — the container, inner `dockerd`, registration, inner `host-gateway` alias resolution, node/zstd/docker-socket access, rootless hygiene (orphan `awmg-mcpg`/`awf` containers and networks), the local cache server, and (with `--check-lockfiles`) stale compiled workflows.

See the [Agentic Workflows guide]({{< ref "agentic-workflows" >}}) for the full architecture, the per-job reset model, and troubleshooting.

> **Removed:** the former native-agentic host setup (host dnsmasq, `/etc/sudoers.d` for iptables, `/opt/hostedtoolcache` bind mount) and the per-instance `agentic_mcp_ports` / `gh-sr-mcp-<port>` label scheme. `profile: agentic` + `runner_mode: native` is now rejected; agentic always uses container isolation.

### GitHub CLI authentication (gh)

Agentic workflows that use `gh` CLI commands (e.g., `gh pr list`, `gh issue list`, `gh api`) require GitHub authentication on the runner host. **GitHub Hosted Runners** have this built-in, but **self-hosted runners need manual setup**.

**Symptoms:** Workflow steps using `gh` fail with errors like:

- `gh: You are not logged into any GitHub hosts`
- `gh: To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN environment variable`
- `Bad credentials` from the GitHub API

**Solution:** Set the `GH_TOKEN` environment variable in the workflow, or authenticate `gh` on the runner host.

**Option 1 — Pass `GH_TOKEN` in the workflow** (recommended for most cases):

```yaml
jobs:
  agentic:
    runs-on: self-hosted
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    steps:
      - uses: actions/checkout@v6
      - name: List PRs
        run: gh pr list
```

**Option 2 — Authenticate `gh` on the runner host** (for local `gh` commands outside workflow steps):

```bash
# On the runner host, authenticate gh with a Personal Access Token
gh auth login --hostname github.com --token <your-PAT>
```

> **Note:** `gh auth login` is interactive by default. For non-interactive setup, use:
>
> ```bash
> printf '<your-token>' | gh auth login --hostname github.com --insecure-stdin-token
> ```

**Verify on the runner host:**

```bash
gh auth status
```

## Windows (OpenSSH and Docker)

- **Windows** — OpenSSH Server enabled for native runners:

  ```powershell
  Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
  Start-Service sshd
  Set-Service -Name sshd -StartupType Automatic
  ```

  **SSH default shell:** OpenSSH may use cmd.exe or PowerShell 7 (`pwsh`) as the remote shell depending on your setup. gh sr runs Windows automation via `powershell.exe` or `pwsh.exe` with `-EncodedCommand`, so it works with either default. Use `windows_ps: pwsh` on the host if you rely on PowerShell 7 only and do not have Windows PowerShell 5.1.

## All remote hosts

- Confirm non-interactive SSH works: `ssh -o BatchMode=yes user@host true` (use the same user and host as in `hosts.*.addr`).
- **Host keys:** when `~/.ssh/known_hosts` exists, gh sr verifies server keys the same way as the Go `knownhosts` package. Connect once with plain `ssh` if you need to accept a new host key before `gh sr doctor` or `gh sr setup`.
- Remote commands run as the SSH user from your config; that user must have permission to install or run runners as documented for each OS below.

Run **`gh sr doctor`** (optionally with `--host` / `--repo`) to confirm connectivity from your control machine.

## Linux

For the full explanation of non-interactive SSH, the `gh sr: remote Linux commands need root…` error, `NOPASSWD`, and verification commands, see [Linux SSH user and privileges](#linux-ssh-user-and-privileges) above.

- **Container mode (`runner_mode: container` / `profile: agentic`):** Requires Docker on the host and `--privileged` container support. If Docker is missing, **`gh sr setup` installs it automatically** via [get.docker.com](https://get.docker.com) (requires **root** or **passwordless sudo** over SSH — see [Linux SSH user and privileges](#linux-ssh-user-and-privileges)). After a fresh install, gh sr adds the SSH user to the **`docker`** group and asks you to **run setup again**; the second invocation opens a new SSH session where `docker` works without sudo. Pre-installing Docker yourself still works and skips the two-run flow.
- **Native mode (default):** Ensure `curl` and `tar` are on `PATH`. `gh sr setup` may still invoke GitHub's `installdependencies.sh` with **`sudo -n`** when the SSH user is not root and passwordless sudo is available.

Run **`gh sr doctor`** after the host is prepared.

## macOS

- **Native (default):** `curl` is usually sufficient; gh sr downloads the Actions runner over HTTPS.
- **Container / agentic:** `runner_mode: container` and `profile: agentic` are **not supported on macOS hosts**. Use a Linux host for agentic workflows or container-isolated runners.

## Windows (runner behavior)

- Install and enable **OpenSSH Server** (see the PowerShell snippet under [Windows (OpenSSH and Docker)](#windows-openssh-and-docker)).
- **Container / agentic:** Not supported on Windows hosts. Use a Linux host for `profile: agentic` or `runner_mode: container`.
- **Native mode:** After `gh sr setup`, run **`gh sr up`** to start the listener. Registration logs under `_diag` show **configure** finishing with exit code 0; that is normal and does not mean the runner is listening. For the **run** phase, check **`%USERPROFILE%\.gh-sr\runners\<instance>\runner.log`**. Over SSH, **`Start-Process` listeners are killed when the session disconnects**; gh sr starts the listener with **`Win32_Process.Create` (CIM)** so it keeps running after `gh sr` exits. If the runner shows **stopped** with `runner registration has been deleted from the server`, GitHub auto-pruned the stale registration. `gh sr up` detects this automatically, re-registers, and retries. You can also fix it manually with **`gh sr update <runner>`** (which removes, re-configures, and starts in one step). If `gh sr status` shows **stopped** immediately after `gh sr up` for other reasons, check `runner.log` for error details. As a fallback you can run **`run.cmd`** from an interactive session (RDP), use **Task Scheduler** at logon, or install the runner as a **Windows service** via GitHub’s `config.cmd` options.

Run **`gh sr doctor`** to confirm SSH readiness (and native prerequisites on non-Linux hosts).
