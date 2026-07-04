# Apple VM Provider

Use `provider: apple-vm` when an Apple Silicon Mac should run tests in a full
ARM64 Linux virtual machine without a cloud account or third-party VM daemon.
Crabbox boots Ubuntu with Apple's `Virtualization.framework`, exposes SSH on a
loopback-only host port, then uses the normal Crabbox sync and command path.

The aliases are `applevm`, and — from before the provider rename — `apple-vz`
and `applevz`. The provider is direct and local: it never contacts the Crabbox
coordinator and needs no cloud credentials. Deprecated `appleVZ:` configuration
keys, `--apple-vz-*` flags, and `CRABBOX_APPLE_VZ_*` variables keep working.

**Target:** Linux ARM64.

**Host:** Apple Silicon macOS.

## When to use it

Apple VM is useful when tests need machine semantics rather than only a
container:

- a real Linux kernel, EFI boot, systemd, cloud-init, and a writable root disk;
- an isolated per-lease VM with explicit CPU, memory, and disk sizing;
- Ubuntu cloud-image behavior close to common cloud VMs;
- no Docker, Multipass, Parallels, or Tart dependency.

Choose a different local provider when its ownership model fits better:

- `apple-container`: fastest container-oriented Linux runs;
- `apple-machine`: persistent Apple Container development machines;
- `local-container`: Docker-compatible images and container tooling;
- `multipass`: Canonical-managed Ubuntu VMs;
- `parallels`: Linux, macOS, or Windows VMs plus checkpoint workflows;
- `tart`: macOS VM leases.

Apple VM is not a claim that containers lack VM-backed isolation. Its
difference is full-machine Linux boot and disk semantics under direct
`Virtualization.framework` control.

## Requirements

- Apple Silicon Mac running macOS 13 or newer;
- Xcode command-line tools;
- hardware virtualization available to the current macOS host;
- enough free disk for the downloaded image, converted raw cache, and lease
  disks.

The Xcode tools provide `codesign`, `hdiutil`, and `newfs_msdos`. Check the
host before provisioning:

```sh
xcode-select -p
sysctl -n kern.hv_support
crabbox doctor --provider apple-vm
```

`kern.hv_support` must report `1`. Nested Apple virtualization is commonly
unavailable inside macOS VMs, including Parallels guests, even when the outer
VM product exposes a nested-virtualization setting.

## Installation

Apple Silicon Homebrew bottles and release archives include both:

```text
crabbox
crabbox-apple-vm-helper
```

Crabbox finds the sibling helper automatically. The helper is pure Go and
carries the Swift VM daemon (`crabbox-apple-vm-vmd`, built on Apple's
`Virtualization.framework` with no third-party dependencies) as an embedded
payload. On first use the helper installs a content-addressed copy of the
daemon in the Crabbox state directory and ad-hoc signs it with the required
virtualization and local-network entitlements; only that signed daemon talks
to `Virtualization.framework`. A managed daemon is refreshed only when its
signed copy no longer matches the recorded SHA-256 digest, and older inactive
copies are pruned as new versions are installed.

For source development:

```sh
go build -trimpath -o ./bin/crabbox ./cmd/crabbox
go build -trimpath -o ./bin/crabbox-apple-vm-helper ./cmd/crabbox-apple-vm-helper
swift build --package-path vmd -c release
cp vmd/.build/release/crabbox-apple-vm-vmd ./bin/

./bin/crabbox doctor --provider apple-vm
```

Source builds resolve the daemon from a sibling `crabbox-apple-vm-vmd`, PATH,
or the `CRABBOX_APPLE_VM_VMD` override; release helpers embed it (built with
`-tags vmdembed` after `scripts/build-vmd.sh`). If the helper is elsewhere,
use `--apple-vm-helper`, `appleVM.helperPath`, or `CRABBOX_APPLE_VM_HELPER`.

## Quick start

```sh
crabbox doctor --provider apple-vm

# One-shot VM: create, sync, run, delete.
crabbox run --provider apple-vm -- uname -a

# Retained VM: create once, inspect, reuse, then delete.
crabbox warmup --provider apple-vm --slug vz-dev
crabbox status --provider apple-vm --id vz-dev
crabbox run --provider apple-vm --id vz-dev -- systemctl is-system-running
crabbox ssh --provider apple-vm --id vz-dev
crabbox stop --provider apple-vm vz-dev
```

Use `crabbox list --provider apple-vm --json` to inspect all local Apple VM
leases. `crabbox cleanup --provider apple-vm` removes stopped or otherwise
cleanup-eligible instances and stale claims. Cold image download or conversion
appears as `starting`; SSH host and port fields remain empty until the VM
daemon publishes a real endpoint.

## Configuration

```yaml
provider: apple-vm
os: ubuntu:26.04
appleVM:
  # Optional for normal Homebrew/release installs.
  helperPath: /custom/path/crabbox-apple-vm-helper

  # Optional image override. Remote URLs require imageSHA256.
  image: https://cloud-images.ubuntu.com/releases/resolute/release-20260520/ubuntu-26.04-server-cloudimg-arm64.img
  imageSHA256: 5e091e27d60116efbb0c743b8dd5cb2d15618e414ef04db0817ed43c8e2d7c7b

  user: crabbox
  workRoot: /work/crabbox
  cpus: 4
  memoryMiB: 8192
  diskGiB: 30
```

Defaults:

| Setting | Default |
| --- | --- |
| Architecture | `arm64` |
| OS image | `ubuntu:26.04` |
| SSH user | `crabbox` |
| Work root | `/work/crabbox` |
| CPUs | `4` |
| Memory | `8192` MiB |
| Disk | `30` GiB |

CPU and disk values must be positive. Memory must be at least `1024` MiB.
These constraints apply consistently to file, environment, and command-line
configuration.

`--arch arm64` is accepted. Explicit `--arch amd64` is rejected because
`Virtualization.framework` on Apple Silicon boots ARM64 guests for this
provider.

Provider flags:

```text
--apple-vm-helper <path>
--apple-vm-image <local-path>
--apple-vm-image-sha256 <sha256>
--apple-vm-user <user>
--apple-vm-work-root <path>
--apple-vm-cpus <n>
--apple-vm-memory <mib>
--apple-vm-disk <gib>
```

Environment overrides:

```text
CRABBOX_APPLE_VM_HELPER
CRABBOX_APPLE_VM_IMAGE
CRABBOX_APPLE_VM_IMAGE_SHA256
CRABBOX_APPLE_VM_USER
CRABBOX_APPLE_VM_WORK_ROOT
CRABBOX_APPLE_VM_CPUS
CRABBOX_APPLE_VM_MEMORY
CRABBOX_APPLE_VM_DISK
```

## Images and integrity

The portable `osImage` selector maps to dated Ubuntu ARM64 cloud images:

- `ubuntu:24.04`: Noble;
- `ubuntu:26.04`: Resolute.

Built-in remote URLs have pinned SHA-256 digests. A custom HTTP or HTTPS image
must also set `appleVM.imageSHA256`; Crabbox refuses an unverified remote image.
A local image path may omit the checksum. When one is set, Crabbox hashes while
copying the image into its owner-only cache and boots only that verified copy.
The image's raw or virtual size must not exceed `appleVM.diskGiB`.

Remote images must use HTTPS. Plain HTTP is accepted only for loopback
development servers. Downloads are capped at 32 GiB before checksum
verification. Apple VM state directories are owner-only (`0700`); downloaded
images, converted bases, VM disks, metadata, and logs are `0600`.

Remote and signed URLs are accepted through `appleVM.image` in a protected
configuration file or `CRABBOX_APPLE_VM_IMAGE`. Never put one on the command
line: the shell and Crabbox process arguments see flag values before Crabbox
can validate them. `--apple-vm-image` accepts local paths only. Crabbox removes
the image variable from helper environments, forwards the complete request
over stdin, and represents remote images in logs and persisted lease metadata
only by a checksum-derived identity such as `remote:sha256:6a61b967ba4a`.

`appleVM.user` must be a valid POSIX account name. `appleVM.workRoot` must be
a clean absolute POSIX guest path. Each path segment may contain letters,
numbers, dots, underscores, and hyphens. Spaces and shell-active characters
are rejected because the path also crosses SSH and rsync command boundaries.

Standalone QCOW2 cloud images are converted once into a sparse raw base image.
Images with backing files or backing chains are rejected so conversion cannot
read other host files. Each lease gets a clone or sparse copy of that base,
resized to `diskGiB`. Changing the image reference, expected checksum, or source
file creates a new cache entry. Interrupted downloads and conversions exit
gracefully, remove their staging files, and detach seed-image mounts. A later
run also removes unlocked staging files left by a forced process termination
or host restart.

## Lifecycle and networking

1. Crabbox creates a per-lease SSH key and asks the helper to start an instance.
2. The helper verifies/downloads the image, prepares the root disk and NoCloud
   seed disk, then spawns the signed VM daemon, which creates the EFI variable
   store and boots the VM.
3. Cloud-init creates the SSH user and work root, then starts a bounded pool of
   guest-initiated VSOCK channels to the daemon.
4. The daemon activates one reverse channel for each connection to its
   ephemeral `127.0.0.1` SSH port. Slow boot and guest proxy restarts cannot
   strand host-side connection attempts, and the SSH endpoint is not exposed
   on the LAN.
5. Crabbox waits for `/usr/local/bin/crabbox-ready`, records the lease claim,
   syncs the checkout, and runs the command.
6. `stop` terminates the owning VM daemon process and removes the per-lease VM
   state. Failed acquisitions roll back even when `--keep` was requested.

The VM uses NAT for outbound networking. There is no inbound LAN address,
Tailscale bootstrap, desktop, browser, VNC, or code-server surface.

## State and disk usage

The default macOS state root is:

```text
~/Library/Application Support/crabbox/state/apple-vm
```

With `XDG_STATE_HOME`, it is:

```text
$XDG_STATE_HOME/crabbox/apple-vm
```

Important paths beneath that root:

```text
cache/downloads/                  verified source downloads
cache/images/                     converted sparse raw images
helper/                           managed signed VM daemon and integrity digests
instances/<name>/instance.json    lifecycle metadata
instances/<name>/helper.log       VM daemon process output
instances/<name>/console.log      Linux serial console output, capped at 8 MiB
instances/<name>/disk.raw         per-lease root disk
```

Downloaded and converted base images are shared caches. Stopping a lease
removes its instance directory but keeps reusable base-image caches. Each VM
run starts a fresh console log. The daemon continues draining guest serial
output after the 8 MiB cap so a noisy or hostile guest cannot grow host storage
or block on a full logging pipe.

## Troubleshooting

Start with:

```sh
crabbox doctor --provider apple-vm
crabbox list --provider apple-vm --json
sysctl -n kern.hv_support
```

### Helper not found

Reinstall the Apple Silicon Homebrew bottle or release archive. For a source
checkout, build `./cmd/crabbox-apple-vm-helper` beside the CLI or pass its path
explicitly.

### Runtime VM creation fails

Confirm the command runs on a physical Apple Silicon macOS host with
`kern.hv_support=1`. A clean macOS guest is useful for installation checks but
usually cannot run another `Virtualization.framework` VM.

### SSH readiness times out

Read the bounded diagnostics printed by Crabbox, then inspect the retained log
files if the instance still exists:

```sh
root="$HOME/Library/Application Support/crabbox/state/apple-vm"
find "$root/instances" -name helper.log -o -name console.log
```

The serial console usually shows cloud-init, filesystem, or boot failures.
`helper.log` shows host-side VM and VSOCK failures from the daemon.

### Disk use grows

List the cache and instance sizes:

```sh
du -sh "$HOME/Library/Application Support/crabbox/state/apple-vm"/*
```

Stop active leases before manually removing state. Converted images are
rebuildable, but deleting them makes the next run reconvert the cloud image.

## Current limits

- experimental, local-only, Apple Silicon only, Linux ARM64 only;
- no checkpoint, fork, restore, or snapshot support;
- no suspend/resume: retained leases remain running until stopped;
- no desktop, browser, VNC, WebVNC, or code-server;
- no Tailscale bootstrap or inbound LAN networking;
- first use downloads and converts the selected cloud image and is slower than
  subsequent leases.
