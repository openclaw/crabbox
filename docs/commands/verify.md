# verify

`crabbox verify` checks a signed run receipt produced by `crabbox run --attest`.
It recomputes the receipt's canonical bytes, verifies the embedded Ed25519
signature, and prints the signer's public key fingerprint, so a reviewer can
confirm a pasted receipt has not been edited after the run.

```sh
crabbox run --attest receipt.json -- go test ./...
crabbox verify receipt.json
```

On success it prints one line and exits 0:

```
PASS receipt.json signer=sha256:6a5f...
```

If any signed field was modified after signing, it prints a failure line and
exits 1:

```
FAIL receipt.json signer=sha256:6a5f...: signature mismatch
```

A receipt that cannot be checked at all (unreadable file, invalid JSON, a
missing or malformed `public_key` or `signature`) exits 2 with an error on
stderr.

## Receipt format

The receipt is a flat JSON object. `schema_version`, `generated_at`,
`provider`, `lease_id`, `command`, `exit_code`, `command_ms`, and `public_key`
are always present; `slug`, `run_id`, `actions_url`, and `log_sha256` appear
when the run produced them. `log_sha256` is the SHA-256 of the combined
stdout and stderr stream as observed by the client during brokered SSH runs.

The signature covers the canonical encoding of every field except `signature`
itself: the object is re-marshaled as compact JSON with lexicographically
sorted keys, and the Ed25519 signature is computed over those bytes. Because
`public_key` is inside the signed payload, swapping in a different key also
fails verification.

## Keys

Signing uses a per-user Ed25519 key that `crabbox run --attest` mints on first
use at `<user config dir>/crabbox/attest/id_ed25519.pem` (PKCS8 PEM, file mode
0600). Pass `--attest-key <path>` to `crabbox run` to sign with an existing
PKCS8 PEM Ed25519 key instead; the override path is never auto-created.

Verification trusts the key embedded in the receipt and reports its
fingerprint. Binding that fingerprint to an identity is out of scope: compare
it out of band with the signer.

## Non-goals

`crabbox verify` proves integrity and possession of the signing key, not
identity or execution provenance. There are no trust roots, no transparency
log, and no coordinator-held keys in this version.
