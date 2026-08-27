# Self-hosted runner manager for GitHub (`gh sr`)

**Self-hosted runner manager for GitHub** is a [GitHub CLI](https://cli.github.com/) extension for managing self-hosted [GitHub Actions](https://docs.github.com/en/actions) runners across many machines from one place—usually your laptop. It connects over **SSH** (or runs **locally** with `addr: local`), installs and starts runners, and gives you a **terminal UI** for day-to-day operations.

**Repository:** [github.com/an-lee/gh-sr](https://github.com/an-lee/gh-sr)  
**Go module:** `github.com/an-lee/gh-sr` (used by `go install` and imports).

## Features

- One set of commands for **Linux**, **macOS**, and **Windows** runners (`setup`, `up`, `down`, `status`, `logs`, `service`, …).
- **Declarative YAML** for hosts and runners; GitHub API auth via **`gh auth login`** only.
- **Docker or native** runners per row, with sensible OS defaults and overrides.
- **Multi-host** from a single config; optional **TUI** dashboard (`gh sr` or `gh sr dashboard` on a TTY).

## Dashboard

```
  gh sr dashboard                              [r]efresh  [?]help  [q]uit

  INSTANCE      HOST         REPO                MODE    LOCAL    GITHUB
  ─────────────────────────────────────────────────────────────────────────
  runner-1      mac-mini     org/backend         native  running  online
  runner-2      linux-vps    org/frontend        docker  running  busy
  runner-3      win-pc       org/infra           native  stopped  offline

  [enter] runner actions  [g] global menu  [f] filter  [j/k] navigate
```

## Documentation

**Full documentation (user guide, architecture, config reference):**  
[https://an-lee.github.io/gh-sr/](https://an-lee.github.io/gh-sr/)

The Markdown sources live under [`docs/`](docs/) in this repo and are built with [Hugo](https://gohugo.io/) (Hugo Book theme).

## Install

Install as a GitHub CLI extension (recommended):

```bash
gh extension install an-lee/gh-sr
```

Then use **`gh sr`** (for example `gh sr status`). Requires [GitHub CLI](https://cli.github.com/) (`gh`) and a recent `gh` version.

From source with Go:

```bash
go install github.com/an-lee/gh-sr/cmd/gh-sr@latest
```

Pin a specific release (same `v*` tags as [GitHub Releases](https://github.com/an-lee/gh-sr/releases)):

```bash
gh extension install an-lee/gh-sr --pin v1.2.0
go install github.com/an-lee/gh-sr/cmd/gh-sr@v1.2.0
```

The resulting binary is named `gh-sr`. To invoke it as **`gh sr`**, install the extension as above, or run `gh-sr` directly if it is on your `PATH`.

Requires a recent **Go** toolchain when building from source (see `go.mod` / CI for the exact version).

## Quick start

```bash
gh auth login           # required: gh sr uses the GitHub CLI token
gh sr init --quick        # interactive: prompts for repo + host, auto-detects everything else
gh sr up                  # auto-setup + start (all in one)
gh sr status
```

Or step by step:

```bash
gh sr init                            # create ~/.gh-sr with template config
gh sr add host my-vps root@10.0.0.1  # add a host (os/arch auto-detected)
gh sr add runner ci --repo owner/repo --host my-vps  # add a runner (labels auto-generated)
gh sr up                              # auto-setup + start
```

Then run `gh sr` on a terminal for the interactive dashboard, or use `gh sr status`, `gh sr logs <name>`, etc.

## GitHub Agentic Workflows (gh-aw)

To run [GitHub Agentic Workflows](https://github.github.com/gh-aw/guides/self-hosted-runners/) on your self-hosted runners, use the `agentic` profile:

```yaml
runners:
  - name: aw-runner
    repo: owner/repo
    host: my-vps
    count: 3           # 3 concurrent agentic jobs, each isolated
    profile: agentic   # always runs in container (DinD) mode
```

`profile: agentic` always runs in **container mode**: each runner instance is a privileged Docker-in-Docker container with its own inner `dockerd`, network namespace, MCP gateway port, and `/tmp/gh-aw`, and every job runs from a pristine inner state (via the runner's job hooks). This is what makes multiple concurrent agentic runners stable on one machine.

The **host only needs Docker** (with privileged-container support). Everything `gh-aw` needs — the `gh-aw` CLI, AWF, `host.docker.internal` DNS, the tool cache, language runtimes — lives inside the image `gh sr` builds during setup. There is no host dnsmasq, `/etc/hosts`, sudoers, or `RUNNER_TEMP` setup to do; `host.docker.internal` resolution is baked into the image (pinned bridge gateway `10.200.0.1` + bundled dnsmasq). `gh sr doctor` verifies the inner Docker, registration, DNS, and AWF hygiene.

For details, see the [Agentic Workflows guide](https://an-lee.github.io/gh-sr/guides/agentic-workflows/).

## Development

```bash
make check    # vet + tests (same idea as CI)
# or: go vet ./... && go test ./... -race -count=1

make bench    # run all benchmarks with memory stats
# or: go test ./... -run='^$' -bench=. -benchmem -count=3
```

Repository layout is described in [docs/reference/file-structure.md](docs/reference/file-structure.md).

### Git blame

The repo ships with a [`.git-blame-ignore-revs`](.git-blame-ignore-revs) file so `git blame` skips large code-movement commits (refactors, helper extractions, SSH round-trip folds) and surfaces the original author. Run `make blame-ignore` once to configure your local repo. GitHub's web UI picks up the file automatically.

## Publishing documentation (maintainers)

Documentation deploys to GitHub Pages via **GitHub Actions** when changes land on `main`. In the GitHub repo, enable **Settings → Pages → Build and deployment → Source: GitHub Actions** once (no branch-based `/docs` hosting required for the built site).

Local preview:

```bash
# from the repository root:
git clone https://github.com/alex-shpak/hugo-book docs/themes/book --depth 1
cd docs && hugo server
```

Build (also run in CI):

```bash
cd docs && hugo --minify
```
