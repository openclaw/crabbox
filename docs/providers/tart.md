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

## Optional live smoke

Run the guarded local smoke when Tart is installed and the configured macOS base
image is available or can be downloaded:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=tart scripts/live-smoke.sh
```

The smoke creates one short-lived Tart VM, waits for SSH readiness, syncs the
current checkout, runs the shared live-smoke command, prints recent history/log
evidence when available, then stops and deletes only the VM it created. It uses
`--ttl 30m --idle-timeout 5m` because the first run may need to pull and boot a
macOS base image, and it follows the same cleanup path as normal
`crabbox stop --provider tart`.

## Configuration

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--tart-image` | `ghcr.io/cirruslabs/macos-sequoia-base@sha256:785c3acb40fa5af6dd5aab96cd60408372c26125e173c14ea417498d086f829c` | OCI base image to clone |
| `--tart-cpu` | 4 | Guest CPU count |
| `--tart-memory` | 8192 | Guest memory in MB |
| `--tart-disk` | (clone default) | Guest disk size in GB; only applied when explicitly set |

YAML (`.crabbox.yaml`):

```yaml
tart:
  # Omit image to use the verified built-in macOS Sequoia image.
  # image: my-local-base  # explicit custom image; operator-managed trust
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
   For the built-in image, Crabbox verifies the clone's disk, NVRAM, and
   configuration against checked-in image metadata before configuring or
   starting it. A failed verification stops provisioning and removes only the
   new clone whose ownership marker still matches.
2. `tart set crabbox-<slug> --cpu N --memory N` configures resources (disk size is only resized when `--tart-disk` is explicitly set).
3. `tart run crabbox-<slug> --no-graphics --no-clipboard --no-audio` starts the VM headless with Tart's automatic host clipboard and audio passthrough disabled.
4. `tart ip crabbox-<slug>` polls for the guest IP (DHCP, typically ~10s).
5. Crabbox waits up to two minutes for the Tart Guest Agent using a harmless
   `tart exec crabbox-<slug> /usr/bin/true` probe, then injects the SSH public key
   with `tart exec crabbox-<slug> bash -c "..."`. An IP alone is not guest-agent
   readiness. Only native connection-pool/agent-startup errors are retried;
   cancellation and other failures stop provisioning. The key-writing command
   is never retried.
6. Crabbox waits for SSH readiness, then syncs and runs commands normally.
7. For `--desktop` leases, `tart exec` turns on the guest's built-in macOS Screen Sharing (native VNC on port 5900). No VNC password is provisioned — authentication uses the guest account's own credentials.
8. `tart stop` + `tart delete` on release.

## Built-in image identity

New leases without an image override use the immutable Sequoia image above.
The original registry manifest and its VM configuration are checked in under
`internal/providers/tart/images`. Crabbox hashes the manifest against the pin,
then checks the cloned disk's exact size and every uncompressed chunk digest,
the NVRAM size and digest, and the configuration before guest execution. Native
Tart MAC-address regeneration and JSON formatting changes are allowed; other
configuration changes, duplicate JSON keys, and suspended state are rejected.
The verified manifest digest is recorded as `image_digest` in lease labels and
preserved on touch.

This check also runs for warm-cache clones: a cache name or successful
`tart clone` is not content verification. It streams the 50 GB disk with bounded
memory, so provisioning includes a full local disk read even on a cache hit.
Cancellation interrupts verification. The check does not protect against an
operator modifying the local VM directory concurrently or after verification.

Existing leases keep their image. Explicit `--tart-image`, `tart.image`, and
`CRABBOX_TART_IMAGE` values remain operator-managed configuration, including
local names and mutable tags. An explicitly configured old `:latest` value
therefore stays mutable; remove the override to adopt the verified default.
There is no fallback to a tag when the pinned image is unavailable or fails
verification. Legacy leases without recorded provenance are not given the new
verified digest retroactively.

Maintainers refresh this pin and its metadata before releases and when upstream
security fixes require it; see [image maintenance](../operations.md#tart-default-image).

## Automatic cleanup ownership

`crabbox cleanup --provider tart` treats names and stopped state as discovery
signals, not permission to delete. Cleanup requires one exact local claim that
binds the provider, lease, slug, VM name, canonical Tart storage directory, and
the VM's ownership marker. New acquisitions create a private `.crabbox-owner`
marker after cloning and store its random identity in the claim. Cleanup never
creates or adopts a missing marker. `TART_HOME` selects the storage directory;
otherwise Crabbox uses `~/.tart` and passes that same directory to Tart.

The claim lock covers fresh inventory and marker checks, any required stop,
deletion, and durable claim removal. A changed claim, missing or replaced marker,
duplicate claim, different store, or inventory failure prevents deletion.
Legacy claims without this binding are retained for explicit operator inspection;
they are not silently upgraded by cleanup. Inspect such VMs with Tart before
choosing any manual `tart stop <name>` / `tart delete <name>` operation.

Cleanup retains local SSH keys because key creation can precede claim publication;
the claim lock alone cannot prove a concurrent acquisition is not using a key.
Missing-instance claim pruning is also limited to the bound storage directory.
Dry runs perform no provider mutations or claim/key removal.

The marker detects ordinary replacement and reuse; it is not a boundary against
an operator modifying the Tart store directly. Tart's name-based deletion API
does not provide an atomic expected-identity check against concurrent raw Tart
or filesystem replacement. Do not modify a VM's store while cleanup is running.

## Run artifacts

Tart uses the ordinary SSH artifact collector, so `crabbox run` supports both
`--artifact-glob` and `--require-artifact` on its macOS guests. Required
evidence is checked after a successful command and before downloads,
collection, or `--stop-after always` teardown; required matches are included
in the downloaded artifact archive. Task-created remote archives are removed
after retrieval.

The guest needs stock `/bin/bash`, `find` with `-print0`, `tar`, `base64`, and
`/bin/rm`. Matching never follows directory symlinks and excludes `.git` and
`.crabbox` components. A matched leaf symlink is accepted only when it resolves
to a regular file. These flags remain unsupported on native Windows, and
delegated-run providers still require their advertised bounded-artifact
capability.

## Desktop / VNC

Lease with `--desktop` to get a visible macOS session:

```sh
crabbox warmup --provider tart --desktop
crabbox webvnc --provider tart --id <lease-id>   # shared portal, or local fallback without coordinator login
crabbox screenshot --provider tart --id <lease-id> --output desktop.png
```

`crabbox webvnc` runs a host-side bridge to the guest's Screen Sharing port; the guest needs no noVNC/`websockify` tooling. With an authenticated coordinator login, Crabbox registers the Tart lease and presents it inside the same portal chrome and controls as Linux and Windows. The registration follows the Tart lease until `crabbox stop` or normal coordinator expiry and never grants the coordinator authority to delete the VM. Without coordinator auth, Crabbox keeps the self-contained mode-0600 localhost viewer as an offline fallback. `crabbox screenshot` uses the same locally configured account credentials for noninteractive capture. noVNC authenticates via macOS Apple (ARD) auth with the lease account credentials. Prefer a native VNC client instead? Tunnel and connect directly:

```sh
ssh -i <lease-key> -o GatewayPorts=no -L 127.0.0.1:5900:127.0.0.1:5900 admin@<lease-ip>
open vnc://127.0.0.1:5900    # macOS Screen Sharing, or any VNC client
```

The VM still starts with `--no-graphics` (the local display is not needed); for `--desktop` leases the provider turns on the guest's built-in macOS **Screen Sharing** (`com.apple.screensharing`). Authentication uses the **guest account's own credentials** (macOS user auth, e.g. `admin`/`admin` on the cirruslabs base image) — crabbox provisions no separate VNC password and passes no credential to the guest.

**Remote control plane (controller → Mac → guest).** When crabbox runs on the Mac over SSH from another machine, the guest's VNC port isn't directly reachable from the controller — forward it *through* the Mac to the guest's tart IP:

```sh
# on the controller (the guest IP is printed by `crabbox warmup`):
ssh -o GatewayPorts=no -L 127.0.0.1:5900:<guest-tart-ip>:5900 <user>@<mac-host>
open vnc://127.0.0.1:5900    # native VNC client on the controller
```

**Exposure boundary:** macOS Screen Sharing binds all guest interfaces, so the VNC server is reachable at the guest's address on the tart host network (not localhost-only), gated by account authentication. tart's network is host-local (only the Mac can reach the guest), so the effective boundary is "account-authenticated VNC, reachable from the tart host." The SSH tunnels above keep the viewer side on `127.0.0.1`.

> The browser viewer is a **host-side** bridge (the guest needs no noVNC/`websockify`). With coordinator auth, `webvnc status`, `reset`, and `daemon` use the shared portal model. Without coordinator auth, remote operators can still forward the printed localhost web port and copy/open the mode-`0600` handoff file while the bridge is running.

## Not yet supported

- Shared-directory mounts (`tart run --dir`; needs explicit host-mount config).
- Checkpoint/fork (tracked as a separate follow-up PR).
