# AWS Image Qualification Authority

This is a pre-merge test boundary for a dedicated AWS sandbox account. It lets an
exact candidate coordinator execute Crabbox Fleet authentication, serialization,
AWS provider lifecycle, image catalog compare-and-swap, revision retirement, and
publisher rollback without receiving AWS or Cloudflare credentials.

It is not a general AWS proxy and is not enabled by default.

## Trust boundary

There are four Workers:

1. A protected controller selects the named `AWSQualificationController`
   entrypoint to enroll, fence, and finalize runs.
2. The exact candidate Worker has no public endpoint. It has only
   `CRABBOX_AWS_QUALIFICATION_TRANSPORT`, bound to the authority's default
   transport entrypoint.
3. A trusted narrow relay is the only workers.dev endpoint exposed to the
   credentialless executor. Its sole service binding targets the private
   candidate Worker.
4. The non-public authority Worker holds the sandbox AWS credentials and signs
   admitted calls.

The relay accepts a distinct ephemeral executor token and injects the candidate
admin token only for the fixed lease, image, and run API methods needed by the
publisher proof. One separate path injects the candidate shared token only for
the `/promote-cas` spoofed-admin probe. It rejects unknown paths, methods,
queries, content types, oversized bodies, and identity/header overrides before
calling the candidate. Callers cannot choose a target Worker, forwarding
headers, or destination URL. The relay has no controller or authority binding
and receives no AWS or Cloudflare credential. It strips literal candidate auth
tokens from bounded responses before returning them to the executor.
The relay rejects an absent or expired run timestamp and rechecks expiry
immediately before candidate dispatch.

The publisher proof records a seeded base revision and failed candidate revision.
It requires the rollback receipt to restore the exact seeded default aliases and
revision, retire the failed candidate revision, and reject a stale
compare-and-swap request naming that failed revision. Full candidate API
readbacks before and after that stale
request must match, including catalog, default, and Fast Snapshot Restore state.
A retired AMI may remain visible as a matching provider-only record, but it
must have no revision, promotion timestamp, or catalog-only marker.

Protected teardown first persists the authority and registry finalization fence,
then disables and verifies the relay's public endpoint and deletes the relay
Worker before running idempotent authority cleanup. Requests already admitted by
the relay cannot reach the AWS signer after that fence, and later requests have
no public credential-injection path to the private candidate.

The candidate binding has immutable `ctx.props` containing `runId`, `owner`,
`candidateSha`, `candidateWorker`, `deploymentHash`, and `expiresAt`. The
controller binding carries the same deployment hash. Enrollment stores the
complete fixed policy and its hash plus the authority source SHA and version;
candidate operations fail closed if the deployed policy later drifts.
The deployment hash also commits to the trusted relay source, fixed bindings,
candidate service target, and digests of all three ephemeral tokens. The final
execution manifest additionally binds the exact deployed candidate and relay
Worker versions.
The authority requires an enrolled exact match.
The absolute expiry must be in the next 120 minutes. A transport failure retries
the same operation ID once and then fails closed; it never falls back to raw
credentials, even if stray credential variables are present.

The authority accepts a structured service, action, and parameter map. It never
accepts a caller-selected URL, HTTP method, headers, or encoded body. It rebuilds
the AWS request after policy validation.

## Fixed sandbox policy

Configure these authority variables:

```text
CRABBOX_AWS_QUALIFICATION_ACCOUNT_ID
CRABBOX_AWS_QUALIFICATION_AUTHORITY_SHA
CRABBOX_AWS_QUALIFICATION_AUTHORITY_VERSION
CRABBOX_AWS_QUALIFICATION_REGION
CRABBOX_AWS_QUALIFICATION_SUBNET_ID
CRABBOX_AWS_QUALIFICATION_SECURITY_GROUP_ID
CRABBOX_AWS_QUALIFICATION_BASE_AMI_ID
CRABBOX_AWS_QUALIFICATION_ROOT_GB
```

The account, Region, subnet, preprovisioned security group, and base AMI are
fixed by the authority deployment. `CRABBOX_AWS_QUALIFICATION_ROOT_GB` must be
8-20. A run may launch at most three sequential on-demand `t3.small` or
`t3a.small` instances, with only one active at a time. This permits the source,
candidate-image, and promoted-image smoke phases while bounding compute to
at most 120 minutes of aggregate instance runtime.
Each launch has an encrypted
`gp3` root volume, no instance profile, IMDSv2 required, and authority-injected
owner/run/SHA/expiry/operation tags.

The run may own one physical key pair and one active AMI with at most one child
snapshot. Candidate traffic is capped at 64 operations, eight unresolved
intents, and the 120-minute expiry. The authority accepts only the five request
fields in the service binding contract, bounds the complete envelope to 64 KiB
before normalization, and requires a flat map of at most 256 string parameters.
Responses are also capped at 64 KiB. Cleanup and inventory calls do not consume
candidate capacity.

The preprovisioned security group is read-only to qualification runs. Its ingress
must already admit the trusted smoke executor. The authority neither creates nor
edits security groups, so final verification proves that the configured group
still exists rather than claiming that it was ephemeral.

Fast Snapshot Restore is rejected by the Worker. The AWS permissions boundary or
SCP must also explicitly deny:

```text
ec2:EnableFastSnapshotRestores
ec2:DisableFastSnapshotRestores
```

Do not grant `iam:PassRole`. The authority role's allow surface is limited to:

```text
sts:GetCallerIdentity
servicequotas:GetServiceQuota
ec2:CreateImage
ec2:CreateTags
ec2:DeleteKeyPair
ec2:DeleteSnapshot
ec2:DeregisterImage
ec2:DescribeImages
ec2:DescribeInstances
ec2:DescribeInstanceTypes
ec2:DescribeKeyPairs
ec2:DescribeSecurityGroups
ec2:DescribeSnapshots
ec2:DescribeVolumes
ec2:ImportKeyPair
ec2:RunInstances
ec2:TerminateInstances
```

Apply `aws:RequestedRegion` to all regional allows. Create and tag allows must
require the `crabbox_qualification_run`, `crabbox_qualification_owner`,
`crabbox_qualification_sha`, and `crabbox_qualification_expiry` request tags.
Mutation of existing resources must require the same run resource tag. Restrict
`RunInstances` to the configured base AMI ARN or an AMI carrying the complete
authority-injected qualification tag set, plus the configured subnet and
security group ARNs and the two allowed instance types. Do not grant a wildcard
AMI resource. IAM provides the outer tagged-image boundary; the authority then
requires the exact derived AMI ID to remain active in the enrolled run ledger,
so another run's or an unrelated tagged image is still rejected. Keep these IAM
constraints even though the authority duplicates them at runtime.

## Retained-image qualification

The protected `image-qualification.yml` workflow defaults to `mode=mint`.
Use `mode=retained` only for a reviewed, already available AMI and its single
backing snapshot. Supply `retained_image_id`, `retained_snapshot_id`,
`retained_source_sha`, and `retained_capsule_sha256` alongside the exact
candidate PR/SHA. This mode never prepares a source lease or captures an image.

The capsule is the SHA-256 of the control script's canonical JSON array of all
13 source records: `internal/cli/actions.go`; both devtools recipe files; the
builder, minimal, manifest-schema and profile-schema Linux recipe files; and
the devtools contract, full Linux smoke, readiness generator, tool installer,
generated readiness and AWS mint scripts. Each record contains the path,
Git mode/blob, byte length and SHA-256. Build admission verifies the reviewed
source capsule and requires all 12 recipe/runtime/proof inputs to match the
candidate checkout exactly. `actions.go` is recorded separately as nonbaked
CLI source; it may differ. This is input equivalence, not whole-source or
runtime equivalence. The candidate CLI and Worker are newly built from the
same admitted candidate SHA, including the current build-control contract.
Before deployment or allocation, retained bundle admission also requires the
installer's Node/pnpm, Go, Bun, Rust and uv smoke-generator declarations.
Admission reads bounded source text without executing candidate code; this
compatibility check does not replace source review or the full runtime smoke.

Retained mode keeps the existing authority account, Region, subnet and security
group. Before enrollment, the authority verifies the exact available, owned
x86-64 AMI, one EBS root mapping, its root-device name, and the exact completed,
owned 400 GiB snapshot. Both AMI root size and snapshot size must be 400 GiB.
The authority records the observed root device and uses it when rebuilding
the launch request. Existing IAM must already permit that exact AMI; a denied
AMI is a prerequisite failure, not permission to widen IAM or change networking.

The run authorizes at most one 400 GiB on-demand lease, with a 30-minute work
window and eight-minute cleanup reserve inside a 38-minute absolute lifetime.
The compute guard, volume size and expiry bound the operation; they are not a
provider billing cap. Obtain a separate spend authorization before dispatch.
At the work cutoff, new work is denied and independent authority cleanup starts.
Until absolute expiry, automatic cleanup permits scoped identity, image, instance
and key reads for receipt restoration and release in the isolated catalog.
Explicit protected finalization durably revokes that allowance at the run owner,
including requests admitted by the registry before revocation. Alarm retries and
object restarts cannot reopen it; pre-existing finalization state without an
automatic-cleanup marker grants no read allowance. A lost launch response is
reconciled by reads only: the authority never dispatches `RunInstances` again,
even with the same idempotency token.

The protected executor performs this sequence:

1. Seed the prior default in the isolated AWS/Linux/OS/Region/architecture
   catalog, then CAS-promote the retained AMI and capture the canonical receipt.
   Require successful CLI output, receipt persistence, and a valid restorable
   receipt before acquiring a lease. Partial receipts are not rollback evidence.
2. Clear the base AMI override and acquire one normal-selection lease for
   Ubuntu 24.04/x86-64 with desktop and browser enabled. Require the lease's
   AMI, Region, `source=promoted`, and recorded `image.revision` to match the
   promotion receipt. A missing legacy revision is unknown, never inferred
   from a timestamp.
3. Run strict generated readiness and the candidate's complete Linux smoke,
   including its archive probes. Only a successful full smoke can trigger
   the intentional exit 86; readiness and incidental commands cannot.
4. Restore the receipt's exact prior aliases and revision, verify retirement
   of the failed catalog revision, require stale CAS rejection with unchanged
   readback, and release the lease.
5. Prove independent cleanup and preserved borrowed image/snapshot state.

The retained AMI and snapshot are borrowed, not run-owned. They are excluded
from active ledgers, tombstones, recovered capture intents, inventory adoption
and deletion. Qualification changes only the isolated candidate catalog;
it does not promote a shared production default or delete the source checkpoint.
The final attestation must prove one launch, no image capture, matching selection
and catalog-revision evidence, zero owned residue and preserved borrowed resources.

Before running retained mode, the infrastructure owner must identify and approve
the protected deployment and credential-custody route for the reviewed authority.
The qualification workflow does not deploy that authority. Infrastructure landing
alone does not establish its availability, qualify an older candidate CLI, or
authorize paid execution.

## Enrollment and binding

The protected workflow separates preparation from deployment. After the candidate
bundle is built, approve **Prepare protected qualification handoff** to start the
absolute clock. This job admits the bundle without AWS, Cloudflare API, or
controller credentials and publishes one immutable
`image-qualification-handoff-<run>-1` artifact. Its `handoff.json` binds the
repository, protected workflow SHA, candidate SHA and artifact, run/attempt,
derived owner, capsule, authority identity, configuration digest, and deadlines.
Private account, network and resource coordinates are bound only by the digest;
they are not published in this record.

For retained mode, `iam-inputs.json` projects the same record into
`authority_owner`, `run_id`, `source_sha`, `starts_at`, `creation_expires_at`, and
`cleanup_expires_at`. Here `source_sha` is the **candidate SHA**, not the workflow
or historical image SHA. Preserve the exact owner, including `@example.invalid`.
The work cutoff is start plus 30 minutes and absolute expiry is start plus
38 minutes. Mint preparation retains its 120-minute limit and emits no
retained-role projection.

The infrastructure operator consumes this verified handoff through the separately
accepted deployment route, verifies exact IAM binding and authority credential
custody, and only then approves **Deploy after exact IAM binding and credential
custody**. Preparation does not apply IAM, issue credentials, deploy the authority,
or establish a cleanup operator. Those remain prerequisites, not effects of
approval. The workflow creates no provisional administrator credential route.

Deployment verifies the immutable artifact ID, digest, run and source, then
consumes the prepared identity without regenerating expiry. Preparation, central
application, credential delivery, approvals and uploads all consume the same
window. An elapsed work cutoff refuses deployment, enrollment and arm; approval
does not promise 30 remaining execution minutes. Delayed provider or registry
reads are rechecked before admission. A run already persisted before a late
refusal retains its cleanup ownership and alarm. Reruns and replacement handoffs
are rejected; an expired preparation requires a newly authorized run, not an
extension. Protected finalization and the independent reaper remain available.

Deploy the authority from
`worker/wrangler.aws-qualification-authority.jsonc`. That config has no route,
workers.dev URL, preview URL, or cron. Supply AWS credentials only to this Worker.

Protected tooling deploys the private candidate and binds the protected caller to
`AWSQualificationController` with the reviewed deployment hash, then calls
`claim` to enroll and activate that exact identity. Candidate execution remains
blocked until enrollment and protected arm succeed. The candidate service
binding props must exactly match the prepared and enrolled identity:

```json
{
  "binding": "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
  "service": "crabbox-aws-qualification-authority",
  "props": {
    "runId": "image-qualification-<run>",
    "owner": "<reviewed-owner>",
    "candidateSha": "<40-hex-same-repo-sha>",
    "candidateWorker": "<isolated-candidate-worker-name>",
    "deploymentHash": "<64-hex-candidate-deployment-hash>",
    "expiresAt": "<absolute-iso8601-within-120m>"
  }
}
```

The mint candidate configuration must use the same fixed Region, AMI, subnet,
security group, and root size; retained mode clears the AMI override and uses
the enrolled 400 GiB root policy. Both modes request `on-demand`; select only `t3.small` or
`t3a.small`; leave `CRABBOX_AWS_INSTANCE_PROFILE` empty; and leave
`CRABBOX_AWS_FAST_SNAPSHOT_RESTORE_AZS` unset. It must also disable routes,
workers.dev, preview URLs, and cron. Protected tooling first uploads an inert
bootstrap, disables workers.dev and verifies the setting, and only then uploads
candidate bytes. The inert bootstrap already carries the run-scoped transport
identity and Durable Object migration, so a finalizer or reaper can discover
and delete it after a partial deployment. The relay enables workers.dev with
preview URLs disabled and also has no routes, custom domain, cron, or Durable
Object namespace.

The protected controller also owns a singleton `AWSQualificationRegistry`
Durable Object. `claim` admits only one active qualification globally and is
idempotent only for the exact run, candidate Worker, candidate SHA, deployment
hash, and expiry. `discover` gives an independent reaper the active run and its
`claimed`, `finalizing`, or `finalized` cleanup state without relying on workflow
artifacts. `beginFinalization` persists the fence before external ingress
teardown, and `finalize` updates that state around per-run cleanup. `retire` clears
the active slot only after the persisted run attestation proves finalization,
is idempotent for the same bounded retirement tombstone, and rejects a different
active run. A retired or finalizing per-run object cannot be enrolled again.
These methods and `attest` exist only on the named controller entrypoint; the
candidate transport exposes only `execute`. The controller persists the per-run
cleanup owner before publishing its global claim, and every candidate call must
match that exact registry record while its state remains `claimed`. Protected
controller finalization records its source alongside the per-run `finalizingAt`
fence before transitioning the registry to `finalizing` or starting cleanup I/O,
and upgrades automatic cleanup to an irreversible controller fence. An already-admitted
candidate call and a failed cleanup cannot reopen candidate dispatch.
The relay name is deterministically derived from the registered run ID, so the
durable registry identity is sufficient for a fresh finalizer or reaper to
discover and delete the exact relay without workflow artifacts.

`attest(runId)` returns versioned evidence built from persisted Durable Object
state. It binds the run, candidate and authority revisions, deployed bundle and
policy hashes, timestamps, and finalized state. Each bounded operation record
contains only operation/request digests, action, a normalized denial reason,
and persisted signer before/after sequence points. The final receipt records
resource counts and pending intent digests at cleanup start, every bounded
cleanup attempt, each inventory and verification outcome, final zero counts,
and normalized failures. Cleanup, inventory, and verification evidence use
bounded rings with total and truncation counters; saturation drops the oldest
proof records without blocking teardown. The attestation never returns raw
tokens, keys, user data, URLs, account IDs, network addresses, or AWS resource
IDs.

## Replay and teardown

Each run has one Durable Object. Before a mutation, it persists the operation ID,
canonical normalized-request hash, and a prepared intent, then marks the intent
dispatched immediately before the signer call. Prepared intents are deleted
without recovery. Finalization uses action-specific read reconciliation for
dispatched or legacy ambiguous launch, termination, image, and key intents. It
never redispatches generic candidate mutations; those intents remain until
inventory, cleanup, and zero-residue verification allow their retirement.
A completed receipt is replayed only for the same hash. `RunInstances` receives
an authority-derived deterministic `ClientToken` derived from the run and
operation IDs. Uncertain `CreateImage` and `ImportKeyPair` results retain their
intents and use bounded read reconciliation; they are never blindly reissued.
Image reconciliation uses authority-injected operation tags. Imported key names
are authority-generated; ownership is recorded only after ID, public key, and
run tags are read back.
If bounded reconciliation proves neither success nor a definitive error, the
intent remains until final inventory and cleanup prove that no run-owned
resource remains; only then is it retired.
Finalization never redispatches a pending `RunInstances` request. It performs
bounded read-only discovery by the authority-injected run and operation tags,
then cleans any discovered instance; a no-effect launch intent is retired only
after the same zero-residue proof. Lost termination responses use bounded
single-instance reads and require repeated absence or an explicit terminal state
before the intent and active slot are retired.

The ledger learns only IDs created by, or discovered beneath, the registered
run. Candidate reads and mutations are restricted to those IDs, except the fixed
base AMI and security group. Lifecycle deletes remove current ledger ownership
instead of accumulating stale IDs; terminating instances remain active until a
single-ID read confirms `terminated` or absent. Before and after teardown, bounded
eventual-consistency inventory scans find resources that appeared after an
ambiguous response. The authority revalidates the configured STS account before
every mutation, so a replacement credential is accepted only when it resolves
to the enrolled account and fixed Region policy. This is an immediate identity
check around each protected qualification operation, not a promise that the
qualification signer retains one immutable credential object. Separately,
managed public AWS release through `AWSProvider.releaseLease` waits for
`TerminateInstances` acknowledgement and a terminal `terminated` or absent
observation; it does not depend on the qualification transport being active.
Successful deletes move exact instance, key-pair, AMI, and snapshot IDs into
tombstone sets bounded by the launch and candidate-operation limits. Tombstones
do not consume active-resource capacity or permit new use, but they preserve
exact verification reads and idempotent cleanup retries while continuing to
reject foreign IDs.
`finalize`
deregisters images, deletes snapshots,
terminates instances, deletes imported key pairs, and verifies zero run-owned
instance/volume/key-pair/image/snapshot residue. The expiry alarm runs the same
cleanup and retries incomplete teardown. Protected teardown fences candidate
mutations, deletes the public relay, deletes the private candidate Worker and
its Fleet Durable Object namespace, verifies their absence, repeats authority
finalization idempotently, and retires the registry record.

Cloud setup, enrollment, candidate deployment, and paid execution remain
explicit maintainer actions. Merging this seam creates no Worker, secret,
Durable Object namespace, AWS resource, or spend.

## Idle controller identity and recovery

The opt-in `idle-controller-run` and `idle-controller-recover` commands in
`scripts/image-qualification-control.mjs` use a separate `idle-controller/v1`
profile. Existing candidate, relay and generic reaper verification remains strict.
These commands do not admit AWS operations or first-walk execution. They require
an explicitly admitted protected tooling commit and a separately reviewed private
authority serving `AWSQualificationController.bindingIdentity` with the
`CF_VERSION_METADATA` version-metadata binding. Updating a local CLI alone does
not install that RPC. Do not merge a private runtime candidate as a shortcut to
production deployment.

The authority RPC returns its actual bound controller props, a fresh public
challenge, a fixed protocol/role and its executing version without accessing any
Durable Object, registry, credential or signer. The trusted controller adds its
executing version. The helper checks both versions and the entire reply, with
management/version fences before discovery. It may defer only the known service
binding's props representation; supplied-but-different props, extra capabilities,
foreign environments and mismatched source/author/version all fail closed. An
explicit default environment is accepted only when witnessed on the exact target.
Requested binding identity and visible binding identity use separate digests and
new tags. No legacy tag is converted and no expected props are copied into readback.

An idle-created controller is limited to `/binding-identity` and `/discover`.
Discovery requires the result `{ "run": null }`; active or unknown results stop
forward work. The helper still attempts ordinary cleanup of its newly owned caller.
Candidate self-report is not an authority attestation; full candidate transport
qualification remains blocked where its strict management readback cannot verify
props.

### Retained custody and frozen admission

Use the existing qualification owner and a retained operator filesystem, never an
ephemeral runner with only a post-run artifact. The operator and fallback must have
access to the exact private recovery directory and reviewed source. Before running,
record that custody and the finite cycle in the owner's durable inventory. Place
an immutable, exclusively created, user-owned 0600 `idle-controller-plan.json` in
a user-owned 0700 directory with no symlink ancestry. The helper independently reads
it in a fresh credential-free process. This does not protect against media loss,
hostile same-user rollback or concurrent trusted administrative changes.

The frozen plan includes `profile`, random 24-hex `attemptId`, exact 40-hex
`sourceSha`, `sources` from `idleSourceIdentity()`, independently evidenced token
and uploader IDs, original complete inventory digest, full authority settings
digest, exact single-module authority content identity, current full `isolation`
admission, witnessed `defaultEnvironment` (initially `production` only), exact
controller `props`, `custodyReceiptSha256`, owner and fallback. `uploadExposure`
explicitly admits the possible initial public/preview boolean pairs; it is not
inferred after upload. Private normalization and temporary authenticated public
controller exposure must be separately accepted. The controller name is fixed;
any existing controller is a collision, never a replacement or adoption target.

`startsAt`, `forwardStopsAt` and `deadline` bind a 20-minute cycle with the final
10 minutes reserved for settlement. `maxRequests` is 800 across the retained
attempt; half is reserved for cleanup. Each management/runtime request is bounded
by 15 seconds and the applicable deadline, and its budget is journaled before
dispatch. The credential must match the frozen token ID and outlive the full cycle
by five minutes. These limits do not create a provider billing cap or authorize
spending. No cloud plan is generated or admitted automatically.

With current separately authorized credentials delivered through the existing
custody route, set `QUALIFICATION_IDLE_SOURCE_SHA`,
`QUALIFICATION_IDLE_PLAN_SHA256` (canonical plan digest), `CLOUDFLARE_ACCOUNT_ID`
and `CLOUDFLARE_API_TOKEN`. Never put token values on argv or in the plan/journal.
Then use exactly one of:

```sh
node scripts/image-qualification-control.mjs idle-controller-run /private/recovery-directory
node scripts/image-qualification-control.mjs idle-controller-recover /private/recovery-directory
```

The state and lock names are `idle-controller-state.json` and
`idle-controller.lock`. Durable create/exposure/delete intents precede mutations.
Identity/discovery results are not restart grants. A fresh process is cleanup-only:
it cannot upload, normalize exposure, call the identity RPC, discover or finalize
a run. It re-proves creation ownership of this attempt's exact first/single
controller version and performs at most one ordinary DELETE per recovery
invocation. It never uses force, retries PUT, adopts a new version or deletes the
authority/candidate/relay/namespaces. No incoming references, target-owned namespace,
domain, route or schedule is allowed. Source/module/version/capability drift stops
cleanup rather than weakening proof.

An unobserved uncertain upload plus absence stays UNKNOWN. A previously proven
owned caller followed by a lost DELETE response can settle only after fresh exact
404, complete inventory absence/reference checks and unchanged authority/namespace
proof. Failed fresh verification clears inherited settlement success. Missing,
corrupt or mismatched original custody is not recreated. The lock is not reclaimed
automatically: the operator must establish that the old executor and in-flight
requests have stopped before ordinary local lock removal. Keep the receipt, source
and cleanup credentials while state is unknown. Recovery cannot extend the frozen
window; an expired window requires explicit new bounded recovery authority through
the existing owner. Existing authority cleanup deadlines are unaffected.

### Local source qualification

Use the Node version in `.node-version`. In the private runtime checkout, run
`npm ci --prefix worker`, `npm test --prefix worker -- aws-qualification-authority.test.ts`,
`npm run check --prefix worker` and
`npm run build:aws-qualification-authority --prefix worker` (dry-run only).
In the tooling checkout, run:

```sh
npm ci --prefix worker
node --test scripts/image-qualification*.test.*
node scripts/image-qualification-idle-local.mjs /absolute/runtime/worker/dist-aws-qualification-authority/aws-qualification-authority.js
```

The last command executes the exact controller and runtime bundle in local workerd,
with outbound traffic denied and registry/credential bindings absent. It tests
real service props, version metadata and negative routing/schema cases. The Node
suite additionally injects lost responses, cancellation, drift and actual process
death at write-ahead boundaries. These are source/local-runtime proofs, not live
Cloudflare upload/readback/recovery qualification and not bootstrap speed evidence.
A substantive patch still needs fresh independent review and separate private
rollout admission before any live cycle.

## Candidate transport proof (opt-in v2 source contract)

`QUALIFICATION_TRANSPORT_PROFILE=candidate-transport-v2` selects a separate ownership
profile. Inert and staging candidate versions have no qualification service binding.
The final version carries the intended complete run identity in a nonsecret intent
binding. Requested props and observed visible coordinates remain distinct. Only this
profile normalizes the observed missing service props and explicit `production`
default environment. Supplied different props, extra capabilities, mixed tags,
foreign namespaces, module/uploader/version drift and legacy mismatches still fail.
The intent coordinate alone proves no runtime binding.

Before enrollment, the trusted controller opens a random 256-bit challenge in the
existing `AWSQualificationRegistry`. One slot stores the complete intended identity,
fixed candidate version, pinned executing authority version, and at most ten-minute
expiry. It performs no signer, network, run or AWS operation. The authenticated relay
permits only `POST /v1/qualification/transport-probe` with one bounded nonce. Normal
coordinator admin authentication applies before the candidate forwards that nonce;
the actual authority transport reads its own `ctx.props` and executing version.
Caller-supplied identity/version is never accepted. The protected controller reads
this canonical observation directly; the candidate's HTTP status, body and headers
are ignored as evidence. Management fences bracket proof and are checked again
before claim. Execution admission also requires the retained canonical verified slot.

Pending, observed, expired or uncertain probes block legacy and new claims and
legacy idle discovery. Registry serialization covers the entire enrollment hop,
including external verification, so admission cannot race a probe check followed by
enrollment. Explicit successful settlement precedes enrollment. Cancellation settles
the exact slot without granting proof; that identity cannot then claim. A settled
slot retains its replay tombstone until expiry, blocks another probe during that
window, and can then be replaced; there is no per-attempt key growth or new alarm.
Expiry alone never establishes cleanup. The challenge and nonsecret intent are
persisted in protected local custody before open. An uncertain response must be
reconciled without candidate redispatch or a new challenge.

Use the same protected v2 source/profile for the existing `reap` command before
admitting a v2 run. It can discover the canonical slot without local artifacts or
a surviving candidate/controller, then bind a recovery controller to its exact
owner for settlement. Probe settlement precedes ordinary run/resource cleanup.
Creation tags remain independently usable for owned Worker/namespace cleanup even
when proof fails. Denied reads, unknown ownership and unresolved responses retain
custody; they do not authorize replacement or deletion. Keep pinned source,
credentials, original recovery directory and independent recovery access until all
resources and registry state settle. Do not downgrade the authority beneath an
unsettled probe or run: older source does not understand this fence.

This is a source capability, not hosted admission. Existing protected workflow
configuration does not automatically enable it. A hosted run needs separately
reviewed exact-source/artifact admission, authority and protected tooling updates,
credential instructions and v2 recovery configuration before dispatch. No production
rollout, AWS execution or representative checkpoint improvement follows from local
qualification. `node scripts/image-qualification-transport-local.mjs` exercises the
exact dry-built authority/candidate plus raw controller/relay in local workerd, with
all Worker outbound denied, persistent registry restart and process-group death at
pending/observed/verified boundaries. Version metadata there is a controlled fixture;
it is not a live provider deployment proof.

The protected `image-qualification` environment variable
`CRABBOX_IMAGE_QUALIFICATION_TRANSPORT_PROFILE` selects this profile for admission,
deployment, arm, finalization and the independent reaper. Set it to
`candidate-transport-v2` only after the exact authority and v2 recovery tooling have
been admitted. An absent or empty value retains strict legacy behavior; every
other value is rejected before provider deployment. Keep the setting unchanged
until all probes, runs and cleanup settle. Installing the tooling alone neither
enables v2 nor updates the authority. The ordinary deploy command proceeds from
transport proof into enrollment and AWS verification; it is not a transport-only
hosted smoke.
