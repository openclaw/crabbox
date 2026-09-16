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

## Related docs

- [run](run.md)
- [verify](verify.md)
- [History and logs](../features/history-logs.md)
