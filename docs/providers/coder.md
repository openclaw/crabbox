# Coder

`provider: coder` is a narrow direct SSH-lease integration for Linux Coder
workspaces. Crabbox asks the local `coder` CLI to create, start, stop, or
optionally delete Crabbox-claimed workspaces, while Coder keeps authentication,
workspace policy, and tunneling ownership. Crabbox then runs its usual SSH sync,
command execution, status, and cleanup flow over `coder ssh --stdio`.

Coder is direct-only. It never routes through the Crabbox coordinator and it
does not store Coder API tokens in Crabbox config.

## Requirements

- Coder CLI installed and on `PATH`, or set `coder.cliPath`.
- A local Coder login, usually from `coder login <url>`.
- A Linux Coder template with `git`, `rsync`, and `tar` available in the
  workspace.
- A template name for new workspaces, supplied by config or
  `--coder-template`.

Run a non-mutating preflight first:

```sh
crabbox doctor --provider coder
crabbox doctor --provider coder --json
```

If the Coder CLI is not logged in, doctor fails with `auth=missing_login` and
`mutation=false`. It does not create, start, stop, or delete workspaces.

## Config

```yaml
provider: coder
coder:
  cliPath: coder
  template: go-dev
  preset: large
  workspacePrefix: crabbox-
  workRoot: /home/coder/crabbox
  wait: yes
  useParameterDefaults: true
  parameters:
    - region=iad
    - size=large
  richParameterFile: ~/.config/coder/rich-parameters.yaml
  deleteOnRelease: false
```

Environment overrides:

```text
CRABBOX_CODER_CLI
CRABBOX_CODER_TEMPLATE
CRABBOX_CODER_PRESET
CRABBOX_CODER_WORKSPACE_PREFIX
CRABBOX_CODER_WORK_ROOT
CRABBOX_CODER_WAIT
CRABBOX_CODER_USE_PARAMETER_DEFAULTS
CRABBOX_CODER_PARAMETERS
CRABBOX_CODER_RICH_PARAMETER_FILE
CRABBOX_CODER_DELETE_ON_RELEASE
```

`CRABBOX_CODER_PARAMETERS` is comma-separated, for example
`region=iad,size=large`. Coder session tokens should stay in Coder's own login
store or supported Coder environment, not in Crabbox config and not on Crabbox
argv.

## Flags

```text
--coder-cli <path>
--coder-template <name>
--coder-preset <name>
--coder-workspace-prefix <prefix>
--coder-work-root <path>
--coder-wait yes|no|auto
--coder-use-parameter-defaults
--coder-parameter name=value[,name=value]
--coder-rich-parameter-file <path>
--coder-delete-on-release
```

`--class` and `--type` are not supported for `provider=coder`; choose sizing in
the Coder template or preset.

## Lifecycle

New leases create a Coder workspace with a Coder-safe name derived from the
Crabbox slug, `coder.workspacePrefix`, and a short lease hash suffix. The local
Crabbox slug remains the friendly lookup handle, while the Coder workspace name
is lease-unique so failed provisioning rollback cannot stop or delete another
same-slug workspace. Workspace names are lowercase, hyphenated, 1-32 characters,
and avoid Coder's reserved `new` and `create` names.

Crabbox can resolve a Coder lease by Crabbox lease ID, local slug, Coder
workspace name, or `owner/workspace` when the Coder inventory contains a unique
match.

Release is conservative:

- By default, `crabbox stop` runs `coder stop --yes <workspace>` and removes the
  local Crabbox claim.
- Deletion requires `coder.deleteOnRelease: true` or
  `--coder-delete-on-release`, which runs `coder delete --yes <workspace>`.
  New local claims persist that release action so later cleanup does not turn an
  originally stop-on-release workspace into a delete-on-cleanup workspace after
  a config change.

Cleanup is also conservative. It lists workspaces that look Crabbox-owned
through the configured workspace prefix or Crabbox labels in Coder JSON, but it
only stops or deletes workspaces that also have a local Crabbox claim with
cleanup metadata. Prefix-owned workspaces without a local claim are skipped,
because stopped Coder workspaces are normal reusable environments and Coder does
not expose a generic `coder create` label flag for Crabbox-owned lifecycle
metadata. `crabbox cleanup --provider coder --dry-run` prints the intended
stop/delete actions without mutating workspaces. Older local claims that do not
record a Coder release action default to stop during cleanup.

## SSH

Coder workspaces use OpenSSH proxy mode:

```text
ProxyCommand coder ssh --stdio --wait yes <workspace>
```

Crabbox marks the target as proxy-backed instead of trying to discover a raw
host or port. The ready check verifies the standard Crabbox sync prerequisites:
`git`, `rsync`, and `tar`.

Coder SSH targets use stable synthetic host aliases for SSH config reuse, but
their `known_hosts` entries are isolated under Crabbox's Coder config directory
and keyed by the Coder workspace identity when available. This avoids stale
global host keys when a disposable workspace is deleted and later recreated with
the same name.

## Examples

```sh
crabbox warmup --provider coder --coder-template go-dev --slug testbox
crabbox run --provider coder --coder-template go-dev -- pnpm test
crabbox ssh --provider coder testbox
crabbox status --provider coder testbox
crabbox stop --provider coder testbox
```

Delete only when the workspace is disposable:

```sh
crabbox stop --provider coder --coder-delete-on-release testbox
```

## Live smoke

Run live smoke only after `coder whoami -o json` succeeds and you have selected
a safe disposable template:

```sh
crabbox doctor --provider coder
crabbox warmup --provider coder --coder-template go-dev --slug coder-smoke
crabbox run --provider coder --id coder-smoke -- bash -lc 'command -v git && command -v rsync && command -v tar && echo ok'
crabbox stop --provider coder coder-smoke
```

If login, template, or quota is unavailable, classify the live smoke as
`environment_blocked` instead of treating deterministic unit tests as live
proof.
