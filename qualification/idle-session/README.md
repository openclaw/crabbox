# Idle qualification authority: replace an expired AWS session

This operator-only adapter changes no production runtime, bootstrap recipe,
workflow or automatic deployment path. It is not a performance repair. It uses
`image-qualification-session-replacement.mjs` with the existing private authority,
isolation admission, idle-controller journal and recovery owner. The original
initial-install helper and its one-use intent remain unchanged.

Source preparation, merging tooling, and prior cloud permissions do not authorize
credential access, delivery, temporary caller deployment or a qualification run.
Obtain separate operation approval with current custody, fallback operator and
cleanup deadlines. Never use this to reset an active or uncertain run.

## Delivery boundary

The adapter issues one 12-hour session through a pinned AWS CLI with SDK retries
explicitly disabled. It verifies the assumed account, role and attempt-specific
session name using the new credentials. Only the three session fields reach the
existing authority; bootstrap credentials never do. Secrets stay in process
memory, intended child environment and the HTTPS delivery body, never argv,
journals or files. The AWS CLI has no inherited configuration, endpoint overrides,
metadata credentials, debug flags or ambient session token.

Delivery uses the documented [bulk-secret PATCH API](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/secrets/methods/bulk_update/)
through the existing management client, once, without redirects or retries.
Unmentioned secrets are preserved. This deliberately does not invoke Wrangler:
its pinned `secret bulk` implementation can create a draft Worker after a
Worker-not-found response, loads dotenv files and writes debug logs by default.
There is no Worker-create/upload fallback in this adapter. Its only management
write is the exact admitted authority's three-secret PATCH. All other requests
are bounded management reads. Raw bodies and errors are never printed by the CLI.

## Frozen inputs

Keep the original recovery directory at a canonical absolute path, owned by the
operator, mode 0700, without symlink ancestors. All JSON inputs must be owned
regular mode-0600 files without links, at most 1 MiB. These are private operational
records; do not commit or upload them as public artifacts.

`session-replacement-plan.json` contains:

- `profile: "idle-session-replacement/v1"`, unique 24-lowercase-hex `attemptId`
  and distinct `postIdleAttemptId`;
- exact clean committed `sourceSha`, `sources` from `replacementSources()`,
  canonical absolute `recoveryDirectory` and admitted Cloudflare `tokenId`;
- original `isolation` using the existing admission schema, raw `authorityModule`
  identity `{module,count:1,sha256}`, full canonical settings digest
  `authoritySettingsSha256`, and `preCustodySha256`;
- `tools` with exactly `aws` and `node`, each `{path,sha256}`. Paths must be
  canonical absolute regular files. Pin the actual Node executable;
- ISO timestamps `startsAt`, `deliveryStopsAt` (start +20 minutes),
  `recoveryStopsAt` (start +60 minutes), `previousSessionExpiresAt` (no later
  than start), `cycleRecoveryBy` (after recovery, no later than start +8 hours),
  `authorityRetentionEndsAt` (at least cycle recovery).

The plan hash is `digest(canonical(plan))`, not the serialized file hash. Node and
AWS CLI installations, their dependencies, the operator host and its administrators
are trusted. Entry hashes do not attest an entire tool installation. Freeze tools
and all source during the operation. Checksums, local state integrity and operator
assertions are not authenticated history or protection from a hostile same-user
writer.

`pre-custody.json` and `post-custody.json` contain
`profile: "idle-session-custody/v1"`, `attemptId`, `sourceSha`, exact
`authorityVersion`, `exclusiveUntil` equal to `recoveryStopsAt`, `observedAt`
(no older than 15 minutes), and all of these true:
`continuousExclusiveCustody`, `noConcurrentWritersOrCallers`,
`noActiveWorkflowWriters`, `previousRunsSettled`, `fallbackReady`.
The pre record is frozen by `preCustodySha256`. These are explicitly trusted
operator assertions. They require exclusive writer/caller custody throughout;
management readbacks cannot provide an atomic concurrency lock. The operator must
substantiate the old session's expiry and prior settlement, never infer them from
an empty inventory. A fresh source-specific private authority compatibility proof
is required before accepting idle-owner evidence.

`pre-idle/` and `post-idle/` contain the original
`idle-controller-plan.json` and `idle-controller-state.json` from the existing
owner, not hand-constructed replacements. Plans bind this same source, token,
module, settings and the corresponding version's isolation admission. States must
show settled, successful, canonical idle discovery within 15 minutes. Discovery
refuses unsettled transport probes, including expired probes; settled tombstones
remain. Post proof must use `postIdleAttemptId` and follow confirmed delivery.

## Execute and recover

Use the pinned Node and clean reviewed checkout. Set the nonsecret
`QUALIFICATION_SESSION_SOURCE_SHA`, `QUALIFICATION_SESSION_PLAN_SHA256` and
`CLOUDFLARE_ACCOUNT_ID` to the frozen identities. Resolve the separately authorized
Cloudflare token only in process memory. For `deliver` alone, resolve the admitted
bootstrap key fields as `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`; no
`AWS_SESSION_TOKEN` is accepted. Do not place values in shell history or argv.

```sh
node scripts/image-qualification-session-replacement.mjs deliver /absolute/private-attempt
node scripts/image-qualification-session-replacement.mjs inspect /absolute/private-attempt
# Obtain the fresh post-delivery idle-owner proof before this command:
node scripts/image-qualification-session-replacement.mjs reseal /absolute/private-attempt
```

`deliver` durably records issuance intent and an exclusive, fsynced
`session-issuance-intent.json` before STS, then delivery intent before PATCH.
The exclusive marker remains even if the main journal is lost. Never clear it,
copy an attempt to a different path, recreate a journal to retry, or issue a new
attempt to evade an uncertain result. POSIX child groups have output/time limits,
are killed and settled after completion/cancellation, and must not detach their
own children into another group. Failure to establish settlement denies progress.
A crash-held journal lock requires the existing owner/operator recovery procedure;
do not blindly remove it while a process or effect might still be active.

`inspect` never issues or delivers. An uncertain issuance/delivery stays unadmitted,
even if a new version exists. Known command success still requires exact new
single-version readback, unchanged raw module, full settings, namespaces and
private exposure, and no foreign clients. Preserve the original admission.

After creating the post proof through the existing idle owner, `reseal` runs
`idle-controller-recover` in a separate process using its original directory and
requires a newer durable recovery receipt. It rechecks management identity and
writes `candidateIsolation` with the exact new version and
`enrollmentAdmitted:false`. This is a resealing candidate, not permission to enroll
or proof that AWS execution will work. The independent source/artifact/handoff,
credential applicability, protected approvals and finite observation-cycle gates
remain required. No automatic rotation or renewal exists; recovery does not
restore expired credentials or delete namespaces.

## Credential-free checks

Use Node from `.node-version`:

```sh
node --test scripts/image-qualification-session-replacement.test.mjs
node --test scripts/image-qualification*.test.mjs scripts/image-qualification*.test.js
```

Tests use fake credentials and local processes only. They do not establish live
serving identity, custody, AWS permissions, private exposure or representative
checkpoint performance.
