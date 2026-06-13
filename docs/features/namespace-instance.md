# Namespace Instance

Read this when you are:

- choosing `provider: namespace-instance`;
- comparing Namespace Compute Instances with Namespace Devboxes;
- debugging the `nsc` lifecycle behind Crabbox sync and run.

`provider: namespace-instance` creates or reuses Namespace Compute Instances and
exposes them to Crabbox as Linux SSH leases. Namespace owns account auth and the
instance lifecycle through `nsc`; Crabbox owns local slugs, repo claims, dirty
checkout sync, command execution, status/list normalization, and cleanup policy.

The provider is Linux-only, direct from the CLI, and never brokered through the
coordinator.

## Namespace Provider Choice

Use the canonical provider names when switching between Namespace products:

| Provider | CLI Crabbox shells out to | Aliases | Best fit |
| --- | --- | --- | --- |
| `namespace-devbox` | `devbox` | `namespace`, `namespace-devboxes` | Namespace Devbox images and Devbox pause/resume semantics. |
| `namespace-instance` | `nsc` | `namespace-compute` | Short-lived Compute Instances managed by `nsc`. |

The short `namespace` alias intentionally remains attached to
`namespace-devbox` for compatibility.

## Setup

Install and authenticate `nsc`:

```sh
nsc login
nsc auth check-login
crabbox doctor --provider namespace-instance
```

Select the provider in config:

```yaml
provider: namespace-instance
namespaceInstance:
  machineType: linux-small
  duration: 15m
  workRoot: /work/crabbox
```

Crabbox does not read or store Namespace credentials. It invokes `nsc` for
auth-sensitive work and keeps provider tokens out of argv and Crabbox config.

## Commands

```sh
crabbox warmup --provider namespace-instance --slug smoke
crabbox status --provider namespace-instance --id smoke --wait
crabbox run --provider namespace-instance --id smoke -- pnpm test
crabbox list --provider namespace-instance
crabbox stop --provider namespace-instance smoke
```

Use `--namespace-instance-duration 30m` or command `--ttl 30m` to request a
short-lived instance duration. Use `--namespace-instance-machine-type` when you
need a specific Namespace machine type.

## Provider Boundary

Namespace Instance is an **SSH-lease** provider, not a delegated-run provider:

- Namespace `nsc` creates, lists, describes, extends, and destroys the instance.
- Crabbox connects over SSH, syncs the checkout with rsync, runs commands, and
  records local lease metadata.
- Cleanup filters `nsc` inventory to Crabbox-owned `namespace-instance` labels
  before destroying stale instances.

The provider declares the `ssh`, `crabbox-sync`, and `cleanup` features.

## Configuration

Set these under `namespaceInstance`, override per invocation with the matching
`--namespace-instance-*` flag, or use the matching
`CRABBOX_NAMESPACE_INSTANCE_*` environment variable. Flag wins over env, which
wins over config file, which wins over the built-in default.

| Config key | Flag | Env var | Default | Notes |
| --- | --- | --- | --- | --- |
| `machineType` | `--namespace-instance-machine-type` | `CRABBOX_NAMESPACE_INSTANCE_MACHINE_TYPE` | `linux-small` via class map | Exact Namespace machine type. |
| `duration` | `--namespace-instance-duration` | `CRABBOX_NAMESPACE_INSTANCE_DURATION` | lease `--ttl` when set | Positive Go-style duration, such as `15m` or `2h`. |
| `ephemeral` | `--namespace-instance-ephemeral` | `CRABBOX_NAMESPACE_INSTANCE_EPHEMERAL` | `true` | Requests ephemeral Namespace instances. |
| `region` | `--namespace-instance-region` | `CRABBOX_NAMESPACE_INSTANCE_REGION` | _(none)_ | Optional `nsc --region` value. |
| `endpoint` | `--namespace-instance-endpoint` | `CRABBOX_NAMESPACE_INSTANCE_ENDPOINT` | _(none)_ | Optional `nsc --endpoint` value. |
| `keychain` | `--namespace-instance-keychain` | `CRABBOX_NAMESPACE_INSTANCE_KEYCHAIN` | _(none)_ | Optional `nsc --keychain` value. |
| `workRoot` | `--namespace-instance-work-root` | `CRABBOX_NAMESPACE_INSTANCE_WORK_ROOT` | `/work/crabbox` | Remote sync root; must be a dedicated absolute subdirectory. |
| `volume` / `volumes` | `--namespace-instance-volume` | `CRABBOX_NAMESPACE_INSTANCE_VOLUME` | _(none)_ | Comma-separated flag/env or YAML list of `nsc --volume` specs. |

Related docs:

- [Provider: Namespace Instance](../providers/namespace-instance.md)
- [Namespace Instance setup](namespace-instance-setup.md)
- [Namespace Devbox](namespace-devbox.md)
- [Providers](providers.md)
