## 1. CLI surface removal

- [x] 1.1 Remove the `--profile` flag and its setup-prompt branch from `cmd/gh-sr/main.go`. The runner-add Long description drops the `Use --profile agentic` paragraph. Verify: `go build ./...` succeeds, `gh sr runner add --help` no longer lists `--profile`.
- [x] 1.2 Remove the agentic-specific paragraph from the `gh sr doctor` long description. Verify: `go build ./...` succeeds; `gh sr doctor --help` no longer mentions agentic or AWF.

## 2. Config field removal

- [x] 2.1 Delete the `Profile` field, `IsAgentic()` method, `agentic_mcp_ports` / `agentic_mcp_port_base` deprecated field stubs, the `profile: agentic → container` forcing branch in `EffectiveRunnerMode()`, the agentic-validation errors in `Validate()`, and the auto-append of the `agentic` label in `internal/config/config.go` and `internal/config/mutate.go`. Verify: `go build ./...` succeeds, `go test ./internal/config/...` passes, `grep -n "agentic\|profile.*agentic" internal/config/*.go` returns no matches.
- [x] 2.2 Remove the `Profile` field from `AddRunnerOpts` and the CLI plumbing that fed it. Verify: `go build ./cmd/...` succeeds, `go vet ./...` clean.

## 3. Agentic package deletion

- [x] 3.1 Delete the entire `internal/agentic/` directory (source files and test files). Verify: `ls internal/agentic/` reports no such directory; `go build ./...` reports only the expected downstream caller errors.
- [x] 3.2 Remove `printAgenticFailures`, the `agentic.ValidateContainerPrereqs` / `agentic.ValidateAWFHygieneInner` / `agentic.ValidateContainerAgenticFanout` call sites, and any agentic-specific import from `internal/doctor/doctor.go`. Verify: `go build ./...` succeeds, `go test ./internal/doctor/...` passes, `grep -rn "agentic\." internal/doctor/` returns no matches.
- [x] 3.3 Remove the comment reference to `agentic.ValidateContainerInnerNetwork` in `internal/runner/environment.go` (and any actual call sites if they exist). Verify: `go build ./...` succeeds; `grep -n "agentic" internal/runner/environment.go` returns no matches.
- [x] 3.4 Remove the agentic-related comment in `internal/runner/native.go` line ~391 (the note that `profile: agentic` always runs in container mode). Verify: `grep -n "agentic" internal/runner/native.go` returns no matches.

## 4. Image replacement

- [x] 4.1 Delete the `internal/runner/agentic-runner-image/` directory (Dockerfile, entrypoint.sh, docker-wrapper.sh, daemon.json, dnsmasq-gh-sr.conf, hooks/, apt-packages-core.txt). Verify: `ls internal/runner/agentic-runner-image/` reports no such directory.
- [x] 4.2 Replace the `//go:embed` lines for the deleted image assets in `internal/runner/assets.go` with new `//go:embed` lines pointing at the replacement wrapper files. Verify: `go build ./internal/runner/...` succeeds; `grep -n "go:embed" internal/runner/assets.go` lists the new files only.
- [x] 4.3 Add the new wrapper image files in `internal/runner/container-runner-image/`: a minimal `Dockerfile` that is `FROM docker:dind` plus the `actions/runner` binary overlay, and an `entrypoint.sh` that does only `rm -f /var/run/docker.pid`, `dockerd &`, wait for `docker info`, then `exec ./run.sh`. Mirror the structure used in [actions/runner-docker](https://github.com/actions/runner-docker)'s `Dockerfile` DinD variant. Verify: `docker build -f internal/runner/container-runner-image/Dockerfile -t test/container-runner:test .` produces an image; running it with `--privileged` reaches `docker info` succeeding inside the container.
- [x] 4.4 Rename `AgenticRunnerImageTag = "gh-sr/agentic-runner"` to `ContainerRunnerImageTag = "gh-sr/container-runner"` in `internal/runner/container.go`. Update every caller. Verify: `go build ./...` succeeds, `go test ./internal/runner/...` passes, `grep -rn "AgenticRunnerImageTag\|gh-sr/agentic-runner" internal/runner/` returns no matches (except in migration message strings, see 4.6).
- [x] 4.5 Update the docker image-revision label namespace from `gh-sr-agentic-image/v1` to `gh-sr-container-image/v1` in `internal/runner/container.go` (`dockerLabelImageLayout*` constants). Verify: `grep -n "gh-sr-agentic-image\|gh-sr-container-image" internal/runner/*.go` returns only the new namespace.
- [x] 4.6 In the `gh sr setup` / `gh sr up` path, detect when an old `gh-sr/agentic-runner` image exists locally. Print a one-line migration message: `Detected legacy image gh-sr/agentic-runner:<v>; remove with 'docker rmi gh-sr/agentic-runner:<v>' and re-run setup.` Build the new tag regardless. Verify: `go test` covers the legacy-image-present and legacy-image-absent paths.
- [x] 4.7 Update `ContainerImageLayoutRevision` so its fingerprint includes the new wrapper file contents (it already fingerprints the embedded strings, so adding the new files to the `WriteString` chain in `imageLayoutFingerprint` is sufficient). Verify: bumping the wrapper entrypoint changes the fingerprint.

## 5. Consumer workflow & dispatcher deletion

- [x] 5.1 Delete every `.github/workflows/*-*.md` and matching `.lock.yml` file (perf-improver, test-improver, doc-updater, grumpy-reviewer, efficiency-improver, duplicate-code-detector, repo-assist). Verify: `ls .github/workflows/` contains only `ci.yml`, `release.yml`, `bench-compare.yml`, and `copilot-setup-steps.yml` (or its successor from 5.4).
- [x] 5.2 Delete `.github/workflows/agentic_commands.yml` and `agentics-maintenance.yml`. Verify: `ls` confirms.
- [x] 5.3 Delete `.github/agents/agentic-workflows.md`. Verify: `ls` confirms.
- [x] 5.4 Read `.github/workflows/copilot-setup-steps.yml`. If it has any role besides `github/gh-aw-actions/setup-cli`, keep the file and strip the gh-aw step. Otherwise delete the file. Verify: `grep -n "gh-aw" .github/workflows/copilot-setup-steps.yml` (if the file still exists) returns no matches, or the file no longer exists.

## 6. Docs

- [x] 6.1 Delete `docs/content/guides/agentic-workflows.md`. Verify: `ls docs/content/guides/` no longer lists it.
- [x] 6.2 Update `docs/content/architecture.md`: remove gh-aw, AWF, MCP gateway, `/tmp/gh-aw`, `agentic-runner` references; rename `gh-sr/agentic-runner` to `gh-sr/container-runner`. Verify: `grep -n "gh-aw\|AWF\|agentic-runner\|agentic" docs/content/architecture.md` returns no matches.
- [x] 6.3 Update `docs/content/configuration.md` similarly. Verify: clean.
- [x] 6.4 Update `docs/content/commands.md` to remove `--profile agentic` from the `gh sr runner add` example. Verify: clean.
- [x] 6.5 Update `docs/content/host-setup.md` to drop the agentic-tooling line. Verify: clean.
- [x] 6.6 Update `README.md` to remove `gh-sr/agentic-runner` references. Verify: clean.
- [x] 6.8 Update `config/runners.yml` to remove the `profile: agentic` example runner. Verify: clean.

## 7. Final validation

- [x] 7.1 Run `go build ./...` and confirm zero errors. Verify: exit status 0, no "undefined" errors.
- [x] 7.2 Run `go test ./...` and confirm zero failures. Verify: exit status 0, no `FAIL` lines.
- [x] 7.3 Run `go vet ./...` and confirm zero warnings. Verify: exit status 0.
- [x] 7.4 Run `openspec validate drop-gh-aw --strict` and confirm the change validates. Verify: exit status 0.
- [x] 7.5 Add a top-level `CHANGELOG.md` entry: `## Drop gh-aw support: --profile flag removed, image retag to gh-sr/container-runner, internal/agentic package deleted, claude-code-action is the recommended replacement for agent workflows.` Verify: the file shows the new entry at the top of the unreleased section.
- [ ] 7.6 Run `gh sr doctor` against a host that has the old `gh-sr/agentic-runner` image locally and confirm the legacy-image migration message prints once. Verify: manual smoke test (no automated test exists because the migration message is informational, not a hard failure).
- [ ] 7.7 Build the new container-mode image with `gh sr setup` against a test host and confirm `gh sr up` for a `runner_mode: container` runner starts the container, reaches `docker info` succeeding inside, and registers with GitHub. Verify: end-to-end smoke test against a sandbox repo.