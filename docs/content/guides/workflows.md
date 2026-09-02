---
title: "Workflows"
weight: 10
---

# Using runners in workflows

Reference runners by label in your workflow files:

```yaml
jobs:
  build-linux:
    runs-on: [self-hosted, Linux, X64]

  build-mac:
    runs-on: [self-hosted, macOS, ARM64]

  build-win:
    runs-on: [self-hosted, Windows, X64]
```

Labels must match what you configure under `runners[].labels` in [Configuration](../configuration.md).

## Service containers (`services:`)

Workflows that need `services:` (postgres, redis, etc.) should run on a `runner_mode: container` runner. The container mode provides an inner Docker daemon that service containers attach to, so each job gets its own isolated service stack. See [Configuration](../configuration.md#runners-runner_mode) and [Host setup](../host-setup.md).

## Organization-level runners

Register a runner against an organization (rather than a single repo) so it can pick up jobs from every repo in the org:

```yaml
runners:
  - name: org-runner
    org: my-org
    group: my-runner-group   # optional; create the group in GitHub org settings first
    host: vps-1
    count: 4
    labels: [self-hosted, Linux, X64]
```

## Ephemeral runners

For security isolation between jobs, use `ephemeral: true` so each runner handles one job and then deregisters:

```yaml
runners:
  - name: ephemeral-runner
    repo: owner/repo
    host: vps-1
    ephemeral: true
    count: 4
    labels: [self-hosted, Linux, X64]
```