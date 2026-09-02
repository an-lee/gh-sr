# inner-dockerd-bootstrap Specification

## Purpose

Define the per-container inner-dockerd startup sequence used by container-mode runners, including the pre-cleanup expected to make restarts idempotent against host-induced hard kills.

## ADDED Requirements

### Requirement: Stale pid file is removed before each dockerd start

When the `entrypoint.sh` baked into `gh-sr/agentic-runner` reaches the inner-dockerd startup step, it MUST remove `/var/run/docker.pid` (if present) before invoking `dockerd`.

#### Scenario: Stale pid file present from a hard-killed predecessor

- **WHEN** the container starts and `/var/run/docker.pid` already exists from a previous ungraceful shutdown
- **THEN** entrypoint MUST remove the file before `dockerd` is invoked
- **AND** `dockerd` MUST NOT log `process with PID … is still running`

#### Scenario: No stale state on first start

- **WHEN** the container starts and `/var/run/docker.pid` does not exist
- **THEN** entrypoint MUST proceed to `dockerd` invocation without error
- **AND** `dockerd` MUST write its own pid file on its own during normal startup

#### Scenario: Pid-file cleanup precedes dockerd invocation

- **WHEN** the entrypoint's dockerd-start block is parsed
- **THEN** the pid-file removal line MUST appear in the script strictly before the `dockerd` invocation
- **AND** the order MUST NOT depend on a conditional (always run, not gated on `if [ -f … ]`)

### Requirement: Single dockerd start per container boot

The entrypoint MUST start `dockerd` exactly once per container start. The retry/cap-and-hold behavior that already exists for genuine host failures MUST continue to apply.

#### Scenario: First attempt succeeds

- **WHEN** `docker info` succeeds within `GH_SR_DOCKERD_START_TIMEOUT` seconds after `dockerd` starts
- **THEN** entrypoint MUST mark the dockerd as up and continue to the dnsmasq/registration steps

#### Scenario: dockerd never responds within timeout

- **WHEN** `docker info` does not succeed within `GH_SR_DOCKERD_START_TIMEOUT` seconds
- **THEN** entrypoint MUST increment the per-instance failure counter at `${RUNNER_STATE_DIR}/dockerd-start-failures`
- **AND** the increment MUST persist across container restarts (counter file lives on the host bind-mount)

#### Scenario: Five consecutive failures cap

- **WHEN** the per-instance failure counter reaches `GH_SR_BOOTSTRAP_MAX_RETRIES`
- **THEN** entrypoint MUST write `${RUNNER_STATE_DIR}/bootstrap-failed` with an ISO-8601 UTC timestamp and a count suffix
- **AND** MUST `exec sleep infinity` so the container holds while the operator fixes the host

### Requirement: Host-MTU pinning and iptables clamp are unaffected

The pre-cleanup MUST NOT change the existing reduced-MTU handling for hosts whose egress MTU is below 1500.

#### Scenario: Sub-1500 host MTU still pins inner bridge and egress MTU

- **WHEN** `GH_SR_HOST_MTU` is set to a value in `[576, 1500)` and the inner dockerd would otherwise start
- **THEN** entrypoint MUST still rewrite `/etc/docker/daemon.json` with the matching `mtu` key
- **AND** MUST still clamp the outer container's egress interface MTU
- **AND** MUST still install the mangle-FORWARD TCPMSS clamp

### Requirement: Image layout revision reflects the cleanup line

When the embedded `entrypoint.sh` changes (including the new pid-file cleanup line), `ContainerImageLayoutRevision` MUST change so existing hosts pick up the rebuilt image on next `gh sr up` / `gh sr rebuild`.

#### Scenario: Cleanup line added

- **WHEN** a new line (`rm -f /var/run/docker.pid`) is added before the `dockerd` invocation in `agenticRunnerEntrypoint`
- **THEN** `ContainerImageLayoutRevision` MUST return a different short hex fingerprint than before the change
