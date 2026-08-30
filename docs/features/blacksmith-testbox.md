# Blacksmith Testbox

Read this page when you are:

- choosing a provider for a command or repo;
- selecting `provider: blacksmith-testbox`;
- wiring Blacksmith CLI defaults (org, workflow, job, ref);
- deciding what Crabbox owns versus what Blacksmith owns.

Crabbox can use [Blacksmith](https://blacksmith.sh) Testboxes as the machine backend
**without** the Crabbox broker. Select it per command with `--provider blacksmith-testbox`,
or set `provider: blacksmith-testbox` in config when a repo or machine should default to it.

`blacksmith-testbox` is a **delegated-run** provider. Crabbox does not provision, bootstrap,
sync, or expose VNC for the Testbox itself; it shells out to the `blacksmith` CLI and keeps
local Crabbox ergonomics (slugs, repo claims, per-Testbox SSH keys, timing summaries) around it.
The provider is Linux-only and never routes through the Cloudflare coordinator.

## Prerequisites

The `blacksmith` CLI must be installed and authenticated. Auth stays entirely with Blacksmith:

```sh
blacksmith auth login
```

Crabbox does not call its own login broker, does not send work to the coordinator, and does not
store Blacksmith credentials.

## Quick start

If Crabbox already holds an exact local claim for a Testbox ID (`tbx_...`), no workflow YAML is required for reuse:

```sh
crabbox run --provider blacksmith-testbox --id tbx_123 -- pnpm test
```

If Crabbox already claimed a friendly slug for that Testbox, the slug works anywhere an ID does:

```sh
crabbox run --provider blacksmith-testbox --id blue-lobster -- pnpm test:changed
crabbox status --provider blacksmith-testbox --id blue-lobster
crabbox stop --provider blacksmith-testbox blue-lobster
```

This path needs Blacksmith auth in the claim’s original organization/API scope and a reachable,
exactly claimed Testbox. Crabbox verifies the ID, native workflow identity and unchanged claim, then forwards the command to `blacksmith testbox run`, and prints
`sync=delegated` in the final summary.

To create a fresh Testbox without YAML, pass the workflow details as flags:

```sh
crabbox warmup \
  --provider blacksmith-testbox \
  --blacksmith-org example-org \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --blacksmith-ref main \
  --idle-timeout 90m
```

The same flags drive one-shot `run` when no `--id` is supplied:

```sh
crabbox run \
  --provider blacksmith-testbox \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  -- pnpm test
```

`warmup` keeps the Testbox until its idle timeout or an explicit `crabbox stop`. A one-shot `run`
that acquired the Testbox stops it on exit unless you pass `--keep` (or `--keep-on-failure` for a
failed run; see [Ownership boundary](#ownership-boundary)).

## Configuration

### Flags

| Flag | Purpose |
| --- | --- |
| `--blacksmith-org` | Blacksmith organization passed as `--org` to the CLI |
| `--blacksmith-workflow` | Testbox workflow file, name, or id |
| `--blacksmith-job` | Workflow job |
| `--blacksmith-ref` | Git ref |
| `--cache-volume` | Provider-backed cache volume, forwarded as a Blacksmith sticky disk |

`--idle-timeout` (a core flag) sets the Testbox idle timeout; it is forwarded to
`blacksmith testbox warmup` as whole minutes.

Configured [`cache.volumes`](cache-volumes.md) are forwarded during warmup as
Blacksmith sticky disks using `key:path`. The `--cache-volume [name=]key:path`
flag is repeatable and marks the volume required for that run.

### Environment variables

Useful for shell defaults and scripts:

- `CRABBOX_BLACKSMITH_ORG`
- `CRABBOX_BLACKSMITH_WORKFLOW`
- `CRABBOX_BLACKSMITH_JOB`
- `CRABBOX_BLACKSMITH_REF`
- `CRABBOX_BLACKSMITH_IDLE_TIMEOUT`
- `CRABBOX_BLACKSMITH_DEBUG` (passes `--debug` to forwarded `testbox run`)
- `CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS` — sync-stall guard (see [Sync-stall guard](#sync-stall-guard))

### Repo config

Use repo YAML when every agent or maintainer should get the same defaults without repeating flags:

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

For repos that already use [Actions hydration](actions-hydration.md), the
`blacksmith.workflow`, `blacksmith.job`, and `blacksmith.ref` keys can be omitted when
`actions.workflow`, `actions.job`, and `actions.ref` carry the same values — Crabbox falls back to
the Actions fields. The fallback is skipped when the Actions workflow looks like a generic
Crabbox hydrate workflow (named `hydrate` or `crabbox`), so a bootstrap-only hydrate workflow is
never mistaken for a Testbox workflow.

`blacksmith.workflow` (or the Actions fallback) is required only when Crabbox needs to **warm or
acquire** a Testbox. Reusing an existing `tbx_...` ID or slug needs no workflow config.

`blacksmith` is accepted as a shorthand provider alias, but docs and scripts should prefer
`blacksmith-testbox`.

## Forwarded commands

Crabbox forwards lifecycle and run operations to the `blacksmith` CLI:

```sh
blacksmith [--org <org>] testbox warmup <workflow> --job <job> --ref <ref> \
  --ssh-public-key <key> --sticky-disk <key:path> --idle-timeout <minutes>
blacksmith [--org <org>] testbox run --id <tbx_id> --ssh-private-key <key> [--debug] <command>
blacksmith [--org <org>] testbox list
blacksmith [--org <org>] testbox list --all
blacksmith [--org <org>] testbox stop --id <tbx_id>
blacksmith [--org <org>] testbox status --id <tbx_id>
```

The wrapper is deliberately thin for warmup, run, and stop. `crabbox list` and `crabbox status`
normalize Blacksmith output into Crabbox's common list/status views so rendering stays
core-owned across providers. Both `list` and `status` read `blacksmith testbox list --all` and
parse its table output.

`crabbox list --provider blacksmith-testbox --json` parses that table into compatibility JSON rows
with the fields Crabbox can see (id, status, repo, workflow, job, ref, created). The parser is a
compatibility layer, not a Blacksmith API contract; if the CLI gains native JSON output, Crabbox
should switch to it and drop table parsing.

For a Testbox with an exact local `blacksmith-testbox` claim, a failed stop triggers one fresh native
`blacksmith testbox status --id <same-id>` query in the same organization and
cleanup context, while the existing local claim lock remains held. Cleanup is
acknowledged only when that query succeeds without cancellation and its stdout
contains the native table header and one complete, unambiguous row identifying
the **exact requested ID** with state **`completed`**. The IP cell may be empty
after native stop clears it; the remaining columns must still be present and
aligned with the header. Successful stops also require terminal confirmation.
Raw IDs without exact local ownership never reach native stop.

Only `completed` establishes terminal status for this reconciliation. A 409 or
“already stopped” error, stderr text, missing inventory/404, malformed or duplicate
rows, other states, and failed or canceled status queries do not authorize local
removal. Without confirmation, Crabbox preserves the original stop error and
diagnostics and retains the unchanged claim and stored key for retry. Confirmed
completion permits an exclusive recheck of the original claim and native status;
only successful local finalization prints a cleanup reconciliation note.

Reconciliation applies to explicit stop and automatic one-shot cleanup. It
preserves an earlier workload failure; unconfirmed cleanup after a successful
workload returns failure consistently in the CLI, timing and saved metadata. A successful
GitHub Actions job is not proof that the delegated command succeeded: idle expiry
can complete the job successfully. `--keep`, `--keep-on-failure`, and reused `--id`
runs retain their existing cleanup policy.

If `blacksmith testbox list --all` and `crabbox status` both work but new warmups stay `queued`
with no IP, treat it as Blacksmith service, queue, org-limit, or billing pressure rather than a
Crabbox provisioning bug. Stop queued IDs you created and switch to another provider until the
Blacksmith account or service recovers. A `warmup` failure prints a hint suggesting a
coordinator-backed provider (for example `--provider aws`) and can roll back only the unique Testbox receipt from that invocation while its local claim is absent. It never infers ownership from newly listed resources.

### Portal visibility

When a coordinator is configured, successful `warmup`, `run`, and `list` also perform a
best-effort sync of the current all-status Blacksmith list into the portal lease table. Those rows
are owner-scoped **visibility records** for Blacksmith-owned Testboxes, rendered muted in the
portal. When a row carries enough context, Crabbox links it to the closest GitHub Actions run and
the workflow definition; the portal shows the Actions status/conclusion, adds a `stuck` filter for
long-queued or long-running workflows, and offers a copyable local cleanup command
(`crabbox stop --provider blacksmith-testbox ...`). Clicking a row opens a visibility-only detail
page with owner/org, Actions ownership, timestamps, and boundary notes.

These rows are **not** Crabbox leases: they expose no box-access actions, do not heartbeat, do not
participate in Crabbox expiry or cost control, and become stale when a later sync no longer sees
the runner.

Each sync shares one five-second budget across inventory, GitHub Actions
enrichment, coordinator credential resolution, and upload, including response
body reads. Earlier caller cancellation wins. An ordinary Actions lookup failure
only omits that optional metadata. If the budget expires or is canceled before
upload, Crabbox skips publication so incomplete inventory cannot mark other rows stale. Failed
syncs warn on stderr without changing command success or Testbox ownership.
Warmup prints its final completion and timing only after this attempt ends, with
bookkeeping included in the total. Reuse the retained lease after a warning;
portal visibility is not an allocation or readiness check.

## Sync-stall guard

Because Blacksmith owns sync, Crabbox watches the forwarded `testbox run` output for sync progress
markers. If the CLI starts syncing but does not print a completion marker within the guard window
(default 5 minutes), Crabbox terminates the local runner and exits `124`. Tune or disable it with
`CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS` (milliseconds; `0` disables the guard).

## Ownership boundary

Stop, reuse, command execution and artifact retrieval require an exact local
claim binding the provider, Testbox ID, slug, repository owner, organization/API
route and native workflow/job/ref. Native status checks and actions share the
unchanged-claim fence. Stop can cancel an active command, then rechecks the
original claim under an exclusive fence before removing the claim and key.
Terminal status must be confirmed; a changed claim or cleanup deadline retains
local state. Uncertainty marks the session kept. A successful
workload with failed cleanup returns nonzero, while an earlier workload failure
preserves its exit code.

Legacy or lost claims require independently verified native Blacksmith cleanup;
raw IDs are accepted only for read-only discovery until a new Crabbox lease is
created. `--reclaim` changes repository association for an already exact claim,
not resource or authority. See the [migration and recovery instructions](../providers/blacksmith-testbox.md#older-leases-and-lost-local-state).

- **Blacksmith owns** provisioning, workflow hydration, remote workspace setup, sync, command
  transport, logs emitted by its CLI, machine connectivity, and idle expiry.
- **Crabbox owns** local YAML/env config, per-Testbox SSH keys, friendly slugs, repo claims,
  provider selection, command quoting, the sync-stall guard, and final timing/proof summaries.

Because Blacksmith owns sync and execution, Crabbox rejects the following `run` options for
`provider=blacksmith-testbox`:

- sync flags: `--no-sync`, `--sync-only`, `--checksum`, `--force-sync-large`, `--full-resync`, `--fresh-pr`;
- execution flags: `--script`, `--script-stdin`, `--env-helper`, `--capture-stdout`,
  `--capture-stderr`, `--capture-on-fail`, `--download`, `--stop-after`;
- environment forwarding: `--allow-env` and `CRABBOX_ENV_ALLOW` are unsupported — configure
  secrets in the Testbox workflow instead;
- `--actions-runner` on `warmup` — Blacksmith owns runner hydration.

Run option admission precedes backend configuration, including when reusing
`--id`. Because `prewarm --probe-command` promises a no-sync shell probe, it
also rejects before configuration, including in dry-run mode. Put readiness
checks in the Blacksmith workflow or use plain `prewarm` without a probe.

`crabbox run` prints `sync=delegated` in the final summary. `--emit-proof` is supported: it
persists a local proof bundle (stdout/stderr logs, `timing.json`, `metadata.json`) and links the
detected GitHub Actions run URL when one appears in the output. Failed runs always save a local
failure bundle with stdout/stderr, timing, and redacted env/config metadata. `--keep-on-failure`
keeps a failed one-shot Testbox inspectable until its idle timeout or an explicit `crabbox stop`.

`--artifact-glob` and `--require-artifact` are supported through Blacksmith's delegated
run-artifact adapter. After the command succeeds, Crabbox asks the same Testbox to validate
required globs and stream one bounded tarball back to the local
`.crabbox/runs/<lease>/blacksmith-artifacts.tgz` path. The adapter caps retrieval at 256 files and
10 MiB by default and still excludes `.git` and `.crabbox` paths.

## Desktop and VNC

Blacksmith can run headless browser automation through its own runner setup, but Crabbox does not
expose `crabbox vnc`, `crabbox webvnc`, or managed screenshots for `provider=blacksmith-testbox` —
Blacksmith owns machine connectivity in this mode. VNC support would require Blacksmith to expose
a stable SSH tunnel or connection-info API that preserves the same security boundary as managed
Crabbox leases.

## Choosing the path

Use the quick-start path when:

- you already have a `tbx_...` ID or slug;
- you are trying Blacksmith on a single command;
- an agent can pass the provider and workflow directly as flags.

Use repo YAML when:

- the repo should default to Blacksmith;
- multiple agents should share the same workflow/job/ref;
- you want `crabbox warmup` to work without extra flags or env.

## Related docs

- [Provider Reference](../providers/README.md)
- [Actions hydration](actions-hydration.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [run command](../commands/run.md)
- [warmup command](../commands/warmup.md)
- [Source map](../source-map.md)
