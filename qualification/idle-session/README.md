# Idle qualification authority: replace an AWS session

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
  `recoveryStopsAt` (start +60 minutes), `previousSessionExpiresAt`,
  `cycleRecoveryBy` (after recovery, no later than start +8 hours),
  `authorityRetentionEndsAt` (at least cycle recovery);
- optional boolean `allowUnexpiredSessionReplacement`. Unless explicitly `true`,
  `previousSessionExpiresAt` must be no later than `startsAt`. Early replacement
  additionally requires stopped-use confirmation in both custody records below.
  Replacing the installed credentials does not revoke the previous session; it
  remains valid until its recorded expiry.

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
When `previousSessionExpiresAt` is later than `startsAt`, both records must also
set `previousSessionUseStopped: true`, confirming previous-session consumers have
stopped. This is required in addition to the plan's explicit opt-in and all idle,
settlement and exclusive-custody checks.
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
Before delivery the full raw settings digest must match exactly. After confirmed
delivery, only Cloudflare's `annotations["workers/triggered_by"]` transition from
`version_upload` to `secret` is also accepted: restoring that one value must
reproduce the original full digest. Other annotations and all configuration
remain checked. The first accepted inspection records `afterSettingsSha256`, the
full raw post-delivery settings digest. Later inspection and recovery must match
that exact digest, including the provenance annotation.

After creating the post proof through the existing idle owner, `reseal` runs
`idle-controller-recover` in a separate process using its original directory and
requires a newer durable recovery receipt. It rechecks management identity and
writes `candidateIsolation` with the exact new version and
`enrollmentAdmitted:false`. This is a resealing candidate, not permission to enroll
or proof that AWS execution will work. The independent source/artifact/handoff,
credential applicability, protected approvals and finite observation-cycle gates
remain required. No automatic rotation or renewal exists; recovery does not
restore expired credentials or delete namespaces.

The post-idle owner's plan must use `afterSettingsSha256` for its
`authoritySettingsSha256`; the original pre-idle plan and session plan retain
their original digest. Freeze the post plan only after inspection succeeds.
Changing this helper does not authorize rewriting an in-flight operation's
source pins, plan, intent or deadlines.

### Reconcile a confirmed predecessor delivery

For the predecessor affected by the provenance mismatch, the corrected CLI has
one recovery-only option:

```sh
node scripts/image-qualification-session-replacement.mjs inspect /absolute/private-attempt --original-source /absolute/original-clean-checkout
node scripts/image-qualification-session-replacement.mjs reseal /absolute/private-attempt --original-source /absolute/original-clean-checkout
```

Use separately accepted corrected source and set `QUALIFICATION_SESSION_SOURCE_SHA`
to that executor's commit. Keep `QUALIFICATION_SESSION_PLAN_SHA256` bound to the
original plan. The original checkout must be clean, canonical, at the original
plan's commit, and contain the exact supported predecessor helper. Every other
effect-bearing dependency must match the corrected executor. The complete commit
trees may differ only in this helper, its test and this README. Other source
changes require separate compatibility work; this is not a general migration.

Only an existing `delivery-confirmed`, `reseal-candidate` or `resealed` journal is
accepted. Prepared or uncertain issuance/delivery cannot use this option; it
cannot issue or deliver credentials. The original exclusive issuance marker,
plan hash and all deadlines remain mandatory. The journal records and pins the
corrected `recoverySourceSha` before reconciliation. Further calls must use that
same recovery executor. Create the post-idle proof with the original checkout
and its original source identity, using the newly inspected raw settings digest.
Independent recovery also executes that exact original idle owner. The option
does not extend an expired recovery deadline or grant enrollment permission.

## Credential-free checks

Use Node from `.node-version`:

```sh
node --test scripts/image-qualification-session-replacement.test.mjs
node --test scripts/image-qualification*.test.mjs scripts/image-qualification*.test.js
```

Tests use fake credentials and local processes only. They do not establish live
serving identity, custody, AWS permissions, private exposure or representative
checkpoint performance.

## Reusable first-walk permissions and run sessions

`image-qualification-run-session.mjs` generates a fixed managed-policy set,
bootstrap-user trust and issuer-policy proposal from an operator-reviewed JSON
configuration. It makes no AWS requests. Configuration has exactly `accountId`,
`region`, `roleArn`, `issuerArn` (an IAM user, not a role), `subnetId`,
`securityGroupId`, `baseAmiId`, `availabilityZone`, and `rootGB` (8–20). Keep real
configuration, generated policies and operation receipts private. Install only
under separate live authorization, retaining the existing permissions boundary.

The fixed grants preserve approved stock AMI/network, small instance types,
required metadata tokens, encrypted bounded gp3 storage, owned snapshot sources
and typed data-copy attachment. `ModifyInstanceAttribute` retains its prior
ownership-scoped IAM permission. AWS's `ec2:Attribute` condition cannot constrain
the complex block-device array. The existing typed first-walk owner alone constructs
the exact delete-on-termination request; the generic candidate action proxy does
not admit this action. IAM does not validate that array's contents. Verify this
exact request in the later live DryRun before allocation. EC2 describes remain regional management
reads under the existing read grants; AWS does not offer resource-tag filtering
as an authorization boundary for those actions. The existing authority still
checks exact IDs, request shapes, resource limits and source-use protection.

The role trust admits only the existing bootstrap IAM user and requires exactly
the four run/owner/source/expiry session-tag keys, a matching role session name
and no transitive tags. Role permissions deny IAM changes, session delegation,
tag removal and changing the data-copy marker. Role/session tags do not prove
workload provenance. The trusted issuer must validate the complete frozen plan,
reviewed policy outputs and hosted build/artifact identities. IAM trust cannot
validate the content or presence of the `AssumeRole` inline `Policy` argument;
that enforcement belongs to the trusted issuer. Candidate and executor code
receive neither its key nor authority credentials and cannot choose sessions.

For new delivery, add `runSession` to the existing replacement plan. It has
exactly these nonsecret fields:

- `profile: "first-walk-aws-session/v1"`, `repository`, `workflowSha`, `candidateSha`;
- `runId: "image-qualification-<workflow-run-id>-1"`, `candidateArtifactId`,
  `candidateArtifactDigest` (including `sha256:`), `manifestSha256`;
- canonical ISO `startsAt`, `workExpiresAt` (start +130 minutes), `expiresAt`
  (start +150 minutes), `recoveryExpiresAt` (start +270 minutes).

Scope start must fall within the existing preparation/recovery hour, and
`cycleRecoveryBy` must equal `recoveryExpiresAt`. Freeze this scope after the
credential-free build and hosted artifact verification, before admission.
Issue/deliver/reseal while the existing owner is idle. Then update the protected
`CRABBOX_IMAGE_QUALIFICATION_AWS_SESSION_JSON` variable to exactly that descriptor,
plus the existing authority-version/isolation readbacks, before approving the
handoff. Delivery changes the serving version: publishing a handoff first is
invalid. Admission requires the scope window already active. Preparation consumes
that fixed window; never move its start or extend deadlines to rescue an attempt.
Check sufficient time for the full diagnostic and settlement before allocation.

The protected admit/deploy/arm jobs bind that scope to hosted run, source,
artifact ID/digest and manifest. Its hash is part of the immutable handoff's
configuration hash. Missing/mismatched scope fails closed for first-walk. No AWS
credential enters the handoff, candidate build or ordinary executor. Non-first-walk
modes ignore the shared descriptor and retain their handoff formats; this issuer
does not create new unscoped or mint/retained sessions. Historical inspect/reseal
and existing recovery checkouts remain available; never rewrite their source pins
to this revision.

The existing direct-user issuer requests twelve hours and rejects ambient
session credentials, avoiding AWS's one-hour role-chaining limit. It passes only
four tags and one compact, literal-date session policy (at most 2,048 characters;
no managed session-policy ARNs). Managed permissions are not squeezed into STS.
Forward actions stop at the forward cutoff. Same-run deletion and inventory
remain usable through the separate recovery cutoff; all authorized session work
stops then even though STS credentials last longer. A later executor failure is
recovered by the existing finalizer/reaper using the authority-held same-run
session. At least one hour of credential life must remain beyond planned recovery.
There is no active-owner renewal or automatic recovery after credential expiry,
revocation or the policy recovery cutoff. That case needs a separately admitted
recovery decision; never issue a broader replacement or delete recovery access.

Migration requires canonical idle/settled state and separately approved IAM/trust
installation. Remove old run grants only after their exact owner, cloud resources,
source pin, callers, registry and independent recovery are settled. Reuse fixed
policies across attempts; remove them only at investigation closeout. Rollback
first stops forward admission, preserves active sessions/grants and supported
original recovery source, and settles them before changing grants or trust.
Changing trust does not revoke already issued credentials. Do not replace an
active authority session with a legacy broad session.

Credential-free tests cover policy construction, the AWS action/resource/condition
matrix, same/cross-run decisions, malformed identities, deadlines, issuer intent,
uncertain responses, cancellation, fresh-process recovery and protected handoff
binding. The local evaluator is not IAM simulation. Before use, obtain separate
authorization for policy validation/DryRun plus one disposable synthetic full
cycle, including executor loss, independent recovery and exact zero-residue/source
preservation proof. No production/campaign input or first-walk performance claim
follows from these checks.
