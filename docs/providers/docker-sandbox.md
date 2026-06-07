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

Crabbox currently depends on these `sbx` commands and flags:

- `sbx version`
- `sbx ls --json`
- `sbx diagnose --output json` as optional doctor enrichment
- `sbx create --name <name> [--template ...] [--cpus ...] [--memory ...] [--clone] shell <repo-root> [extra-workspaces...]`
- `sbx exec [--workdir ...] [--env-file ...] <sandbox-name> <command...>`
- `sbx rm --force <sandbox-name>`

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
  mcp: []            # reserved for future sbx support; non-empty values are rejected today
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
--docker-sandbox-mcp   # currently rejected until sbx create supports MCP attachments
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
CRABBOX_DOCKER_SANDBOX_MCP   # currently rejected until sbx create supports MCP attachments
CRABBOX_DOCKER_SANDBOX_KIT
```

`extraWorkspaces` and `kit` environment values are comma-separated. `mcp` is reserved for future `sbx` support and non-empty values are rejected today.

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
5. `crabbox stop --provider docker-sandbox <slug>` maps to
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
- Agent: `shell` only. Other `dockerSandbox.agent` values are rejected until
  Crabbox has a stable non-shell agent contract.
- MCP attachments: not supported yet. Crabbox rejects non-empty
  `dockerSandbox.mcp` / `--docker-sandbox-mcp` / `CRABBOX_DOCKER_SANDBOX_MCP`
  until `sbx create` exposes a supported MCP attachment flag.
- Forwarded env: supported through `sbx exec --env-file`.

## Safety Notes

- Crabbox never passes Docker credentials as command-line arguments.
- `list`, `status`, and `stop` operate only on local Crabbox claims for
  `provider=docker-sandbox`.
- `--docker-sandbox-clone` requires a normal Git repository workspace before
  Crabbox calls `sbx create --clone`.
- Forwarded environment values are written to a temporary local env file and
  passed with `sbx exec --env-file`; Crabbox does not place selected values in
  local `sbx exec` process arguments.

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
| Config upgrade surface | `crabbox config show --json` | Includes `dockerSandbox.cliPath`, `agent`, runtime sizing, `clone`, `workdir`, `extraWorkspaces`, and `kit`. |
| Alternate `sbx` path | `CRABBOX_DOCKER_SANDBOX_CLI=/path/to/sbx scripts/live-docker-sandbox-smoke.sh` | Doctor honors the configured CLI path instead of requiring `sbx` on `PATH`. |
| Missing CLI or login | `crabbox doctor --provider docker-sandbox` | Fails with guidance to install/configure `sbx` or run `sbx login`. |
| Host/runtime blockers | `crabbox doctor --provider docker-sandbox` | Surfaces virtualization, hypervisor, KVM, control-plane, quota, or capacity blockers without claiming live success. |
| Clone mode | `crabbox warmup --provider docker-sandbox --docker-sandbox-clone ...` | Requires a normal Git repository workspace before calling `sbx create --clone`. |
| Unsupported delegated flags | Docker Sandbox run flags and config validation | Rejects unsupported agent, MCP attachment, desktop, Tailscale, `--class`, and `--type` surfaces clearly. |
| Live lifecycle | `scripts/live-docker-sandbox-smoke.sh` | Prints `classification=live_sbx_smoke_passed ... cleanup=complete` only after create, run, list, and stop complete. |

Related docs:

- [Provider reference](README.md)
- [Local Container](local-container.md)
- [Provider backends](../provider-backends.md)
- [run](../commands/run.md)
- [stop](../commands/stop.md)
