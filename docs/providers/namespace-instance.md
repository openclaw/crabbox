# Namespace Compute Instance Provider

Use `provider: namespace-instance` for short-lived Linux Compute Instances managed
by the Namespace `nsc` CLI. The alias is `namespace-compute`; the existing
`namespace` alias continues to select `namespace-devbox`.

## Setup

Install `nsc`, authenticate it, then verify access:

```sh
nsc login
nsc auth check-login
crabbox doctor --provider namespace-instance
```

Crabbox does not read or store Namespace credentials. It invokes the configured
`nsc` binary and relies on its keychain.

## Usage

```sh
crabbox warmup --provider namespace-instance --class standard --ttl 15m
crabbox run --provider namespace-instance -- go test ./...
crabbox list --provider namespace-instance --json
crabbox stop --provider namespace-instance <lease-id-or-slug>
```

Crabbox injects a per-lease SSH public key, connects through
`nsc proxy --service ssh`, uses the normal SSH/rsync path, and destroys the
instance on release. Namespace duration remains a provider-side safety deadline;
`--namespace-instance-duration` overrides the global `--ttl` used at creation.

## Configuration

```yaml
provider: namespace-instance
target: linux
namespaceInstance:
  cli: nsc
  machineType: 4x8
  duration: 30m
  region: ""
  endpoint: ""
  keychain: ""
  volumes: []
  workRoot: /work/crabbox
  bare: true
```

Class defaults are `standard=4x8`, `fast=8x16`, `large=16x32`, and
`beast=32x64`. Use `--type` or `--namespace-instance-machine-type` for an exact
Namespace `CPUxMemoryGB` shape.

Provider flags:

```text
--namespace-instance-cli
--namespace-instance-machine-type
--namespace-instance-duration
--namespace-instance-region
--namespace-instance-endpoint
--namespace-instance-keychain
--namespace-instance-volume
--namespace-instance-work-root
--namespace-instance-bare
```

`--namespace-instance-volume` is repeatable and passes
`kind:tag:mountpoint:size` directly to `nsc create --volume`. Kind must be
`cache` or `persistent`; mountpoint must be an absolute Linux path.
Volume attachments, CLI path, endpoint, region, and keychain are accepted only
from trusted user config, environment variables, or explicit flags, not
repository-local config.
Custom endpoints must not include URL credentials, query parameters, or
fragments. Machine-type OS prefixes must be Linux.

## Lifecycle

- Linux only; coordinator disabled.
- `nsc create --bare --duration ... --ssh_key ...` provisions the instance.
- Ownership labels restrict list and cleanup to Crabbox-created resources.
- `touch` uses `nsc extend --ensure_minimum` only for the remaining original
  TTL; activity never moves the maximum lifetime forward.
- `stop` uses `nsc destroy --force`.
- `cleanup` never destroys unlabeled Namespace resources.
- `--keep` keeps the instance after the current command, but its Namespace
  duration deadline still applies.

The deprecated `nsc create --ephemeral` flag is intentionally not used; current
`nsc` versions ignore it. Duration controls automatic destruction.

See [Namespace CLI create reference](https://namespace.so/docs/reference/cli/create).
