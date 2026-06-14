# Firecracker

Read this when you:

- choose `provider: firecracker`;
- stage a self-hosted Linux KVM host plus guest assets for Firecracker-backed
  Crabbox runs;
- need the `firecracker.*` config keys, `CRABBOX_FIRECRACKER_*` env overrides,
  or `--firecracker-*` flags;
- want the direct lifecycle, read-only doctor contract, and guarded readiness
  smoke.

`provider: firecracker` is the direct self-hosted Firecracker SSH-lease surface
for Linux KVM hosts. Crabbox prepares per-lease state, copies the configured
rootfs into a writable lease artifact, writes a cloud-init config drive, starts
the microVM through the Firecracker Go SDK on Linux, records the guest endpoint,
and uses the normal Crabbox SSH sync/run path after the guest is reachable.

## Current contract

- Provider id: `firecracker`
- Aliases: none
- Kind: `ssh-lease`
- Targets: Linux only
- Coordinator support: never
- Declared features: `ssh`, `crabbox-sync`, `cleanup`
- Default server type: `microvm`
- Default SSH port: `22`
- Supported network mode: `cni` only

## When to use it

Use Firecracker when you want a direct Linux microVM path on infrastructure you
control and you are comfortable preparing the host, kernel, rootfs, and CNI
pieces yourself. It is the self-hosted counterpart to delegated Firecracker
surfaces such as [E2B](e2b.md) and [Tensorlake](tensorlake.md).

Choose a different provider when another ownership model fits better:

- [Local Container](local-container.md) for the fastest local Linux smoke path;
- [Multipass](multipass.md) for a Canonical-managed local Ubuntu VM;
- [Incus](incus.md) when you already have an Incus control plane;
- [Proxmox](proxmox.md) or [XCP-ng](xcp-ng.md) when you want template-clone VM
  lifecycle instead of raw microVM assembly;
- [E2B](e2b.md) or [Tensorlake](tensorlake.md) when the hosted provider should
  own the Firecracker lifecycle end to end.

## Lifecycle

Already usable surfaces:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
./bin/crabbox providers --json
CRABBOX_PROVIDER=firecracker ./bin/crabbox config show --json
./bin/crabbox doctor --provider firecracker --json
./bin/crabbox warmup --provider firecracker
```

`warmup`, `run`, `ssh`, `stop`, and `cleanup` use the same SSH-lease lifecycle
as other direct providers once the host prerequisites pass. `release` stops the
VMM and removes Crabbox's local lease state, copied rootfs, cloud-init drive,
socket, log, network namespace, and CNI cache when `firecracker.deleteOnRelease`
is true. Set `--keep` or `firecracker.deleteOnRelease=false` when you want to
retain the local artifacts for inspection.

## Host requirements

Before this provider can become ready, stage a host that meets all of these
requirements:

- Linux host. `doctor` fails on macOS and Windows because this provider expects
  a Linux KVM environment.
- `/dev/kvm` must exist and be openable by the account running Crabbox.
- The configured Firecracker binary must exist on `PATH` or at
  `firecracker.binary`.
- `firecracker.jailer` must stay unset; jailer launch is not supported yet and
  doctor fails configured jailer paths instead of reporting false readiness.
- `firecracker.kernel` and `firecracker.rootfs` must point to real files.
- `firecracker.network` must remain `cni`, and `firecracker.cniNetwork`,
  `firecracker.cniConfDir`, and `firecracker.cniBinDir` must resolve to a real
  CNI setup on the host. Doctor loads the named CNI config from the configured
  conf dir before reporting readiness.
- Size the host so the requested `firecracker.cpus`, `firecracker.memoryMiB`,
  and `firecracker.diskMiB` are realistic for the guest you plan to boot.

This provider rejects non-Linux targets and Tailscale-managed networking.

## Guest image contract

The lifecycle expects the configured kernel and rootfs pair to boot an
SSH-ready Linux guest that matches the rest of Crabbox's Linux SSH-lease
contract:

- SSH listens on port `22`.
- The configured `firecracker.user` can log in with the per-lease Crabbox SSH
  key.
- `firecracker.workRoot` is an absolute writable guest path.
- The guest image includes the normal Linux sync/run prerequisites such as
  `git`, `rsync`, `tar`, `curl`, and `python3`.
- The kernel, rootfs, architecture, and CNI networking model are compatible
  with each other.
- The configured kernel accepts the default Crabbox command line, which mounts
  the first Firecracker drive as a writable `root=/dev/vda` root filesystem.

Current `doctor` checks do **not** validate guest-side tools or boot behavior.
They validate the host-side contract and configured file paths before lifecycle
commands attempt to start a microVM.

## Configuration

```yaml
provider: firecracker
target: linux
firecracker:
  binary: firecracker
  # Leave jailer unset until jailer launch support lands.
  jailer: ""
  kernel: /var/lib/crabbox/firecracker/vmlinux
  rootfs: /var/lib/crabbox/firecracker/rootfs.ext4
  user: crabbox
  workRoot: /work/crabbox
  cpus: 4
  memoryMiB: 4096
  diskMiB: 16384
  network: cni
  cniNetwork: crabbox-firecracker
  cniConfDir: /etc/cni/conf.d
  cniBinDir: /opt/cni/bin
  launchTimeout: 2m
  deleteOnRelease: true
```

Defaults:

- `binary=firecracker`
- `kernel=/var/lib/crabbox/firecracker/vmlinux`
- `rootfs=/var/lib/crabbox/firecracker/rootfs.ext4`
- `user=crabbox`
- `workRoot=/work/crabbox`
- `cpus=4`
- `memoryMiB=4096`
- `diskMiB=16384`
- `network=cni`
- `cniNetwork=crabbox-firecracker`
- `cniConfDir=/etc/cni/conf.d`
- `cniBinDir=/opt/cni/bin`
- `launchTimeout=2m`
- `deleteOnRelease=true`

For this provider Crabbox also normalizes `serverType` to `microvm`, `ssh.port`
to `22`, and clears SSH fallback ports.

Host execution paths (`binary`, `jailer`, `kernel`, `rootfs`, `cniNetwork`,
`cniConfDir`, and `cniBinDir`) are honored only from trusted user config, env,
or flags. Repo-local `.crabbox.yaml` can still set non-executable sizing and
guest identity fields, but it cannot redirect local host executables, guest
images, or CNI plugin inputs.

`launchTimeout` bounds both Firecracker startup and SSH readiness. When
`deleteOnRelease=true`, release and cleanup remove Crabbox's local microVM
artifacts after stopping the VMM.

## Environment overrides

```text
CRABBOX_FIRECRACKER_BINARY
CRABBOX_FIRECRACKER_JAILER
CRABBOX_FIRECRACKER_KERNEL
CRABBOX_FIRECRACKER_ROOTFS
CRABBOX_FIRECRACKER_USER
CRABBOX_FIRECRACKER_WORK_ROOT
CRABBOX_FIRECRACKER_CPUS
CRABBOX_FIRECRACKER_MEMORY_MIB
CRABBOX_FIRECRACKER_DISK_MIB
CRABBOX_FIRECRACKER_NETWORK
CRABBOX_FIRECRACKER_CNI_NETWORK
CRABBOX_FIRECRACKER_CNI_CONF_DIR
CRABBOX_FIRECRACKER_CNI_BIN_DIR
CRABBOX_FIRECRACKER_LAUNCH_TIMEOUT
CRABBOX_FIRECRACKER_DELETE_ON_RELEASE
```

## Flags

```text
--firecracker-binary
--firecracker-jailer
--firecracker-kernel
--firecracker-rootfs
--firecracker-user
--firecracker-work-root
--firecracker-cpus
--firecracker-memory-mib
--firecracker-disk-mib
--firecracker-network
--firecracker-cni-network
--firecracker-cni-conf-dir
--firecracker-cni-bin-dir
--firecracker-launch-timeout
--firecracker-delete-on-release
```

`--class` and `--type` are intentionally rejected for `provider=firecracker`.
Use explicit Firecracker sizing plus explicit kernel/rootfs assets instead.

## Doctor contract

`crabbox doctor --provider firecracker` is read-only. It never creates,
starts, stops, or deletes a microVM. The provider-specific checks are stable:

| Check | Meaning |
| --- | --- |
| `host` | The current host must be Linux. |
| `kvm` | `/dev/kvm` exists and is openable by the current user. |
| `binary` | `firecracker.binary` resolves to an executable. |
| `jailer` | `skip` when unset; failed when configured because jailer launch is not supported yet. |
| `kernel` | `firecracker.kernel` exists and is a file. |
| `rootfs` | `firecracker.rootfs` exists and is a file. |
| `network` | `firecracker.network=cni`, `firecracker.cniNetwork` is set, the CNI config/bin directories exist, and the named CNI config loads. |

Provider checks include `mutation=false` in their details. Missing host assets
such as the Linux KVM surface, Firecracker binary, kernel, rootfs, or CNI
directories fail with `class=environment_blocked`. Blank or structurally wrong
config values such as an empty `firecracker.cniNetwork` fail with
`class=configuration_incomplete`.

## Guarded readiness smoke

The repo includes a Firecracker-specific readiness helper:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_BIN=./bin/crabbox scripts/live-firecracker-smoke.sh --dry-run
CRABBOX_BIN=./bin/crabbox scripts/live-firecracker-smoke.sh
```

The script is read-only. It validates the JSON shape from
`crabbox doctor --provider firecracker --json`, then classifies the result as:

- `classification=readiness_passed` when all Firecracker readiness checks are
  green;
- `classification=environment_blocked` when the host, `/dev/kvm`, Firecracker
  assets, or CNI prerequisites are unavailable;
- `classification=validation_failed` when the smoke harness cannot validate the
  expected doctor JSON contract.

On macOS, Windows, or Linux hosts that are missing KVM or Firecracker assets,
`environment_blocked` is the honest expected result. The helper does not fake a
successful live proof when the current host cannot run Firecracker.

## Limits

- Linux target only
- direct CLI only, no coordinator path
- `firecracker.network=cni` only
- no Tailscale-managed networking
- no provider-native snapshot, fork, or restore flow

## Troubleshooting

- `host=<os> requires a Linux KVM host`: run the provider on a Linux machine,
  not on macOS or Windows.
- `/dev/kvm unavailable`: KVM is missing, hidden, or inaccessible to the
  current user.
- `firecracker.binary unavailable`: install Firecracker or point
  `firecracker.binary` at the correct executable path.
- `firecracker.kernel unavailable` / `firecracker.rootfs unavailable`: fix the
  configured image paths or stage the missing files.
- `network=cni ... firecracker.cniConfDir ... firecracker.cniBinDir ...`:
  install or point Crabbox at the correct CNI config and plugin directories.
- `--class is not supported for provider=firecracker`: use
  `--firecracker-cpus`, `--firecracker-memory-mib`, and
  `--firecracker-disk-mib`.
- `--type is not supported for provider=firecracker`: use explicit
  `--firecracker-kernel` and `--firecracker-rootfs` instead.
- `provider=firecracker requires a Linux KVM host`: run lifecycle commands on a
  Linux host with `/dev/kvm`; non-Linux hosts can still run read-only docs and
  doctor-shape checks.

## Related docs

- [Provider reference](README.md)
- [providers](../commands/providers.md)
- [doctor](../commands/doctor.md)
- [Provider backends](../provider-backends.md)
- [Provider feature overview](../features/providers.md)
