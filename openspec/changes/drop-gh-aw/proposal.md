## Why

GitHub Agentic Workflows (gh-aw) have proven brittle in self-hosted setups despite repeated patches (most recently the 2026-09-02 inner-dockerd pid-file recovery and SessionConflict handling). The `internal/agentic/` package, the embedded `agentic-runner` image, and the `profile: agentic` config path together are the source of most ongoing maintenance burden in this repo. Meanwhile, `anthropics/claude-code-action` now provides a simpler replacement: it runs as a normal Actions step, needs no special tooling, and removes the rationale for gh-aw's per-instance `/tmp/gh-aw`, AWF iptables, MCP gateway port, and `RUNNER_TEMP` non-`/tmp` machinery.

## What Changes

- **BREAKING** Delete the entire `internal/agentic/` package (~3,300 lines including tests). Its public functions (`ValidatePrereqs`, `ValidateContainerPrereqs`, `ValidateAWFHygieneInner`, `ValidateContainerInnerNetwork`, `ValidateContainerAgenticFanout`) and the `PrereqFailure` type are removed.
- **BREAKING** Remove `RunnerConfig.Profile`, `IsAgentic()`, `EffectiveRunnerMode()`'s agentic → container forcing branch, and the deprecated `AgenticMCPPorts` / `AgenticMCPPortBase` field stubs in `internal/config/config.go`.
- **BREAKING** Remove the `gh sr runner add --profile` flag and the "agentic profile" branch in the setup prompt and doctor long description in `cmd/gh-sr/main.go`.
- **BREAKING** Remove the auto-append of the `agentic` label in `internal/config/mutate.go`.
- **BREAKING** Remove the agentic-specific preflight in `internal/doctor/doctor.go` (`printAgenticFailures`, `ValidateContainerPrereqs`/`ValidateAWFHygieneInner`/`ValidateContainerAgenticFanout` call sites).
- **BREAKING** Delete the embedded image directory `internal/runner/agentic-runner-image/` (Dockerfile, entrypoint.sh, docker-wrapper.sh, daemon.json, dnsmasq-gh-sr.conf, hooks/, apt-packages-core.txt).
- **BREAKING** Replace the embedded image build with a thin wrapper image based on `docker:dind` + the `actions/runner` entrypoint. Same approach as GitHub's official [`actions/runner-docker`](https://github.com/actions/runner-docker) DinD variant. Container-mode runners keep DinD because the user's CI workflow (`ohmyxin/ci.yml`) needs isolated `services:` (postgres, redis) for 4-shard parallelism.
- Rename `AgenticRunnerImageTag` constant to `ContainerRunnerImageTag` and the docker image label `dockerLabelImageRevision` value's namespace to `gh-sr-container-image/v1` (currently `gh-sr-agentic-image/v1`).
- **BREAKING** Drop the `profile: agentic` example runner from `config/runners.yml`.
- **BREAKING** Delete gh-sr's own consumer workflows under `.github/workflows/`: all `*-*.md` and `*.lock.yml` files (perf-improver, test-improver, doc-updater, grumpy-reviewer, efficiency-improver, duplicate-code-detector, repo-assist), plus `agentic_commands.yml` and `agentics-maintenance.yml`.
- **BREAKING** Delete `.github/agents/agentic-workflows.md` (the Claude Code dispatcher agent for gh-aw).
- Rewrite `.github/workflows/copilot-setup-steps.yml` to remove the `github/gh-aw-actions/setup-cli` reference; keep the Copilot setup steps that don't depend on gh-aw, or remove the workflow entirely if Copilot setup has another home.
- **BREAKING** Delete `docs/content/guides/agentic-workflows.md` (the 205-line agentic guide).
- Update remaining docs (`architecture.md`, `configuration.md`, `commands.md`, `host-setup.md`, README.md, CHANGELOG.md) to drop gh-aw references, rename the image to a generic container-mode name, and remove the `--profile` flag from command tables.
- Pool semantics: the `agent` and `ci` runner label pools collapse to a single pool. User projects that today label workflows `[self-hosted, Linux, X64, agent]` will need to drop the `agent` label (or any equivalent) when they migrate their agent workflows to `anthropics/claude-code-action@beta`.

## Capabilities

### Modified Capabilities

- `runner-mode-display`: remove the "Agentic profile reports container mode" scenario. The profile is gone; nothing should report a mode based on a profile string.
- `inner-dockerd-bootstrap`: rename references from `gh-sr/agentic-runner` and `agenticRunnerEntrypoint` to the new generic image name and entrypoint identifier. The pid-file cleanup requirement itself is unchanged — it's the same DinD startup sequence, just on a different base image.

## Impact

- **Code**: `internal/agentic/` directory, `internal/config/config.go`, `internal/config/mutate.go`, `internal/runner/assets.go`, `internal/runner/container.go`, `internal/runner/environment.go`, `internal/runner/native.go`, `internal/runner/disk.go`, `internal/doctor/doctor.go`, `cmd/gh-sr/main.go`, `config/runners.yml`.
- **Embedded assets**: `internal/runner/agentic-runner-image/` directory (deleted); replacement image built from `docker:dind` + a small `actions/runner` overlay.
- **Tests**: ~6 test files under `internal/agentic/` deleted; the existing `internal/runner/container_test.go` and `internal/doctor/doctor_test.go` lose their agentic fixtures.
- **Docs**: `docs/content/guides/agentic-workflows.md` deleted; references updated in `architecture.md`, `configuration.md`, `commands.md`, `host-setup.md`, `README.md`.
- **CI on this repo**: `.github/workflows/*.md` (gh-aw workflows), `.lock.yml` files, `agentic_*.yml`, and `.github/agents/agentic-workflows.md` deleted. The repo's own CI (`ci.yml`, `release.yml`, `bench-compare.yml`, `copilot-setup-steps.yml` after rewrite) stays.
- **User projects**: any user who today runs `gh sr runner add … --profile agentic` will get an "unknown flag" error on upgrade. They must delete those runners from `runners.yml` and re-add as plain `runner_mode: container` or native runners. Any project that today calls `baizhiheizi/agents/.github/workflows/agent-*.yml` reusable workflows must migrate to `anthropics/claude-code-action@beta`. There is no gh-sr-side flag for either — the migration is config + workflow rewrites on the user side.
- **Deferred (separate change)**: local `actions/cache` proxying. Discussed 2026-09-02, deferred because it adds meaningful new scope (server lifecycle, runner binary env-var quirk) that is independent of the gh-aw removal.