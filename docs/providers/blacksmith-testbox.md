# Blacksmith Testbox Provider

Read when:

- choosing `provider: blacksmith-testbox`;
- wrapping an existing Blacksmith Testbox workflow with Crabbox;
- changing `internal/providers/blacksmith`.

Blacksmith Testbox is a delegated-run provider. Crabbox does not provision,
bootstrap, rsync, or expose VNC for the remote machine. It shells out to the
authenticated `blacksmith` CLI and adds Crabbox ergonomics on top: stable lease
IDs and slugs, repo claims, timing summaries, proof artifacts, and normalized
`list`/`status` output. Target OS is Linux only.

Configured [`cache.volumes`](../features/cache-volumes.md) are forwarded
as Blacksmith sticky disks during Testbox warmup. Use them for package-manager
stores and other rebuildable dependency caches; keep secrets, checkout state,
and proof artifacts out of sticky disks.

## When to use

Use Blacksmith when the repository already has a Testbox workflow and the remote
workspace should be owned and synced by Blacksmith. Choose AWS, Hetzner, Static
SSH, or Daytona instead when Crabbox needs to own SSH sync, interactive access,
or VNC/code surfaces.

## Commands

One-shot run:

```sh
crabbox run \
  --provider blacksmith-testbox \
  --blacksmith-org example-org \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --blacksmith-ref main \
  --timing-json \
  -- pnpm test
```

Reuse a Testbox with an exact local Crabbox claim, by ID or slug:

```sh
crabbox run --provider blacksmith-testbox --id tbx_123 -- pnpm test
crabbox status --provider blacksmith-testbox --id tbx_123
crabbox stop --provider blacksmith-testbox tbx_123
```

Run delegated sync from a full Git checkout. Crabbox rejects a checkout when
sparse rules or `skip-worktree` index state leave tracked paths absent because
those paths can otherwise be misread as deletions during a later full sync. A
sparse configuration that materializes every tracked path remains supported.
Materialize a temporary full checkout when the source workspace must stay sparse.
Git 2.41 or newer is required only to distinguish an intentional deletion from
a hidden path after sparse index metadata becomes ambiguous; older Git fails
closed in that state with the same full-checkout remediation.

Keep a Testbox between runs via a JSON session handle:

```sh
crabbox run --provider blacksmith-testbox --keep --lease-output /tmp/session.json -- npm test
lease_id="$(node -e 'console.log(require("/tmp/session.json").leaseId)')"
crabbox run --provider blacksmith-testbox --id "$lease_id" -- npm run smoke
crabbox stop --provider blacksmith-testbox "$lease_id"
```

Warm a fresh Testbox:

```sh
crabbox warmup \
  --provider blacksmith-testbox \
  --blacksmith-org example-org \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --blacksmith-ref main
```

`blacksmith` is accepted as an alias, but docs and scripts should prefer
`blacksmith-testbox`.

## Live Smoke

Run the shared smoke only when the selected workflow is a real Testbox workflow:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blacksmith-testbox CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

The smoke exits before any Blacksmith `list` or `run` call when it cannot derive
an org, when the configured workflow file is missing, or when that workflow file
does not contain a `useblacksmith/testbox`, `useblacksmith/begin-testbox`, or
`useblacksmith/run-testbox` step. With a valid org and workflow, it lists the
current inventory and runs one delegated `echo blacksmith-crabbox-ok && pwd`
command through the configured workflow/job/ref.

## Auth

Authentication lives entirely in the `blacksmith` CLI. Log in once before using
the provider:

```sh
blacksmith auth login
```

Crabbox never handles Blacksmith credentials directly; it invokes the
already-authenticated `blacksmith` binary on your PATH.

## Config

```yaml
provider: blacksmith-testbox
blacksmith:
  org: example-org
  workflow: .github/workflows/ci-check-testbox.yml
  job: test
  ref: main
  idleTimeout: 90m
  debug: false
```

Provider flags (override config):

```text
--blacksmith-org
--blacksmith-workflow
--blacksmith-job
--blacksmith-ref
```

Environment variables supply the same defaults:

```text
CRABBOX_BLACKSMITH_ORG
CRABBOX_BLACKSMITH_WORKFLOW
CRABBOX_BLACKSMITH_JOB
CRABBOX_BLACKSMITH_REF
CRABBOX_BLACKSMITH_IDLE_TIMEOUT
CRABBOX_BLACKSMITH_DEBUG
```

`blacksmith.workflow` (or `actions.workflow`, when it is not a generic
`hydrate`/`crabbox` workflow name) is required only to create a new Testbox.
Reusing an existing ID or slug does not need it. `idleTimeout` falls back to the
global `idleTimeout` when unset, and `debug` passes `--debug` through to the
Blacksmith CLI.

### Environment forwarding is unsupported

`--env-from-profile`, `--allow-env`, and `CRABBOX_ENV_ALLOW` help SSH-backed
providers but cannot inject CLI-side environment values into a delegated Testbox
command. When any of those knobs are present, Crabbox prints an
`env forwarding ... unsupported` summary and exits before warmup. Put live
secrets in the Blacksmith workflow instead. Repo-level env allowlists are
ignored for this provider so they can still cover SSH-backed providers.

## Lifecycle

Crabbox forwards to the Blacksmith CLI:

```sh
blacksmith testbox warmup <workflow> ...
blacksmith testbox run --id <tbx-id> ...
blacksmith testbox list
blacksmith testbox list --all
blacksmith testbox stop --id <tbx-id>
```

On warmup, Crabbox generates a per-Testbox SSH key locally, passes the public
key to `blacksmith testbox warmup --ssh-public-key`, parses the returned `tbx_`
ID, checks its native status, and durably binds the exact Testbox ID, provider,
repo owner, friendly slug, organization, API endpoint, and observed workflow/job/ref.
The key is moved into the lease directory under the same absent-claim fence;
existing key or claim state is never replaced. Reusing a lease across repos needs
`--reclaim`, which changes only the repo association of an already exact claim.

Stop, reuse, delegated artifact commands, and one-shot cleanup require the
unchanged claim. Each operation pins the selected organization and API endpoint
with explicit native CLI flags and checks the exact Testbox status under the
claim fence. A stop can cancel an active command without allowing claim writers
to change its authority. After terminal confirmation, stop takes an exclusive
fence and rechecks the original claim and native status before removing the claim
and key. A changed claim or a command that fails to exit within the cleanup
deadline leaves local ownership intact. `hydration_failed` prevents reuse but
still requires confirmed termination for cleanup. Missing, malformed, duplicate or mismatched status is not proof of
termination. Uncertain cleanup retains ownership; a successful workload with
failed cleanup returns a failure and reports the session as kept. An earlier
workload failure keeps its own exit code.

Local connection artifacts must be removed successfully before the exact claim
is deleted; an unsafe or undeletable lease key directory reports cleanup failure
and retains the claim for retry. Missing lease key directories are already clean.
Failed stops report both the native failure and any independent verification or
finalization failure, preserving the native exit code. Failed-query stderr is
diagnostic only and never proves completion.

A never-assigned Testbox can move directly from `queued` to `completed`, with
empty IP and `RUN URL` cells. This permits cleanup only after a successful,
uncanceled native status query returns the exact owned identity in a complete
native table, with nonempty `CREATED`, aligned columns, trailing padding through
the empty `RUN URL` cell, and the final newline. Present run URLs remain
validated. Missing or failed status is still not completion evidence; the
exclusive claim/status recheck and key-before-claim finalization remain required.

Use the same organization/API route when reusing or stopping a lease. Workflow
flags are still unnecessary for reuse; the provider checks stored native
workflow/job/ref metadata. Token rotation within the same organization remains
supported. This adapter uses the native status table (verified with Blacksmith
CLI 0.4.57), and rejects an unsupported table format rather than guessing.

### Older leases and lost local state

Legacy claims without the exact resource/scope binding, and resources with no
local claim, remain available to read-only `list`/`status`. They cannot authorize
Crabbox stop or reuse, including with `--reclaim`. After independently verifying
the organization and exact Testbox in Blacksmith, use native recovery:

```sh
blacksmith --org example-org testbox status --id tbx_EXACT_ID
blacksmith --org example-org testbox stop --id tbx_EXACT_ID
blacksmith --org example-org testbox status --id tbx_EXACT_ID
```

Verify the final status is terminal, then create a new Crabbox lease. Native stop
also cancels the backing GitHub Actions run. Do not reconstruct claims from IDs,
inventory or copied metadata.

One-shot runs stop the Testbox and remove the local claim and key after the
command completes, unless `--keep` is set. `--keep-on-failure` keeps a failed
one-shot Testbox alive for debugging; successful runs still stop normally. Unconfirmed cleanup leaves the Testbox and its local claim/key available for
inspection and an exact stop retry.

If `list`/`status` work but new warmups sit `queued` with no IP, Blacksmith is
accepting requests but not assigning capacity. Stop any queued IDs you created
and fall back to AWS, Hetzner, Static SSH, or Daytona until Blacksmith service,
billing, or org limits recover. Failed warmup can roll back only a unique bare creation receipt from that
invocation, under its pinned route and while its claim is absent. There is no
inventory sweep. An ambiguous receipt, appearing/partial claim or existing key
state prevents rollback. Missing or ambiguous receipts retain the invocation's
pending SSH key and print its identifier for independently verified native
recovery; they never authorize a guessed stop. Uncertain rollback prints the exact resource and pending
key identifier for native inspection; it does not erase recovery state.

### Failure bundles and proof

Failed runs write a local failure bundle (stdout, stderr, timing, redacted
env/config metadata) even though remote file capture is delegated to Blacksmith.
Captured streams are size-capped so a verbose successful run does not fill local
temp storage.

`--emit-proof <path>` works for successful Blacksmith runs. Crabbox renders the
same proof block used by SSH-backed runs from the delegated stdout/stderr
transcript, command timing, the Testbox ID, and any GitHub Actions run URL found
in the stream. When proof is requested, Crabbox also writes bounded transcript
artifacts under `.crabbox/runs/<testbox-id>/`:

```text
blacksmith.stdout.log
blacksmith.stderr.log
timing.json
metadata.json
```

### Sync stall guard

Crabbox terminates a local `blacksmith` invocation that stays in the sync phase
for five minutes without printing a sync-completion marker. Set
`CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS=0` to disable the guard, or a larger
millisecond value for intentionally huge local diffs. (`OPENCLAW_TESTBOX_SYNC_TIMEOUT_MS`
is also honored for legacy compatibility.)

### Portal visibility

With a configured coordinator, successful `warmup`, `run`, and `list` sync
visibility-only Testbox rows into the portal lease table. If Crabbox can
infer the owning GitHub Actions run, the portal links the row to the run and
workflow, shows the Actions status/conclusion, flags long-queued or long-running
rows as `stuck`, exposes a copyable local stop command, and provides a
visibility-only detail page.

This optional bookkeeping shares a five-second budget, including inventory,
Actions lookups, coordinator credentials, and HTTP. Cancellation or timeout
stops further work and warns on stderr; it does not fail a successful warmup,
stop its retained Testbox, or retry allocation. Warmup's final completion and
timing include the bookkeeping attempt. See [portal visibility](../features/blacksmith-testbox.md#portal-visibility)
for inventory and enrichment semantics.

## Capabilities

- SSH: no Crabbox SSH lease.
- Crabbox sync: no.
- Provider sync: yes, Blacksmith-owned.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Proof: yes, from the delegated stream, timing, and metadata.
- Actions hydration: Blacksmith owns workflow setup; not Crabbox SSH hydration.
- Coordinator: no (always direct from the CLI).

## Gotchas

- `--no-sync` exits 2 before backend configuration, whether acquiring or reusing
  a Testbox, because Blacksmith has no supported skip-sync contract. Callers
  that need no file transfer must choose a provider that supports skipping sync.
- `prewarm --probe-command` requires `--no-sync`, so a nonblank probe exits 2
  before backend configuration, warmup, key generation, provider calls, or claim
  changes, including with `--dry-run`. Plain `prewarm` (also an empty or whitespace-only probe)
  stays supported; omit `--no-sync` when reusing the resulting Testbox. Put
  readiness checks in the Blacksmith workflow or use a provider that supports
  no-sync probes.
- Named jobs with `noSync: true` also exit 2 before warmup, hydration, run, or
  stop, including dry runs and existing leases. No keys are generated or claims
  changed; omit `noSync` or set it to `false` for ordinary Blacksmith jobs.
- `--sync-only`, `--checksum`, and `--force-sync-large` do not apply because
  Blacksmith owns sync.
- `--script`, `--script-stdin`, `--fresh-pr`, local stdout/stderr captures, and
  `--download` are rejected because Blacksmith owns command transport and remote
  file transport. Use `--emit-proof` for PR-ready transcript proof.
- `--artifact-glob` and `--require-artifact` run through the Blacksmith adapter:
  an adapter-owned supervisor collects in the original native invocation after
  a normal terminal workload exit, including failures below 128. No follow-up
  native run or re-sync occurs. Signal-like exits skip collection. Publication
  requires a fresh complete receipt, clean native transport, an uncanceled
  caller, and the original unchanged claim fence; stopped-lease recovery is not
  supported. Collection failures preserve an earlier workload failure; after
  workload success they still fail the run. Required globs remain all-or-nothing.
  The defaults are 256 files and 10 MiB compressed, stored privately under
  `.crabbox/runs/<lease>/blacksmith-artifacts.tgz`, with protected paths and
  symlink handling unchanged. Linux `timeout` with `--kill-after` is required
  before execution; collection has its own 30-second budget and bounded local
  wait, subordinate to caller cancellation, not a workload deadline. Command
  timing ends at the workload receipt; collection and cleanup count toward total.
  Artifacts from a failed run are not success proof or remote source attestation.
  See the [artifact contract](../features/blacksmith-testbox.md#run-artifacts).
- `--actions-runner` is rejected; Blacksmith owns runner hydration.
- `--tailscale`, desktop helpers, screenshots, VNC, and `artifacts collect` are
  rejected because Blacksmith owns machine connectivity.
- `list` and `status` are core-rendered from parsed Blacksmith CLI output.

Related docs:

- [Feature: Blacksmith Testbox](../features/blacksmith-testbox.md)
- [Provider backends](../provider-backends.md)
