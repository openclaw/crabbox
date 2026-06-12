# Namespace Devbox Provider

Read this when you:

- choose `provider: namespace-devbox`;
- configure the Devbox image, size, repository, site, volume, or release behavior;
- change `internal/providers/namespace`.

Namespace Devbox is an **SSH-lease** provider. The Namespace `devbox` CLI owns
authentication, creation, SSH configuration, listing, shutdown, and deletion.
Crabbox layers its normal contract on top: local slugs, repo claims,
dirty-tree sync, command execution, timing, and normalized list/status output.

The provider runs **direct from the CLI** — it never goes through the broker
coordinator. Targets Linux only.

## When to use

Use Namespace Devbox when your environment should come from a Namespace Devbox
image and you want Crabbox's standard SSH sync/run path on top. If you instead
want the provider to own sync and command execution, use the Blacksmith Testbox
provider, which runs through `blacksmith testbox run`.

## Prerequisites

Install the Namespace `devbox` CLI and authenticate before using Crabbox:

```sh
devbox login
```

Crabbox does not store Namespace credentials — it shells out to `devbox` for
every lifecycle operation, so the CLI must already be logged in.

## Commands

```sh
crabbox warmup --provider namespace-devbox --namespace-image builtin:base --namespace-size M
crabbox run --provider namespace-devbox --id swift-crab -- pnpm test
crabbox ssh --provider namespace-devbox --id swift-crab
crabbox status --provider namespace-devbox --id swift-crab
crabbox stop --provider namespace-devbox swift-crab
```

Aliases for the provider name: `namespace`, `namespace-devboxes`.

## Configuration

```yaml
provider: namespace-devbox
target: linux
namespace:
  image: builtin:base
  size: M
  repository: github.com/example-org/my-app
  site: ""
  volumeSizeGB: 100
  autoStopIdleTimeout: 30m
  workRoot: /workspaces/crabbox
  deleteOnRelease: false
```

Defaults baked into Crabbox: `image: builtin:base`,
`workRoot: /workspaces/crabbox`, `autoStopIdleTimeout: 30m`. `size`,
`repository`, `site`, and `volumeSizeGB` are unset by default (size is then
derived from `--class`, see below).

Provider flags:

```text
--namespace-image
--namespace-size
--namespace-repository
--namespace-site
--namespace-volume-size-gb
--namespace-auto-stop-idle-timeout
--namespace-work-root
--namespace-delete-on-release
```

Environment overrides:

```text
CRABBOX_NAMESPACE_IMAGE
CRABBOX_NAMESPACE_SIZE
CRABBOX_NAMESPACE_REPOSITORY
CRABBOX_NAMESPACE_SITE
CRABBOX_NAMESPACE_VOLUME_SIZE_GB
CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT
CRABBOX_NAMESPACE_WORK_ROOT
CRABBOX_NAMESPACE_DELETE_ON_RELEASE
```

## Lifecycle

1. Crabbox writes a temporary Devbox spec and runs `devbox create --from <spec>`.
2. It runs `devbox configure-ssh` and reads the generated SSH config and key
   (connecting as user `devbox`).
3. It waits for SSH readiness plus the `git`, `rsync`, and `tar` tools the sync
   path needs.
4. It records a local Crabbox lease claim and runs through the normal SSH
   executor.
5. `crabbox stop` shuts the Devbox down (`devbox shutdown --force`) by default.
   The local claim and generated SSH files remain available for reuse. Set
   `namespace.deleteOnRelease: true` (or `--namespace-delete-on-release`) to
   delete the Devbox and those local SSH files instead (`devbox delete --force`).

## Capabilities

- **SSH**: yes.
- **Crabbox sync**: yes — standard rsync over SSH.
- **Cleanup**: yes — `crabbox cleanup --provider namespace-devbox` sweeps
  stale local Devbox SSH config/key artifacts.
- **Actions hydration**: yes — same Linux SSH contract as other SSH-lease
  providers.
- **Desktop / browser / code**: not surfaced for this provider.
- **Coordinator (broker)**: never; always direct from the CLI.

## Size mapping

`--namespace-size` accepts `S`, `M`, `L`, or `XL`. When size is unset, Crabbox
derives it from `--class`:

| Class      | Size |
| ---------- | ---- |
| `standard` | `S`  |
| `fast`     | `M`  |
| `large`    | `L`  |
| `beast`    | `XL` |

An empty class resolves to `M`. An explicit `--namespace-size` (or
`namespace.size`) always wins over the class mapping.

## Gotchas

- Run `devbox login` first. Crabbox does not store Namespace credentials.
- `builtin:base` is the current Namespace built-in base image. Avoid `default`;
  the current CLI treats that as a Docker image reference.
- Namespace Devboxes can pause/resume outside Crabbox; stopped Devboxes keep
  their persistent storage. Crabbox also keeps the stopped lease claim and SSH
  configuration so later `run`, `ssh`, and `pond connect` commands can resume it.
- `namespace.repository` asks Namespace to clone a repo on create, but Crabbox
  still syncs your local dirty checkout into `namespace.workRoot`.
- `namespace.workRoot` must be an absolute path under a dedicated subdirectory;
  broad roots such as `/`, `/home`, `/tmp`, or `/workspaces` are rejected.

## Related docs

- [Feature: Namespace Devbox](../features/namespace-devbox.md)
- [Namespace Devbox setup](../features/namespace-devbox-setup.md)
- [Provider backends](../provider-backends.md)
