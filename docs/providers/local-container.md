# Local Container Provider

Read when:

- choosing `provider: local-container`, `provider: docker`, or `provider: container`;
- using Docker Desktop, OrbStack, Colima, Lima, or another Docker-compatible local runtime;
- changing `internal/providers/localcontainer`.

Local Container is an SSH lease provider for Linux containers on the local
machine. Crabbox uses the configured Docker-compatible CLI to start a labeled
container, publish its SSH port on loopback, syncs the current checkout into the
container over SSH, runs the command, and removes the container on stop.

## When To Use

Use Local Container when:

- you want a zero-cloud Linux smoke path;
- the local Docker-compatible runtime is already warm;
- you want a local visible desktop, browser, screenshot, or input smoke before
  spending cloud capacity;
- you want the same Crabbox sync, logs, artifacts, scripts, and `ssh` workflow
  before moving to remote capacity.

Use AWS, Azure, Google Cloud, Hetzner, Proxmox, or another remote provider when
you need stronger host separation, larger capacity, cross-OS coverage,
coordinator-backed portal desktops, shared team infrastructure, or
provider-owned cleanup.

## Quick Start

```sh
docker info
crabbox run --provider local-container -- pnpm test
crabbox warmup --provider docker --slug local-smoke
crabbox run --provider docker --id local-smoke -- pnpm test:changed
crabbox ssh --provider docker --id local-smoke
crabbox stop --provider docker local-smoke
```

Desktop/browser smoke:

```sh
crabbox warmup --provider docker --desktop --browser --slug local-ui
crabbox desktop doctor --provider docker --id local-ui
crabbox screenshot --provider docker --id local-ui --output desktop.png
crabbox desktop click --provider docker --id local-ui --x 120 --y 120
crabbox webvnc --provider docker --id local-ui
```

`docker` is an alias for `local-container`. The provider talks only to the
Docker-compatible CLI and daemon; it does not use Docker Desktop-specific APIs.
OrbStack works when it is the active `docker` context.

## Config

```yaml
provider: local-container
localContainer:
  runtime: docker
  image: debian:bookworm
  user: crabbox
  workRoot: /work/crabbox
  cpus: 0
  memory: ""
  network: bridge
  dockerSocket: false
```

Provider flags:

```text
--local-container-runtime <path-or-name>
--local-container-image <image>
--local-container-user <user>
--local-container-work-root <path>
--local-container-cpus <n>
--local-container-memory <size>
--local-container-network <network>
--local-container-docker-socket
```

Environment overrides:

```text
CRABBOX_LOCAL_CONTAINER_RUNTIME
CRABBOX_LOCAL_CONTAINER_IMAGE
CRABBOX_LOCAL_CONTAINER_USER
CRABBOX_LOCAL_CONTAINER_WORK_ROOT
CRABBOX_LOCAL_CONTAINER_CPUS
CRABBOX_LOCAL_CONTAINER_MEMORY
CRABBOX_LOCAL_CONTAINER_NETWORK
CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET
```

Set `localContainer.dockerSocket: true` or
`CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET=1` when commands inside the lease need
Docker. Crabbox mounts the active local Unix Docker socket into the container as
`/var/run/docker.sock`, so `docker` commands run against the active local
Docker-compatible daemon. Remote Docker contexts are rejected. When the socket is
enabled and no work root is explicitly configured, Crabbox uses a host-visible
cache work root and mounts it at the same absolute path inside the lease, so
nested Docker bind mounts can see the synced checkout.

## Behavior

1. `warmup` or a fresh `run` creates a per-lease SSH key.
2. The provider runs `docker run -d` with Crabbox labels, loopback SSH port
   publishing, and public-key auth environment for the bootstrap script.
3. The container installs `openssh-server`, `git`, `rsync`, `curl`, and `sudo`
   when the image is Debian/Ubuntu-compatible and missing those tools.
4. With `--desktop`, the container installs and starts Xvfb, XFCE, x11vnc,
   xdotool, screenshot tools, ffmpeg, noVNC, and websockify without requiring
   systemd.
5. With `--browser`, the container installs a real package-manager browser
   where the image provides one and writes `/var/lib/crabbox/browser.env`.
6. Crabbox waits for SSH readiness, syncs tracked and nonignored files into
   `localContainer.workRoot`, and uses the normal SSH executor.
7. `status`, `list`, and `stop` inspect or remove labeled containers.

## Limits

- Linux target only.
- No Crabbox coordinator support; lifecycle is local to the machine running the
  CLI.
- Desktop, browser, VNC, WebVNC, screenshot, video, and desktop input helpers
  are local-only. `webvnc` starts noVNC/websockify on the target and tunnels it
  over SSH; it does not use the authenticated Crabbox portal.
- No code-server, Tailscale bootstrap, or native checkpoint support yet.
- Docker socket pass-through is opt-in and gives the lease access to the host
  Docker daemon.
- `warmup --actions-runner` is not supported; use normal `crabbox run` for
  local container smoke tests or a remote SSH provider for GitHub runner
  registration.
- The Docker daemon is a powerful local capability. Do not treat this as the
  same host isolation boundary as a remote VM or microVM.
- The current checkout is synced into the container by default. Crabbox does not
  bind-mount the repo or mount the Docker socket.
- The default `debian:bookworm` image bootstraps packages on first start. Use a
  prebuilt image with SSH/Git/rsync/desktop/browser packages when startup time
  matters.

## Runtime Notes

The provider expects Docker-compatible behavior for:

- `docker run`;
- `docker ps`;
- `docker inspect`;
- `docker rm`;
- labels;
- loopback port publishing.

That keeps the backend portable across Docker Desktop, OrbStack, Colima, and
other runtimes that expose the standard Docker CLI. Remote Docker contexts are
not the intended MVP path because Crabbox connects to the published SSH port
from the local machine.

## Related

- [Provider reference](README.md)
- [Static SSH](ssh.md)
- [Sync](../features/sync.md)
- [Provider backends](../provider-backends.md)
