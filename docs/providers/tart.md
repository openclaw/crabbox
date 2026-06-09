# Tart Provider

Read this when you:

- choose `provider: tart` (aliases `local-tart`, `macos-vm`);
- want local macOS VMs on Apple Silicon through Cirrus Labs tart;
- change `internal/providers/tart`.

Tart is a local SSH-lease provider. Crabbox drives the `tart` CLI on an
Apple Silicon Mac, clones a macOS VM from an OCI base image, configures
CPU/memory/disk, starts the VM headless, injects an SSH key via `tart exec`,
syncs the checkout over SSH, runs commands through the normal Crabbox SSH
executor, and deletes the VM on `stop`.

The provider is local only. It never uses the coordinator or cloud credentials.

**Targets:** macOS.

**Hosts:** Apple Silicon Macs with tart installed (`brew install cirruslabs/cli/tart`).

## Configuration

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--tart-image` | `ghcr.io/cirruslabs/macos-sequoia-base:latest` | OCI base image to clone |
| `--tart-cpu` | 4 | Guest CPU count |
| `--tart-memory` | 8192 | Guest memory in MB |
| `--tart-disk` | (clone default) | Guest disk size in GB; only applied when explicitly set |

YAML (`.crabbox.yaml`):

```yaml
tart:
  image: ghcr.io/cirruslabs/macos-ventura-base:latest
  user: admin
  workRoot: /Users/admin/crabbox
  cpus: 4
  memory: 8192
  # disk: 80  # only set to resize beyond the base image default
```

Environment variables: `CRABBOX_TART_IMAGE`, `CRABBOX_TART_USER`,
`CRABBOX_TART_WORK_ROOT`, `CRABBOX_TART_CPUS`, `CRABBOX_TART_MEMORY`,
`CRABBOX_TART_DISK`.

## How it works

1. `tart clone <image> crabbox-<slug>` creates a new VM from the base image.
2. `tart set crabbox-<slug> --cpu N --memory N` configures resources (disk size is only resized when `--tart-disk` is explicitly set).
3. `tart run crabbox-<slug> --no-graphics` starts the VM headless.
4. `tart ip crabbox-<slug>` polls for the guest IP (DHCP, typically ~10s).
5. `tart exec crabbox-<slug> bash -c "..."` injects the SSH public key.
6. Crabbox waits for SSH readiness, then syncs and runs commands normally.
7. For `--desktop` leases, `tart exec` enables the guest's built-in macOS Screen Sharing (VNC on `127.0.0.1:5900`) and provisions the VNC credential.
8. `tart stop` + `tart delete` on release.

## Desktop / VNC

Lease with `--desktop` to get a visible macOS session:

```sh
crabbox warmup --provider tart --desktop
crabbox webvnc --id <lease-id>
```

The VM still starts with `--no-graphics` (the local display is not needed); instead the provider enables the guest's native **Screen Sharing** service over SSH. During acquire it generates a 16-character password at `/var/db/crabbox/vnc.password`, sets the lease user's account password to match, and starts `com.apple.screensharing`. `crabbox webvnc` then tunnels VNC on `127.0.0.1:5900` over SSH and bridges it to the portal — the same managed-VNC path the other desktop-capable providers use.

## Not yet supported

- Shared-directory mounts (`tart run --dir`; needs explicit host-mount config).
- Checkpoint/fork (tracked as a separate follow-up PR).
