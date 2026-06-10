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
`CRABBOX_TART_PASSWORD`, `CRABBOX_TART_WORK_ROOT`, `CRABBOX_TART_CPUS`,
`CRABBOX_TART_MEMORY`, `CRABBOX_TART_DISK`.

`CRABBOX_TART_PASSWORD` (or `tart.password` in the mode-0600 user config printed
by `crabbox config path`) is the guest account password the local WebVNC viewer
uses for macOS Apple/ARD authentication. It defaults to `admin` (the cirruslabs
base-image account) and is **only** handed to the local browser viewer over an
authenticated localhost endpoint — never written to the guest. Do not put
`tart.password` in repo-local `crabbox.yaml` or `.crabbox.yaml`. There is
intentionally no CLI flag for it, which keeps the password out of shell history
and process metadata.

## How it works

1. `tart clone <image> crabbox-<slug>` creates a new VM from the base image.
2. `tart set crabbox-<slug> --cpu N --memory N` configures resources (disk size is only resized when `--tart-disk` is explicitly set).
3. `tart run crabbox-<slug> --no-graphics` starts the VM headless.
4. `tart ip crabbox-<slug>` polls for the guest IP (DHCP, typically ~10s).
5. `tart exec crabbox-<slug> bash -c "..."` injects the SSH public key.
6. Crabbox waits for SSH readiness, then syncs and runs commands normally.
7. For `--desktop` leases, `tart exec` turns on the guest's built-in macOS Screen Sharing (native VNC on port 5900). No VNC password is provisioned — authentication uses the guest account's own credentials.
8. `tart stop` + `tart delete` on release.

## Desktop / VNC

Lease with `--desktop` to get a visible macOS session:

```sh
crabbox warmup --provider tart --desktop
crabbox webvnc --provider tart --id <lease-id>   # browser viewer (host-side bridge)
crabbox screenshot --provider tart --id <lease-id> --output desktop.png
```

`crabbox webvnc` runs a host-side bridge: it SSH-tunnels to the guest's Screen Sharing port, creates a self-contained mode-0600 noVNC viewer file, and opens that file in the browser — no noVNC/`websockify` tooling on the guest. The temporary viewer talks only to literal `127.0.0.1`, fetches the account credentials with an authenticated POST, and authenticates the WebSocket relay with a per-session subprotocol, keeping the bearer out of process arguments and browser URLs. The file contains only the ephemeral bridge token and is removed when the bridge exits. `crabbox screenshot` uses the same locally configured account credentials for noninteractive capture. noVNC authenticates via macOS Apple (ARD) auth with the lease account credentials (handed to the local viewer only). Prefer a native VNC client instead? Tunnel and connect directly:

```sh
ssh -i <lease-key> -L 5900:127.0.0.1:5900 admin@<lease-ip>
open vnc://127.0.0.1:5900    # macOS Screen Sharing, or any VNC client
```

The VM still starts with `--no-graphics` (the local display is not needed); for `--desktop` leases the provider turns on the guest's built-in macOS **Screen Sharing** (`com.apple.screensharing`). Authentication uses the **guest account's own credentials** (macOS user auth, e.g. `admin`/`admin` on the cirruslabs base image) — crabbox provisions no separate VNC password and passes no credential to the guest.

**Remote control plane (controller → Mac → guest).** When crabbox runs on the Mac over SSH from another machine, the guest's VNC port isn't directly reachable from the controller — forward it *through* the Mac to the guest's tart IP:

```sh
# on the controller (the guest IP is printed by `crabbox warmup`):
ssh -L 5900:<guest-tart-ip>:5900 <user>@<mac-host>
open vnc://127.0.0.1:5900    # native VNC client on the controller
```

**Exposure boundary:** macOS Screen Sharing binds all guest interfaces, so the VNC server is reachable at the guest's address on the tart host network (not localhost-only), gated by account authentication. tart's network is host-local (only the Mac can reach the guest), so the effective boundary is "account-authenticated VNC, reachable from the tart host." The SSH tunnels above keep the viewer side on `127.0.0.1`.

> The browser viewer is a **host-side** bridge (the guest needs no noVNC/`websockify`). For the remote control-plane case, run `crabbox webvnc` on the Mac, tunnel the printed web port to your machine with `ssh -L <port>:127.0.0.1:<port> <user>@<mac>`, copy the printed handoff file to your machine with `scp`, and open the copied file while the bridge is running. The `webvnc status`/`reset`/`daemon` subcommands (the Linux noVNC-daemon model) remain unsupported for macOS and point you to `crabbox webvnc`.

## Not yet supported

- Shared-directory mounts (`tart run --dir`; needs explicit host-mount config).
- Checkpoint/fork (tracked as a separate follow-up PR).
