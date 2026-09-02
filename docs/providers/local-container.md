# Local Container Provider

Read this when you:

- choose `provider: local-container` (aliases `docker`, `container`, `local-docker`);
- run Crabbox against Docker Desktop, OrbStack, Colima, Lima, or another
  Docker-compatible local runtime;
- change `internal/providers/localcontainer`.

Local Container is an SSH-lease provider that runs leases as Linux containers on
the local machine. Crabbox starts a labeled container through the configured
Docker-compatible CLI, publishes the container's SSH port on loopback, syncs the
current checkout into the container over SSH, runs the command with the normal
SSH executor, and removes the container on `stop`. Everything stays on the
machine running the CLI — there is no coordinator involvement.

## When to use it

Reach for Local Container when you want:

- a zero-cloud Linux smoke path;
- to reuse an already-warm local Docker-compatible runtime;
- a local visible desktop, browser, screenshot, or input smoke before spending
  cloud capacity;
- the same Crabbox sync, logs, artifacts, scripts, and `ssh` workflow you use
  remotely, but locally.

Use a remote provider — [AWS](aws.md), [Azure](azure.md), [Google Cloud](gcp.md),
[Hetzner](hetzner.md), [Proxmox](proxmox.md), or [static SSH](ssh.md) — when you
need stronger host separation, larger capacity, cross-OS coverage,
coordinator-backed portal desktops, shared team infrastructure, or
provider-owned cleanup.

## Quick start

```sh
docker info
crabbox run --provider local-container -- pnpm test

crabbox warmup --provider docker --slug local-smoke
crabbox run --provider docker --id local-smoke -- pnpm test:changed
crabbox ssh --provider docker --id local-smoke
crabbox stop --provider docker local-smoke
```

Retained run-session handle:

```sh
crabbox run --provider local-container --keep --no-sync \
  --lease-output session.json -- true
jq . session.json
# Run the exact cleanupCommand from session.json when finished.
```

`--lease-output` also works with a reused `--id` lease when its final stop
policy cannot release the lease (the default or `--stop-after never`). Fresh
runs require `--keep`. Crabbox rejects conflicting stop policies before it
creates or resolves a container, records the handle after the exact local claim,
and leaves it in place if later sync or command execution fails.

Cache volume smoke:

```sh
crabbox run --provider local-container \
  --cache-volume pnpm-store=my-app-linux-pnpm:/var/cache/crabbox/pnpm \
  -- pnpm test
```

Desktop and browser smoke:

```sh
crabbox warmup --provider docker --desktop --browser --slug local-ui
crabbox desktop doctor --provider docker --id local-ui
crabbox screenshot --provider docker --id local-ui --output desktop.png
crabbox desktop click --provider docker --id local-ui --x 120 --y 120
crabbox webvnc --provider docker --id local-ui
```

The provider talks only to a Docker-compatible CLI and daemon; it does not use
Docker Desktop-specific APIs. Crabbox detects an installed `docker` or `podman`
CLI and uses that runtime. Set `localContainer.runtime` when you need a specific
CLI.

Runtime discovery rejects empty Docker context, endpoint, or daemon identities
and empty Podman identities, even when the runtime command exits successfully.
If the command supplies stderr, Crabbox includes its bounded diagnostic in the
error so daemon startup failures are not hidden behind an empty-identity message.
These failures stop acquisition before a lease or container is created.

### Fixed-ID replay

```sh
crabbox warmup --provider local-container \
  --lease-id cbx_abcdef123456 --slug my-app-operation
```

A fixed lease ID makes warmup replay-safe across process restarts. Crabbox
persists the runtime identity and normalized container-create intent before
starting the container, then reuses an existing container only when its lease,
slug, runtime scope, and intent fingerprint match. Changing the image or other
container-shaping configuration returns `lease_id_conflict` instead of silently
reusing the lease. An unresolved create attempt or a missing previously
acquired container also fails closed without starting a second container.
If the exact container exits, stops, or becomes dead before SSH is ready,
fixed-ID warmup fails promptly while retaining its original fixed claim and
recovery metadata when kept.

Stopping a fixed lease preserves a terminal local tombstone, so the same
operation ID cannot create another container after release. Use a new fixed
lease ID for each later operation.

## Configuration

```yaml
provider: local-container
localContainer:
  runtime: docker          # Docker-compatible CLI to invoke; detects docker/podman by default
  # image: debian:bookworm # optional trusted custom-image override
  user: crabbox            # SSH user created inside the container
  workRoot: /work/crabbox  # remote Crabbox work root
  cpus: 0                  # CPU limit; 0 leaves the runtime default
  memory: ""               # memory limit, e.g. 8g
  network: bridge          # container network
  dockerSocket: false      # mount the host Docker-compatible socket into the lease
```

Defaults applied when unset: `runtime=docker`, a reviewed Ubuntu OCI index
selected by `--os` (default `ubuntu:26.04`), `user=crabbox`, `network=bridge`,
`workRoot=/work/crabbox`, SSH port `2222`. The image reference includes an
immutable SHA-256 digest; Docker/Podman select the native platform from that
fixed index and validate its content before starting bootstrap. An unavailable
or mismatched digest fails rather than falling back to a floating tag. The
exact reference appears in provisioning output and the lease's image label.

Explicit image overrides remain trusted operator configuration, including
floating tags. Updating the OS catalog affects new leases, not retained
containers; a fixed-ID operation may report an intent conflict after a default
rotation. See [default-image rotation](../operations.md#default-container-image-pins).
When `runtime` is unset or left at `docker`, Crabbox detects an installed
container CLI. If both `docker` and `podman` are available, `docker` is selected
unless `runtime` is set explicitly.

An explicit `--arch amd64|arm64` is a native-architecture assertion against the
selected Docker daemon or Podman service, including a named remote context or
connection. Crabbox normalizes `x86_64` to `amd64` and `aarch64` to `arm64`,
continues only when the daemon matches, and reports the normalized architecture
in `--preflight` output. A mismatch or unrecognized daemon response fails before
container creation. Crabbox never adds `--platform` or opts into emulation for
this assertion. When `--arch` is omitted, the runtime keeps its existing native
behavior without an added architecture guarantee or probe.
`config show` describes this omitted-architecture state as `arch=native`
(JSON: `"architecture":"native"`), rather than presenting the compiled AMD64
compatibility default as an effective runtime architecture. Explicit selections
remain `amd64` or `arm64`; `native` is output-only.

### Memory-failure evidence

For every user command, Local Container reads the container cgroup OOM-kill
counter immediately before execution. After a nonzero exit or SSH transport
failure, it reads the counter again. Only a positive increment for that command
is reported as `resource_exhaustion=memory`; an OOM from an earlier command on a
reused lease does not classify a later ordinary failure. Docker/Podman cgroup
paths and counter parsing remain inside this provider.

The failure digest marks this condition non-retryable with the unchanged
configuration and suggests increasing `localContainer.memory` (or
`--local-container-memory`) or reducing workload concurrency. If cgroup evidence
cannot be read, Crabbox prints a diagnostic warning and preserves the original
command failure and ordinary user-command classification.

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
--local-container-volume host:container[:ro]   (repeatable)
```

`--local-container-volume` bind-mounts a host directory into the container.
Repeatable for multiple mounts. Passes through to Docker `-v`. Read-only
mounts use the `:ro` suffix. Container targets must not overlap Crabbox-managed
work, SSH, cache, or desktop configuration paths because bootstrap changes
ownership under those paths. System paths such as `/etc`, `/usr`, `/var`,
`/home`, and `/tmp` are also rejected; use an application mount point such as
`/mnt/my-app` or `/cache`.

`--local-container-docker-socket` installs Docker's CLI when the image does not
already provide it. On Debian and Ubuntu, Crabbox accepts Docker's APT
repository only after its primary signing fingerprint matches the reviewed
pin; failed verification preserves existing trust files and falls back to the
distro `docker.io` package.

**Host access:** This flag is CLI-only. Crabbox does not load bind mounts from
repo-local `.crabbox.yaml`; operators must name each host path explicitly on
the command line.

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

Host bind mounts must be passed explicitly with `--local-container-volume`.
Crabbox intentionally ignores `localContainer.volumes` from config files because
host paths are machine-specific and benefit from invocation-time review.

For runtimes that use Docker contexts or Docker-compatible API sockets, the
active socket is selected from `DOCKER_HOST` or the Docker context when socket
pass-through is enabled. Remote TCP contexts are not the intended path because
Crabbox connects to the published SSH port from the local machine. Architecture
assertions still describe the selected daemon, not the machine running the
Crabbox CLI.

### Socket pass-through

Set `localContainer.dockerSocket: true` or
`CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET=1` when commands inside the lease need to
run `docker`. Crabbox mounts the active local Unix Docker-compatible socket into
the container at `/var/run/docker.sock`, so in-lease `docker` commands run
against the host engine. This grants the lease the same effective authority as
the user controlling that engine; it is not a container isolation boundary.
Repository configuration is trusted project automation and can enable this
mode. For Podman, point `DOCKER_HOST` at the Podman socket,
for example `unix://$XDG_RUNTIME_DIR/podman/podman.sock`. Remote TCP hosts are
rejected in this mode. Basic Podman leases do not require socket pass-through.

When the socket is enabled and no work root is explicitly configured, Crabbox
uses a host-visible cache work root so nested Docker bind mounts can see the
synced checkout:

- On POSIX clients it mounts that root at the same absolute path inside the
  lease.
- On Windows npipe clients it mounts the host cache root at the Linux guest work
  root instead, because Windows paths are not valid Linux container work paths.

Socket mode syncs without preserving mtimes, so host-mounted local VM
filesystems (Docker Desktop, OrbStack, Colima, and similar) do not fail on
metadata updates.

## Lease behavior

Custom cache directories must be mounted into the Docker VM. If cache settings
change after creation, stop refuses to delete an existing bootstrap directory
outside the current trusted cache/temp roots and retains the claim and key.
Restore the original cache settings, or explicitly remove the verified residue,
then retry stop. Orphan cleanup also preserves claims with bootstrap residue.
Bootstrap paths from older releases under the current system temp root remain
supported.

1. `warmup` or a fresh `run` creates a per-lease SSH key.
2. The provider writes its bootstrap script under the user's cache directory,
   normally shared with desktop Docker VMs, then runs
   `docker run -d` with Crabbox labels, loopback SSH port
   publishing, and the public-key auth environment the bootstrap script needs.
3. On Debian/Ubuntu-compatible images, the container installs
   `openssh-server`, `git`, `rsync`, `curl`, and `sudo` when they are missing,
   then restores the selected image's declared `PATH` before each managed SSH
   login profile. Profiles added after bootstrap can prepend to or intentionally
   replace that baseline; the profile selected during bootstrap keeps its final
   managed restore block.
4. With `--desktop`, the container installs and starts Xvfb, XFCE, x11vnc,
   xdotool, screenshot tools, ffmpeg, noVNC, and websockify — no systemd
   required.
5. With `--browser`, the container preserves a working Chrome, Chromium, Firefox
   ESR, or Firefox executable and writes `/var/lib/crabbox/browser.env`. When
   Ubuntu needs a browser, bootstrap installs native Firefox from Mozilla's
   signed APT repository, with a pinned signing-key fingerprint and a
   source-specific keyring; it excludes Ubuntu's Snap transition package.
   Other Debian-compatible images try their distro Chromium, Firefox ESR, and
   Firefox packages in order, advancing when the executable's version probe
   fails. If Mozilla trust or installation fails, select a prebuilt image with
   a working browser or a supported Debian image; bootstrap does not disable
   authentication or force a downgrade of an installed transition package.
6. As soon as the runtime returns an exact container ID, before the first
   inspect or readiness probe, Crabbox durably records a `state=provisioning`
   claim bound to that ID, the runtime context/endpoint/daemon identity, SSH
   key, bootstrap directory, and host-work-root ownership. Later inspect and
   endpoint discoveries are compare-and-swap claim transitions. The claim
   changes to `state=ready` only after the SSH readiness check succeeds.
7. When `--lease-output` is requested for a retained run, core writes the
   provider-neutral run-session handle after the exact claim is recorded and
   before sync. It includes the exact lease ID and cleanup command, but no SSH
   endpoint, key path, workdir, claim internals, or container metadata.
8. Crabbox syncs tracked and non-ignored files into
   `localContainer.workRoot`, then drives the command over the normal SSH
   executor.
9. `status`, `list`, and `stop` inspect or remove labeled containers.
10. `heartbeat` and `status --wait` hydrate the exact claim's recorded runtime
   route, verify its context, endpoint, daemon, and container ID, then
   compare-and-swap touched timestamps, expiry, and any explicit idle-timeout
   replacement into that same claim. Omitting `--idle-timeout` preserves the
   stored timeout even when current config differs.
11. `cleanup --provider docker` removes stopped containers and running
   non-`keep` containers whose local claim or lease labels are stale past the
   idle timeout plus a safety grace period.
12. If a local claim remains after its container was removed outside Crabbox,
   `crabbox stop --provider docker <lease-or-slug>` removes the stale claim and
   stored SSH key.

When `warmup` or `run --keep` creates the container but SSH readiness is
canceled, fails, or times out, Crabbox keeps the exact pending claim, container,
key, and bootstrap directory. If that exact container exits, stops, or becomes
dead, acquisition and `status --wait` fail promptly instead of waiting out the
SSH timeout. `status` and inventory report the observed terminal runtime state
even when the retained ownership claim is still `provisioning` or previously
reached `ready`; checking terminal status never rewrites recovery metadata.

The failed command prints copyable, runtime-scoped `inspect` and `stop`
commands, plus `run --reclaim --sync-only` when recovery remains possible.
Terminal bootstrap failures also report the runtime state and, when the
container is restartable and its runtime route can be reproduced safely, an
exact-container `docker start` or `podman start` command; restart that container
before reclaiming it. A `dead` container cannot be restarted or reclaimed and
must be cleaned up. Running pending claims stay visible as `provisioning`, while
a disappeared container is reported as `missing` with its provisioning label
intact. `stop` accepts the exact pending claim without requiring a successful
intervening run and fences the container deletion against concurrent claim
changes. The provider cleanup sweep leaves a missing keep-enabled pending claim
for explicit recovery or `stop` instead of discarding its ownership evidence; a
missing non-keep pending claim is fenced and removed with its key and bootstrap
state. Fresh one-shot runs and `warmup --keep=false` still remove the container,
key, bootstrap directory, and active claim when readiness does not complete;
fixed IDs retain only their terminal single-use tombstone.
If an exact inspect returns a replacement container, Crabbox refuses all
destructive cleanup and retains the original ownership evidence.

Named Docker contexts and Podman connections are included in those recovery
commands. Custom runtime endpoints remain private in the local claim rather
than being printed; exact-ID commands hydrate that route from the claim before
their first runtime lookup and reject a changed endpoint or daemon identity.
Heartbeat uses the same fail-closed ownership boundary. A missing claim, stale
resolved snapshot, changed provider/runtime scope, replaced daemon, or different
container ID leaves both the container and claim untouched.

## Limits and caveats

- Linux target only; `--tailscale` and non-Linux targets are rejected.
- No coordinator support; lifecycle is local to the machine running the CLI.
- Desktop, browser, VNC, WebVNC, screenshot, video, and desktop input helpers
  are local-only. `webvnc` starts noVNC/websockify on the target and tunnels it
  over SSH; it does not use the authenticated Crabbox portal.
- No code-server and no Tailscale bootstrap.
- Native checkpoints use `docker commit` (opt in with `--mode native`):
  `crabbox checkpoint create` captures the container filesystem as a Docker image
  tagged `crabbox-checkpoint-<name>-<digest>`, `crabbox checkpoint inspect <id>
  --verify` (or `checkpoint list --verify`) confirms it, and `crabbox checkpoint
  delete <id>` removes its verified Crabbox-owned tag while preserving
  user-created tags and dependent containers. Committed images have Crabbox
  lease ownership labels cleared so derived containers are not inventoried as
  the source lease, and their mount-dependent bootstrap command is replaced with
  a persistent default command. `auto` mode keeps the workspace-archive default.
  Each checkpoint records its Docker context, context-store path, resolved
  daemon endpoint, and Docker system ID; verify and delete fail closed if that
  context or daemon is later replaced.
  This native path is currently Docker-only; Podman and nerdctl keep using
  workspace archives. Crabbox rejects native checkpoints when the workspace is
  stored in a mounted volume because `docker commit` omits mounted data.
- `crabbox checkpoint fork <id>` launches a fresh lease from the committed
  image. Fork validates the checkpoint tag and Docker system ID, replays the
  recorded Docker runtime and daemon scope, disables Docker socket passthrough,
  relocates the saved workspace into the new lease path, and persists the scope
  for later lease commands even when ambient Docker settings change. The source
  container user and work root are also replayed so relocation keeps ownership
  and path semantics intact. Fixed-ID ownership belongs to each new allocation:
  ordinary forks do not inherit the source's fixed intent, including when using
  an older checkpoint image, and can be stopped normally. When host volumes are
  attached to a fork, Crabbox resolves the restore workdir inside the container and rejects any overlap
  before clearing or extracting checkpoint data.
- `warmup --actions-runner` is not supported. Use plain `crabbox run` for local
  container smoke tests, or a remote SSH provider for GitHub runner registration.
- Socket pass-through is opt-in and grants the lease access to the host
  container engine. Do not treat the container as the same host-isolation
  boundary as a remote VM or microVM.
- The current checkout is synced into the container by default rather than
  bind-mounted; the engine socket is mounted only when explicitly enabled.
- Cache volumes persist as Docker-compatible named volumes after a container is
  stopped.
  Remove them with the Docker-compatible runtime when the cache key is obsolete.
- The default `debian:bookworm` image bootstraps packages on first start. Use a
  prebuilt image with SSH/Git/rsync/desktop/browser packages when startup time
  matters.

## Runtime expectations

The backend relies on standard Docker-compatible behavior:

- `docker`/`podman run`, `ps`, `inspect`, and `rm`;
- Docker-compatible named volumes;
- container labels;
- loopback port publishing.

That keeps it portable across Docker Desktop, OrbStack, Colima, Podman, and
other runtimes exposing the standard Docker-compatible CLI.

## Optional live smoke

Run the guarded local smoke when a Docker-compatible daemon is available:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=local-container scripts/live-smoke.sh
```

The smoke creates one short-lived local container, waits for SSH readiness,
syncs the current checkout, runs the shared live-smoke command, prints recent
history/log evidence when available, then stops only the lease it created. It
uses `--ttl 15m --idle-timeout 5m` and the same cleanup path as normal
`crabbox stop --provider local-container`.

For lower-level Docker-backed E2E coverage, run:

```sh
go test -tags localcontainer ./cmd/crabbox
```

Set `CRABBOX_LOCAL_CONTAINER_E2E_IMAGE` to use a prebuilt image for faster
startup. The test skips when the Docker CLI or daemon is unavailable.

## Related

- [Provider reference](README.md)
- [Static SSH](ssh.md)
- [Sync](../features/sync.md)
- [Provider backends](../provider-backends.md)
