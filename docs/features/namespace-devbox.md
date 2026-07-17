# Namespace Devbox

Read this when you are:

- choosing `provider: namespace-devbox`;
- comparing Namespace Devbox with other delegated or SSH-lease providers;
- debugging the Namespace CLI lifecycle that backs Crabbox sync and run.

`provider: namespace-devbox` (aliases `namespace`, `namespace-devboxes`) creates
or reuses [Namespace](https://namespace.so) Devboxes and exposes them to Crabbox
as Linux SSH leases. Namespace owns the Devbox lifecycle, SSH config, and auth;
Crabbox owns the local checkout sync, command execution, Actions hydration, and
run timing. The provider is Linux-only and runs direct from the CLI — it is never
brokered through the coordinator.

## Setup

Install and authenticate the Namespace CLI (`devbox`):

```sh
devbox login
```

Crabbox shells out to the `devbox` binary for create, configure-ssh, list,
shutdown, and delete. It reads the generated SSH config from `~/.namespace/ssh/`,
so a working `devbox login` session is the only credential Crabbox needs.

Select the provider in config:

```yaml
provider: namespace-devbox
namespace:
  image: builtin:base
  size: M
  workRoot: /workspaces/crabbox
```

## Commands

```sh
crabbox warmup --provider namespace-devbox --namespace-image builtin:base
crabbox run --provider namespace-devbox --id <slug> -- pnpm test
crabbox ssh --provider namespace-devbox --id <slug>
crabbox list --provider namespace-devbox
crabbox stop --provider namespace-devbox <slug>
```

Lease-acting commands accept either the canonical `cbx_…` lease ID or the
friendly slug via `--id <devbox-name-or-slug>`.

## Provider boundary

Namespace Devbox is an **SSH-lease** provider, not a delegated-run provider. That
distinction shapes the integration:

- A delegated-run provider owns sync and command transport end to end; Crabbox
  hands it the workspace and a command and gets results back.
- Namespace Devbox only owns provisioning: create, the generated SSH config, and
  list. Crabbox then drives rsync, SSH command execution, Actions hydration, and
  timing directly against the box, exactly as it does for any other SSH lease.

The provider declares the `ssh`, `crabbox-sync`, and `cleanup` features.

## Configuration

Set these under the `namespace` config section, override per invocation with the
matching `--namespace-*` flag, or via the `CRABBOX_NAMESPACE_*` environment
variables. Flag wins over env, which wins over config file, which wins over the
built-in default.

| Config key            | Flag                                | Env var                                    | Default              | Notes |
| --------------------- | ----------------------------------- | ------------------------------------------ | -------------------- | ----- |
| `image`               | `--namespace-image`                 | `CRABBOX_NAMESPACE_IMAGE`                  | `builtin:base`       | Devbox image. |
| `size`                | `--namespace-size`                  | `CRABBOX_NAMESPACE_SIZE`                   | `M`                  | One of `S`, `M`, `L`, `XL` (case-insensitive). |
| `repository`          | `--namespace-repository`            | `CRABBOX_NAMESPACE_REPOSITORY`             | _(none)_             | Optional repo for Namespace to clone into the Devbox. |
| `site`                | `--namespace-site`                  | `CRABBOX_NAMESPACE_SITE`                   | _(none)_             | Optional Namespace site. |
| `volumeSizeGB`        | `--namespace-volume-size-gb`        | `CRABBOX_NAMESPACE_VOLUME_SIZE_GB`         | _(none)_             | Persistent volume size in GiB; must be non-negative. |
| `autoStopIdleTimeout` | `--namespace-auto-stop-idle-timeout`| `CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT` | `30m`                | Namespace idle auto-stop; falls back to Crabbox `--idle-timeout` if unset. |
| `workRoot`            | `--namespace-work-root`             | `CRABBOX_NAMESPACE_WORK_ROOT`              | `/workspaces/crabbox`| Crabbox sync root; must be a dedicated absolute subdirectory. |
| `deleteOnRelease`     | `--namespace-delete-on-release`     | `CRABBOX_NAMESPACE_DELETE_ON_RELEASE`      | `false`              | Delete the Devbox on release instead of shutting it down. |

The `workRoot` is validated: it must resolve to an absolute path and may not be a
broad system directory (e.g. `/`, `/home`, `/tmp`, or `/workspaces` itself);
choose a dedicated subdirectory such as the default `/workspaces/crabbox`.

Related docs:

- [Provider: Namespace Devbox](../providers/namespace-devbox.md)
- [Namespace Devbox setup](namespace-devbox-setup.md)
- [Provider Reference](../providers/README.md)
