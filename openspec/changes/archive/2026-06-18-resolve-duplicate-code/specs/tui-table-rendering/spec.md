## ADDED Requirements

### Requirement: Column widths are computed from a single helper

The TUI package SHALL provide `computeColumnWidths(headers []string, rows [][]string) []int` that returns the maximum rendered width per column (header length vs cell lengths). All table views in `internal/tui` MUST use this helper instead of inline width loops.

#### Scenario: Host metrics table uses shared width helper

- **WHEN** `PrintHostMetricsTable` or `FormatHostMetricsTo` renders a metrics table
- **THEN** column widths MUST be computed via `computeColumnWidths`

#### Scenario: Runner status table uses shared width helper

- **WHEN** `PrintStatusTable` or the dashboard runner list renders column widths
- **THEN** column widths MUST be computed via `computeColumnWidths` (directly or via rows built from status cells)

#### Scenario: Dashboard host-metrics panel uses shared width helper

- **WHEN** the dashboard host-metrics overlay renders its table
- **THEN** column widths MUST be computed via `computeColumnWidths`

### Requirement: Runner status row cells are derived from a single helper

The TUI package SHALL provide `runnerStatusCells(s runner.RunnerStatus) []string` returning nine cells in fixed order: Instance, Host, Repo, Mode, Image, Build, Local, GitHub status, Labels. Empty ContainerImage and ContainerImageBuild MUST be displayed as `"-"`. GitHub status MUST use the existing `formatGitHubStatus` logic.

#### Scenario: CLI status table matches dashboard cells

- **WHEN** the same `RunnerStatus` value is rendered via `PrintStatusTable` and the dashboard runner list
- **THEN** the nine cell values MUST be identical

#### Scenario: Empty image fields show dash

- **WHEN** a runner has empty `ContainerImage` or `ContainerImageBuild`
- **THEN** the corresponding table cell MUST display `"-"`
