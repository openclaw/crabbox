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
- Features: `ssh`, `crabbox-sync`, `cleanup`
- Authentication model: reuse existing Incus client trust state or an explicit
  socket/address override; no Crabbox-specific token flags

The first implementation is intentionally small:

- Linux guests only
- direct Incus control only
- no broker/Worker path
- no delegated `incus exec` mode
- no provider-native checkpoint/fork support in v1

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
- `auth=unix_socket|tls_client_cert|tls_server_cert|insecure_tls|tls|oidc|public`
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
  proxyListenHost: 0.0.0.0
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
  `images:ubuntu/24.04/cloud`
- `incus.profile`: optional Incus profile applied to created instances
- `incus.user`: SSH user inside the guest. Default: `crabbox`
- `incus.workRoot`: Crabbox work root inside the guest. Default:
  `/work/crabbox`
- `incus.deleteOnRelease`: delete the instance instead of stopping it on
  release. Default: `true`
- `incus.startTimeout`: create/start/address wait timeout. Default: `10m`
- `incus.launchPort`: guest SSH port. Default: `22`
- `incus.proxyListenHost`: host-side bind address for the optional Incus proxy
  device. Default: `0.0.0.0`
- `incus.proxyListenPort`: host-side published SSH port. When set, Crabbox uses
  this as the returned SSH port
- `incus.proxyDevice`: Incus device name for the SSH proxy. Default:
  `crabbox-ssh`
- `incus.tlsServerCert`: trusted Incus server certificate path for explicit
  `incus.address` mode
- `incus.insecureTLS`: allow untrusted TLS certs in explicit `incus.address`
  mode
- `incus.remoteImageServer`: simplestreams image server URL used for alias-based
  image source resolution when needed

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
CRABBOX_INCUS_PROXY_LISTEN_HOST=0.0.0.0
CRABBOX_INCUS_PROXY_LISTEN_PORT=2222
CRABBOX_INCUS_PROXY_DEVICE=crabbox-ssh
CRABBOX_INCUS_TLS_SERVER_CERT=$HOME/.config/incus/server.crt
CRABBOX_INCUS_INSECURE_TLS=false
CRABBOX_INCUS_REMOTE_IMAGE_SERVER=https://images.linuxcontainers.org
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

On acquire, Crabbox:

1. allocates a Crabbox lease id and slug;
2. generates a per-lease SSH key;
3. creates an Incus instance from the configured image;
4. injects Crabbox cloud-init plus provider metadata in `user.crabbox.*`
   instance config keys;
5. optionally adds an Incus TCP proxy device when `incus.proxyListenPort` is set;
6. starts the instance and waits for a reachable SSH path;
7. returns a normal Crabbox SSH lease target.

Crabbox-managed metadata is stored on the instance itself in `user.crabbox.*`
keys. That is how list/resolve/touch/cleanup identify managed leases without any
extra Incus-side service.

On release:

- default behavior deletes the instance;
- if `incus.deleteOnRelease: false`, Crabbox stops the instance and keeps the
  retained lease reusable through later `--id` resolves.

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

Use an explicit address and project:

```sh
crabbox warmup \
  --provider incus \
  --incus-address https://incus-host.example:8443 \
  --incus-project crabbox \
  --incus-insecure-tls
```

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
CRABBOX_LIVE=1 CRABBOX_BIN=bin/crabbox CRABBOX_LIVE_PROVIDERS=incus CRABBOX_LIVE_REPO=$PWD scripts/live-smoke.sh
```

The doctor smoke only proves daemon/control-plane readiness. The full live
smoke proves `warmup`, `status --wait`, `run`, `list`, `stop`, and one retained
reuse cycle from the Mac, then forces a final delete so repeat runs do not
strand test instances.

## Limits

- Linux only
- coordinator unsupported
- no Windows or macOS guests
- no provider-native snapshot/checkpoint/fork support in v1

## Troubleshooting

- `unknown provider "incus"`: the binary was built without the built-in provider
  registry import or from an older checkout
- `provider=incus supports target=linux only`: remove a non-Linux target override
- `provider=incus: incus.remote, incus.address, or incus.socket not configured ...`:
  the default `local` Unix-socket remote is Linux-only; on macOS point Crabbox
  at a reachable Linux Incus daemon instead of the local remote stub
- `provider=incus address mode requires Incus TLS trust material or --incus-insecure-tls`:
  explicit HTTPS address mode needs trusted TLS material unless you intentionally
  opt into insecure TLS
- `crabbox doctor --provider incus` now prints `mode`, `endpoint`, `project`,
  and `auth`; use those fields to confirm Crabbox picked the intended socket,
  explicit address, or named remote before blaming the live smoke path
- timeout waiting for an Incus address: the guest started, but Crabbox could not
  derive a host-reachable address from runtime state or proxy-device settings
- SSH bootstrap timeout: the instance is up, but the published SSH path is still
  wrong or the guest bootstrap did not finish yet
