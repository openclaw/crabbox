# claims

`crabbox claims list` prints the lease claims stored on the current machine. It
is a read-only, credential-free inventory: it does not load provider
configuration, initialize a provider backend, contact a provider, refresh a
claim, or remove stale state.

```sh
crabbox claims list
crabbox claims list --json
```

Human output is headed `unverified local state` because a structurally valid
claim may be stale. Provider-scoped [`crabbox list`](list.md) remains the command
for provider-backed machine inventory.

## JSON

`--json` emits this versioned envelope, including empty arrays when no claims or
problems exist:

```json
{
  "version": 1,
  "source": "local-claims",
  "claims": [],
  "problems": []
}
```

Each claim contains only these public fields:

```json
{
  "leaseId": "cbx_abcdef123456",
  "slug": "blue-lobster",
  "provider": "aws",
  "repoRoot": "/path/to/repo",
  "pond": "",
  "targetOS": "linux",
  "windowsMode": "",
  "claimedAt": "2026-08-16T10:00:00Z",
  "lastUsedAt": "2026-08-16T10:05:00Z",
  "idleTimeoutSeconds": 1800
}
```

Cloud and resource identifiers, provider scope, hosts and endpoints, SSH
details, labels, login and registration URLs or IDs, credentials, cache
metadata, revisions, and fixed-create internals are never included. Claims are
sorted by lease ID, provider, and slug.

Malformed files do not hide valid claims. For this read-only inventory, each
local claim file has an inclusive 1 MiB implementation limit. Files that exceed
the limit while being read use `claim_too_large` and are never read into memory
in full; other concurrent changes remain `read_error`. This inventory limit does
not change the runtime claim loader. Each problem has stable `file`, `code`, and
`message` fields. Supported codes are
`invalid_filename`, `invalid_json`, `claim_too_large`, `empty_lease_id`,
`lease_id_mismatch`, `read_error`, and `invalid_claim`. Unsafe or overlong
filenames are represented by a short SHA-256 fingerprint instead of their
contents. Non-regular claim paths use `non_regular_file`. At most 100 problem
entries are emitted; the final
`problems_truncated` entry reports when additional files were omitted. Problems
are sorted by file reference and code.

## Exit codes

- `0`: the snapshot is missing, empty, or contains only valid claims.
- `2`: one or more malformed claim records were found. Valid claims and bounded
  problem entries are still written before exit.
- Other nonzero codes: the local state directory itself could not be read or the
  output could not be written.

## Flags

```text
--json    print the stable JSON envelope
```

## Prune

```sh
crabbox claims prune --dry-run
crabbox claims prune --older-than 7d --json
```

`--older-than` defaults to `7d` and accepts positive Go durations such as `168h`
or positive whole days. A claim is eligible only when `lastUsedAt` (falling back
to `claimedAt` when absent) is older than that threshold **and** its provider's
remotely enforced lifetime bound has elapsed. A short threshold cannot override
the provider bound, though a bound only proves expiry for use recorded by this
machine; keep the default threshold unless you know a lease is gone. The command
reads provider capability metadata without configuring or authenticating a
backend. No provider resources, coordinator records, SSH keys, or claim lock
files are removed. Removal uses the existing lease operation lock and compares
the complete claim again; concurrent use or modification retains it.

Providers opt in by declaring a remotely enforced lifetime bound. **E2B** caps
create/connect at one hour. **Blacksmith Testbox** uses 36 days: a Testbox claim
is retained until native completion and the exact associated GitHub run have
settled, and GitHub cancels any workflow run after 35 days, so an older claim can
no longer be needed by a stop retry. All other providers default to retention.
Cloud claims do not reliably distinguish direct resources from
coordinator-managed leases or record a hard expiry bound, so cloud providers are
not opted in merely because they support a coordinator.

Static hosts, local containers/VMs, fixed-create intents (including terminal
receipts), pending or active runtime-adapter registrations, coordinator
registrations, and checkpoint capture bindings are retained. Unparseable files,
invalid timestamps, oversized files, and nonregular paths are retained and
reported. `--dry-run` performs no removal and takes no claim operation locks.

JSON output contains `version: 1`, `source: "local-claims"`, `dryRun`, `eligible`
and `pruned` arrays of lease IDs, a `kept` count, and the bounded `problems` array.
`kept` includes files whose removal could not be confirmed; a directory-sync
error can occur after unlink, so consult `problems` and `claims list` on failure.
In addition to inventory problem codes, pruning can report `invalid_timestamp`
or `claim_not_removed` (concurrent modification or removal failure). Problems
produce exit code 2 after output; cancellation or state-directory failures produce
a nonzero exit. Partial removals are included in output if a pass is interrupted.

### Automatic maintenance

After a successful explicit `stop` or `release`, Crabbox opportunistically prunes
claims older than seven days. It runs after the command's main work and output,
never before lease acquisition. A pass has a five-second cooperative budget and
honors command cancellation, including while waiting for claim operation locks.
An individual filesystem call cannot be interrupted by the context.

An atomically written `claims-prune.stamp` in the state directory limits complete
passes to once per 24 hours. Incomplete passes leave the stamp unchanged and save
a separate filename cursor, so the next successful stop resumes after already
processed files. A separate maintenance lock prevents overlapping passes; files
added behind a saved cursor are considered on the following full pass. Maintenance
errors do not change stop/release success and are silent unless `CRABBOX_DEBUG=true`.

Disable automatic maintenance in user or repository config:

```yaml
claims:
  autoPrune: false
```

Or set `CRABBOX_CLAIMS_AUTO_PRUNE=false`; the environment overrides config. The
explicit `claims prune` command remains available when automatic pruning is off.
