# Tensorlake Provider

Read when:

- choosing `provider: tensorlake` (aliases: `tl`, `tensorlake-sbx`);
- configuring the Tensorlake sandbox image, snapshot, sizing, organization, or
  project;
- changing `internal/providers/tensorlake`.

Tensorlake is a delegated run provider (provider family `firecracker`). Crabbox
shells out to the `tensorlake` CLI (`tensorlake sbx ...`) for sandbox lifecycle
and command execution. Tensorlake owns the Firecracker MicroVM and the command
transport; Crabbox owns local config, repo claims, sync manifests and
guardrails, slugs, timing summaries, and normalized list/status rendering.

## When To Use

Use Tensorlake when the remote sandbox should be a Tensorlake Firecracker
MicroVM and commands should run through `tensorlake sbx exec`. Use AWS, Hetzner,
Static SSH, or Daytona when you need Crabbox-native SSH access, since Tensorlake
does not expose SSH through Crabbox.

## Prerequisites

- The `tensorlake` CLI must be on `PATH`, or pointed at with `--tensorlake-cli`
  / `tensorlake.cliPath`. Crabbox invokes `tensorlake sbx create`, `exec`, `cp`,
  `describe`, `ls`, and `terminate`.
- A Tensorlake API key. Crabbox passes it to the CLI through the
  `TENSORLAKE_API_KEY` environment variable; it is never placed on the command
  line.

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

The API key is read from `CRABBOX_TENSORLAKE_API_KEY` or `TENSORLAKE_API_KEY`.
`TENSORLAKE_API_URL` (or `tensorlake.apiUrl`) overrides the default
`https://api.tensorlake.ai`. `TENSORLAKE_ORGANIZATION_ID` and
`TENSORLAKE_PROJECT_ID` select the org and project when your account spans more
than one; the namespace also falls back to `INDEXIFY_NAMESPACE`.

## Config

```yaml
provider: tensorlake
target: linux
tensorlake:
  apiUrl: https://api.tensorlake.ai
  cliPath: tensorlake
  image: ""              # CLI default image when empty; pin a registered image otherwise
  snapshot: ""           # snapshot ID to restore from (alternative to image)
  organizationId: ""
  projectId: ""
  namespace: ""
  workdir: /workspace/crabbox  # absolute path; sync target and -w for exec
  cpus: 1.0
  memoryMB: 1024
  diskMB: 10240
  timeoutSecs: 0         # sandbox lifetime timeout; 0 leaves it to Tensorlake
  noInternet: false      # block outbound internet from the sandbox
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

Each flag has a matching `CRABBOX_TENSORLAKE_*` environment override (for
example `CRABBOX_TENSORLAKE_IMAGE`, `CRABBOX_TENSORLAKE_CPUS`,
`CRABBOX_TENSORLAKE_NO_INTERNET`). The API URL, organization, project, and
namespace are passed to the CLI as `--api-url`, `--organization`, `--project`,
and `--namespace`.

### Runtime environment forwarding

Forwarding uses the normal Crabbox allowlist:

```sh
crabbox run --provider tensorlake --allow-env API_TOKEN -- printenv API_TOKEN
crabbox run --provider tensorlake --env-from-profile ~/.my-live.profile --allow-env API_TOKEN -- npm test
```

Crabbox prints only redacted presence/length metadata for the forwarded names.
The allowed values are written to a temporary local shell profile, uploaded
into the sandbox under `/tmp`, sourced for the duration of the command, and
removed (local and remote) best-effort afterward. Values are never placed on the
local `tensorlake` process argv.

## Lifecycle

1. `warmup` or `run` without `--id` generates a Crabbox-owned sandbox name
   (`crabbox-<repo-slug>-<random6>`) and runs `tensorlake sbx create` with the
   configured CPU, memory, disk, timeout, image, and snapshot. The
   Tensorlake-assigned sandbox ID is parsed from stdout and used as the
   canonical identifier.
2. The local lease is stored as `tlsbx_<sandbox-id>` with a friendly slug and a
   repo claim.
3. By default `run` archive-syncs the working tree: a `git ls-files`-driven
   manifest is packed into a gzipped tar locally, uploaded with
   `tensorlake sbx cp` to `/tmp/crabbox-sync-*.tgz`, and extracted into the
   configured workdir. Pass `--no-sync` to skip the archive step (the workdir is
   still created).
4. The command runs via `tensorlake sbx exec -w <workdir> <id> -- <cmd>`,
   streaming stdout and stderr back through Crabbox.
5. On release the sandbox is terminated with `tensorlake sbx terminate <id>`
   unless `--keep` was set. `--keep-on-failure` retains a newly created sandbox
   after a failed run and prints a rerun/stop hint.

## Capabilities

- SSH: not driven by Crabbox. The `tensorlake` CLI offers its own
  `tensorlake sbx ssh`, but Crabbox does not proxy it.
- Crabbox sync: yes — gzipped tar uploaded via `tensorlake sbx cp` and extracted
  in-sandbox.
- Provider sync: no separate Tensorlake sync command.
- URL bridge: no — Tensorlake does not expose a per-sandbox ingress URL through
  Crabbox today.
- Desktop / browser / code: no Crabbox VNC or code-server surface.
- Actions hydration: no.
- Coordinator: no — Tensorlake always runs direct from the CLI and never goes
  through the broker.

## Gotchas

- `--sync-only` and `--checksum` are rejected because Tensorlake does not expose
  Crabbox's rsync semantics. Other transport-owning flags (such as local
  stdout/stderr captures, `--download`, `--artifact-glob`, and
  `--require-artifact`) are rejected by the core delegated-sync gate. Use
  `--no-sync` with an explicit `--id` if the sandbox is already primed.
- Large-sync guardrails still apply; pass `--force-sync-large` when a large
  archive sync is intentional.
- `--shell` wraps the command as `bash -lc '<joined args>'`. Plain commands that
  contain shell metacharacters (`&&`, `|`, `>`, etc.) or a leading `KEY=VALUE`
  assignment are auto-wrapped the same way.
- Forwarded environment values live in a temporary in-sandbox profile for the
  duration of the command. Avoid forwarding broad wildcard allowlists unless you
  trust the sandbox and command.
- `tensorlake.workdir` must be an absolute path (default `/workspace/crabbox`)
  and cannot be a broad system directory such as `/`, `/tmp`, or `/workspace`.
  It serves as both the sync target and the `-w` working directory for exec.
- IDs accepted by `--id` and `stop` are Crabbox slugs and `tlsbx_<sandbox-id>`
  lease IDs that have a local Crabbox claim. Sandboxes without a local claim are
  rejected (the same Crabbox-owned-only safety pattern as Islo).

Related docs:

- [Provider backends](../provider-backends.md)
