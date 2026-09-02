# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release builds are tagged as `vMAJOR.MINOR.PATCH`. Each Git tag triggers a
[GitHub Release](https://github.com/an-lee/gh-sr/releases); the workflow also
generates release notes on publish. Use this changelog for a concise, curated
history when you want more than the auto-generated GitHub summary.

## [Unreleased]

### Removed

- **Drop `gh-aw` (GitHub Agentic Workflows) support entirely.** The `--profile` flag, the `internal/agentic` package (≈3,300 lines including tests), the `agentic-runner-image/` directory, and the auto-append of the `agentic` label are removed. Container mode stays for CI services isolation (`services:` blocks like postgres/redis run inside the runner container with their own inner `dockerd`). The embedded runner image is replaced: `gh-sr/agentic-runner:<version>` → `gh-sr/container-runner:<version>`, layered on [`docker:dind`](https://hub.docker.com/_/docker) plus the [`actions/runner`](https://github.com/actions/runner) binary. `gh sr setup` and `gh sr up` detect pre-rename images and print a one-line migration hint (`docker rmi gh-sr/agentic-runner:<v>`). Agent workflows in user projects should migrate from `baizhiheizi/agents/.github/workflows/agent-*.yml` (gh-aw reusable workflows) to [`anthropics/claude-code-action@beta`](https://github.com/anthropics/claude-code-action). See the migration notes in the change proposal.

### Added

- Organization-level self-hosted runners are now first-class: a dedicated [Organization runners](https://an-lee.github.io/gh-sr/guides/org-runners/) guide covers repo vs org scope, runner groups, workflow targeting, authentication, security, and migration from per-repo pools. The `org` / `group` fields in `runners[].yml` are documented in the config reference, an org-scoped runner example is added to `config/runners.yml` and `internal/config/runners.yml.template`, and a new "Organization runners" entry is registered in the guides index. `gh sr runner add --org` / `--group` and the existing `org:` / `group:` config fields are the public entry points; `gh sr doctor` reports `org <name>: list runners OK (<n> registered)` and prints an `admin:org` / org-owner hint on 403. `gh sr status` and the TUI now show a uniform `target=org:<name> [group=<name>]` (or `target=owner/repo` for repo-scoped runners) via a new `RunnerConfig.DisplayTarget()` helper. (#187)
- Added `BenchmarkLoad_Small`, `BenchmarkLoad_Large`, `BenchmarkValidate_Small`, and `BenchmarkValidate_Large` benchmarks in `internal/config` to establish a performance baseline for config loading and validation. Run with `make bench`. (#37)

### Changed

- `gh sr down` on container-mode runners is faster for multi-instance configs: `docker stop` and `docker rm -f` now run in a single SSH round-trip per instance instead of two, reducing round-trips from 4N to 3N (~25% fewer SSH calls for an N-instance teardown). (#264)
- `gh sr doctor` now runs host SSH checks and GitHub API checks (repos and orgs) concurrently, reducing wall-clock time from O(N × latency) to O(latency) for configurations with multiple hosts or targets. Output within each section is printed in sorted order. (#33)
- `gh sr service install`, `gh sr service uninstall`, and `gh sr service status` now open one SSH connection per host concurrently via `runPerHostParallel`, matching the behavior already in place for `up`, `down`, `restart`, `update`, and `status`. For single-host configs there is no observable difference; for multi-host configs completion time drops from O(N × SSH_latency) to O(SSH_latency). Output order across hosts is non-deterministic. (#30)

### Fixed

- Container-mode runners now pin their Docker MTU to the host's egress MTU, fixing workflow downloads (e.g. `actions/setup-go`, `actions/setup-node`) that failed with `Client network socket disconnected before secure TLS connection was established` on hosts whose real path MTU is below 1500 (cloud overlay networks such as GCP's 1460 default, VPN/WireGuard, nested virtualisation). The outer container's `eth0` and the inner `dockerd` bridge previously kept Docker's 1500 default; with PMTUD black-holed, large TLS-handshake packets were silently dropped while small packets (and the host itself) worked. `gh sr` auto-detects the host egress MTU at container-create time and `entrypoint.sh` applies it to both layers (plus an MSS clamp for forwarded inner traffic). Run `gh sr rebuild <name>` to adopt the fix. A `container_runner_image.mtu` override is available for hosts where a tunnel lowers the path MTU below the NIC MTU, and `gh sr doctor` flags a stale image as `container-mtu`.
- The agentic runner image now bundles **Node.js LTS** (NodeSource) and `npm` on `PATH` for `gh-aw` activation setup. `gh-aw` v0.79+ enables `safe-output-artifact-client` during activation, which runs `npm install` *before* `actions/setup-node`; a stale image with no `npm` aborted with `npm is not available. Cannot install @actions/artifact package.` Run `gh sr rebuild <name>` to pick up the fix. `gh sr doctor` flags a stale image as `container-node-npm` (see the [Agentic Workflows guide](https://an-lee.github.io/gh-sr/guides/agentic-workflows/) → §6 Health checks / §8 Troubleshooting).

### Removed
