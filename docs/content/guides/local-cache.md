# Local Actions Cache

gh-sr can deploy a local GitHub Actions cache server on every Linux host, so `actions/cache`, `actions/cache/restore|save`, and gh-aw's `cache-memory` / usage caches all resolve against the local machine instead of downloading from GitHub at the start of every job.

The server is [falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server) — a drop-in replacement implementing the Actions cache **v2 protocol**, so official `actions/cache` steps work unmodified.

## Topology

- **One `gh-sr-cache` container per Linux host**, shared by all container-mode runners on that host.
- **Wiring**: the runner container receives `CUSTOM_ACTIONS_RESULTS_URL` pointing at the local server. The runner binary must come from the [falcondev fork image](https://github.com/falcondev-oss/actions-runner) — which is exactly the base gh-sr's runner image is built from — because the stock runner overrides the env var with the job message's `ACTIONS_RESULTS_URL` and silently falls back to GitHub.
- **Artifacts are unaffected**: `upload-artifact` / `download-artifact` and other non-cache ACTIONS_RESULTS requests are proxied through the cache server back to GitHub (`DEFAULT_ACTIONS_RESULTS_URL`), so cross-host safe-outputs keep working.
- **Binding**: the server publishes on the **docker0 gateway IP** (auto-detected, typically `172.17.0.1`) so only containers on the host can reach it — not your LAN. Without a docker0 interface it falls back to `0.0.0.0` and `gh sr doctor` warns about the exposure.
- **Host firewalls**: a container reaching a host-published port traverses the host's **INPUT** chain. Default-accept firewalls need nothing; a default-deny firewall (ufw et al) silently blackholes the cache URL — every `upload-artifact` / `actions/cache` step then times out while the host-side health check stays green. `gh sr doctor` probes the cache URL **from inside a runner container** and, when blocked, prints the exact allow rule (e.g. `sudo ufw allow in on docker0 to 172.17.0.1 port 27420 proto tcp comment allow-docker-cache`).

## Configuration (runners.yml)

The whole `cache:` section is optional; a per-host server is deployed automatically (`enabled` defaults to `true`) whenever `gh sr setup` / `gh sr up` / `gh sr update` / `gh sr rebuild` run.

```yaml
cache:
  enabled: true                    # default true; set false to keep using GitHub's cache service
  port: 27420                      # host-side published port (built-in default; override on collision)
  bind_addr: 172.17.0.1            # host-side published-port bind only; empty = docker0 gateway IP; 0.0.0.0 = all interfaces
  storage_path: ~/.gh-sr/cache     # host directory for cached data ($HOME expansion supported)
  retention_days: 90               # 0 = server default (90)
  max_size_bytes: 0                # 0 = unbounded
  max_usage_percent: 90            # 0 = server default (90)
  image: ghcr.io/falcondev-oss/github-actions-cache-server:latest   # pin a digest for reproducible deploys
  management_api_key: env:CACHE_MGMT_KEY   # optional; empty = auto-generate and persist one
  url_override: http://10.0.0.5:3000/      # escape hatch for exotic topologies; must include scheme
```

Notes:

- `management_api_key` supports `env:VAR` references resolved from the environment at CLI startup. When empty, gh-sr generates a random key on first deploy and persists it (mode `0600`) under the storage directory.
- Native-mode runners are out of scope: they keep GitHub's cache service (the injection is a container-env mechanism).

## CLI

```bash
gh sr cache status             # per-host: running/healthy, URL, bind, storage usage
gh sr cache deploy             # explicit deploy / upgrade (idempotent)
gh sr cache prune              # trigger the management-API cleanup endpoint (best-effort)
gh sr cache remove             # stop and remove the cache container (runners keep running)
gh sr cache remove --purge-data  # also delete the storage directory
```

`gh sr cache remove` is the only uninstall path — removing a runner never removes the cache.

## Health checks

`gh sr doctor` (on hosts with container-mode runners) verifies the cache when enabled:

- **FAIL** — the container exists but `/health` is not healthy: check `docker logs gh-sr-cache`;
- **FAIL** — the host-side health is fine but the URL is **not reachable from a runner container** (a default-deny INPUT firewall blackholing container traffic — the failure mode behind timeouts on every `upload-artifact` / `actions/cache` step): the check prints the exact allow rule to run;
- **WARN** — enabled but not deployed: run `gh sr cache deploy`;
- **WARN** — bound to `0.0.0.0`: the cache API answers on every host interface; set `cache.bind_addr` (e.g. the docker0 gateway IP);
- OK — healthy at the runner-facing URL, plus a per-runner reachability probe result.

`gh sr doctor` also reports `container-cache-env` on agentic instances whose runner `.env` is missing `CUSTOM_ACTIONS_RESULTS_URL` (fix with `gh sr up <name>` after deploying the cache).

`gh sr disk usage` includes the per-host cache storage directory as a `gh-sr-cache` row.

## How a restore hits the server

1. Runner starts; the entrypoint writes `CUSTOM_ACTIONS_RESULTS_URL=http://<docker0-gateway>:27420/` into the runner `.env` (only when the cache is enabled and a URL resolves).
2. The fork runner propagates it to `actions/cache` (node) and cache hook steps.
3. Restore/save requests go to the local server; everything else flows through to GitHub unchanged.
