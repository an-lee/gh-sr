# runner-session-recovery Specification

## Purpose

Define how gh-sr detects when a container-mode runner is stuck because its actions-runner registration against GitHub has lapsed (registration deleted or session conflict), and how it routes the affected container through the existing recovery flow.

## ADDED Requirements

### Requirement: Recoverable errors are detected from container logs

`gh sr` MUST detect recoverable runner registration errors by inspecting the most recent `docker logs` of the runner container, using fixed-string matches so no regex parser is introduced.

#### Scenario: Registration deleted message present

- **WHEN** `docker logs --tail 200 <container>` contains the substring `registration deleted on GitHub`
- **THEN** the predicate MUST report the container as needing recovery

#### Scenario: Session conflict message present

- **WHEN** `docker logs --tail 200 <container>` contains the substring `Session Conflict` (covers `Error: SessionConflictException` and `Runner listener exit with Session Conflict error`)
- **THEN** the predicate MUST report the container as needing recovery

#### Scenario: Healthy container log has neither message

- **WHEN** `docker logs --tail 200 <container>` contains neither substring
- **THEN** the predicate MUST report the container as not needing recovery
- **AND** `gh sr up` MUST NOT route the container through recovery

### Requirement: Recovery path recreates the container and re-registers

When the predicate reports a container as needing recovery, `gh sr up` MUST route it through the existing `recoverContainerStaleRegistration` flow: `docker rm -f`, remove `.runner` / `.credentials` / `.credentials_rsaparams`, `createContainerInstance`, `startContainer`.

#### Scenario: Recovery triggers on the existing detection path

- **WHEN** the predicate returns true during `gh sr up`
- **THEN** the operator MUST see the same `re-creating container…` / `container re-created with fresh registration` message used for the related stale-registration case

#### Scenario: Recovery only affects the failing instance

- **WHEN** the predicate returns true for one instance in a multi-instance runner group
- **THEN** only that instance's container MUST be recreated
- **AND** other instances in the group MUST continue running

### Requirement: Detection is local and cheap

The detection MUST be a single `docker logs --tail 200 … | grep -F …` invocation per container per `gh sr up` call — same shape as the existing stale-registration check.

#### Scenario: Detection runs once per instance

- **WHEN** `gh sr up` is invoked for a runner with `count: N > 1`
- **THEN** the predicate MUST be evaluated exactly once per instance
- **AND** MUST not call the GitHub API

### Requirement: No false-positive recreation in steady state

The predicate MUST NOT match on common runner chatter (e.g. `error`, `connection refused`) so `gh sr up` does not gratuitously recreate healthy runners.

#### Scenario: Healthy runner working a job

- **WHEN** `docker logs --tail 200 <container>` shows a healthy runner actively working (no `Session Conflict`, no `registration deleted on GitHub`)
- **THEN** `gh sr up` MUST complete without triggering recovery on that instance
