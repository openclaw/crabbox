# Coder

`provider: coder` leases Linux Coder workspaces through the local `coder` CLI
and exposes them to Crabbox as normal SSH leases. Crabbox uses Coder for
workspace lifecycle, authentication, and tunneling, then runs its usual SSH
sync, command execution, status, and cleanup flow over `coder ssh --stdio`.

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
Crabbox slug and `coder.workspacePrefix`. Workspace names are lowercase,
hyphenated, 1-32 characters, and avoid Coder's reserved `new` and `create`
names.

Crabbox can resolve a Coder lease by Crabbox lease ID, local slug, Coder
workspace name, or `owner/workspace` when the Coder inventory contains a unique
match.

Release is conservative:

- By default, `crabbox stop` runs `coder stop --yes <workspace>` and removes the
  local Crabbox claim.
- Deletion requires `coder.deleteOnRelease: true` or
  `--coder-delete-on-release`, which runs `coder delete --yes <workspace>`.

Cleanup is also conservative. It only acts on workspaces that look
Crabbox-owned through the configured workspace prefix or Crabbox labels in
Coder JSON. `crabbox cleanup --provider coder --dry-run` prints the intended
stop/delete actions without mutating workspaces.

## SSH

Coder workspaces use OpenSSH proxy mode:

```text
ProxyCommand coder ssh --stdio --wait yes <workspace>
```

Crabbox marks the target as proxy-backed instead of trying to discover a raw
host or port. The ready check verifies the standard Crabbox sync prerequisites:
`git`, `rsync`, and `tar`.

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
