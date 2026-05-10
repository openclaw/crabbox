# Tensorlake Provider

Read when:

- choosing `provider: tensorlake`;
- configuring Tensorlake sandbox image, snapshot, sizing, or organization;
- changing `internal/providers/tensorlake`.

Tensorlake is a delegated run provider. Crabbox shells out to the `tensorlake`
CLI (`tensorlake sbx ...`) for sandbox lifecycle and command exec. Tensorlake
owns the Firecracker MicroVM and command transport; Crabbox owns local config,
repo claims, slugs, timing summaries, and normalized list/status rendering.

## When To Use

Use Tensorlake when the remote sandbox should be a Tensorlake Firecracker
MicroVM and command execution should happen through `tensorlake sbx exec`. Use
AWS, Hetzner, Static SSH, or Daytona when you need Crabbox-native SSH access.

## Prerequisites

- The `tensorlake` CLI must be on `PATH` (or pointed at via
  `--tensorlake-cli` / `tensorlake.cliPath`). Install it via
  `pip install tensorlake` — the wheel ships a Rust `tensorlake` binary at
  `<env>/bin/tensorlake`.
- A Tensorlake API key. Crabbox passes it through to the CLI via the
  `TENSORLAKE_API_KEY` environment variable; it is never placed on `argv`.

## Commands

```sh
crabbox warmup --provider tensorlake --tensorlake-image <image>
crabbox run --provider tensorlake --no-sync -- pnpm test
crabbox run --provider tensorlake --no-sync --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider tensorlake --id blue-lobster
crabbox stop --provider tensorlake blue-lobster
```

## Auth

```sh
export TENSORLAKE_API_KEY=tl_apiKey_...
```

`TENSORLAKE_API_URL` (or `tensorlake.apiUrl`) can override the default
`https://api.tensorlake.ai`. `TENSORLAKE_ORGANIZATION_ID` and
`TENSORLAKE_PROJECT_ID` select the org/project when your account spans more
than one.

## Config

```yaml
provider: tensorlake
target: linux
tensorlake:
  apiUrl: https://api.tensorlake.ai
  cliPath: tensorlake
  image: ""             # required for new sandboxes when no snapshot is given
  snapshot: ""          # snapshot ID to restore from (alternative to image)
  organizationId: ""
  projectId: ""
  namespace: ""
  cpus: 1.0
  memoryMB: 1024
  diskMB: 10240
  timeoutSecs: 0
  noInternet: false
```

Provider flags:

```text
--tensorlake-api-url
--tensorlake-cli
--tensorlake-image
--tensorlake-snapshot
--tensorlake-organization-id
--tensorlake-project-id
--tensorlake-namespace
--tensorlake-cpus
--tensorlake-memory-mb
--tensorlake-disk-mb
--tensorlake-timeout-secs
--tensorlake-no-internet
```

Environment overrides (`CRABBOX_TENSORLAKE_*` and the corresponding
`TENSORLAKE_*` variables read by the CLI itself) take precedence over file
config. The API key is sourced from `CRABBOX_TENSORLAKE_API_KEY` or
`TENSORLAKE_API_KEY`.

## Lifecycle

1. `warmup` / `run` (without `--id`) generates a Crabbox-owned sandbox name
   (`crabbox-<repo-slug>-<random6>`) and runs `tensorlake sbx create` with the
   configured CPU/memory/disk/image/snapshot.
2. The local lease is stored with the `tlsbx_` prefix and a friendly slug.
3. `run` invokes `tensorlake sbx exec <name> -- <command>` and streams
   stdout/stderr back through Crabbox.
4. On release the sandbox is terminated via `tensorlake sbx terminate <name>`
   unless `--keep` was set.

## Capabilities

- SSH: no (Tensorlake exposes `tensorlake sbx ssh` directly; Crabbox does not
  proxy it yet).
- Crabbox sync: not yet — v1 requires `--no-sync`. Archive sync via
  `tensorlake sbx cp` plus an in-sandbox `tar -xzf` is the planned follow-up.
- Provider sync: no.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- `--sync-only`, `--checksum`, `--force-sync-large`, `--capture-stdout`, and
  `--download` are rejected by core for delegated providers, and `run` further
  requires `--no-sync` until archive sync lands.
- `--shell` wraps the command as `bash -lc '<joined args>'` before passing it
  to `tensorlake sbx exec`.
- IDs accepted by `--id` and `stop`: Crabbox slugs, `tlsbx_<name>` lease IDs,
  and Crabbox-created sandbox names. Sandboxes whose name does not start with
  `crabbox-` are rejected to avoid touching unrelated workloads in the
  Tensorlake account.
- The `tensorlake` CLI authenticates from `TENSORLAKE_API_KEY` in env; never
  pass `--api-key` on the command line because process listings expose argv.

Related docs:

- [Provider backends](../provider-backends.md)
