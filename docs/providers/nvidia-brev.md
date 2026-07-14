# NVIDIA Brev Provider

Read when:

- choosing `provider: nvidia-brev`;
- using Crabbox on NVIDIA Brev GPU workspaces;
- changing `internal/providers/nvidiabrev`.

NVIDIA Brev is a **direct-only Linux SSH-lease** provider. Crabbox shells out
to the local `brev` CLI to create, list, refresh, stop, and delete Brev
workspaces. After Brev writes its SSH config, Crabbox uses the normal SSH
transport for sync, `run`, `ssh`, `status`, `list`, and `stop`.

Crabbox does not store or accept Brev secrets. Authentication stays in the Brev
CLI's own credential store.

## Prerequisites

- Install the Brev CLI and authenticate it:

  ```sh
  brev login
  ```

- Confirm Brev can list workspaces:

  ```sh
  brev ls --json
  ```

- For OAuth login, select the organization used for lifecycle operations:

  ```sh
  brev set
  ```

  API-key and workspace contexts may provide an explicit organization scope
  without an active OAuth organization.

- Keep OpenSSH and `rsync` available locally for Crabbox's SSH workflow.
- Use an account with enough Brev quota and a GPU type that is available in the
  selected Brev cloud.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes, from Brev's generated SSH config |
| Crabbox sync (rsync over SSH) | Yes |
| Provider-managed sync | No |
| Desktop / browser / code | No |
| Actions hydration | Yes, as a normal Linux SSH lease |
| Coordinator (broker) | No - direct only |
| Tailscale | No, Brev CLI-managed SSH access is used |
| Cleanup | Yes, for Crabbox-owned workspaces with local claims |

Aliases: `brev`, `nvidia`.

## Configuration

```yaml
provider: nvidia-brev
target: linux
nvidiaBrev:
  cli: brev
  type: ""
  gpuName: A100
  provider: ""
  mode: vm
  launchable: ""
  startupScript: ""
  releaseAction: delete
  target: container
  user: ""
  workRoot: /tmp/crabbox
```

Defaults:

- `cli`: `brev`
- `gpuName`: `A100`
- `mode`: `vm`
- `releaseAction`: `delete`
- `target`: `container`
- `workRoot`: `/tmp/crabbox`

`startupScript` follows Brev's native syntax: use an inline command such as
`pip install torch`, or prefix a local file path with `@`, for example
`@setup.sh` or `@/opt/setup.sh`. Local `@file` startup scripts are accepted only
from trusted user config, environment overrides, or command-line flags; project
config cannot select local files.

Provider flags:

```text
--nvidia-brev-cli
--nvidia-brev-org
--nvidia-brev-type
--nvidia-brev-gpu-name
--nvidia-brev-provider
--nvidia-brev-mode
--nvidia-brev-launchable
--nvidia-brev-startup-script
--nvidia-brev-release-action
--nvidia-brev-target
--nvidia-brev-user
--nvidia-brev-work-root
```

Environment overrides:

```text
CRABBOX_NVIDIA_BREV_CLI
CRABBOX_NVIDIA_BREV_ORG
CRABBOX_NVIDIA_BREV_TYPE
CRABBOX_NVIDIA_BREV_GPU_NAME
CRABBOX_NVIDIA_BREV_PROVIDER
CRABBOX_NVIDIA_BREV_MODE
CRABBOX_NVIDIA_BREV_LAUNCHABLE
CRABBOX_NVIDIA_BREV_STARTUP_SCRIPT
CRABBOX_NVIDIA_BREV_RELEASE_ACTION
CRABBOX_NVIDIA_BREV_TARGET
CRABBOX_NVIDIA_BREV_USER
CRABBOX_NVIDIA_BREV_WORK_ROOT
```

`nvidiaBrev.org` scopes read-only inventory through `brev ls --org`. Brev's
mutating commands and `brev refresh` do not accept that selector, so Crabbox
rejects lifecycle and SSH resolution when `org` is configured. Use `brev set`
to select the active organization before running mutating Crabbox commands.

## Lifecycle

### Doctor

`doctor` is read-only. It checks the Brev CLI version and lists the current Brev
inventory:

```sh
crabbox doctor --provider nvidia-brev
crabbox doctor --provider nvidia-brev --json
```

It does not create, stop, or delete a workspace.

### Acquire

`warmup` and `run` create cost-bearing Brev workspaces:

```sh
crabbox warmup --provider nvidia-brev --slug gpu-smoke --keep
crabbox run --provider nvidia-brev -- nvidia-smi
```

Acquire flow:

1. list existing workspaces to allocate a Crabbox slug;
2. run `brev create <name> --detached` with the configured Brev selectors;
3. wait for the workspace to report ready;
4. run `brev refresh`;
5. read the Brev SSH config from `~/.brev/ssh_config`;
6. resolve the configured target alias;
7. wait for SSH readiness;
8. write a local Crabbox lease claim and run the normal SSH workflow.

Crabbox names workspaces as `crabbox-<slug>-<lease-suffix>` so `list` and
`cleanup` can distinguish Crabbox-owned Brev workspaces from manual ones.

If `brev create` returns an ambiguous transport error, Crabbox checks inventory
for the deterministic workspace name and stores a recovery claim. Non-kept
workspaces are deleted when ownership is confirmed; kept workspaces remain
manageable by lease ID or slug. Name-only recovery claims report `failed` until
the workspace appears and can be cleared by explicit release after the recovery
grace period.

### SSH target selection

`nvidiaBrev.target` controls which Brev SSH config host Crabbox selects:

- `container` (default) selects the workspace host alias.
- `host` selects the `<workspace-name>-host` alias.

Brev may emit either a direct `HostName`/`Port` target or a `ProxyCommand`.
Crabbox supports both forms as long as the SSH config entry includes a user and
identity file.

### Release and cleanup

The default release action is `delete`:

```sh
crabbox stop --provider nvidia-brev gpu-smoke
```

`delete` records the claim as deleting, clears its SSH endpoint, runs
`brev delete`, and keeps polling until Brev inventory confirms that the
workspace is absent. The local claim remains available for a later `stop` or
`cleanup` retry if polling is interrupted.

Crabbox stores the active Brev organization ID in each claim. Because Brev
mutations do not accept an organization selector, lifecycle commands reject an
active-org mismatch and retain the claim. Run `brev set` for the lease's
original organization, then retry the command.

If the active organization changes while a workspace is being created, Crabbox
does not delete from either organization automatically. It retains a recovery
claim with both observed organization IDs for manual reconciliation, avoiding
deletion of an unrelated same-named workspace.

If `nvidiaBrev.releaseAction` is `stop`, Crabbox runs `brev stop`, keeps the
local claim, and records the lease as stopped for later reuse or cleanup.

`cleanup` only mutates Brev workspaces that have matching local Crabbox claims:

```sh
crabbox cleanup --provider nvidia-brev --dry-run
crabbox cleanup --provider nvidia-brev
```

Manual Brev workspaces and unclaimed Crabbox-looking workspaces are skipped
rather than deleted blindly.

## Examples

Run a GPU smoke and delete the workspace after the command:

```sh
crabbox run --provider nvidia-brev -- nvidia-smi
```

Warm a reusable GPU workspace, run a CUDA check, then release it:

```sh
crabbox warmup --provider nvidia-brev --slug cuda-box --keep
crabbox run --provider nvidia-brev --id cuda-box --no-sync -- python3 - <<'PY'
print("cuda-ready")
PY
crabbox ssh --provider nvidia-brev --id cuda-box
crabbox status --provider nvidia-brev --id cuda-box --wait
crabbox stop --provider nvidia-brev cuda-box
```

Choose a different Brev GPU selector:

```sh
crabbox run \
  --provider nvidia-brev \
  --nvidia-brev-gpu-name L40S \
  -- nvidia-smi
```

## Live smoke

The repository live smoke is intentionally separate from default CI because it
can create a billable GPU workspace:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nvidia-brev scripts/live-smoke.sh
CRABBOX_NVIDIA_BREV_LIVE=1 scripts/live-nvidia-brev-smoke.sh
```

The top-level script dispatches to the provider-specific script with
`CRABBOX_NVIDIA_BREV_LIVE=1`. The provider-specific script builds or reuses
`bin/crabbox`, runs `doctor`, creates one workspace with `--keep=false` so
partial acquisition failures roll back, proves `nvidia-smi` through
`crabbox run`, lists the lease, and then deletes the lease with `crabbox stop`.
It prints a stable classification:

- `live_nvidia_brev_smoke_passed` when the full GPU path passes;
- `environment_blocked` for missing CLI/auth/configuration;
- `provider_quota_blocked` for quota or rate-limit failures;
- `capacity_blocked` when Brev cannot allocate the requested GPU;
- `validation_failed` when Crabbox output is malformed or missing the smoke
  lease.

Without `CRABBOX_NVIDIA_BREV_LIVE=1`, the script exits successfully with
`classification=environment_blocked reason=CRABBOX_NVIDIA_BREV_LIVE_not_enabled`
and creates no workspace.

## Cost and privacy discipline

- Treat every `warmup` or `run` without an existing `--id` as cost-bearing.
- Prefer `releaseAction: delete` for disposable test runs.
- Run `crabbox list --provider nvidia-brev --all` before and after live tests
  when auditing inventory.
- Do not put Brev secrets, organization identifiers, private SSH material, or
  local credential files in repository config, docs, scripts, logs, or command
  arguments.
- Use neutral slugs and examples in shared repos.

Related docs:

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
