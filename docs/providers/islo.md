# Islo Provider

Read when:

- choosing `provider: islo`;
- configuring the Islo sandbox image, sizing, snapshot, or gateway profile;
- changing `internal/providers/islo`.

Islo is a delegated-run provider. Crabbox uses the Islo Go SDK for sandbox
lifecycle (create, list, status, pause, resume) and calls the HTTP API directly
for delete (an empty-body `DELETE`), archive upload, shares, and command
output — reading a Server-Sent Events stream from the
`POST /sandboxes/{name}/exec/stream` endpoint. Islo owns sandbox state and
command transport; Crabbox owns local config, repo claims, sync manifests and
guardrails, slugs, timing summaries, and normalized
`list`/`status` rendering. There is no Crabbox SSH lease and no broker
coordinator — the CLI talks to Islo directly.

## When to use

Use Islo when the remote Linux sandbox should be owned by Islo and commands run
through Islo's API. Choose AWS, Hetzner, Static SSH, or Daytona instead when you
need Crabbox-managed SSH access to the box.

Islo is Linux-only. Desktop, browser, code, Actions hydration, and SSH-based run
options are not available.

## Commands

```sh
crabbox warmup --provider islo --islo-image docker.io/library/ubuntu:26.04
crabbox run --provider islo -- pnpm test
crabbox run --provider islo --id swift-crab --shell 'pnpm install && pnpm test'
crabbox status --provider islo --id swift-crab --wait
crabbox pause --provider islo swift-crab
crabbox resume --provider islo swift-crab
crabbox stop --provider islo swift-crab
crabbox list --provider islo --json
```

`warmup` keeps the sandbox until an explicit `stop`. The lease ID, slug, or
Crabbox-created sandbox name printed by `warmup`/`run` can be passed to later
commands via `--id`.

If local claim state was intentionally lost, adopt a canonical Islo sandbox
before lifecycle mutation, then use its new claim normally:

```sh
crabbox run --provider islo --id isb_crabbox-repo-abcdef --reclaim --no-sync -- true
crabbox stop --provider islo --id isb_crabbox-repo-abcdef
```

Read-only status lookup can still use a canonical sandbox name without a claim.
Delete, pause, resume, SSH reuse, and delegated reuse cannot.

A sandbox name is an addressing convenience, not the sandbox's identity: the
immutable Islo sandbox `id` is. That Islo `id` is not what Crabbox's `--id`
flag takes — `--id` accepts a Crabbox lease id, a Crabbox-generated sandbox
name, or a slug, and rejects anything else with exit code 4. Existence is
decided by a get on the exact sandbox, never by the eventually consistent list
endpoint. See
[identity and absence semantics](../features/islo.md#identity-and-absence-semantics).

## Auth

```sh
export ISLO_API_KEY=ak_...
```

`CRABBOX_ISLO_API_KEY` is also accepted and takes precedence over
`ISLO_API_KEY`. Do not pass the key as a command-line argument.

`ISLO_BASE_URL` (or `CRABBOX_ISLO_BASE_URL`, or `islo.baseUrl`) overrides the
default API base URL `https://api.islo.dev`.

## Config

```yaml
provider: islo
target: linux
islo:
  baseUrl: https://api.islo.dev
  image: docker.io/library/ubuntu:26.04
  workdir: crabbox
  gatewayProfile: ""
  snapshotName: ""
  vcpus: 2
  memoryMB: 4096
  diskGB: 20
```

Provider flags (each overrides the matching `islo.*` config key):

```text
--islo-base-url
--islo-image
--islo-workdir
--islo-gateway-profile
--islo-snapshot-name
--islo-vcpus
--islo-memory-mb
--islo-disk-gb
```

Every key also reads a `CRABBOX_ISLO_*` environment variable, which takes
precedence over the config file: `CRABBOX_ISLO_BASE_URL`, `CRABBOX_ISLO_IMAGE`,
`CRABBOX_ISLO_WORKDIR`, `CRABBOX_ISLO_GATEWAY_PROFILE`,
`CRABBOX_ISLO_SNAPSHOT_NAME`, `CRABBOX_ISLO_VCPUS`, `CRABBOX_ISLO_MEMORY_MB`,
and `CRABBOX_ISLO_DISK_GB`.

The resolved defaults are kept in Crabbox config for display and override
compatibility, but the Islo create request omits implicit default `image`,
`vcpus`, `memoryMB`, and `diskGB` values so Islo can use the tenant's current
defaults. Values explicitly supplied through config, environment, or flags are
still sent, including values equal to the Crabbox defaults.

### Gateway profile

`--islo-gateway-profile` / `islo.gatewayProfile` accepts an Islo gateway
profile name or id. Crabbox passes the value opaquely in the sandbox create
request; the profile itself is created and managed on the Islo side and
configures Islo's own egress gateway for the sandbox. It is unrelated to the
Crabbox coordinator. When unset, the field is omitted from the create request
so Islo applies its own default.

### Workdir resolution

`--islo-workdir` / `islo.workdir` is a relative directory below `/workspace`
(default `crabbox`, so the workspace is `/workspace/crabbox`). Crabbox validates
this before sandbox creation, then applies it to archive upload and command
execution rather than the create request. Absolute paths and `..` escapes are
rejected before workspace preparation and sync.

## Lifecycle

1. Create or resolve a Crabbox-owned Islo sandbox. New sandboxes are named
   `crabbox-<repo>-<hex>`; the local lease ID is the sandbox name prefixed with
   `isb_`, paired with a friendly slug.
2. Write a local repo claim binding the lease to the current checkout.
3. Validate the workdir, build the Crabbox sync manifest, and upload a gzipped
   archive into `/workspace/<workdir>` through Islo's files-archive API. If that
   upload fails, Crabbox falls back to a base64 chunked upload over the exec
   endpoint.
4. Execute the command through Islo's streaming exec endpoint in that workdir.
5. Require an `exit` event before treating a stream as successful.
6. Delete the sandbox on release unless the lease is kept.

Run outcomes and timing are finalized after retention or guarded cleanup. A
cleanup-only failure exits `1`, reports `provider-error`, and keeps the recovery
session and claim when teardown is unproven. An earlier command, setup, or
artifact failure stays primary when cleanup or timing output also fails; the
secondary diagnostics remain visible and reachable error causes are retained.
Total time includes cleanup; command time excludes downloads and cleanup.

Once a run has a bound session, `--keep-on-failure` also retains failures during
workspace ownership repair, sync, and preparation. Errors before acquisition or
reuse admission finishes remain with those operations' existing rollback and
ownership policy. A later timing-output failure cannot undo successful deletion.

For an enrolled reused lease, existing claim/scope/live-identity admission stays
before the run session is bound. Resume, root health checks, daemon recovery, and
metadata updates then run inside bound-session finalization: a failure returns a
kept reused session and final timing, without running the workload or deleting
the reused resource. Plain and legacy leases keep their existing admission
semantics; this does not add a universal live lookup or upgrade an ID-less claim.
Post-admission Tailscale errors retain their actual causes while preserving the
existing public codes and messages, including fallback `1` for opaque validation
errors. A status code alone does not create a context-cancellation cause.

Observed SSE command exits keep their exact codes when stream decoding and output
delivery complete successfully. A stream read, decode, or stdout/stderr delivery
failure returns `1` even after an exit event was observed. Transport and cancellation
failures also return `1` with their known failure origin and reachable cause.
Closing the stream does not establish that the remote process stopped.
Setup/helper failures preserve their public code without being mislabeled as
user-command exits. Required-artifact and download failures after command success
are provider errors: required-artifact failures keep `7`, and local download-write
failures keep `2`. A first typed timing-writer failure keeps its own public code.
The final error message redacts the configured Islo API key; workload output and
upstream errors that already discarded causes are not reconstructed by this run
finalizer.

`crabbox status --wait` polls the sandbox every 2 seconds until it reports
`running`, bounded by `--wait-timeout` (default 5 minutes). If the sandbox
enters a terminal state (`failed`, `stopped`, `stopping`, or `deleted`) before
becoming ready, the wait fails fast with exit code 5 instead of polling out
the deadline; hitting the deadline also exits 5.

`crabbox ssh` resolves the sandbox before rendering the SSH command: a paused
sandbox is resumed automatically and given up to 2 minutes to report `running`
again before the command fails. A sandbox in a terminal state fails
immediately with exit code 5.

### Teardown safety

Crabbox atomically records the API-assigned sandbox ID with a new or adopted
claim; it never overwrites a claim that appeared during the API lookup.
`DELETE /sandboxes/{name}` is name-only. For an ID-bound lease, `stop` first
identifies the current name as the claimed ID. Cleanup requires a matching
by-ID response with terminal status `deleted`, or a name 404 after positively
identifying the resource during that teardown. A timestamp alone or list
omission is not proof. Only confirmed cleanup removes the local claim.

These checks are not an atomic delete-by-ID guarantee: the API's name-only
DELETE can still race with an out-of-band resource replacement after the read.
Legacy claims without an ID can fall back to the weaker `name-404-unbound`
proof with a warning when no resource was positively identified.

When neither identity lookup can tie the recorded name to the claimed resource -
during an API read outage, for instance - `stop` refuses to delete, keeps the
claim, and exits 5. Retrying it once reads answer again is the normal fix, and
the sandbox may be running and billable until then.

Run cleanup and Tailscale setup rollback use the resource identity and repository
owner captured when the claim was published. They leave a replacement resource
or repository claim untouched; a bookkeeping refresh does not transfer cleanup
authority. An unproven teardown retains its recovery claim. If that claim is
missing or unreadable, cleanup fails without deleting by the stored name. Verify
the original resource ID and ownership before cleanup; the sandbox may remain
running and billable.

## Capabilities

- SSH: yes for direct login to existing Crabbox-created sandboxes. `crabbox ssh
  --provider islo --id <slug>` renders `ssh islo@<sandbox>.islo` on port 22
  by default. Crabbox still does not use SSH for Islo `run` or sync: its adapter
  does not provision an SSH endpoint or manage a per-lease SSH credential,
  expiry, and revocation. See
  [why the provider kind stays delegated-run](../features/islo.md#why-the-provider-kind-stays-delegated-run).
- Crabbox sync: yes, archive sync through the Islo files-archive API, with a
  base64 exec-upload fallback.
  `--no-sync` creates the workspace directory if needed without deleting
  existing files. Workspace replacement applies only during archive sync when
  `sync.delete` is enabled; disabling it preserves existing files before upload.
- URL bridge: yes. Exposed ports become public HTTPS shares through Islo's
  `/sandboxes/{name}/shares` API, surfaced by `--expose` and the pond bridge
  plane. Share creation is idempotent per port. Requested TTLs are clamped
  into Islo's legal 60s–7d range, and an existing share is only reused while
  it has more than 30 seconds left before expiry, so a nearly-expired share is
  replaced instead of handed out.
- Pause / resume: yes. `crabbox pause` snapshots the sandbox to disk and frees
  its CPU/memory via Islo's pause API; `crabbox resume` restores it. The lease
  claim is preserved across a pause.
- Bounded run downloads: yes. Safe relative single-file `--require-artifact`
  and `--download` requests are retrieved through Islo exec after command
  success and capped at 64 KiB per file.
- Delegated POSIX script: yes (`posix-script`). `--script` and `--script-stdin`
  payloads are packed into a gzipped tar, placed in the workspace through the
  files-archive API with mode 0700, and run as `bash -lc 'exec bash "$@"' bash
  <path>` (or `exec "$@"` when the script carries a shebang), so the remote path
  is always a positional argument and never spliced into shell text. Trailing
  argv is forwarded to the script. On Tailnet leases, ownership is handed to
  the workload user in the configured workspace before execution. Cleanup
  uses the administrative user that uploaded the archive, so it can remove
  the file even from directories created by that upload. Removal is attempted
  through a separate bounded context after execution, cancellation, or upload
  and ownership failures. A failed removal is warned about on stderr and never
  changes the workload exit code.
- Desktop / browser / code: no.
- Actions hydration: no.
- Coordinator (broker): no — always direct from the CLI.

## Gotchas

- Direct SSH requires the authenticated Islo CLI and a one-time `islo ssh
  --setup`. The Islo CLI reads `islo login` state or `ISLO_API_KEY`;
  `CRABBOX_ISLO_API_KEY` alone only authenticates Crabbox.
- `--sync-only` and `--checksum` are rejected because `run` still uses Islo's
  delegated archive/exec transport, not Crabbox-managed rsync.
- `--full-resync`, `--force-sync-large`, `--fresh-pr`, `--env-helper`, local
  stdout/stderr captures, `--capture-on-fail`, `--artifact-glob`,
  `--emit-proof`, and `--stop-after` are rejected because Islo owns sync and
  command transport in delegated-run mode. Required artifacts and downloads
  accept only bounded single files, not globs, absolute paths, URLs, `.git`, or
  `.crabbox` paths.
- `--keep-on-failure` keeps a newly created failed sandbox until an explicit
  `stop` or provider-side expiry.
- Large-sync guardrails still apply. Because `--force-sync-large` is rejected,
  trim the checkout (more `.gitignore`/sync excludes) when a sync trips the
  large-archive guardrail.
- `--shell` passes the raw shell string to `bash -lc` in the workdir.
- `--id` accepts a Crabbox slug, an `isb_<name>` lease ID, or a canonical
  normalized sandbox name starting with `crabbox-`. A claimless canonical name
  is read-only until an explicit supported `--reclaim` reuse persists a local
  claim. Names that require case, whitespace, or punctuation normalization and
  non-Crabbox sandboxes are rejected.

## Create deadlines and uncertain responses

Sandbox creation has a five-minute total client budget, including authentication,
response headers, response body, and existing SDK retries. An earlier caller
cancellation or deadline still wins. The internally owned create transport does
not apply the ordinary 30-second response-header cutoff; ordinary API and auth
requests retain it. Command streams remain governed by their caller context,
cleanup retains its separate 15-second budget, and bounded run-file reads retain
20 seconds. Explicitly supplied HTTP clients keep their own transport/timeouts.

These are client limits, not a provider provisioning SLA, resource TTL, or
billing cap. A create timeout, lost response, or incomplete response can leave a sandbox running even
though Crabbox has no acquired lease. The error reports the requested name as an
**unconfirmed attempt locator**, not an ownership claim. Inspect the resource's
identity and the intended repository/account before explicitly using the
existing `--reclaim` adoption flow and `crabbox stop`. Crabbox does not
invent a pending claim, automatically adopt/delete by that name, or add a create
retry. The locator is not a crash-safe journal or an exactly-once guarantee.

## Live testing

Two opt-in smoke tests in `internal/providers/islo/backend_live_test.go`
(build tag `smoke`) exercise the real Islo API. Neither runs in the default
`go test -race -timeout=20m ./...` job, and both skip in `go test -short` mode. Both read
`ISLO_API_KEY` only — `CRABBOX_ISLO_API_KEY`, which authenticates normal
Crabbox runs, is not consulted — and skip when it is unset.

Read-only status classification (lists live sandboxes against
`https://api.islo.dev`, mutates nothing):

```sh
export ISLO_API_KEY=ak_...
go test -tags smoke -run TestLiveIsloStatusClassification -v ./internal/providers/islo
```

Full pause/resume lifecycle (creates, pauses, resumes, runs a command in, and
deletes a real sandbox — separately opt-in because it mutates paid provider
state):

```sh
CRABBOX_LIVE_ISLO_PAUSE_RESUME=1 ISLO_API_KEY=... \
  go test -tags smoke -run TestLiveIsloPauseResumeLifecycle -v ./internal/providers/islo
```

The lifecycle test honors `ISLO_BASE_URL` (default `https://api.islo.dev`) and
`CRABBOX_LIVE_ISLO_IMAGE` (default `docker.io/library/ubuntu:26.04`). After it
captures the lease ID, it attempts to delete the sandbox on exit even when a
later assertion fails. Success requires a matching terminal by-ID tombstone,
name not-found, and local claim absence; a cleanup failure fails the test.
If setup fails before lease capture or the process is
interrupted, remove the leftover `crabbox-pause-resume-live-*` sandbox through
the `--reclaim` adoption flow above followed by `crabbox stop`, or delete it on
the Islo side.

Related docs:

- [Feature: Islo](../features/islo.md)
- [Feature: Provider live smoke](../features/provider-live-smoke.md)
- [Provider backends](../provider-backends.md)
