# Incus

Read when you:

- want a built-in local or self-hosted Linux SSH-lease provider backed by Incus;
- need the `incus:` config keys, `CRABBOX_INCUS_*` env overrides, or `--incus-*`
  flags;
- are validating the split between deterministic provider checks and the separate
  Apple Silicon local E2E testbed contract.

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
- if `incus.deleteOnRelease: false`, Crabbox stops the instance unless the stop
  path is forced.

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

Implementation-complete checks for this provider are deterministic and do not
require a live Incus daemon:

```sh
go test -count=1 ./internal/cli ./internal/providers/...
go test -count=1 ./...
go vet ./...
scripts/check-docs.sh
```

These prove the built-in provider registration, typed config surface,
fake-backed lifecycle behavior, and docs/catalog consistency.

## Real local E2E

Real Apple Silicon local smoke is a separate validation tier, not part of the
deterministic provider gate. Use the preserved local testbed contract:

- preferred host route: Tart plus
  `~/Desktop/xcp/ISOs-ARM/ubuntu-26.04-desktop-arm64.iso`
- first acceptable proof: container-backed Incus guest reached from the Mac over
  SSH
- `scripts/live-smoke.sh` remains unchanged until that path is repeatable enough
  to maintain

Treat local Incus smoke as complete only when the Mac reaches both the Incus
daemon and an Incus-managed guest over SSH.

## Limits

- Linux only
- coordinator unsupported
- no Windows or macOS guests
- no provider-native snapshot/checkpoint/fork support in v1

## Troubleshooting

- `unknown provider "incus"`: the binary was built without the built-in provider
  registry import or from an older checkout
- `provider=incus supports target=linux only`: remove a non-Linux target override
- `provider=incus address mode requires Incus TLS trust material or --incus-insecure-tls`:
  explicit HTTPS address mode needs trusted TLS material unless you intentionally
  opt into insecure TLS
- timeout waiting for an Incus address: the guest started, but Crabbox could not
  derive a host-reachable address from runtime state or proxy-device settings
- SSH bootstrap timeout: the instance is up, but the published SSH path is still
  wrong or the guest bootstrap did not finish yet
