# Incus

Read when you:

- want a built-in local or self-hosted Linux SSH-lease provider backed by Incus;
- need the `incus:` config keys, `CRABBOX_INCUS_*` env overrides, or `--incus-*`
  flags;
- are validating the deterministic doctor contract or the opt-in Apple Silicon /
  local live smoke path.

`provider: incus` is a direct `ssh-lease` backend. Crabbox talks to Incus
through the official Go client, creates a Crabbox-managed instance, waits for a
reachable SSH target, then uses the normal Crabbox SSH sync/run lifecycle.

## Current contract

- Canonical provider id: `incus`
- Kind: `ssh-lease`
- Targets: Linux only
- Coordinator: never
- Features: `ssh`, `crabbox-sync`, `cleanup`, `workspace-checkpoint`, `workspace-fork`
- Authentication model: reuse existing Incus client trust state or an explicit
  socket/address override; no Crabbox-specific token flags

The provider stays focused on:

- Linux guests only
- direct Incus control only
- no broker/Worker path
- no delegated `incus exec` mode
- private disk-image checkpoints and forks for root-disk-only Linux containers (VM native captures are rejected)

## Connection modes

Crabbox connects to Incus in this order:

1. `incus.socket` / `--incus-socket`
2. `incus.address` / `--incus-address`
3. named remote from the local Incus client config (`incus.remote`)

Named remote resolution uses the official Incus client config loader, so the
provider can reuse `incus remote add ...`, project defaults, and local TLS
material instead of shelling out to the `incus` CLI.

## Doctor contract

`crabbox doctor --provider incus` is read-only. It resolves the same
socket/address/remote selection order as the provider, runs a cheap inventory
list, and reports the selected connection context in the provider line:

- `mode=socket|address|remote`
- `control_plane=local|remote`
- `endpoint=<socket-or-address>`
- `project=<incus-project>`
- `auth=unix_socket|tls_client_cert|tls_client_cert_insecure_tls|tls|oidc|public`
- `remote=<name>` when named-remote mode is active

The check stays non-mutating (`api=list mutation=false`). On a configured
machine it should return `ok provider ... runtime=go_client ...`; on an
unconfigured or broken machine it should fail with the normal doctor
`class=config|auth|network|provider` contract instead of creating or changing
Incus resources.

## Config

YAML:

```yaml
provider: incus
incus:
  remote: local
  project: default
  instanceType: container
  image: images:ubuntu/24.04/cloud
  user: crabbox
  workRoot: /work/crabbox
  deleteOnRelease: true
  startTimeout: 10m
  launchPort: "22"
  proxyListenHost: 127.0.0.1
  proxyListenPort: "2222"
  proxyDevice: crabbox-ssh
```

Key fields:

- `incus.remote`: named Incus remote from the local client config. Default:
  `local`
- `incus.project`: Incus project override. Default: `default`
- `incus.address`: explicit HTTPS Incus API address, for example
  `https://incus-host.example:8443`
- `incus.socket`: explicit Unix socket path override
- `incus.instanceType`: `container` or `vm`. Default: `container`
- `incus.image`: image alias/fingerprint. Default:
  `images:ubuntu/24.04/cloud`. Unqualified aliases and fingerprints use the
  active Incus daemon unless `incus.remoteImageServer` is set
- `incus.profile`: optional Incus profile applied to created instances
- `incus.user`: SSH user inside the guest. Default: `crabbox`
- `incus.workRoot`: Crabbox work root inside the guest. Default:
  `/work/crabbox`
- `incus.deleteOnRelease`: delete the instance instead of stopping it on
  release. Default: `true`
- `incus.startTimeout`: create/start/address wait timeout. Default: `10m`
- `incus.launchPort`: guest SSH port. Default: `22`
- `incus.proxyListenHost`: host-side bind address for the optional Incus proxy
  device. Default: `127.0.0.1`
- `incus.proxyListenPort`: host-side published SSH port. When set, Crabbox uses
  this as the returned SSH port. Proxy devices are supported for containers;
  VMs must be directly reachable because Incus VM proxies require a static NIC
- `incus.proxyDevice`: Incus device name for the SSH proxy. Default:
  `crabbox-ssh`
- `incus.tlsServerCert`: trusted Incus server certificate path for explicit
  `incus.address` mode
- `incus.insecureTLS`: allow untrusted TLS certs in explicit `incus.address`
  mode
- `incus.remoteImageServer`: optional simplestreams image server URL used when
  an unqualified alias should resolve remotely, or as a fallback when a named
  image remote cannot be loaded from the local Incus config

## Environment

Environment overrides follow the normal `CRABBOX_<PROVIDER>_*` pattern:

```sh
CRABBOX_PROVIDER=incus
CRABBOX_INCUS_REMOTE=local
CRABBOX_INCUS_PROJECT=default
CRABBOX_INCUS_ADDRESS=https://incus-host.example:8443
CRABBOX_INCUS_SOCKET=$HOME/.config/incus/unix.socket
CRABBOX_INCUS_INSTANCE_TYPE=container
CRABBOX_INCUS_IMAGE=images:ubuntu/24.04/cloud
CRABBOX_INCUS_PROFILE=crabbox
CRABBOX_INCUS_USER=crabbox
CRABBOX_INCUS_WORK_ROOT=/work/crabbox
CRABBOX_INCUS_DELETE_ON_RELEASE=true
CRABBOX_INCUS_START_TIMEOUT=10m
CRABBOX_INCUS_LAUNCH_PORT=22
CRABBOX_INCUS_PROXY_LISTEN_HOST=127.0.0.1
CRABBOX_INCUS_PROXY_LISTEN_PORT=2222
CRABBOX_INCUS_PROXY_DEVICE=crabbox-ssh
CRABBOX_INCUS_TLS_SERVER_CERT=$HOME/.config/incus/server.crt
CRABBOX_INCUS_INSECURE_TLS=false
```

## Flags

```sh
crabbox warmup \
  --provider incus \
  --incus-remote local \
  --incus-project default \
  --incus-instance-type container \
  --incus-image images:ubuntu/24.04/cloud \
  --incus-user crabbox \
  --incus-work-root /work/crabbox \
  --incus-proxy-listen-port 2222
```

Supported flags:

- `--incus-remote`
- `--incus-project`
- `--incus-address`
- `--incus-socket`
- `--incus-instance-type`
- `--incus-image`
- `--incus-profile`
- `--incus-user`
- `--incus-work-root`
- `--incus-delete-on-release`
- `--incus-start-timeout`
- `--incus-launch-port`
- `--incus-proxy-listen-host`
- `--incus-proxy-listen-port`
- `--incus-proxy-device`
- `--incus-tls-server-cert`
- `--incus-insecure-tls`
- `--incus-remote-image-server`

## Lease behavior

On acquire, Crabbox generates a per-lease SSH key and durably records the
creation intent before submitting the Incus request. It creates a named instance
with Crabbox cloud-init and `user.crabbox.*` metadata, starts it, and waits for
SSH readiness. Container leases can use the optional TCP proxy device; VMs need
a directly reachable address.

`warmup --lease-id cbx_0123456789ab` and `checkpoint fork --lease-id ...` support
fixed, idempotent allocation. Use `run --id cbx_0123456789ab` to run on an acquired lease. Repeating the same intent reuses the same instance and
key, including after a lost create response or client restart. A per-ID CLI lock
serializes acquisition through registration and fork preparation; provider claim
locks continue to fence ownership changes. Configuration, resolved profile
contents, checkpoint, endpoint, project, daemon certificate, and resource UUID
conflicts fail closed. A fixed ID becomes terminal after deletion and cannot be
recycled. Normal generated-ID leases use the same durable creation path.

Claims preserve the original connection settings and bind the resolved endpoint,
project, daemon certificate fingerprint, instance name, and UUID. Changing a
remote alias or replacing a daemon does not redirect a claim to another resource.
Use the original connection settings for lease reuse and cleanup; certificate
rotation requires operator reconciliation. A failed or inconclusive allocation
keeps its claim and key when cleanup cannot be confirmed. Retry the same fixed
ID to reconcile it, or stop the recorded lease. Incomplete leases cannot be run
or started through ordinary reuse; replay the original warmup/fork intent to
finish preparation. A validated deletion is recorded before removing the instance,
so a lost delete reply can be reconciled by another stop or cleanup. Do not discard the claim to
work around an ownership conflict.

Release deletes the instance by default. With `incus.deleteOnRelease: false`,
release stops it and retains the key and claim for later `--id` reuse. Retained
leases created by older Crabbox versions remain usable through their original
name-based scope, but cannot be captured natively; create a new lease to obtain
the durable identity contract.

## Native disk checkpoints

`--mode native` creates an `incus-image` checkpoint. The generic default remains
`--mode auto` with workspace-archive fallback for direct providers. Native
`--strategy auto` normalizes to `disk-snapshot`; `--strategy image` uses the same
snapshot-to-image implementation. Capture always waits for Incus operations to
finish, including with `--wait=false`; `--wait-timeout` bounds the snapshot and
publish wait (45 minutes by default), retaining an uncertain record on timeout.

Crabbox creates a **stateless disk snapshot**, publishes it as a **private,
non-expiring, non-updating Incus image**, and removes the temporary snapshot. The image has an
independent lifetime: delete the source lease, verify the checkpoint, fork it,
and then delete the checkpoint. Existing forks survive image deletion. Images
stay private; Crabbox never publishes with public visibility. Publication explicitly
clears inherited base-image expiry. Before snapshot creation, the canonical local
checkpoint record durably stores the source UUID, snapshot name, and exact Incus
connection identity. Recovery records the image fingerprint before deleting it,
so interrupted deletion can safely reconcile a missing image. An unresolved
publication retains its record and snapshot until its outcome can be established.

Capture does not stop/reboot the source, reset its cloud-init, or change its
credentials. A running source produces a crash-consistent disk capture; quiesce
applications yourself when application consistency matters. Memory, running
processes, CRIU, and live migration are not captured. All attached non-root disk
devices are rejected, and a running source's canonical workspace must not
intersect a guest mount. Symlinked or non-normalized workspace paths are rejected
so path aliases cannot bypass mount checks. Use `--mode archive` for mounted workspaces. In-guest
mounts outside the workspace are not captured as part of the root disk.

Native captures and forks currently require Linux **containers** with the
Crabbox SSH bootstrap. VMs use workspace archives: the pinned Incus API cannot
edit their files without starting the guest agent, which would expose inherited
SSH authority before replacement.

Before starting a fork, Crabbox disables inherited image templates and cloud-init
on the stopped clone, replaces the configured user's `authorized_keys`, installs
a root-owned authorization file containing only the new lease's public key, and
replaces `sshd_config` to use that file exclusively. It generates a new Ed25519
host key, machine ID, and hostname. Incus receives a fresh resource UUID and
creates fresh virtual network identity. Forks do not rerun the source's
cloud-init commands or preserve custom SSH configuration. Autostart is disabled
so an incomplete clone stays stopped across daemon restarts. Identity write
failure leaves the clone stopped and its claim available for retry or cleanup.
The source lease's private key cannot authenticate to the fork's managed SSH
service. Workspace contents, installed dependencies, and other root-disk data
remain available; application-specific credentials are data, not rotated by
Crabbox. Treat checkpoint images as sensitive even though they are private.

Verify, fork, and delete use the recorded connection and recheck image
fingerprint, checkpoint ownership, source UUID, and daemon/project identity.
Partial snapshot/publish failures retain a pending local checkpoint record;
verification can discover a completed image after a lost response. Delete
refuses to remove the last cleanup identity while capture remains inconclusive.
Image or temporary-snapshot deletion failures retain the record for retry.

```sh
crabbox warmup --provider incus --lease-id cbx_0123456789ab
crabbox run --provider incus --id cbx_0123456789ab -- npm ci
crabbox checkpoint create --provider incus --id cbx_0123456789ab --mode native --name prepared
crabbox stop --provider incus cbx_0123456789ab
crabbox checkpoint inspect <checkpoint-id> --verify
crabbox checkpoint fork <checkpoint-id> --lease-id cbx_abcdef012345
crabbox checkpoint delete <checkpoint-id>
```

The implementation uses the pinned [Incus v7.1.0 client interfaces](https://github.com/lxc/incus/blob/v7.1.0/client/interfaces.go)
and [image API types](https://github.com/lxc/incus/blob/v7.1.0/shared/api/image.go).
The upstream [container driver](https://github.com/lxc/incus/blob/4411badd84fdb1740232fb0906f51e6fecf8e696/internal/server/instance/drivers/driver_lxc.go)
mounts a stopped container's root filesystem for file access and applies image
templates during startup; the fork clears those templates before writing
identity. [Instance creation](https://github.com/lxc/incus/blob/4411badd84fdb1740232fb0906f51e6fecf8e696/internal/server/instance/instance_utils.go)
assigns UUID and cloud-init instance identity. A new cloud-init instance ID alone
does not guarantee old authorized keys are removed, so forks use the documented
[cloud-init disable marker](https://docs.cloud-init.io/en/23.2.2/howto/disable_cloud_init.html)
and replace SSH authority explicitly instead.

## Examples

Warm and run through a local socket-backed daemon:

```sh
crabbox warmup --provider incus --incus-socket /var/lib/incus/unix.socket
crabbox run --provider incus --id blue-lobster -- echo incus-ok
crabbox stop --provider incus blue-lobster
```

Use a named remote that already exists in the local Incus client config:

```sh
incus remote add local-incus-testbed <host-or-token>
crabbox warmup --provider incus --incus-remote local-incus-testbed
```

Use an explicit address that matches an authenticated remote in the local
Incus client config:

```sh
incus remote add local-incus-testbed https://incus-host.example:8443
crabbox warmup \
  --provider incus \
  --incus-remote local-incus-testbed \
  --incus-address https://incus-host.example:8443 \
  --incus-project crabbox
```

Crabbox reuses that matching remote's TLS client certificate or OIDC tokens.
`--incus-insecure-tls` only disables server certificate verification; it does
not authenticate the client to a private Incus API.

## Deterministic verification

Implementation-complete checks for this provider are mostly deterministic; the
final doctor probe stays read-only and validates the configured control-plane
contract:

```sh
go test -count=1 ./internal/providers/incus ./internal/cli
go test -count=1 ./...
go vet ./...
go build -trimpath -o bin/crabbox ./cmd/crabbox
scripts/check-docs.sh
go run ./cmd/crabbox doctor --provider incus --json
```

These prove the built-in provider registration, typed config surface,
fake-backed lifecycle behavior, the hardened read-only doctor contract, and
docs/catalog consistency. The doctor command should either emit explicit
connection metadata or fail with the documented config/auth contract without
mutating any Incus state.

## Opt-in live smoke

The live Incus path stays opt-in because most maintainer machines do not have a
reachable local daemon and guest route by default. The documented contract is:

- `crabbox doctor --provider incus` must pass first
- Crabbox config or env must resolve one of `incus.socket`, `incus.address`, or
  `incus.remote`
- the Mac must reach the Incus-managed guest either directly over the bridge or
  through an Incus-published SSH path such as `incus.proxyListenPort`
- `CRABBOX_LIVE_REPO` must point at the repo you want the smoke to sync and run

The default live-smoke matrix still skips Incus. Opt in explicitly:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_BIN=bin/crabbox CRABBOX_LIVE_DOCTOR_PROVIDERS=incus scripts/live-doctor-smoke.sh
CRABBOX_LIVE=1 \
CRABBOX_BIN=bin/crabbox \
CRABBOX_LIVE_PROVIDERS=incus \
CRABBOX_LIVE_REPO=$PWD \
scripts/live-smoke.sh
```

The doctor smoke only proves daemon/control-plane readiness. The full live
smoke first requires local `jq` and `rg` before it calls Incus. It then proves
`doctor`, `warmup`, `status --wait`, `run`, `list`, `stop`, and one retained
reuse cycle from the Mac, then forces a final delete so repeat runs do not strand
test instances.

For the native lifecycle, run the separate bounded smoke against a dedicated
root-disk-only container profile. It creates two leases and one private image,
checks fixed-ID replay, deletes the source before forking, checks disk contents
and changed machine/SSH host identity, rejects the source private key against
the fork, and deletes all tracked resources. Configure the connection and
`instanceType: container` through `CRABBOX_CONFIG` or the existing environment
settings; the script accepts no arguments. This keeps creation-only flags out of
commands such as `inspect` that do not accept them.

```sh
CRABBOX_LIVE=1 CRABBOX_BIN=/tmp/crabbox-incus-candidate \
  CRABBOX_LIVE_REPO=$PWD scripts/live-incus-checkpoint-smoke.sh
```

The smoke's temporary source-key copy is mode 0600 and is removed on exit.
Cleanup failures print the exact tracked IDs and fail the run. Fake-backed Go
tests are not live Incus proof.

## Limits

- Linux only
- coordinator unsupported
- no Windows or macOS guests
- SSH proxy devices are container-only; VM leases require direct guest
  reachability
- OIDC remotes require a macOS or Linux Crabbox client so refreshed credentials
  can be persisted atomically; Windows clients can use TLS certificate auth
- native disk checkpoints are container-only and exclude attached volumes

## Troubleshooting

- `unknown provider "incus"`: the binary was built without the built-in provider
  registry import or from an older checkout
- `provider=incus supports target=linux only`: remove a non-Linux target override
- `provider=incus: incus.remote, incus.address, or incus.socket not configured ...`:
  the default `local` Unix-socket remote is Linux-only; on macOS point Crabbox
  at a reachable Linux Incus daemon instead of the local remote stub
- `provider=incus address mode requires a matching authenticated Incus remote ...`:
  add the address as an Incus remote first so Crabbox can reuse its TLS client
  certificate or OIDC tokens; server trust and `--incus-insecure-tls` do not
  provide client authentication
- `crabbox doctor --provider incus` now prints `mode`, `endpoint`, `project`,
  and `auth`; use those fields to confirm Crabbox picked the intended socket,
  explicit address, or named remote before blaming the live smoke path
- timeout waiting for an Incus address: the guest started, but Crabbox could not
  derive a host-reachable address from runtime state or proxy-device settings
- SSH bootstrap timeout: the instance is up, but the published SSH path is still
  wrong or the guest bootstrap did not finish yet
