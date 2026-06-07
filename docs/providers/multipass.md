# Multipass Provider

Read this when you:

- choose `provider: multipass` (aliases `mp`, `canonical-multipass`);
- want local Ubuntu VMs through Canonical Multipass instead of a local
  container;
- change `internal/providers/multipass`.

Multipass is a local SSH-lease provider. Crabbox drives the configured
`multipass` CLI on the same workstation, launches an Ubuntu VM with cloud-init,
creates a per-lease SSH user/key, syncs the checkout over SSH, runs commands
through the normal Crabbox SSH executor, and deletes the VM on `stop`.

The provider is local only. It never uses the coordinator or cloud credentials.

**Targets:** Linux.

**Hosts:** macOS, Linux, or Windows workstations with the Multipass CLI and
daemon installed.

**Capabilities:** SSH, Crabbox sync, cleanup, cache volumes.

## Host Requirements

Install Canonical Multipass before selecting this provider:

```sh
multipass version
multipass list --format json
```

On macOS, install from Canonical's installer or with Homebrew:

```sh
brew install --cask multipass
```

The first Crabbox run may take longer while Multipass downloads the selected
Ubuntu image and initializes the local VM backend.

## When To Use It

Reach for Multipass when you want:

- a local Ubuntu VM smoke path with stronger isolation than a container;
- the same Crabbox `warmup`, `run`, `ssh`, `stop`, and `cleanup` workflow used
  for remote SSH providers;
- a quick way to test against Canonical Ubuntu cloud images on macOS, Windows,
  or Linux hosts where Multipass is installed.

Use [Local Container](local-container.md) when startup speed and container cache
reuse matter more than VM isolation. Use [Parallels](parallels.md),
[Proxmox](proxmox.md), [AWS](aws.md), [Azure](azure.md), [Google Cloud](gcp.md),
or [Hetzner](hetzner.md) when you need larger machines, non-Linux targets,
desktop/browser/code surfaces, native snapshots, tailnet reachability, or shared
team infrastructure.

Multipass is not a Docker replacement in Crabbox. Docker-compatible providers
run containers that share a host kernel; Multipass runs a full Ubuntu VM with
its own kernel, systemd, cloud-init, SSH daemon, disk, and network identity.
Choose Multipass when the test needs VM-like behavior. Choose Local Container
when the test only needs a fast disposable Linux userland.

## Quick Start

```sh
multipass version
multipass list --format json

crabbox run --provider multipass -- go test ./...

crabbox warmup --provider mp --slug multipass-smoke
crabbox status --provider mp --id multipass-smoke
crabbox run --provider mp --id multipass-smoke -- uname -a
crabbox ssh --provider mp --id multipass-smoke
crabbox stop --provider mp multipass-smoke
```

Cache volume smoke:

```sh
crabbox run --provider multipass \
  --cache-volume gomod=my-app-linux-go:/var/cache/crabbox/go \
  -- go test ./...
```

## Configuration

```yaml
provider: multipass
multipass:
  cliPath: multipass      # Multipass CLI to invoke
  image: "26.04"          # Multipass image selector
  user: crabbox           # SSH user created by cloud-init
  workRoot: /work/crabbox # remote Crabbox work root
  cpus: 4
  memory: 8G
  disk: 30G
  launchTimeout: 20m
```

Defaults applied when unset: `cliPath=multipass`, `image=26.04`,
`user=crabbox`, `workRoot=/work/crabbox`, `cpus=4`, `memory=8G`, `disk=30G`,
`launchTimeout=20m`, SSH port `22`.

The Multipass image follows Crabbox's portable `osImage` default unless
`multipass.image`, `--multipass-image`, or `CRABBOX_MULTIPASS_IMAGE` is set.
For example, `osImage: ubuntu:24.04` selects `multipass.image: "24.04"`.

Provider flags:

```text
--multipass-cli <path-or-name>
--multipass-image <image>
--multipass-user <user>
--multipass-work-root <path>
--multipass-cpus <n>
--multipass-memory <size>
--multipass-disk <size>
--multipass-launch-timeout <duration>
```

Environment overrides:

```text
CRABBOX_MULTIPASS_CLI
CRABBOX_MULTIPASS_IMAGE
CRABBOX_MULTIPASS_USER
CRABBOX_MULTIPASS_WORK_ROOT
CRABBOX_MULTIPASS_CPUS
CRABBOX_MULTIPASS_MEMORY
CRABBOX_MULTIPASS_DISK
CRABBOX_MULTIPASS_LAUNCH_TIMEOUT
```

## Lease Behavior

1. `warmup` or a fresh `run` creates a per-lease SSH key.
2. The provider writes a temporary cloud-init file using Crabbox's Linux
   bootstrap and launches an instance with `multipass launch --name ...`.
3. Optional cache volumes are host directories under the Crabbox user cache.
   On macOS with the QEMU driver, Crabbox launches the VM, stops it, attaches
   each cache with `multipass mount --type native`, then starts it again.
   Other drivers use Multipass classic launch mounts with repeated
   `--mount host:guest` arguments.
4. Crabbox reads instance details with `multipass info --format json`, waits for
   SSH readiness on port `22`, syncs the checkout to `multipass.workRoot`, then
   runs the command over the normal SSH path.
5. The provider records a local Crabbox claim with the Multipass instance name.
6. `list`, `status`, and `stop` resolve by lease ID, slug, or Multipass instance
   name. `stop` deletes and purges the VM.
7. `cleanup --provider multipass` deletes stopped Crabbox VMs and running
   non-`keep` VMs whose local claim is stale past the idle timeout plus the
   direct-provider grace window.

Multipass does not expose provider labels. Crabbox therefore treats the instance
name and local lease claim as the source of ownership. Unclaimed user-created
instances are ignored by `list` unless their name starts with `crabbox-`, and
`cleanup` skips unclaimed running instances.

## Limits And Caveats

- Linux target only; non-Linux targets are rejected.
- No coordinator support; lifecycle is local to the machine running the CLI.
- No Tailscale bootstrap. Use a remote SSH provider when tailnet reachability is
  required.
- No desktop, browser, VNC, WebVNC, code-server, or native checkpoint support in
  this first implementation.
- `warmup --actions-runner` is not supported. Use plain `crabbox run` for local
  VM smoke tests, or a remote SSH provider for GitHub runner registration.
- Cache volumes are host directories, not provider-managed disks. Remove stale
  directories under the user cache when a cache key is obsolete.
- On macOS with the default QEMU driver, Crabbox uses Multipass native mounts
  for cache volumes. Other Multipass drivers use the standard classic mount
  path.
- Startup time depends on Multipass image availability, the host hypervisor, and
  first-boot package setup.

## Runtime Expectations

The backend relies on the standard Multipass CLI:

- `multipass version`
- `multipass get local.driver`
- `multipass launch --name <name> --cloud-init <file> --cpus <n> --memory <size>
  --disk <size> --timeout <seconds> [--mount <host>:<guest>] <image>`
- `multipass stop <name>`
- `multipass mount --type native <host> <name>:<guest>`
- `multipass start <name>`
- `multipass list --format json`
- `multipass info --format json <name>`
- `multipass delete --purge <name>`

See Canonical's [Multipass product page](https://canonical.com/multipass) and
the Ubuntu [Multipass CLI reference](https://documentation.ubuntu.com/multipass/latest/reference/command-line-interface/)
for installation and CLI behavior.
