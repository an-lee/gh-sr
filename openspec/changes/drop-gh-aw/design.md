## Context

See `proposal.md` for motivation. The constraint that shapes this design: container-mode runners must continue to provide per-instance DinD isolation, because the user's CI workflow (`ohmyxin/ci.yml`) uses `services: postgres` and `services: redis` and runs 4 test shards in parallel on the same host. Today's DinD comes from a custom image built from scratch in `internal/runner/agentic-runner-image/`. The image is heavily over-built: it bundles gh-aw tooling, AWF, `/tmp/gh-aw` paths, MCP gateway port handling, `RUNNER_TEMP` non-`/tmp` redirection, and a custom dnsmasq config — all of which existed solely to support gh-aw.

Today `gh-sr/agentic-runner` is the image tag for **all** `runner_mode: container` runners, not just agentic ones. Renaming the tag is a breaking change for any host that has the old image built locally — those hosts need `docker rmi gh-sr/agentic-runner` (or equivalent) once on upgrade, then the next `gh sr setup` builds the new tag.

## Goals / Non-Goals

**Goals:**
- Replace the bespoke embedded image with a thin wrapper based on `docker:dind` + `actions/runner`, matching the approach GitHub documents in [`actions/runner-docker`](https://github.com/actions/runner-docker).
- Strip every gh-aw-specific line from runner image, config, doctor preflight, and CLI surface so the maintenance burden that prompted this change is removed.
- Preserve the inner-dockerd pid-file cleanup (Sep 2 fix) — that fix is host-induced hard-kill recovery, not gh-aw-specific.
- Preserve the existing host-MTU / iptables clamp logic — those are general DinD concerns, not gh-aw-specific.

**Non-Goals:**
- Local `actions/cache` proxying (deferred to a separate change — see project memory `deferred-cache-proxy.md`).
- Renaming the binary itself, the `gh sr` command surface, or the public `internal/` Go API beyond what is strictly required by the removal.
- Adding new runner features (e.g. macOS VMs, k8s scheduling).
- Migrating user projects' `agent-*.yml` workflows from gh-aw reusable workflows to `claude-code-action` — that's consumer-side work in projects like `ohmyxin`, not a gh-sr change.

## Decisions

### 1. Base the new image on `docker:dind`, not a from-scratch Dockerfile

**Decision:** The replacement image is `FROM docker:dind` + a small overlay that adds the `actions/runner` binary and a thin wrapper script that starts `dockerd`, waits for it, then execs the runner binary.

**Rationale:** `docker:dind` is the official Docker Engine in a container image, maintained by Docker, supports `--privileged`, includes the daemon, CLI, and storage drivers. Reusing it means gh-sr no longer ships apt-package manifests, daemon.json, dnsmasq configs, or Dockerfiles for the runner image — those concerns belong to `docker:dind`.

**Alternatives considered:**
- **`docker:dind-rootless`**` — does not require `--privileged` but has limits (no `--network host`, fewer storage drivers). Defer; revisit if a future use case needs it.
- **`summerwind/actions-runner-dind`** — runner + DinD combined; based on Ubuntu 18.04, older than what `docker:dind` ships today.
- **Keep the current bespoke image** — rejected because the bespoke image is the bulk of the maintenance burden. The Dockerfile itself is fine; what's wrong is everything in it.

### 2. Replace `AgenticRunnerImageTag` constant with `ContainerRunnerImageTag`

**Decision:** Rename the public constant in `internal/runner/container.go`. Update all callers in `internal/runner/`, the docker image-revision label namespace (`gh-sr-agentic-image/v1` → `gh-sr-container-image/v1`), and any docs.

**Rationale:** `Agentic*` is a misleading name for a constant that backs all `runner_mode: container` runners. Renaming now is cheap; doing it later (when users depend on the tag) is a larger break.

**Migration:** When a host runs `gh sr up` / `gh sr setup` after the change and finds the old `gh-sr/agentic-runner:<version>` image locally, gh-sr prints a one-line message: `Detected legacy image gh-sr/agentic-runner:<v>; remove with 'docker rmi gh-sr/agentic-runner:<v>' and re-run setup.` Then build the new tag. No automatic migration — the old tag has gh-aw content we don't want to keep.

### 3. Strip the `entrypoint.sh` to start-dockerd-then-run, drop hooks

**Decision:** The new wrapper entrypoint does three things only:
1. `rm -f /var/run/docker.pid` (the Sep 2 fix).
2. `dockerd &` + wait for `docker info` to succeed.
3. `exec ./run.sh` (the upstream `actions/runner` entrypoint).

The `job-started.sh` / `job-completed.sh` hooks are deleted. They existed only to clean `/tmp/gh-aw`, remove leftover AWF containers, and reset iptables — none of that is needed without gh-aw.

**Rationale:** Smaller, more obviously correct, mirrors what [`actions/runner-docker`](https://github.com/actions/runner-docker) does today.

### 4. Delete `internal/agentic/` entirely; do not retain any subset

**Decision:** The whole package is removed, including its test files. Doctor does not gain a replacement preflight for "is this host ready to run claude-code-action jobs" — claude-code-action needs only Node.js (present in `docker:dind` via the `actions/runner` overlay) and network egress.

**Rationale:** The package's purpose was validating gh-aw isolation preconditions (Linux, iptables, `RUNNER_TEMP` not `/tmp`, AWF hygiene). All of those preconditions vanish with gh-aw. A partial retention would just be vestigial.

### 5. Remove the `agentic` label auto-append

**Decision:** `internal/config/mutate.go` no longer appends `"agentic"` to a runner's labels when `profile: agentic` is set (since `profile` is gone). Existing GitHub-side runner registrations that carry the `agentic` label are unchanged — gh-sr stops adding it but doesn't actively remove it.

**Rationale:** The label had no purpose outside gh-aw. Users with manually-added `agentic` labels for their own routing reasons can keep them.

### 6. Remove `--profile` flag and the `profile` config field with a hard cut

**Decision:** `gh sr runner add --profile` is removed. `runner.profile` in `runners.yml` is rejected at validation with a clear migration message: `runner %q: "profile: agentic" is no longer supported; delete the profile field. For container isolation, set "runner_mode: container" explicitly. Agent workflows using claude-code-action need no special profile — drop the field.`

**Rationale:** A deprecation cycle adds two releases of complexity (deprecation warning + migration message + cleanup). Given the user-facing change is "this feature is gone," a hard cut is honest. The two deprecated `agentic_mcp_*` field stubs in `Config.go` are removed in the same change — they already emitted migration messages and have been dead weight since the agentic → container collapse.

### 7. Delete the consumer workflows and dispatcher agent whole

**Decision:** Delete every `.github/workflows/*-*.md` and `.lock.yml` file (the gh-aw workflow manifests and their permission locks), `agentic_commands.yml`, `agentics-maintenance.yml`, and `.github/agents/agentic-workflows.md`. Replace `.github/workflows/copilot-setup-steps.yml` with a version that has no gh-aw reference (or delete it entirely if Copilot setup has another home in the repo's CI).

**Rationale:** These files were consumers of the gh-aw tooling that this change removes. Keeping them means keeping the rationale this change rejects.

### 8. `mutate.go` is the only auto-label logic; no replacement

**Decision:** With the `agentic` label auto-append gone, `mutate.go` no longer has profile-aware behavior. Future label additions, if any, would land in `mutate.go` again — but we don't add any here.

**Rationale:** Smaller surface, fewer implicit behaviors.

## Risks / Trade-offs

- **[Image tag rename breaks upgrade path]** → Mitigation: surface the migration message in `gh sr up` / `gh sr setup` whenever the old tag is found locally; document the manual `docker rmi` step in the release notes; the old image stops working for new runners (the new code doesn't reference it) but stays usable for any host that pins to it and never upgrades gh-sr.
- **[Existing runners registered with the `agentic` label keep that label on GitHub's side]** → Mitigation: nothing required. The label is metadata; removing it would require a GitHub API call per runner which is out of scope.
- **[Workflows in user projects that call `baizhiheizi/agents/.github/workflows/agent-*.yml` break at the next job dispatch]** → Mitigation: out of scope for gh-sr (consumer-side), but flagged in the release notes; the `claude-code-action` migration is documented at <https://github.com/anthropics/claude-code-action>.
- **[The new image is a thin wrapper — we inherit any `docker:dind` quirks]** → Mitigation: the Sep 2 pid-file cleanup is preserved in the wrapper; the image-revision layout fingerprint includes the wrapper contents so rebuilds happen on wrapper changes. Inheriting `docker:dind` is no worse than maintaining a custom DinD layer ourselves, which is the whole point.
- **[Removing the agentic-specific doctor preflight means the host-side preconditions for claude-code-action jobs are unchecked]** → Mitigation: claude-code-action's only preconditions are Node.js and network egress. Node.js is present in the standard `actions/runner` image. Network egress is a generic concern that's already outside gh-sr's scope. No replacement preflight is needed.
- **[The `agentics-maintenance.yml` and `agentic_commands.yml` files are gh-aw-generated; deleting them is irreversible if the upstream tooling is ever re-enabled]** → Mitigation: they're version-controlled and the change is committed; a future `gh aw compile` would regenerate them. Net cost of regenerating is low.

## Migration Plan

1. Cut a release with the change.
2. Release notes call out: (a) `--profile agentic` is gone, drop `profile:` from `runners.yml`; (b) image tag moved from `gh-sr/agentic-runner` to `gh-sr/container-runner`, run `docker rmi gh-sr/agentic-runner:*` then `gh sr setup` to pick up the new tag; (c) agent workflows using gh-aw reusable workflows must migrate to `claude-code-action`.
3. No rollback path — the change is reversible in git but the image migration (docker rmi) is not. If a rollback were required, users would need to rebuild the old tag from a pre-change commit.
4. User projects (`ohmyxin` etc.) can migrate at their own pace; nothing in this change breaks them at compile time. Their first `@claude` mention after they remove `baizhiheizi/agents/.github/workflows/agent-*.yml` references without adding a replacement will fail at job dispatch.

## Open Questions

- **Does `copilot-setup-steps.yml` have any non-gh-aw role?** Need to confirm by reading the file at apply time. If it does, retain a stripped-down version; if not, delete it.
- **Should the new image include the standard `actions/runner` extras (Node.js, build-essential, etc.) baked in, or rely on `actions/setup-node` per workflow?** Default to whatever `actions/runner-docker`'s DinD variant does; revisit if a specific user workflow needs something baked in.
- **Should the `agentic_mcp_*` field removal emit a one-release deprecation cycle first, or is the hard cut acceptable given the field has been emitting a migration message for several releases already?** Decision: hard cut, per Decision 6 above. Confirmed by reading the current migration message in `Config.go`.