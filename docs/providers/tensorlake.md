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
crabbox run --provider tensorlake -- pnpm test
crabbox run --provider tensorlake --id blue-lobster --shell 'pnpm install && pnpm test'
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
  image: ""              # ubuntu-minimal default; pin a registered image otherwise
  snapshot: ""           # snapshot ID to restore from (alternative to image)
  organizationId: ""
  projectId: ""
  namespace: ""
  workdir: /workspace/crabbox  # absolute path; sync target and -w for exec
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
--tensorlake-workdir
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

Run-time environment forwarding uses the normal Crabbox allowlist:

```sh
crabbox run --provider tensorlake --allow-env API_TOKEN -- printenv API_TOKEN
crabbox run --provider tensorlake --env-from-profile ~/.my-live.profile --allow-env API_TOKEN -- npm test
```

Crabbox prints only redacted presence/length metadata. For Tensorlake it writes
the allowed values to a temporary local shell profile, uploads that file into
`/tmp`, sources it for the command, and removes both copies best-effort after
the run. Values are not placed on the local `tensorlake` process argv.

## Lifecycle

1. `warmup` / `run` (without `--id`) generates a Crabbox-owned sandbox name
   (`crabbox-<repo-slug>-<random6>`) and runs `tensorlake sbx create` with the
   configured CPU/memory/disk/image/snapshot. The Tensorlake-assigned sandbox
   ID is captured from stdout and used as the canonical identifier.
2. The local lease is stored as `tlsbx_<sandbox-id>` with a friendly slug.
3. By default `run` archive-syncs the working tree: `git ls-files`-driven
   manifest → `tar -czf` locally → `tensorlake sbx cp` upload to
   `/tmp/crabbox-sync-*.tgz` → `tensorlake sbx exec -- bash -lc 'tar -xzf …'`
   into the configured workdir. Pass `--no-sync` to skip the archive step.
4. The user command runs via `tensorlake sbx exec -w <workdir> <id> -- <cmd>`,
   streaming stdout/stderr back through Crabbox.
5. On release the sandbox is terminated via `tensorlake sbx terminate <id>`
   unless `--keep` was set. `--keep-on-failure` retains newly-created failed
   sandboxes and prints a rerun/stop hint.

## Capabilities

- SSH: no (Tensorlake exposes `tensorlake sbx ssh` directly; Crabbox does not
  proxy it).
- Crabbox sync: yes — gzipped tar uploaded via `tensorlake sbx cp` and
  extracted in-sandbox.
- Provider sync: no separate Tensorlake sync command.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- `--sync-only`, `--checksum`, `--force-sync-large`, `--capture-stdout`, and
  `--download` are rejected because Tensorlake doesn't expose Crabbox's
  rsync semantics. Use `--no-sync` plus an explicit `--id` if you've already
  primed the sandbox.
- `--shell` wraps the command as `bash -lc '<joined args>'` before passing it
  to `tensorlake sbx exec`. Plain commands containing shell metacharacters
  (`&&`, `|`, `>`, etc.) or leading `KEY=VALUE` env assignments are also
  auto-wrapped.
- Forwarded environment values are written to a temporary in-sandbox profile
  for the duration of the command. Avoid forwarding broad wildcard allowlists
  unless you trust the sandbox and command.
- `tensorlake.workdir` must be absolute (default `/workspace/crabbox`). It's
  used as both the sync target and the `-w` working directory for exec.
- IDs accepted by `--id` and `stop`: Crabbox slugs and `tlsbx_<sandbox-id>`
  lease IDs that have a local Crabbox claim. Sandboxes without a local claim
  are rejected (matches the islo/Crabbox-owned-only safety pattern).
- The `tensorlake` CLI authenticates from `TENSORLAKE_API_KEY` in env; never
  pass `--api-key` on the command line because process listings expose argv.

Related docs:

- [Provider backends](../provider-backends.md)
