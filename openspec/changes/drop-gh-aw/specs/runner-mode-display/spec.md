## MODIFIED Requirements

### Requirement: Disk usage mode uses EffectiveRunnerMode

When measuring disk usage for a configured runner, `DiskUsageEntry.Mode` MUST be set from `RunnerConfig.EffectiveRunnerMode()` rather than hand-assigned `"container"` / `"native"` strings. Orphan instances (nil config) MUST retain mode `"unknown"`.

#### Scenario: Container runner reports container mode

- **WHEN** `MeasureDiskUsage` is called with a runner config where `EffectiveRunnerMode()` returns `"container"`
- **THEN** `DiskUsageEntry.Mode` MUST be `"container"`

#### Scenario: Orphan directory reports unknown mode

- **WHEN** `MeasureDiskUsage` is called with `rc == nil`
- **THEN** `DiskUsageEntry.Mode` MUST be `"unknown"` and `Orphan` MUST be true

## REMOVED Requirements

### Requirement: Agentic profile reports container mode
**Reason**: The `profile: agentic` configuration option and its associated `IsAgentic()` predicate are removed in this change. `EffectiveRunnerMode()` no longer has an agentic branch — only explicit `runner_mode: container` (or the default `native`) determines the mode. The "agentic → container" forcing behavior existed solely to give gh-aw workflows per-instance DinD isolation; with gh-aw replaced by `claude-code-action` running as a normal Actions step, no profile-based mode forcing is required.
**Migration**: Runners that today set `profile: agentic` MUST drop the field. Runners that need container isolation (e.g. CI workflows with `services:` blocks) MUST set `runner_mode: container` explicitly. Runners that today use `profile: agentic` purely to run agent workflows (e.g. `anthropics/claude-code-action`) MUST drop both the profile and the `runner_mode` field (defaulting to native), since claude-code-action needs no special tooling or isolation.