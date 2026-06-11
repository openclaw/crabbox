# Apple VZ Provider

Read this when you:

- choose `provider: apple-vz` (alias `applevz`);
- want a local Linux VM on Apple Silicon using Apple's `Virtualization.framework`;
- change `internal/providers/applevz` or `internal/applevzhelper`.

`apple-vz` is an experimental local SSH-lease provider. Crabbox shells out to a
small macOS helper, boots an Ubuntu cloud image with `Virtualization.framework`,
exposes guest SSH through a host-local VSOCK proxy, then reuses the normal
Crabbox SSH sync and command path.

The provider is local only. It never uses the coordinator or cloud
credentials.

**Targets:** Linux.

**Hosts:** Apple Silicon macOS only.

## Host requirements

- macOS on Apple Silicon
- Xcode command-line tools, for `codesign`, `hdiutil`, and `newfs_msdos`
- a built helper binary, for example:

```sh
go build -o ./bin/crabbox-apple-vz-helper ./cmd/crabbox-apple-vz-helper
```

Crabbox copies that helper into its user state directory and ad-hoc signs the
managed copy with the Apple virtualization and local networking entitlements it
needs. The source helper binary itself is left untouched.

## Quick start

```sh
go build -o ./bin/crabbox ./cmd/crabbox
go build -o ./bin/crabbox-apple-vz-helper ./cmd/crabbox-apple-vz-helper

./bin/crabbox doctor --provider apple-vz --apple-vz-helper ./bin/crabbox-apple-vz-helper
./bin/crabbox run --provider apple-vz --apple-vz-helper ./bin/crabbox-apple-vz-helper -- echo apple-vz-ok

./bin/crabbox warmup --provider apple-vz --apple-vz-helper ./bin/crabbox-apple-vz-helper --slug apple-vz-smoke
./bin/crabbox status --provider apple-vz --id apple-vz-smoke
./bin/crabbox ssh --provider apple-vz --id apple-vz-smoke
./bin/crabbox stop --provider apple-vz apple-vz-smoke
```

## Configuration

```yaml
provider: apple-vz
appleVZ:
  helperPath: ./bin/crabbox-apple-vz-helper
  image: https://cloud-images.ubuntu.com/releases/resolute/release/ubuntu-26.04-server-cloudimg-arm64.img
  user: crabbox
  workRoot: /work/crabbox
  cpus: 4
  memoryMiB: 8192
  diskGiB: 30
```

Defaults applied when unset: `user=crabbox`, `workRoot=/work/crabbox`,
`cpus=4`, `memoryMiB=8192`, `diskGiB=30`.

The default `appleVZ.image` follows Crabbox's portable `osImage` selector:

- `ubuntu:24.04` → Noble arm64 cloud image
- `ubuntu:26.04` → Resolute arm64 cloud image

Provider flags:

```text
--apple-vz-helper <path>
--apple-vz-image <path-or-url>
--apple-vz-user <user>
--apple-vz-work-root <path>
--apple-vz-cpus <n>
--apple-vz-memory <mib>
--apple-vz-disk <gib>
```

Environment overrides:

```text
CRABBOX_APPLE_VZ_HELPER
CRABBOX_APPLE_VZ_IMAGE
CRABBOX_APPLE_VZ_USER
CRABBOX_APPLE_VZ_WORK_ROOT
CRABBOX_APPLE_VZ_CPUS
CRABBOX_APPLE_VZ_MEMORY
CRABBOX_APPLE_VZ_DISK
```

## Lease behavior

1. Crabbox ensures a per-lease SSH key and starts the `apple-vz` helper.
2. The helper downloads or reuses the configured Ubuntu cloud image, converts
   qcow2 images to sparse raw once, clones a per-lease disk, and writes a
   NoCloud seed disk.
3. The helper boots the VM headless with NAT networking, an EFI variable store,
   and a VSOCK device.
4. Cloud-init creates the SSH user, installs the Crabbox readiness marker, and
   starts a guest-side VSOCK to SSH bridge.
5. The helper exposes that bridge on a host-local `127.0.0.1:<port>` listener.
6. Crabbox waits for SSH readiness, then syncs and runs commands normally.
7. `stop` terminates the helper process and removes the per-lease VM state.

## Limits and caveats

- Experimental, local-only, Apple Silicon only, Linux only.
- No desktop, browser, VNC, WebVNC, or code-server surfaces in v1.
- No checkpoint, fork, restore, or snapshot flow.
- No Tailscale bootstrap.
- Requires the separate helper binary today.
- The first run can take longer while Crabbox downloads and converts the base
  cloud image.

## Runtime expectations

The provider depends on:

- the `crabbox-apple-vz-helper` binary
- `codesign`
- `hdiutil`
- `newfs_msdos`
- Apple's `Virtualization.framework`

The helper validates those prerequisites during `crabbox doctor --provider apple-vz`.
