# Orgo Provider

Read when:

- choosing `provider: orgo` (alias `orgo-ai`);
- running a command on an [Orgo](https://www.orgo.ai/) cloud computer;
- changing `internal/providers/orgo`.

Orgo provides Linux cloud computers with an HTTP control API. Crabbox uses the
REST API directly: it creates a workspace when needed, creates a computer, runs
commands through `POST /computers/{id}/bash`, and deletes resources on cleanup.

This is a delegated-run provider. Crabbox does not provision an SSH lease, does
not rsync the local workspace, and does not use the coordinator broker.

## When to use

Use Orgo when you want a managed Linux desktop/computer surface with API-driven
shell execution and fast startup. Pick an SSH-lease provider such as AWS,
Hetzner, Daytona, or Static SSH when you need Crabbox-managed SSH, rsync,
Actions hydration, or interactive `crabbox ssh`.

Orgo is Linux-only in Crabbox. Desktop, browser, code-server, normal SSH,
Actions hydration, and brokered coordinator routing are not available through
this adapter.

## Commands

```sh
crabbox run    --provider orgo --no-sync -- echo crabbox-orgo-ok
crabbox warmup --provider orgo --slug orgo-smoke
crabbox run    --provider orgo --id orgo-smoke --no-sync -- uname -a
crabbox status --provider orgo --id orgo-smoke --wait
crabbox stop   --provider orgo orgo-smoke
crabbox list   --provider orgo
crabbox doctor --provider orgo
```

Without `--id`, `run` creates a new computer, runs the command, then deletes the
computer unless `--keep` is set. With `--id <computer-id-or-slug>`, it runs the
command on the existing computer and leaves it running.

`warmup` creates a persistent computer and records a local Crabbox slug so later
commands can use `--id <slug>`.

## Auth

Resolve the Orgo API key in this precedence:

1. `CRABBOX_ORGO_API_KEY` — explicit per-run override, CI-friendly.
2. `orgo.apiKey` — trusted user or explicit `CRABBOX_CONFIG` value; repository
   config cannot supply provider secrets.
3. `ORGO_API_KEY` — canonical Orgo environment variable.

The API key is never exposed as a CLI flag; do not pass secrets on the command
line.

## Config

```yaml
provider: orgo
target: linux
orgo:
  apiBase: https://www.orgo.ai/api
  workspaceID: 550e8400-e29b-41d4-a716-446655440000
  ramGB: 4
  cpus: 1
  diskGB: 8
  resolution: 1280x720x24
```

If `workspaceID` is omitted, Crabbox creates a temporary workspace for each new
computer and deletes that workspace during cleanup. For workspace-scoped Orgo
keys, set `workspaceID` because scoped keys cannot create workspaces.

Provider flags, each overriding the matching `orgo.*` config key:

```text
--orgo-api-base
--orgo-workspace-id
--orgo-ram
--orgo-cpu
--orgo-disk
--orgo-resolution
```

Environment overrides:

- `CRABBOX_ORGO_API_BASE`, then `ORGO_API_BASE_URL`
- `CRABBOX_ORGO_WORKSPACE_ID`, then `ORGO_WORKSPACE_ID`
- `CRABBOX_ORGO_RAM_GB`
- `CRABBOX_ORGO_CPUS`
- `CRABBOX_ORGO_DISK_GB`
- `CRABBOX_ORGO_RESOLUTION`

Defaults: API base `https://www.orgo.ai/api`, 4 GB RAM, 1 CPU, 8 GB disk,
and resolution `1280x720x24`.

Repository config cannot redirect an inherited Orgo API key. Set
`CRABBOX_ORGO_API_BASE` or pass `--orgo-api-base` to explicitly approve a
custom credential destination. Non-loopback API endpoints must use HTTPS.

## Lifecycle

`crabbox run`:

1. With `--id <computer-id-or-slug>`, resolve the computer and call the bash
   endpoint.
2. Otherwise, create a workspace when needed, create a computer, claim a local
   Crabbox slug, run the command, then delete the computer and any temporary
   workspace unless `--keep` is set.

`--keep-on-failure` preserves a newly created computer when the command fails,
so it can be inspected with Orgo tooling before manual cleanup.

## Capabilities

- SSH: no — Orgo command execution is HTTP API delegated.
- Crabbox sync: no — use `--no-sync`.
- Desktop / browser / code: no through Crabbox flags.
- Actions hydration: no.
- Coordinator: no — direct CLI-to-Orgo API calls only.

## Live Smoke

Keep the API key in the environment:

```sh
export CRABBOX_ORGO_API_KEY=your-orgo-api-key
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox doctor --provider orgo
bin/crabbox run --provider orgo --no-sync -- echo crabbox-orgo-ok
```

Or run the shared live provider harness:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=orgo scripts/live-smoke.sh
```

Related docs:

- [Provider reference](README.md)
- [Provider authoring](../features/provider-authoring.md)
- [Orgo API reference](https://docs.orgo.ai/api-reference/introduction)
