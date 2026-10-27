# receipt

`crabbox receipt <run-id>` retrieves a brokered run's committed terminal
receipt, retained log, and terminal run record. It verifies the Ed25519
signature and exact coordinator-observable bindings before printing JSON.

```sh
crabbox receipt run_a1b2c3d4
crabbox receipt run_a1b2c3d4 --expected-signer sha256:<64-hex-digits>
```

Verification covers the run ID, lease ID, slug, provider, raw command digest,
final exit code, sync and command duration, coordinator start timestamp,
client-observed end timestamp, retained log hash, truncation state, public key,
and signer fingerprint. For an untruncated log it also independently checks the
full stream hash.

The receipt's command end timestamp is client-observed. Verification rejects an
end before the coordinator start or more than 30 seconds after the
coordinator-observed terminal timestamp. It does not prove the client could not
backdate its own signed timestamp.

The retained log is canonical UTF-8 text: each malformed output byte becomes
U+FFFD, and the byte-bounded tail starts only at a complete codepoint. Its
`retained_log_sha256` hashes exactly the text sent to and stored by the
coordinator. Valid UTF-8 within the cap is unchanged.

`log_truncated: true` means this text is not byte-complete: bytes were dropped
at the tail cap, lost through UTF-8 normalization, or routed exclusively to
local captures. The receipt's `log_sha256` still hashes the full **raw** stream,
not normalized text. The coordinator cannot independently recompute that raw
hash from an incomplete representation, but always verifies the retained-text
hash and signature. When `log_truncated: false`, it also independently checks
the full-stream hash. This uses the existing schema v2 fields and checks.

If the run has no committed receipt, the command exits without inferring
success or failure from logs, events, or lease state. The execution evidence
remains ambiguous.

The coordinator record is durable, but the caller still needs its run ID.
Interactive use can capture the printed `recording run ...` line. Automation
that must recover after losing client output should use `run --lease-output
<file>` on a supported retained run; the handle is written before remote
execution and includes `runID`.

The signer is self-signed. A passing receipt proves integrity under the embedded
key, not signer continuity, a human identity, or an external hardware trust
root. Use `--expected-signer sha256:<hex>` to require a fingerprint obtained
through a trusted channel or pinned by an orchestration campaign.

## Stored JSON contract

The coordinator stores the signed schema v2 object on the terminal run record.
The serialized receipt is limited to 16 KiB. Text fields are limited to 4 KiB;
`lease_id` and `slug` are limited to 256 bytes. Unknown fields, invalid types,
and unverifiable signatures are rejected before the terminal transaction.

Required fields are `schema_version`, `receipt_type`, `started_at`, `ended_at`,
`provider`, `run_id`, `command`,
`command_sha256`, `exit_code`, `sync_ms`, `command_ms`, `duration_ms`,
`log_sha256`, `retained_log_sha256`, `log_truncated`, `public_key`, `signer`,
and `signature`. `lease_id` and `slug` are optional only when the run record
does not have those identities.

## Recover an uncommitted original finish

Brokered signed finishes are retained privately before the first submission,
even when `run --attest` is not set. The pending outbox is under
`$XDG_STATE_HOME/crabbox/pending-finishes`, or the platform Crabbox config
directory's `state/pending-finishes` when `XDG_STATE_HOME` is unset. If terminal
submission or readback cannot be confirmed, the returned error includes the
exact file path, original signer fingerprint, and finish-request digest. These
pins stay with the failure diagnostic rather than adding progress output after
the final timing JSON. A normal confirmed finish removes its pending file. An
ambiguous or conflicting finish keeps it; import never deletes the supplied
original. If local cleanup fails after a confirmed commit, the original task
outcome stays committed and a cleanup warning is printed. A removal failure
leaves the exact pending file intact; a directory-sync failure means removal
durability is unconfirmed. With `--timing-json`, that late warning is followed
by the same frozen timing JSON again so the final line remains parseable. This
does not append another benchmark observation or resubmit the signed finish.

After resolving the transport failure, use the original coordinator and the
same authenticated owner and organization:

```sh
crabbox receipt run_a1b2c3d4 \
  --recover /private/path/pending-finish.json \
  --expected-signer sha256:<original-signer-64-hex-digits> \
  --expected-finish-sha256 sha256:<original-request-64-hex-digits>
```

Obtain both pins from the original trusted run, not an untrusted imported file.
The self-signed receipt authenticates its schema v2 fields, **not** arbitrary
finish metadata such as parsed test results or telemetry. The separately pinned
request digest binds those fields and the exact original HTTP bytes. The
versioned outbox wraps those bytes as base64 with their digest and original
coordinator, run, owner, and organization. A bare receipt is not a finish file.

For ordinary finalization with `CRABBOX_COORDINATOR_TOKEN_COMMAND`, the CLI
invokes the original configured helper once again after the task ends. It checks
the fresh identity and the recorded run against the admitted owner and organization, then
uses those same fresh credentials for the finish and receipt readback, including
bounded retries. It does not reuse an expired admission token or switch to a
later coordinator/helper. An administrator with a different owner or organization
is refused too. Static credentials keep their original admission binding.
Refresh or identity-check failures leave the signed original pending for recovery;
credentials are never stored in the pending file. This does not change the
schema-1 envelope or schema-2 signed receipt format.

Recovery checks the private regular file, schema, digest, signature, retained
log, run identity and original command/timing bindings before submission. It
then makes one ordinary authenticated, idempotent finish request, without
re-encoding the body, and verifies the committed run, receipt, and retained log.
The coordinator's existing terminal transaction accepts identical retries and
rejects conflicting finishes. Even an administrator cannot use this command to
substitute a different owner or organization. A failed submission or failed
readback is not reported as accepted; preserve the file and resolve the cause
before an explicit retry. There is no background retry loop.

These files contain retained task output and metadata: treat them as sensitive.
They do not store HTTP authentication headers or private signing keys. Retention
failure prevents submission rather than silently discarding the original.
An existing different finish for the same run is never overwritten.

This does not rerun a task, renew or retire a lease, reset a budget, release a
claim, or change an external collector's acceptance rules. A finish transport
failure still exits with code 7, but does not rewrite or re-sign the original
`--attest` receipt to that transport error. The task outcome and confirmation
of its terminal commit are distinct.

Older runs without a retained original finish cannot be reconstructed from
logs, result files, a bare receipt, or a newly generated signature. Recovery
requires the original bytes and trusted pins from native/coordinator retention;
if they are unavailable, this feature cannot recover that historical run.

## Related docs

- [run](run.md)
- [verify](verify.md)
- [History and logs](../features/history-logs.md)
