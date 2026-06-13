# Namespace Compute Instance Provider

Read this when you:

- choose `provider: namespace-instance`;
- configure the Namespace Compute machine type, duration, region, endpoint, or
  volumes;
- change `internal/providers/namespaceinstance`.

Namespace Compute Instance is an **SSH-lease** provider. Crabbox shells out to
the Namespace `nsc` CLI to create, inspect, extend, list, and destroy Compute
instances, then uses Crabbox's normal SSH sync/run path on the leased Linux
box.

The provider runs **direct from the CLI**. It never goes through the broker
coordinator and supports Linux targets only.

## When to use

Use Namespace Compute Instance when you want a fresh Namespace Compute VM with a
short lease, Crabbox-owned labels, and the standard Crabbox SSH data plane. Use
Namespace Devbox (`provider: namespace-devbox`) when you specifically need the
Namespace Devbox product and its Devbox lifecycle.

## Prerequisites

Install the Namespace `nsc` CLI and authenticate before using Crabbox:

```sh
nsc auth login
```

Crabbox does not store Namespace credentials. It passes the configured endpoint,
keychain, region, volumes, duration, and machine type to `nsc`.

## Commands

```sh
crabbox warmup --provider namespace-instance --namespace-instance-machine-type 4x8
crabbox run --provider namespace-instance --id swift-crab -- pnpm test
crabbox ssh --provider namespace-instance --id swift-crab
crabbox status --provider namespace-instance --id swift-crab
crabbox stop --provider namespace-instance swift-crab
```

Aliases for the provider name: `namespace-compute`, `nsc`.

## Configuration

```yaml
provider: namespace-instance
target: linux
namespaceInstance:
  machineType: 4x8
  duration: 30m
  ephemeral: true
  region: ""
  endpoint: ""
  keychain: ""
  volumes: []
  workRoot: /workspaces/crabbox
```

Defaults baked into Crabbox: `duration: 30m`, `ephemeral: true`,
`workRoot: /workspaces/crabbox`. `machineType`, `region`, `endpoint`,
`keychain`, and `volumes` are unset by default. When `machineType` is unset,
Crabbox derives it from `--class`.

Provider flags:

```text
--namespace-instance-machine-type
--namespace-instance-duration
--namespace-instance-ephemeral
--namespace-instance-region
--namespace-instance-endpoint
--namespace-instance-keychain
--namespace-instance-volume
--namespace-instance-work-root
```

Environment overrides:

```text
CRABBOX_NAMESPACE_INSTANCE_MACHINE_TYPE
CRABBOX_NAMESPACE_INSTANCE_DURATION
CRABBOX_NAMESPACE_INSTANCE_EPHEMERAL
CRABBOX_NAMESPACE_INSTANCE_REGION
CRABBOX_NAMESPACE_INSTANCE_ENDPOINT
CRABBOX_NAMESPACE_INSTANCE_KEYCHAIN
CRABBOX_NAMESPACE_INSTANCE_VOLUMES
CRABBOX_NAMESPACE_INSTANCE_WORK_ROOT
```

## Lifecycle

1. Crabbox creates a per-lease SSH key and runs `nsc create` with Crabbox
   ownership labels and the configured machine options.
2. It reads the instance JSON from `nsc`, waits for SSH readiness, and records a
   local Crabbox lease claim.
3. `crabbox run`, `crabbox ssh`, and `crabbox pond connect` use the normal SSH
   executor and sync path against the instance.
4. `crabbox stop` runs `nsc destroy --force` and removes the local lease claim
   and generated key.
5. `crabbox cleanup --provider namespace-instance` lists Crabbox-owned
   instances and destroys expired leases.

## Capabilities

- **SSH**: yes.
- **Crabbox sync**: yes — standard rsync over SSH.
- **Cleanup**: yes — stale Crabbox-owned instances are destroyed through `nsc`.
- **Actions hydration**: yes — same Linux SSH contract as other SSH-lease
  providers.
- **Desktop / browser / code**: not surfaced for this provider.
- **Coordinator (broker)**: never; always direct from the CLI.

## Machine type mapping

`--namespace-instance-machine-type` accepts any `nsc create --machine_type`
value and always wins over the class mapping. When it is unset, Crabbox maps
`--class` as follows:

| Class      | Machine type |
| ---------- | ------------ |
| `standard` | `4x8`        |
| `fast`     | `4x8`        |
| `large`    | `8x16`       |
| `beast`    | `16x32`      |

An empty class resolves to `4x8`. Other class values pass through as the
machine type.

## Gotchas

- Run `nsc auth login` first. Crabbox does not store Namespace credentials.
- `namespace-instance` does not replace `namespace-devbox`; the bare
  `namespace` alias remains reserved for Namespace Devboxes.
- `--namespace-instance-volume` accepts comma-separated volume specs and passes
  each value through to `nsc --volume`.
- `--tailscale` is rejected because Namespace Compute instances expose SSH
  directly in this adapter.
- `namespaceInstance.workRoot` must be an absolute path under a dedicated
  subdirectory; broad roots such as `/`, `/home`, `/tmp`, or `/workspaces` are
  rejected.

## Related docs

- [Namespace Devbox provider](namespace-devbox.md)
- [Provider backends](../provider-backends.md)
