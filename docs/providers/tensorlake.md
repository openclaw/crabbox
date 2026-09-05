# Tensorlake Provider

Read when:

- choosing `provider: tensorlake` (aliases: `tl`, `tensorlake-sbx`);
- configuring the Tensorlake sandbox image, snapshot, sizing, organization, or
  project;
- changing `internal/providers/tensorlake`.

[Tensorlake](https://tensorlake.ai) is a delegated run provider (provider family
`firecracker`). Crabbox
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
  `describe`, `ls`, `terminate`, and `whoami --output json`. Use a current native
  CLI with API-key scope introspection and exact sandbox details (verified with
  Tensorlake CLI 0.5.118).
- A Tensorlake API key from [cloud.tensorlake.ai](https://cloud.tensorlake.ai).
  Crabbox passes it to the CLI through the `TENSORLAKE_API_KEY` environment
  variable; it is never placed on the command line.

## Commands

```sh
crabbox warmup --provider tensorlake --tensorlake-image <image>
crabbox run --provider tensorlake -- pnpm test
crabbox run --provider tensorlake --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider tensorlake --id blue-lobster
crabbox stop --provider tensorlake blue-lobster
```

Tensorlake publishes a Crabbox-ready public image, `tl-crabbox`
(`--tensorlake-image tl-crabbox`): the standard Ubuntu base plus a writable
`/workspace` (Crabbox's default workdir) and pnpm preinstalled. On the stock
Tensorlake images, commands run as `tl-user`, which cannot create `/workspace`;
either pin `tl-crabbox` or set `tensorlake.workdir` to a user-writable path such
as `/home/tl-user/crabbox`.

Ordinary nonzero native CLI exits remain command exits. Transport, cancellation,
deadline, and output errors instead fail the run with exit code 1 and the matching
timing status, even when the local process also reports a nonzero exit code.
An already observed command exit is not replaced by later cancellation. Native
CLI diagnostic exits cannot be distinguished from remote workload exits without
stronger evidence from the native protocol.

## Auth

```sh
export TENSORLAKE_API_KEY=tl_apiKey_...
```

The API key is read from `CRABBOX_TENSORLAKE_API_KEY` or `TENSORLAKE_API_KEY`.
`TENSORLAKE_API_URL` (or `tensorlake.apiUrl`) overrides the default
`https://api.tensorlake.ai`. `TENSORLAKE_ORGANIZATION_ID` and
`TENSORLAKE_PROJECT_ID` select the org and project when your account spans more
than one; the namespace also falls back to `INDEXIFY_NAMESPACE`.

Crabbox pins the effective API URL and namespace on every native invocation
(defaults: `https://api.tensorlake.ai` and `default`). The API key's introspected
organization and project must agree with any explicit selectors. Rotating a
key within the same scope is supported; changing accounts, endpoints, or
namespaces does not retarget existing leases. Native login/config defaults and
inherited PAT, debug, or Git-token overrides cannot replace that binding.
Scope probes capture and discard native credential prefixes; neither keys nor
their prefixes are stored in ownership claims or printed by these probes.

## Config

```yaml
provider: tensorlake
target: linux
tensorlake:
  apiUrl: https://api.tensorlake.ai
  cliPath: tensorlake
  image: tl-crabbox      # Crabbox-ready public image; "" uses the CLI default
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
   durable repo claim bound to the exact sandbox ID, API endpoints, API-key
   organization/project, requested namespace, and reported sandbox namespace.
3. By default `run` archive-syncs the working tree: a `git ls-files`-driven
   manifest is packed into a gzipped tar locally, uploaded with
   `tensorlake sbx cp` to `/tmp/crabbox-tensorlake-sync-*.tgz`, and extracted into
   the configured workdir. The complete archive is checked and built before
   fresh allocation. Delete-sync stages extraction before replacing the existing
   workspace; non-delete sync merges into it. A bounded cleanup attempt removes
   partial uploads and staging directories even when transfer fails or is
   canceled, warning on cleanup failure without replacing the original outcome.
   Pass `--no-sync` to skip the archive step (the workdir is still created).
4. The command runs via `tensorlake sbx exec -w <workdir> <id> -- <cmd>`,
   streaming stdout and stderr back through Crabbox.
5. On release the original claim and provider scope are rechecked while claim
   changes are fenced, then the sandbox is terminated with
   `tensorlake sbx terminate <id>` unless `--keep` was set. The claim is removed
   only after exact sandbox metadata confirms `terminated`. Failed or ambiguous
   confirmation retains the claim; retrying stop can confirm archived terminated
   metadata without issuing another termination. `--keep-on-failure` retains a
   newly created sandbox after a failed run and prints a rerun/stop hint.

Reuse, one-shot teardown, and failed-acquisition rollback use the same exact
identity checks. A changed claim blocks stale cleanup. Native control calls are
bounded; authentication, malformed output, and missing metadata fail closed.
An empty list or a `not found` response alone is not deletion proof.

### Legacy and uncertain ownership

Older provider-only claims cannot prove account or resource ownership. Crabbox
will not reuse, stop, or silently adopt them, including with `--reclaim`. Preserve
the claim and inspect the exact sandbox with the native CLI using the intended
API endpoint, namespace, and API key. After independently verifying ownership,
an operator can terminate it with `tensorlake sbx terminate <sandbox-id>` and
confirm its terminated state with `tensorlake sbx describe <sandbox-id>`. Create
a fresh Crabbox lease for future managed runs; do not edit an old claim to add
guessed binding fields. Uncertain creation reports the generated sandbox name
or ID for this same manual inspection path.

The local claim fence coordinates Crabbox processes, not external Tensorlake
administrators. Native termination has no atomic expected-account condition;
Crabbox verifies scope immediately before it and never falls back from a
canonical sandbox ID to a mutable name.

`run --lease-output` records the Tensorlake lease, reuse/retention state, and
matching `crabbox stop --provider tensorlake --id ...` cleanup command for
orchestrators that need to inspect or clean up retained sandboxes later.

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
- `--shell` wraps the command as `bash -lc '<joined args>'`. Inferred shell
  source and unquoted operators or leading assignments use the same shell.
  Literal profile arguments stay data, including assignment-shaped executable
  names. Adding an environment profile does not reinterpret those arguments
  as shell syntax; a single inferred source string remains executable source.
- Forwarded environment values live in a temporary in-sandbox profile for the
  duration of the command, with an unpredictable per-operation name. The private
  local source is removed after upload returns, including partial-upload failure.
  Cleanup is attempted after upload failure or cancellation with a fresh
  30-second budget, but refuses remote mutation if the original claim or provider
  scope no longer matches. Cleanup failures warn without replacing the original
  outcome. The command does not run if sourcing its profile fails. Remote file
  permissions remain governed by native `sbx cp`, not a new Crabbox permission
  guarantee. Avoid forwarding broad wildcard allowlists unless you trust the
  sandbox and command.
- `tensorlake.workdir` must be an absolute path (default `/workspace/crabbox`)
  and cannot be a broad system directory such as `/`, `/tmp`, or `/workspace`.
  It serves as both the sync target and the `-w` working directory for exec. The
  default requires a writable `/workspace`; the `tl-crabbox` image provides one,
  otherwise point `workdir` at a user-writable path.
- IDs accepted by `--id` and `stop` are Crabbox slugs, `tlsbx_<sandbox-id>`
  lease IDs, and canonical 21-character lowercase-alphanumeric sandbox IDs
  that have an exact bound local Crabbox claim. Sandboxes without such a claim
  are rejected; an exact ID never falls back to a matching slug.

Related docs:

- [Run your test suite with Crabbox](https://docs.tensorlake.ai/sandboxes/crabbox) — Tensorlake's walkthrough for running Crabbox on Tensorlake sandboxes.
- [Provider backends](../provider-backends.md)
