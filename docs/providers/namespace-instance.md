# Namespace Instance Provider

Read this when you:

- choose `provider: namespace-instance`;
- configure Namespace Compute Instance machine type, duration, region, endpoint,
  keychain, volumes, or work root;
- compare Namespace Compute Instances with Namespace Devboxes.

Namespace Instance is a direct **SSH-lease** provider for Namespace Compute
Instances. Crabbox shells out to the Namespace `nsc` CLI for auth, inventory,
create, describe, extend, and destroy, then uses the normal Crabbox SSH sync/run
path once the instance exposes SSH.

The provider runs direct from the CLI and is never brokered through the Crabbox
coordinator. Targets Linux only.

## Namespace Devbox Boundary

Crabbox has two Namespace-backed providers:

- `namespace-devbox` uses the Namespace `devbox` CLI and keeps the `namespace`
  and `namespace-devboxes` aliases.
- `namespace-instance` uses the Namespace `nsc` CLI for Compute Instances. Its
  alias is `namespace-compute`.

Use `namespace-devbox` when you want Devbox images and Devbox lifecycle
semantics. Use `namespace-instance` when you want short-lived Compute Instances
with `nsc`-managed instance lifecycle.

## Prerequisites

Install `nsc` and authenticate before using Crabbox:

```sh
nsc login
nsc auth check-login
```

Crabbox does not store Namespace credentials and does not accept Namespace API
tokens on the command line. It relies on the auth state that `nsc` resolves on
the operator machine.

Run a read-only readiness check before creating an instance:

```sh
crabbox doctor --provider namespace-instance
```

`doctor` verifies that `nsc` is available, `nsc auth check-login` succeeds, and
`nsc list -o json` can read inventory.

## Commands

```sh
crabbox warmup --provider namespace-instance --slug smoke --namespace-instance-machine-type linux-small
crabbox status --provider namespace-instance --id smoke --wait
crabbox run --provider namespace-instance --id smoke --no-sync -- echo crabbox-ok
crabbox list --provider namespace-instance
crabbox stop --provider namespace-instance smoke
crabbox cleanup --provider namespace-instance --dry-run
```

Lease-acting commands accept the Crabbox lease ID, the friendly slug, or the
Namespace instance ID for Crabbox-managed instances.

## Configuration

```yaml
provider: namespace-instance
target: linux
namespaceInstance:
  machineType: linux-small
  duration: 15m
  ephemeral: true
  region: ""
  endpoint: ""
  keychain: ""
  workRoot: /work/crabbox
  volume:
    - cache:/cache
```

Defaults baked into Crabbox: `ephemeral: true` and `workRoot: /work/crabbox`.
When `duration` is unset and the command has a `--ttl`, Crabbox uses the lease
TTL as the requested Namespace duration. When `machineType` is unset, each
Crabbox class currently maps to `linux-small`.

Provider flags:

```text
--namespace-instance-machine-type
--namespace-instance-duration
--namespace-instance-ephemeral
--namespace-instance-region
--namespace-instance-endpoint
--namespace-instance-keychain
--namespace-instance-work-root
--namespace-instance-volume
```

Environment overrides:

```text
CRABBOX_NAMESPACE_INSTANCE_MACHINE_TYPE
CRABBOX_NAMESPACE_INSTANCE_DURATION
CRABBOX_NAMESPACE_INSTANCE_EPHEMERAL
CRABBOX_NAMESPACE_INSTANCE_REGION
CRABBOX_NAMESPACE_INSTANCE_ENDPOINT
CRABBOX_NAMESPACE_INSTANCE_KEYCHAIN
CRABBOX_NAMESPACE_INSTANCE_WORK_ROOT
CRABBOX_NAMESPACE_INSTANCE_VOLUME
```

## Lifecycle

1. `doctor` checks the local `nsc` CLI, auth, and inventory access without
   creating resources.
2. `warmup` creates a Crabbox lease ID and slug, creates or reuses the per-lease
   SSH key, and calls `nsc create` with Crabbox labels, machine type, duration,
   SSH public key, optional region/endpoint/keychain, and optional volumes.
3. Crabbox reads the returned instance JSON, describes the instance when needed,
   resolves the SSH target, waits for SSH readiness, and records a local lease
   claim.
4. `run`, `ssh`, `sync`, `status`, and `list` use the normal SSH-lease
   contract and Crabbox-managed labels.
5. `stop` calls `nsc destroy --force` for the claimed instance and removes the
   local claim. Already-gone instances are treated as a successful cleanup path.
6. `cleanup` lists `nsc` inventory, filters to Crabbox-owned
   `namespace-instance` labels, and destroys expired or stale instances. Use
   `--dry-run` to preview.

## Capabilities

- **SSH**: yes.
- **Crabbox sync**: yes — standard rsync over SSH.
- **Cleanup**: yes — destroys stale Crabbox-owned Namespace Compute Instances.
- **Actions hydration**: yes — same Linux SSH contract as other SSH-lease
  providers.
- **Desktop / browser / code**: not surfaced for this provider.
- **Coordinator (broker)**: never; always direct from the CLI.

## Live Smoke

The repository live smoke is opt-in and destructive because it creates and
destroys a real Namespace Compute Instance. Run it only from an authenticated
operator machine:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=namespace-instance \
CRABBOX_LIVE_REPO=/path/to/my-app \
scripts/live-smoke.sh
```

The smoke requires `nsc`, `jq`, `rg`, `CRABBOX_LIVE=1`, explicit
`CRABBOX_LIVE_PROVIDERS=namespace-instance`, and an explicit
`CRABBOX_LIVE_REPO`. It runs `doctor`, `warmup`, `status`, `run`, `list`, and
`stop`, and it attempts `stop` from a cleanup trap after a lease has been
created.

## Gotchas

- The `namespace` alias belongs to `namespace-devbox`, not
  `namespace-instance`.
- `nsc` auth is external to Crabbox. If `nsc auth check-login` fails, fix the
  local CLI login before debugging Crabbox.
- Keep `workRoot` under a dedicated absolute subdirectory. Broad roots such as
  `/`, `/home`, or `/tmp` are rejected.
- Phase one depends on the SSH target fields returned by `nsc` JSON. If an
  account or instance shape does not expose host, user, and port, Crabbox fails
  closed rather than guessing an API-backed SSH path.

## Related docs

- [Feature: Namespace Instance](../features/namespace-instance.md)
- [Namespace Instance setup](../features/namespace-instance-setup.md)
- [Provider: Namespace Devbox](namespace-devbox.md)
- [Provider backends](../provider-backends.md)
