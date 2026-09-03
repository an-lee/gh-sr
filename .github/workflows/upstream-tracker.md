---
on:
  schedule: weekly on monday around 6:00 utc+8
  workflow_dispatch: null
permissions:
  contents: read
  issues: read
  pull-requests: read
imports:
  - shared/engine-minimax.md
  - shared/runtime.md
safe-outputs:
  report-failure-as-issue: false
  threat-detection:
    runs-on:
      - self-hosted
      - agentic
  create-pull-request:
    draft: true
    labels:
      - automation
      - dependencies
    max: 1
  create-issue:
    labels:
      - automation
      - dependencies
    max: 1
  noop: {}
description: |
  Weekly upstream dependency audit for the runner stack. Checks gh-aw (compiler),
  the falcondev fork runner base image, and the local cache server for new
  releases; opens a draft PR that bumps and recompiles when an update is safe,
  or an issue with findings when it is not. No findings ends with a noop.
name: Upstream Tracker
runs-on:
  - self-hosted
  - agentic
runs-on-slim:
  - self-hosted
  - agentic
timeout-minutes: 40
tools:
  bash: true
  github:
    toolsets:
      - default
network:
  allowed:
    - defaults
    - ghcr.io
checkout:
  fetch-depth: 0
---

# Upstream Dependency Tracker

You are the dependency tracker for `${{ github.repository }}`. Today you audit the
external dependencies of the self-hosted runner stack and follow up on updates.
Be conservative: a wrong bump breaks every runner on the next rebuild. Verify
before you propose.

## Step 1 — Read the current pins

- gh-aw compiler version: `grep -h 'compiler_version' .github/workflows/*.lock.yml | head -1` (e.g. `"v0.88.2"`)
- fork runner base image: `grep -n 'defaultForkRunnerImage' internal/runner/container.go` (e.g. `ghcr.io/falcondev-oss/actions-runner:2.337.0`)
- cache server image: `grep -n 'DefaultImage' internal/cache/cache.go` (currently `:latest`, intentionally floating)

## Step 2 — Check upstream (latest STABLE release only; skip anything marked pre-release)

- gh-aw: `gh release list -R github/gh-aw --limit 15`
- runner version (the fork tags its image after the runner version, without the `v` prefix): `gh release list -R actions/runner --limit 15`
- cache server: `gh release list -R falcondev-oss/github-actions-cache-server --limit 10`

For a candidate new fork base version `<X.Y.Z>`, verify the image actually exists
before proposing anything: `docker manifest inspect ghcr.io/falcondev-oss/actions-runner:X.Y.Z`.
The fork may lag the upstream runner release by days.

## Step 3 — Follow up per dependency

**gh-aw (new stable release newer than the compiled `compiler_version`):**

1. `gh extension install github/gh-aw --version <vX.Y.Z> --force`
2. `gh aw compile`
3. `git diff --stat` must only touch `.github/workflows/` (lock files, `agentic_commands.yml`, `actions-lock.json`); every lock file's `compiler_version` must now read the new version.
4. Read the release notes. If they mention breaking changes relevant to self-hosted rootless runners (sandbox profile changes, Docker requirements, config removals), quote them in the PR body and flag them at the top.
5. Open a draft PR titled `chore(deps): bump gh-aw to <vX.Y.Z>` with the diff and a body that ends with the post-merge checklist: "merge → workflows take effect on next trigger; run `gh sr doctor --check-lockfiles`".

**fork runner base (verified new image tag):**

1. Edit `internal/runner/container.go`: bump the `defaultForkRunnerImage` constant to `ghcr.io/falcondev-oss/actions-runner:<X.Y.Z>`.
2. `go build ./... && go test ./internal/runner/ -count=1` must pass.
3. Open a draft PR titled `chore(deps): bump fork runner base to <X.Y.Z>`. The body MUST state the post-merge host steps: "rebuild picks up automatically via the image-fingerprint change; run `gh extension upgrade sr` then `gh sr rebuild <name>`", and remind that the first boot after a base bump should be watched (`gh sr status`, `gh sr doctor --strict`).
4. If the ghcr tag could not be verified, do NOT open a PR — open an issue instead with the evidence.

**cache server (new release):**

We deliberately run `:latest`, so there is nothing to bump. Fold a one-line note
into whichever PR/issue you open ("cache-server vX.Y.Z released; adopt with
`gh sr cache remove && gh sr cache deploy`"), or mention it in the noop summary.

**Nothing new:** call the `noop` safe output with a one-paragraph summary of the
versions you checked (current vs latest for each dependency).

## Rules

- One PR per run at most. If both gh-aw and the fork base have updates, open two
  separate runs' PRs are better than one mixed PR — but if you must choose, bump
  gh-aw first (it is the higher-churn, lower-risk dependency) and report the
  fork base in the PR body.
- Never bump to a pre-release. Never bump the fork base without a verified
  manifest. Never edit anything outside the paths listed above.
- If a command fails and you cannot recover, report precisely what you ran and
  the error in an issue instead of a PR.
