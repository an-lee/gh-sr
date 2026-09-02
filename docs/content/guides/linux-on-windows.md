---
title: "Linux on Windows"
weight: 40
---

# Linux runners on a Windows host

You can run Linux container runners on a Windows machine without a separate SSH endpoint into WSL2. Set `mode: docker` on a runner that targets an `os: windows` host and **gh sr** will manage the Docker container over the same SSH connection (Docker CLI invoked through the same encoded PowerShell path as native Windows runners).

**Requirements on the Windows host:**

- OpenSSH Server enabled (see [Host setup — Windows](../host-setup.md#windows-openssh-and-docker))
- Docker Desktop installed and running with the default Linux containers mode

```yaml
hosts:
  win-pc:
    addr: user@192.168.1.51
    os: windows
    arch: amd64

runners:
  # Native Windows runner
  - name: myapp-win
    repo: owner/repo
    host: win-pc
    labels: [self-hosted, Windows, X64]

  # Linux runner via Docker Desktop on the same Windows host
  - name: myapp-linux
    repo: owner/repo
    host: win-pc
    mode: docker
    labels: [self-hosted, Linux, X64]

  # Linux runner in a Docker container for service-container isolation
  - name: myapp-linux-container
    repo: owner/repo
    host: win-pc
    mode: docker
    labels: [self-hosted, Linux, X64]
```

Both runners share a single SSH connection to Windows. The native runner starts `run.cmd` via PowerShell; the Docker runner calls `docker run` the same way, which talks to Docker Desktop's Linux engine.

Docker-mode Linux containers include a bind mount of `/var/run/docker.sock` so jobs can run `docker` against the same engine (GitHub Actions service containers, image pulls, and similar). Use **Linux containers** in Docker Desktop (the default); Windows containers mode cannot run the `actions-runner` Linux image.

**Docker socket permissions:** Inside Docker Desktop's Linux engine, `/var/run/docker.sock` is often group-restricted (like on Linux). The official `actions-runner` image runs as user `runner` (uid 1001), so **gh sr** probes the socket's owning GID by running a short disposable `docker run` (using the same runner image) and, when it gets a non-zero numeric GID, passes **`--group-add`** on `docker run` for your runner container (the same fix as on bare Linux hosts, without needing Unix `stat` on the Windows side). If the probe fails, gh sr still bind-mounts the socket (mount-only fallback). No `docker_socket` override is needed or supported on Windows hosts.

**Service containers:** For workflows using `services:`, the container mode provides an inner Docker daemon that service containers attach to. Use **`runner_mode: container`** (or in this guide's `mode: docker` form, leave the default DinD setup). See [Host setup](../host-setup.md#container-mode).
