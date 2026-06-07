# Docker Sandbox Provider

Read when:

- choosing `provider: docker-sandbox`;
- configuring Docker Sandboxes through the standalone `sbx` CLI;
- changing `internal/providers/dockersandbox`.

Docker Sandbox is a delegated-run provider. Crabbox shells out to the standalone
`sbx` CLI for sandbox lifecycle and command execution. Docker owns the sandbox
runtime and transport; Crabbox owns provider selection, local config, friendly
slugs, local ownership claims, timing summaries, and normalized list/status
rendering.

This provider is intentionally separate from
[Local Container](local-container.md). `docker-sandbox` has no aliases, and the
existing `docker`, `container`, and `local-docker` aliases continue to resolve
to `local-container`.

## When To Use

Use Docker Sandbox when you want a Docker-managed Linux sandbox and command
execution through `sbx exec`. Use Local Container when you want a local
Docker-compatible container with Crabbox-managed SSH, rsync, desktop/browser
surfaces, or local Docker socket pass-through.

## Prerequisites

- The standalone `sbx` CLI must be on `PATH`, or configured with
  `--docker-sandbox-cli` / `dockerSandbox.cliPath`.
- Run `sbx login` before Crabbox lifecycle commands when your Docker Sandbox
  account requires authentication.
- The host must satisfy Docker Sandbox virtualization requirements. Doctor
  surfaces KVM, hypervisor, or virtualization errors when the CLI reports them.

## Supported `sbx` Contract

Crabbox intentionally targets the standalone Docker Sandbox CLI surface, not an
internal Docker API.

Validated local proof for this PR used this `sbx` version pair:

- `sbx` client `v0.31.3`
- `sbx` server `v0.31.3`

This is the validated compatibility baseline, not a promise that older or newer
`sbx` releases have the same behavior. Crabbox treats other `sbx` versions as
best-effort compatible only when they preserve the command and JSON contract
below. When Docker changes this contract, update this document and the
provider-local tests in `internal/providers/dockersandbox` in the same change.
`crabbox doctor --provider docker-sandbox` reports `sbx_compatibility=ok` when
the installed `sbx` version matches this baseline and a warning when it is a
best-effort version.

Crabbox currently depends on these `sbx` commands and flags:

- `sbx version`
- `sbx ls --json`
- `sbx diagnose --output json` as optional doctor enrichment
- `sbx create --name <name> [--template ...] [--cpus ...] [--memory ...] [--clone] shell <repo-root> [extra-workspaces...]`
- `sbx exec [--workdir ...] [--env-file ...] <sandbox-name> <command...>`
- `sbx ports <sandbox-name> [--json] [--publish ...] [--unpublish ...]`
- `sbx cp [-L] <src> <dst>`
- `sbx rm --force <sandbox-name>`

These calls match the official Docker Sandboxes docs. The public starting
point is <https://docs.docker.com/ai/sandboxes/>; the matching local scrape is
`../docs/content/manuals/ai/sandboxes/_index.md`. The install and first-run
walkthrough lives at <https://docs.docker.com/ai/sandboxes/get-started/> and
`../docs/content/manuals/ai/sandboxes/get-started.md`. The sibling Docker docs
checkout also includes exact CLI references:
`../docs/data/sbx_cli/sbx_create_shell.yaml` documents that `sbx create shell
PATH` mounts the workspace path inside the sandbox, and
`../docs/data/sbx_cli/sbx_exec.yaml` documents that `sbx exec` receives a
sandbox name, command arguments, `--workdir`, and `--env-file`.
`../docs/data/sbx_cli/sbx_ports.yaml` documents the publish/list/unpublish port
surface, and `../docs/data/sbx_cli/sbx_cp.yaml` documents `sbx cp` with
`SANDBOX:PATH` syntax.

`sbx ls --json` compatibility currently accepts either a top-level array or an
object wrapper containing arrays under `sandboxes`, `items`, `data`, or
`results`. Per-item field parsing accepts common variants: `id`, `ID`,
`sandboxId`, and `sandbox_id`; `name`, `Name`, `sandboxName`, and
`sandbox_name`; `state`, `status`, and `Status`; `agent` and `Agent`; and
`workspace`, `workdir`, `workingDir`, and `working_dir`. Records without a name
or id are ignored.

## Commands

```sh
crabbox doctor --provider docker-sandbox
crabbox warmup --provider docker-sandbox --slug live-smoke
crabbox run --provider docker-sandbox -- echo ok
crabbox run --provider docker-sandbox --id live-smoke -- pwd
crabbox ports --provider docker-sandbox --id live-smoke --publish 3000
crabbox cp --provider docker-sandbox --id live-smoke SANDBOX:/tmp/output.log ./output.log
crabbox list --provider docker-sandbox --json
crabbox status --provider docker-sandbox --id live-smoke
crabbox stop --provider docker-sandbox live-smoke
```

## Config

```yaml
provider: docker-sandbox
target: linux
dockerSandbox:
  cliPath: sbx
  agent: shell
  template: ""
  cpus: 0
  memory: ""
  clone: false
  workdir: ""        # empty means the current repo root path inside the sandbox
  extraWorkspaces: []
  mcp: []            # optional sbx MCP server names; repeated values pass through to sbx create --mcp
  kit: []
```

Provider flags:

```text
--docker-sandbox-cli
--docker-sandbox-agent
--docker-sandbox-template
--docker-sandbox-cpus
--docker-sandbox-memory
--docker-sandbox-clone
--docker-sandbox-workdir
--docker-sandbox-extra-workspace
--docker-sandbox-mcp   # repeatable; forwarded to sbx create --mcp in order
--docker-sandbox-kit
```

Environment overrides:

```text
CRABBOX_DOCKER_SANDBOX_CLI
CRABBOX_DOCKER_SANDBOX_AGENT
CRABBOX_DOCKER_SANDBOX_TEMPLATE
CRABBOX_DOCKER_SANDBOX_CPUS
CRABBOX_DOCKER_SANDBOX_MEMORY
CRABBOX_DOCKER_SANDBOX_CLONE
CRABBOX_DOCKER_SANDBOX_WORKDIR
CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES
CRABBOX_DOCKER_SANDBOX_MCP   # comma-separated MCP server names; forwarded to sbx create --mcp
CRABBOX_DOCKER_SANDBOX_KIT
```

`extraWorkspaces`, `mcp`, and `kit` environment values are comma-separated. Crabbox passes non-empty `mcp` entries through unchanged to repeatable `sbx create --mcp` flags.

## Lifecycle

1. `warmup` or `run` without `--id` creates a Crabbox-owned sandbox name such as
   `crabbox-my-app-1a2b3c`.
2. Crabbox runs `sbx create --name <name> ... shell <repo-root>` and records a
   local claim as `dsbx_<sandbox-name>`.
3. `run` executes through `sbx exec --workdir <repo-root> <sandbox-name> <cmd>`
   by default, matching the workspace path passed to `sbx create shell`. Set
   `dockerSandbox.workdir` only when your template mounts the workspace
   somewhere else. Commands with shell operators, leading environment
   assignments, or `--shell` are wrapped as `sh -lc`.
   Crabbox forwards selected `--allow-env` / `--env-from-profile` values through
   a temporary `sbx exec --env-file` so values do not appear in local process
   arguments.
4. `list`, `status`, and `stop` intersect `sbx ls --json` with local Crabbox
   claims for `provider=docker-sandbox`. Sandboxes without a local Crabbox claim
   are not listed and cannot be stopped by Crabbox.
5. `crabbox ports` resolves a Crabbox-owned claim, then maps directly to
   `sbx ports` for listing, publishing, or unpublishing host port forwards.
6. `crabbox cp` resolves a Crabbox-owned claim, rewrites the special
   `SANDBOX:PATH` side to the real sandbox name, and then calls `sbx cp` for
   host-to-sandbox or sandbox-to-host copies.
7. When `dockerSandbox.clone` / `--docker-sandbox-clone` is enabled, Crabbox
   requires the current workspace to be the main checkout of a normal Git
   repository. Linked Git worktrees are rejected before `sbx create --clone`,
   matching Docker Sandbox upstream clone restrictions.
8. Fresh one-shot `run` calls in clone mode keep the newly created sandbox after
   a successful command, even without `--keep`, so unfetched in-sandbox commits
   are not discarded by automatic cleanup. Crabbox prints the matching manual
   cleanup command instead of calling `sbx rm --force` on success.
9. `crabbox stop --provider docker-sandbox <slug>` maps to
   `sbx rm --force <sandbox-name>` and removes the local claim. Crabbox does
   not map stop to `sbx stop`, because Crabbox stop means provider resource
   cleanup, while `sbx stop` retains sandbox state.

## Doctor

`crabbox doctor --provider docker-sandbox` is non-mutating. It runs:

- `sbx version`
- `sbx ls --json`
- `sbx diagnose --output json` as optional diagnostic enrichment

Common blockers:

| Symptom | Action |
| --- | --- |
| `sbx` not found | Install the standalone Docker Sandbox CLI or set `dockerSandbox.cliPath`. |
| Authentication failure | Run `sbx login` and retry. |
| Virtualization, KVM, or hypervisor error | Fix host virtualization prerequisites for Docker Sandboxes. |
| Malformed `sbx ls --json` output | Upgrade or inspect the installed `sbx` CLI; Crabbox parses common array and object wrapper shapes. |

## Capabilities

- Target: Linux.
- Kind: delegated-run.
- Coordinator: never. Docker Sandbox always runs direct from the CLI.
- Aliases: none.
- SSH, desktop, browser, code-server, Tailscale, Actions hydration, VNC, and
  Crabbox rsync are not supported in v1. Inherited global SSH config is ignored
  for delegated `sbx exec` runs.
- `--lease-output` is supported for run cleanup metadata; the cleanup command
  points back to `crabbox stop --provider docker-sandbox <slug>`.
- `crabbox ports` and `crabbox cp` are supported for Crabbox-owned Docker
  Sandbox leases only.
- Agent: `shell` only. Other `dockerSandbox.agent` values are rejected until
  Crabbox has a stable non-shell agent contract.
- MCP attachments: supported as create-time passthrough. Crabbox forwards
  `dockerSandbox.mcp` / `--docker-sandbox-mcp` / `CRABBOX_DOCKER_SANDBOX_MCP`
  entries to repeatable `sbx create --mcp` flags without additional runtime
  discovery semantics.
- Forwarded env: supported through `sbx exec --env-file`.

## Safety Notes

- Crabbox never passes Docker credentials as command-line arguments.
- Docker Sandbox is inside the runtime trust boundary for this provider:
  `sbx exec` receives the command, workspace path, and any environment values
  explicitly selected by Crabbox env-forwarding rules.
- Crabbox-owned controls are provider selection, config validation, env
  allowlist selection, diagnostic redaction, temporary env-file creation and
  cleanup, local process-argument avoidance for forwarded values, and local
  claim filtering.
- Docker-owned controls are the `sbx` client/server contract, account
  authentication, command transport, microVM isolation, virtualization
  prerequisites, and sandbox-internal Docker runtime.
- `list`, `status`, and `stop` operate only on local Crabbox claims for
  `provider=docker-sandbox`.
- `ports` and `cp` also operate only on local Crabbox claims for
  `provider=docker-sandbox`; they do not cross into user-created sandboxes.
- `--docker-sandbox-clone` requires the main checkout of a normal Git
  repository before Crabbox calls `sbx create --clone`; linked Git worktrees
  are rejected.
- Successful one-shot clone-mode runs keep newly created sandboxes by default so
  unfetched in-sandbox commits are preserved until you run `crabbox stop`.
- Forwarded environment values are written to a temporary local env file and
  passed with `sbx exec --env-file`; Crabbox does not place selected values in
  local `sbx exec` process arguments, and the temporary file is removed after
  the run returns.
- The official Docker Sandboxes docs describe direct workspace mounts as part
  of the sandbox trust model and recommend stored secrets over raw environment
  variables. Crabbox's `--allow-env` forwarding remains an explicit operator
  choice, and Docker Sandbox remains inside the runtime trust boundary for
  those forwarded values.

## Live Smoke

When the host has a usable Docker Sandbox provider configuration, run:

```sh
scripts/live-docker-sandbox-smoke.sh
```

The script builds `bin/crabbox`, runs `crabbox doctor --provider docker-sandbox`
as the authoritative preflight, confirms the doctor output includes
`sbx_version`, creates a unique Crabbox-owned sandbox, runs `echo ok` and `pwd`,
lists it, and then stops it. The doctor step honors `dockerSandbox.cliPath`,
`--docker-sandbox-cli`, and `CRABBOX_DOCKER_SANDBOX_CLI`, so the smoke script
works even when `sbx` is not on `PATH`. If the provider is missing, logged out,
or blocked by host prerequisites, the script reports `environment_blocked` with
the failing command. If Docker Sandbox capacity or account quota blocks the run,
the script reports `quota_blocked`. If the doctor command succeeds but does not
emit the expected version diagnostic, the script reports `diagnostic_only` and
does not claim live success.

Use this matrix when proving a fresh install or upgrade:

| Proof row | Command or coverage | Expected result |
| --- | --- | --- |
| Fresh local CLI build | `go build -trimpath -o bin/crabbox ./cmd/crabbox` | `bin/crabbox` builds without generated files in git. |
| Provider discovery | `bin/crabbox providers --json` | Includes `docker-sandbox`, family `docker-sandbox`, kind `delegated-run`, target `linux`, coordinator `never`, and feature `run-session`. |
| Config upgrade surface | `crabbox config show --json` | Includes `dockerSandbox.cliPath`, `agent`, runtime sizing, `clone`, `workdir`, `extraWorkspaces`, `mcp`, and `kit`. |
| Alternate `sbx` path | `CRABBOX_DOCKER_SANDBOX_CLI=/path/to/sbx scripts/live-docker-sandbox-smoke.sh` | Doctor honors the configured CLI path instead of requiring `sbx` on `PATH`. |
| Missing CLI or login | `crabbox doctor --provider docker-sandbox` | Fails with guidance to install/configure `sbx` or run `sbx login`. |
| Host/runtime blockers | `crabbox doctor --provider docker-sandbox` | Surfaces virtualization, hypervisor, KVM, control-plane, quota, or capacity blockers without claiming live success. |
| Clone mode | `crabbox warmup --provider docker-sandbox --docker-sandbox-clone ...` | Requires the main checkout of a normal Git repository workspace before calling `sbx create --clone`; linked Git worktrees are rejected. |
| Unsupported delegated flags | Docker Sandbox run flags and config validation | Rejects unsupported agent, desktop, Tailscale, `--class`, and `--type` surfaces clearly while allowing create-time MCP passthrough. |
| Live lifecycle | `scripts/live-docker-sandbox-smoke.sh` | Prints `classification=live_sbx_smoke_passed ... cleanup=complete` only after create, run, list, and stop complete. |
| Trust boundary handoff | `scripts/docker-sandbox-trust-boundary-smoke.sh` | Uses a fake `sbx` at the process boundary to prove Crabbox sends the workspace path, user command, and `--env-file`, while the forwarded value stays out of local argv and is present only in the env-file handoff. |

Related docs:

- [Provider reference](README.md)
- [Local Container](local-container.md)
- [Provider backends](../provider-backends.md)
- [run](../commands/run.md)
- [ports](../commands/ports.md)
- [cp](../commands/cp.md)
- [stop](../commands/stop.md)
