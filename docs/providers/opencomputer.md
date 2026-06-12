# OpenComputer Provider

Read when:

- choosing `provider: opencomputer` (aliases: `oc`, `open-computer`);
- configuring the OpenComputer sandbox sizing, workdir, or API URL;
- changing `internal/providers/opencomputer`.

OpenComputer is a delegated run provider. Crabbox talks to the
[OpenComputer](https://app.opencomputer.dev) REST API directly (no `oc` CLI
process at runtime) for sandbox lifecycle, command execution, and file sync.
OpenComputer owns the Linux VM and the command transport; Crabbox owns local
config, repo claims, sync manifests and guardrails, slugs, timing summaries, and
normalized list/status rendering.

The API key travels only in the `X-API-Key` header and every payload (command
env, file content) travels in request bodies, so nothing sensitive is ever
placed on a process command line.

## When To Use

Use OpenComputer when the remote box should be an OpenComputer full Linux VM.
Use AWS, Hetzner, Static SSH, or Daytona when you need Crabbox-native SSH
access, since OpenComputer does not expose SSH through Crabbox.

## Prerequisites

An OpenComputer API key (`osb_...`). Prefer loading it from a secret manager or
prompting into the environment so the value never appears in shell history or
process arguments:

```sh
export CRABBOX_OPENCOMPUTER_API_KEY="$(
  python3 -c 'import getpass; print(getpass.getpass("OpenComputer API key: "))'
)"
```

Crabbox also reads an existing `oc` CLI config file automatically. The `oc`
binary is **not** required at runtime. `OPENCOMPUTER_API_KEY` is accepted as an
environment fallback.

## Commands

```sh
crabbox warmup --provider opencomputer
crabbox run --provider opencomputer -- pnpm test
crabbox run --provider opencomputer --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider opencomputer --id blue-lobster
crabbox stop --provider opencomputer blue-lobster
```

## Auth

Crabbox resolves the API key from, in order, `CRABBOX_OPENCOMPUTER_API_KEY`,
`OPENCOMPUTER_API_KEY`, then the `oc` CLI config file (`~/.oc/config.json`,
if already configured). It is sent only in the `X-API-Key` header, never
persisted in Crabbox config and never placed on argv. If no key is resolvable,
operations fail with a clear error.

Local lease claims are scoped to the normalized API URL and a random ownership
marker stored in both the local claim and the sandbox tags. The API key is
never stored. Before reusing or deleting a retained sandbox, Crabbox verifies
that both markers match using the dedicated sandbox-tags endpoint. This
prevents a claim from being applied to a sandbox at another endpoint or account
while allowing API-key rotation within the same account.

The API base URL defaults to `https://app.opencomputer.dev`. A trusted local
override can come from `--opencomputer-api-url`,
`CRABBOX_OPENCOMPUTER_API_URL`, `OPENCOMPUTER_API_URL`, or the `api_url` in the
local `oc` config. Repository YAML cannot set the API URL: that prevents a
checked-in config from redirecting an automatically loaded API key. Overrides
must use HTTPS and cannot contain userinfo, query parameters, or fragments;
plain HTTP is accepted only for `localhost` or a loopback IP during local
development.

## Config

```yaml
provider: opencomputer
target: linux
openComputer:
  workdir: /workspace/crabbox  # absolute path; sync target and exec cwd
  cpu: 0                       # vCPUs; service infers memory when set alone
  memoryMB: 0                  # memory; service infers CPU when set alone
  timeoutSecs: 0               # sandbox idle timeout; 0 leaves it to the default
  execTimeoutSecs: 3600        # command/sync-helper timeout
  burst: false                 # opt into best-effort burst capacity
```

Provider flags:

```text
--opencomputer-api-url
--opencomputer-workdir
--opencomputer-cpu
--opencomputer-memory-mb
--opencomputer-timeout-secs
--opencomputer-exec-timeout-secs
--opencomputer-burst
--opencomputer-forget-missing
```

Configuration flags have matching `CRABBOX_OPENCOMPUTER_*` environment
overrides (for example `CRABBOX_OPENCOMPUTER_WORKDIR`,
`CRABBOX_OPENCOMPUTER_CPU`, `CRABBOX_OPENCOMPUTER_EXEC_TIMEOUT_SECS`). The API
URL also reads `OPENCOMPUTER_API_URL`. `--opencomputer-forget-missing` is
deliberately CLI-only so stale-claim removal always requires explicit intent.

> **Sizing tiers.** When both `cpu` and `memoryMB` are set, they must form an
> allowed tier (for example `1/1024`, `1/4096`, `2/8192`, `4/16384`). When only
> one is set, OpenComputer infers the matching value. Leaving both at `0` uses
> the service default tier.

Set `openComputer.burst: true`, `CRABBOX_OPENCOMPUTER_BURST=true`, or pass
`--opencomputer-burst` to request best-effort burst capacity. Burst is disabled
by default, so normal on-demand or reserved capacity remains the standard path.
Burst sandboxes are alpha: filesystem state persists across infrastructure
restarts, but processes, memory, terminal sessions, and network connections may
restart. Use burst only for restart-tolerant runs.

### Environment forwarding

`--allow-env` / `--env-from-profile` are supported and never place values on the
command line: forwarded env is sent in the body of the exec request (`POST
/api/sandboxes/<id>/exec/run`, the `envs` field), so values never appear in any
process argv.

```sh
crabbox run --provider opencomputer --allow-env API_TOKEN -- printenv API_TOKEN
```

## Lifecycle

1. `warmup` or `run` without `--id` calls `POST /api/sandboxes` with the
   configured timeout and sizing tier and `metadata` tagging the box as
   Crabbox-owned (`crabbox=true`, `crabbox-name=crabbox-<repo-slug>-<random6>`),
   then writes the random ownership marker as the `crabbox.claim` sandbox tag.
   The returned sandbox ID (`sb-...`) is the canonical identifier.
2. The local lease is stored as `ocbx_<sandbox-id>` with a friendly slug and a
   repo claim.
3. By default `run` archive-syncs the working tree: a `git ls-files`-driven
   manifest is packed into a gzipped tar locally, uploaded via the file API
   (`PUT /api/sandboxes/<id>/files?path=...`, content in the request body), and
   extracted into the configured workdir. `--no-sync` skips the archive step
   (the workdir is still created); `--sync-only` syncs and stops without running
   a command.
4. The command runs via `POST /api/sandboxes/<id>/exec/run` with `cwd` set to
   the workdir and `envs` carrying any forwarded env; the buffered stdout/stderr
   are streamed back and the remote exit code is mirrored.
5. On release the sandbox is deleted (`DELETE /api/sandboxes/<id>`) unless
   `--keep` was set. `--keep-on-failure` retains a newly created sandbox after a
   sync, workspace setup, or command failure and prints a rerun/stop hint.
   Best-effort cleanup calls are bounded to 15 seconds; a failed rollback
   reports the remote sandbox ID for manual cleanup in the OpenComputer console.
   A newly acquired sandbox already removed during automatic cleanup still
   clears its local claim. For an existing lease, a 404 is account-ambiguous and
   keeps the claim by default; pass `--opencomputer-forget-missing` to `stop`
   only after confirming the sandbox is gone in the intended account.

## Capabilities

- SSH: not driven by Crabbox.
- Crabbox sync: yes — gzipped tar uploaded via the file API (off-argv) and
  extracted in-sandbox. The archive-sync feature is advertised, so
  `--force-sync-large` and `--sync-only` are honored.
- Env forwarding: yes — off-argv, in the exec request body.
- Provider sync: no separate provider-side copy command is required.
- URL bridge: no — OpenComputer preview URLs are managed by the OpenComputer
  control plane rather than the Crabbox bridge plane (same honest-unsupported
  pattern as Modal and Tensorlake).
- Desktop / browser / code: no Crabbox VNC or code-server surface.
- Actions hydration: no.
- Coordinator: no — OpenComputer always runs direct against the API and never
  goes through the broker.

## Gotchas

- `--checksum` is rejected because OpenComputer does not expose Crabbox's rsync
  checksum semantics. `--sync-only` and `--force-sync-large` are supported.
- `--no-sync` only ensures the workdir exists. It never applies `sync.delete`,
  so reusing a retained sandbox does not erase its workspace.
- With `sync.delete`, Crabbox uploads and extracts into a sibling staging
  directory, then replaces the workdir only after extraction succeeds.
- `crabbox list` starts from local OpenComputer claims and fetches each remote
  status, so kept sandboxes remain visible after hibernation. A 404 remains
  visible as `missing-or-inaccessible` until explicitly forgotten. A malformed
  matching claim fails the command instead of silently hiding a retained
  sandbox.
- `status --wait` applies its wait timeout to in-flight API requests as well as
  polling sleeps.
- `warmup --actions-runner` is rejected because OpenComputer is a delegated
  execution provider, not an SSH runner host.
- Command and sync-helper requests use a 3600-second timeout by default instead
  of OpenComputer's 60-second API default. Override it with
  `openComputer.execTimeoutSecs` or
  `CRABBOX_OPENCOMPUTER_EXEC_TIMEOUT_SECS`.
- Large-sync guardrails still apply; pass `--force-sync-large` when a large
  archive sync is intentional.
- `--shell` wraps the command as `bash -lc '<joined args>'`. Plain commands that
  contain shell metacharacters (`&&`, `|`, `>`, etc.) or a leading `KEY=VALUE`
  assignment are auto-wrapped the same way.
- `exec/run` is buffered: stdout/stderr are returned when the command finishes
  rather than streamed line-by-line.
- `openComputer.workdir` must be an absolute path (default `/workspace/crabbox`)
  and cannot be a broad system directory such as `/`, `/tmp`, or `/workspace`.
  It is both the sync target and the exec working directory.
- IDs accepted by `--id` and `stop` are Crabbox slugs and `ocbx_<sandbox-id>`
  lease IDs that have a local Crabbox claim. Sandboxes without a local claim are
  rejected (the same Crabbox-owned-only safety pattern as Islo).
- Do not edit the reserved `crabbox.claim` sandbox tag. A missing or changed
  value fails closed for reuse and deletion; restore it from the local claim or
  clean up the sandbox explicitly in OpenComputer.

Related docs:

- [Provider backends](../provider-backends.md)
