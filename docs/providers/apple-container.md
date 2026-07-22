# Apple Container Provider

Read this when you:

- choose `provider: apple-container` (aliases `apple`, `applecontainer`);
- run Crabbox against Apple's [`container`](https://github.com/apple/container)
  runtime on Apple silicon macOS;
- change `internal/providers/applecontainer`.

Apple Container is an SSH-lease provider that runs leases as Linux containers on
the local Mac using Apple's native `container` CLI. Crabbox starts a labeled
container through `container run`, reads the container's routable IP address from
`container inspect`, syncs the current checkout into the container over SSH, runs
the command with the normal SSH executor, and removes the container on `stop`.
Everything stays on the Mac running the CLI — there is no coordinator
involvement.

This provider is a provider-owned adapter: it does not reuse the Docker-specific
code in [Local Container](local-container.md), because Apple's runtime is not
assumed to be Docker-compatible. In particular, Apple containers receive their
own routable IP on the host bridge, so Crabbox connects straight to the
container IP on the standard SSH port instead of publishing a loopback host
port.

## When to use it

Reach for Apple Container when you want:

- a zero-cloud Linux smoke path on Apple silicon macOS using Apple's first-party
  container runtime;
- to avoid installing a Docker-compatible runtime when `container` is already
  present.

Use [Local Container](local-container.md) when you run Docker Desktop,
OrbStack, Colima, or another Docker-compatible runtime, or when you need
desktop/browser smoke. Use a remote provider — [AWS](aws.md), [Azure](azure.md),
[Google Cloud](gcp.md), [Hetzner](hetzner.md), or [static SSH](ssh.md) — when you
need stronger host separation, larger capacity, or shared team infrastructure.

## Prerequisites

- macOS 26 (or newer) on Apple silicon. Apple's `container` runtime is supported
  on macOS 26; older releases are not supported. The provider is gated to
  `darwin/arm64`; `doctor` reports a clear error on other platforms.
- Install Apple's `container` CLI from <https://github.com/apple/container> and
  start the background service once:

  ```sh
  container system start
  ```

## Quick start

```sh
container system status
crabbox run --provider apple-container -- pnpm test

crabbox run --provider apple-container --arch arm64 -- uname -m
crabbox run --provider apple-container --arch amd64 -- uname -m

crabbox warmup --provider apple --slug apple-smoke
crabbox run --provider apple --id apple-smoke -- pnpm test:changed
crabbox ssh --provider apple --id apple-smoke
crabbox stop --provider apple apple-smoke
```

Cache-volume smoke:

```sh
crabbox run --provider apple-container \
  --cache-volume pnpm-store=my-app-apple-container-pnpm:/var/cache/crabbox/pnpm \
  -- pnpm test
```

## Configuration

```yaml
provider: apple-container
target: linux
architecture: arm64        # native default; set amd64 for an x86_64 guest
appleContainer:
  cliPath: container        # path to Apple's container CLI
  image: ubuntu:26.04       # base image; defaults to the Crabbox OS image (--os)
  user: crabbox             # SSH user created inside the container
  workRoot: /work/crabbox   # remote Crabbox work root
  cpus: 0                   # CPU limit; 0 leaves the runtime default
  memory: ""                # memory limit, e.g. 8g
  extraRunArgs: []          # extra args appended to `container run`
```

Defaults applied when unset: `cliPath=container`, `image=` the Crabbox OS image
default (follows `--os`; currently `ubuntu:26.04`), `user=crabbox`,
`workRoot=/work/crabbox`, SSH port `22`. If provider code is constructed
directly without the normal config layer, an empty `appleContainer.image` falls
back to the same Crabbox OS image default.

The implicit guest architecture is native `arm64`. Explicit `--arch arm64` and
`--arch amd64` selections are forwarded to `container run --arch`, so the
reported Crabbox architecture matches the guest runtime architecture.

Provider flags:

```text
--apple-container-cli <path-or-name>
--apple-container-image <image>
--apple-container-user <user>
--apple-container-work-root <path>
--apple-container-cpus <n>
--apple-container-memory <size>
--apple-container-extra-run-args "<space separated args>"
```

Environment overrides:

```text
CRABBOX_APPLE_CONTAINER_CLI
CRABBOX_APPLE_CONTAINER_IMAGE
CRABBOX_APPLE_CONTAINER_USER
CRABBOX_APPLE_CONTAINER_WORK_ROOT
CRABBOX_APPLE_CONTAINER_CPUS
CRABBOX_APPLE_CONTAINER_MEMORY
CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS
```

No secrets are passed as CLI arguments. The only key material handed to the
container is the per-lease SSH public key, supplied through an environment
variable consumed by the bootstrap script.

## Lease behavior

1. `warmup` or a fresh `run` creates a per-lease SSH key.
2. The provider runs `container run -d` with Crabbox labels and the public-key
   auth environment the bootstrap script needs. Apple containers are reachable
   on their own IP, so no host port is published.
3. On Debian/Ubuntu-compatible images, the container installs
   `openssh-server`, `git`, `rsync`, `curl`, and `sudo` when missing, creates the
   SSH user, writes the authorized key, and starts `sshd`.
4. Configured `cache.volumes` are created as host cache directories under the
   local user cache directory and mounted into the container with
   `container run --volume <host-cache>:<path>`.
5. Crabbox reads the container IP from `container inspect`
   (`networks[0].address` or `networks[0].ipv4Address`, stripped of its CIDR suffix), waits for SSH
   readiness, syncs tracked and non-ignored files into `appleContainer.workRoot`,
   then drives the command over the normal SSH executor.
6. `status`, `list`, and `stop` inspect or remove labeled containers via
   `container ls --all --format json`, `container inspect`, and
   `container delete --force`.
7. `cleanup --provider apple-container` removes exactly claimed stopped containers
   and exactly claimed running non-`keep` containers whose local claim is stale
   past the idle timeout plus a safety grace period, and prunes orphaned claims.

## Limits and caveats

- macOS on Apple silicon only; the provider is gated to `darwin/arm64`.
- Linux target only; `--tailscale` and non-Linux targets are rejected.
- `--actions-runner` is not supported: the container runs `sshd` as PID 1 with no
  init system, so the GitHub Actions runner installer (which needs `systemctl`)
  cannot run. Use a remote SSH provider for `--actions-runner` workflows.
- No coordinator support; lifecycle is local to the Mac running the CLI.
- No desktop, browser, VNC, code-server, Tailscale bootstrap, or checkpoint
  support. Use [Local Container](local-container.md) for local desktop/browser
  smoke.
- `cache.volumes` are supported for rebuildable dependency caches. They are
  local to the Mac user account and are not shared with other machines.
- The default image (the Crabbox OS image, currently `ubuntu:26.04`) bootstraps
  packages on first start. The bootstrap expects a Debian/Ubuntu-compatible
  image with `apt-get`; use a prebuilt image with SSH/Git/rsync packages when
  startup time matters, when you choose another compatible base, or when the
  container has no network egress to install them.
- If Apple's default container DNS setup does not inherit a working resolver,
  Crabbox passes detected host resolvers through `--dns` by default. Pass an
  explicit resolver through `--apple-container-extra-run-args '--dns <resolver>'`
  or configure the equivalent `appleContainer.extraRunArgs` value to override it.
- If the bootstrap container exits before SSH is ready, Crabbox fails as soon as
  the runtime reports the stopped state and includes a short `container logs`
  tail instead of waiting for the full SSH timeout.

## Optional live smoke

Run the guarded local smoke on an Apple silicon Mac with Apple's `container`
service running:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=apple-container scripts/live-smoke.sh
```

The smoke creates one short-lived Apple container, waits for SSH readiness,
syncs the current checkout, runs the shared live-smoke command, prints recent
history/log evidence when available, then stops only the lease it created. It
uses `--ttl 15m --idle-timeout 5m` and the same cleanup path as normal
`crabbox stop --provider apple-container`.

## Runtime expectations

The backend relies on the documented Apple `container` CLI surface:

- `container system start` / `container system status`;
- `container run -d --name --label --env --cpus --memory --volume <image> <args>`;
- `container ls --all --format json`;
- `container inspect <id>` (network address from `networks[].address` or `networks[].ipv4Address`);
- `container delete --force <id>`.

A few details of the JSON shape (the exact label location and whether
`container run` echoes an id on stdout) are not fully pinned down by the public
docs; the adapter chooses the most CLI-consistent behavior and notes those
assumptions in code comments. Full create/SSH/sync/run/status/delete coverage
requires a real Apple silicon Mac with `container` installed.

## Related

- [Provider reference](README.md)
- [Local Container](local-container.md)
- [Static SSH](ssh.md)
- [Sync](../features/sync.md)
