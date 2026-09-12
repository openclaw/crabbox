# History And Logs

Read when:

- changing how `crabbox run` records progress;
- debugging a failed remote command after the fact;
- deciding what belongs in coordinator-stored run history.

Shared history and logs are a **brokered-mode** feature. When `crabbox run` executes
against a brokered provider (`aws`, `azure`, `daytona`, `gcp`, `hetzner` with a
coordinator configured), the CLI mirrors the run into coordinator storage as a
durable, queryable record. Direct-provider runs and delegated runs do not produce
central history — there you have only the live terminal output and any local
captures you ask for, unless you explicitly enable private local history.

## Private local history

`crabbox run --record-local -- <command>` retains a bounded local record keyed
by the same printed `run_...` ID. The record survives normal lease removal and
does not require or upload to a coordinator. The default is off. Enable it for
future runs in trusted user configuration with `history.local.enabled: true`;
repository configuration cannot enable or disable that policy. An explicit
`--record-local=false` overrides the user policy.

```sh
crabbox run --provider local-container --record-local -- printf 'example\n'
crabbox history --source local
crabbox logs run_<id> --source local
crabbox results run_<id> --source local --json
crabbox history prune --source local
crabbox history delete run_<id> --source local
```

Local history lives under the existing state directory's `history` directory,
separate from lease claims. Files and directories are private to their owner.
It retains up to 100 inactive records, 256 MiB overall, and 30 days; abandoned
incomplete records are included. Active writers hold a lock and reserve bounded
space. Admission fails before acquisition if private storage cannot be reserved.
An unavailable final write warns without changing the command's existing exit,
timing, or receipt. An interrupted or uncommitted record is marked incomplete,
not successful; its output may be unavailable.

The log uses the same 8 MiB UTF-8 tail and capture-omission policy described
below. Metadata is capped at 256 KiB and serialized parsed results at 1 MiB.
Result totals remain intact when detailed entries are omitted or clipped; local
readback exposes those limits. Raw JUnit XML and arbitrary remote files are not
retained. Caller output is not automatically secret-redacted: opt in only when
private retention is appropriate. Command/display diagnostics use the existing
diagnostic redactor.

Workload writers are observed separately from Crabbox's own diagnostic output.
Some delegated native CLIs mix command and provider messages; those retained
streams are labelled `provider-run`, not a pure workload transcript. Streams a
provider does not expose and streams directed exclusively to captures remain
explicitly unavailable or omitted. Unsupported delegated JUnit collection is
not fabricated into a passing result.

`history`, `logs`, and `results` accept `--source local|coordinator|all`.
With a coordinator configured, the existing default broker route and JSON stay
unchanged. Without one, existing local history is readable, including after
retention is disabled. Explicit local reads stay offline. Source-qualified
output distinguishes local-only data, acknowledged coordinator records, and
both; correlation includes a noncredential endpoint reference, not just an ID.
A local self-record is never coordinator-attested evidence. `events`, `attach`,
and `receipt` retain their coordinator-only contract.

## What a recorded run contains

The CLI creates a run handle (`run_<hex>`) before remote command execution,
after lease resolution when the coordinator requires a lease ID, then appends
ordered events as it advances:

- `run.started`
- `leasing.started`, `lease.created`, `lease.replace.started|finished|failed`
- `bootstrap.waiting`
- `sync.started`, `sync.finished`
- `actions.hydrate.started|finished|failed` (when hydrating from a GitHub
  Actions workflow)
- `script.uploaded` (when running a `--script` file)
- `command.started`, streamed `stdout`/`stderr` events, `command.finished`
- `lease.released`
- `run.failed` (if the run errors before the command finishes)

Current clients admit runs through `PUT /v1/runs/<run-id>`, using a cryptographically
random ID known before the request. The coordinator commits the record and its
first event atomically. Matching admission replay returns the retained record;
changed request content conflicts, and another actor cannot adopt the record.
The original request binding remains unchanged when later events attach or
replace a lease. Legacy clients can still use `POST /v1/runs` for coordinator-issued
IDs, but that route cannot recover a lost create response. Upgrade an older
coordinator before using the current client's admission route.

Admission recovery is limited to the original live CLI invocation before command
execution. A terminal or already-progressed record is a historical result, not
permission to execute again. Record retention is unchanged; clients must never
reuse an ID for a new invocation.

Crabbox-generated event messages are redacted before they enter coordinator
storage. The recorder removes configured and provider-discovered runtime
credentials, authorization headers, credential-bearing URLs, and other known
secret encodings while preserving useful diagnostic context. Raw `stdout` and
`stderr` event data and retained command logs remain caller-owned output and are
not automatically redacted.

Phase and stream diagnostics publish through one bounded queue while the workload
continues. An existing lease's run creation already records its binding; a new
or replacement lease binding first drains and joins diagnostics, then gets its
own acknowledged request before command admission. Missing diagnostic endpoints
do not block an already bound run; a changed binding must be accepted because
the signed terminal receipt requires the exact lease, slug, and provider.
Sync-only runs keep their optional-history behavior and warn when binding is
unavailable. Before
terminal recording, the CLI drains diagnostics for up to two seconds, cancels
remaining publication, and joins the publisher. Queue overflow or drain expiry
produces a warning; retained logs and verified terminal receipts remain the
completion record.

Each event carries a sequence number, type, phase, and stream. Streamed output
events are capped at **64 KiB total per run**; once the cap is hit the CLI emits
a single `output.truncated` marker pointing you at `crabbox logs` for the full
retained output.

When the command exits, the CLI finishes the run with:

- exit code;
- sync duration, command duration, and total duration;
- initiating actor owner and org;
- backing lease IDs and owner/org identities for lease-backed runs;
- provider, target, class, and server type;
- the retained command log (capped — see below);
- a parsed test-result summary when JUnit results are available;
- a failure classification (`blockedStage`, `retryLikely`) on non-zero exits;
- optional Linux telemetry: a start sample, bounded mid-run samples (every 15s,
  up to 60 retained), and an end sample covering load, memory, disk, and uptime.
- a signed schema v2 terminal receipt from current clients, atomically committed
  with the terminal run record. Current clients verify the exact stored receipt
  before reporting the finish as recorded; legacy clients may finish without
  one.

## Reading history

```sh
crabbox history                      # recent runs
crabbox history --lease cbx_abcdef01 # runs for one lease
crabbox history --state failed       # filter by state
crabbox logs run_a1b2c3d4            # full retained command log
crabbox logs run_a1b2c3d4 --tail 80  # last 80 lines only
crabbox events run_a1b2c3d4          # ordered run events
crabbox events run_a1b2c3d4 --type stderr  # filter events by type
crabbox attach run_a1b2c3d4          # follow an in-progress run live
crabbox receipt run_a1b2c3d4         # retrieve and verify terminal evidence
```

`crabbox attach` follows a still-running run, preferring the broker's live
control WebSocket and falling back to polling. Use `attach` for active runs and
`logs` for the retained output of a finished run. All four commands accept
`--json`.

The run record is created in coordinator storage before remote command
execution. If the finish response or local receipt write is lost, use its run ID
with `crabbox receipt`. The CLI prints that ID. Automation that must recover it
after losing client output should also use `run --lease-output <file>` on a
supported retained run; the handle is written before SSH wait, sync, or command
execution and includes `runID`. A missing receipt is not reconstructed from logs
or events. A receipt-bearing CLI fails closed against a coordinator that accepts
the finish but cannot return the exact stored receipt.

If terminal recording fails, the CLI reports the attempted finish submission
and receipt-verification errors, the attempt count, and the recovery command.
These diagnostics remain available when the shared 60-second recording deadline
expires. That failure does not establish the remote command's exit status; use
the committed receipt to resolve an ambiguous result.

Run records keep the initiating actor in `owner`/`org` and retain every backing
lease identity used by a replacement flow. Each backing lease owner has
read-only access to history, details, logs, events, telemetry, live event
subscriptions, and portal pages for auditing work on their lease. Only the
initiating actor or an admin can append events or telemetry and finish the run.
Lease shares do not grant access to runs created by other actors.

Once a finish is committed, later events remain in the ordered audit trail but
do not change the run's terminal state, phase, end time, or backing lease
metadata. This includes delayed output, duplicate failure notifications, and
post-finish lease cleanup events; the committed logs and receipt remain authoritative.

## Storage limits

History records, run events, and run logs all live in coordinator storage:
Durable Object storage on Cloudflare or PostgreSQL on Node. Log text is stored
separately from run metadata and is intentionally bounded so noisy commands
cannot exhaust storage:

- The CLI keeps a **UTF-8 text tail of at most 8 MiB**. It replaces each
  malformed output byte with U+FFFD and truncates only at codepoint boundaries,
  so a retained tail can be a few bytes smaller than the cap.
- `logTruncated` (the receipt's `log_truncated`) means the retained text is not
  byte-complete: output exceeded the cap, malformed UTF-8 was normalized, or
  output went exclusively to local captures. An exact-cap valid UTF-8 stream
  with no omitted output is not truncated.
- The broker stores the same **8 MiB** cap, chunked at **64 KiB** per storage
  value and reassembled by `crabbox logs`.

These caps are independent of the per-run **64 KiB** streamed-event budget
described above; events are for live tailing, `logs` is for the full retained
output.

## Local debug artifacts

For uncapped, local-only output, mirror the streams to files:

```sh
crabbox run --capture-stdout out.log --capture-stderr err.log -- ./test.sh
```

These captures preserve raw bytes on the operator's machine and bypass
coordinator run-log storage entirely. The signed full-stream hash also remains
raw; only the retained-text hash uses the normalized representation. Nonempty
captured streams therefore make the retained log byte-incomplete even when its
text is below the cap. Use distinct paths for stdout, stderr, and any
`--download remote=local` artifacts — Crabbox rejects path collisions before the
command runs.

On a non-zero exit, SSH-backed and Blacksmith delegated runs also write a local
failure bundle under `.crabbox/captures/` by default (the bundled streams are
capped at 16 MiB each). `--capture-on-fail` is accepted as a no-op compatibility
alias; bundles save automatically on failure. An unwritable project capture
destination falls back to the Crabbox user state directory's `captures/`
subdirectory; the `failure-bundle local=...` line reports the actual path. See
[local capture storage](../observability.md#capturing-run-output-locally) for
fallback boundaries and retention. Treat captured logs and bundles as
secret-bearing files unless you redact them before sharing.

## Phase timings

A command can annotate its own timeline by printing `CRABBOX_PHASE:<name>` on
stdout or stderr. Phase markers surface in `crabbox run --timing-json` under
`commandPhases`, and the failure digest reports the observed and final phases.
The marker line stays in the normal output stream, so scripts and humans see the
same text.

## Portal view

In the authenticated browser portal, `/portal/runs/<run-id>` renders the same
run as a human page: command metadata, result summary, searchable and paginated
recent events, compact resource deltas, short telemetry trend lines, and a
copyable retained log tail. `/portal/runs/<run-id>/logs` stays a plain-text log
endpoint and `/portal/runs/<run-id>/events` stays JSON, both for easy copying or
browser-side inspection.

## Related docs

- [history command](../commands/history.md)
- [logs command](../commands/logs.md)
- [events command](../commands/events.md)
- [attach command](../commands/attach.md)
- [results command](../commands/results.md)
- [receipt command](../commands/receipt.md)
- [Observability](../observability.md)
