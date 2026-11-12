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

### Single-copy first ownership walk

`mode=first-walk` is an opt-in **synthetic qualification**, not a bootstrap
optimization or a representative checkpoint import API. It reuses this controller,
authority, registry, Fleet, relay, alarm and protected finalizer. Normal image
qualification and bootstrap rendering are unchanged. A client update alone cannot
enable it: both the private candidate and the qualification authority must include
the typed first-walk transport, and the protected executor must include its runner.

The credentialless executor boots one stock `t3.small`, writes a small mixed-owner
fixture under its saved work root and `/var/cache/crabbox`, and verifies those roots
share one filesystem. It creates one managed image checkpoint and releases the
source guest before booting a second stock guest. Only after successful stock
readiness does it hold an existing checkpoint-use claim and ask the authority to
create and attach one encrypted gp3 copy in the target's AZ. Restored data is never
present during boot. Launching the captured image as root is denied in this mode.

The existing run object persists the exact request before dispatch. Create intent
has one deterministic client token; uncertain create/attach responses reconcile by
reads and never authorize a replacement. The authority checks exact snapshot,
account, instance, AZ, volume, attachment and ownership tags. It sets and reads back
DeleteOnTermination. No raw CreateVolume, AttachVolume or DeleteVolume capability
is exposed to the candidate. The initial scope is one data copy, at most 400 GiB,
3000 IOPS and 125 MiB/s; no FSR, initialization request or force detach is admitted.
The synthetic source is limited by the existing 20 GiB stock-root policy.

The claim remains bound to the immutable qualification run/deployment through
attachment, execution, failures and cleanup, even past ordinary claim expiry.
Provisioning, abort, completion and checkpoint deletion cannot consume that claim.
Only an authority `finalized` readback can explicitly release it; a crash before
the first RPC is handled by the same proof. The private namespace is deleted only
after authority finalization. It may then discard its synthetic checkpoint and
held claim records; it is not an external source registry. Importing a checkpoint
from another Fleet is not supported by this mode.

The guest resolves **the exact copy volume ID** through its unique NVMe serial,
requires partition 1, rejects a mounted copy or active automounter, and mounts it
explicitly in a private mount namespace. It never chooses UUID/label or scans the
tree before the routine. An exclusive stock-root marker prohibits warm replay.
It runs the unchanged serial diagnostic recipe once for both roots, with a
45-minute walk bound inside a one-hour transient unit (2 GiB, 32 tasks, 65536 FDs).
Afterward one verification pass checks ownership and records fanout/depth. The
result retains elapsed time, counters, wait4 CPU and block I/O, last sampled
`/proc/PID/io`, diskstats and mount/replay observations. Sampled proc I/O may omit
the final interval; wait4 totals are complete. Mount/ancestor metadata reads and
`noatime` are recorded limitations, as is omission of original root-boot contention.

At most two sequential stock launches and one active instance are permitted.
Admission starts a 120-minute absolute clock with work fenced at minute 100,
reserving 20 minutes for cleanup. Source capture, approvals and deployment all
consume that clock. An expired window cannot be extended or replaced in the same
run. The authority terminates writers first, explicitly deletes any surviving
unattached copy, inventories root **and** data volumes and verifies exact absence.
Uncertain/failed cleanup, unexpected volume identity, pagination or incomplete
inventory retain the finalization fence and retry alarm. Originals borrowed under
the existing retained-source contract are never deletion-owned. Synthetic images
and snapshots are run-owned and deleted only by ordinary finalization.

Before live qualification, the infrastructure owner must deploy the reviewed
authority through the existing private-Worker credential-custody route and approve
the protected workflow. The current workflow does **not** establish that custody.
Its sandbox role additionally needs scoped `ec2:CreateVolume`, `ec2:AttachVolume`,
`ec2:ModifyInstanceAttribute` (only the DeleteOnTermination operation at the
authority), and `ec2:DeleteVolume`; retain the regional, creation-tag and existing
resource-tag constraints above. Scope attachment to this run's instance and copy,
snapshot reads to the admitted source, and KMS use/grants to the selected source
and destination keys. Do not grant PassRole, DetachVolume or FSR. IAM alone cannot
express every request-field constraint; the reviewed authority enforces the fixed
size, performance and attachment policy. Credentials go only to the authority,
never the candidate, relay, executor or artifacts.

Passing synthetic tests proves this bounded lifecycle, not an improvement in a
historical restore. Representative use additionally requires the capture owner to
admit one exact preserved snapshot containing both roots and hold its authoritative
use pin until final zero residue. Cross-owner capture admission/custody is an
explicit remaining dependency; this synthetic workflow must not bypass it.

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

## First-walk source admission

First-walk requires a tooling prerequisite on the protected default branch and
an open same-repository candidate PR. The prerequisite changes only the
qualification workflow, protected scripts and inert test fixtures. Keep the
ownership instrumentation, reporting fix, generated bootstrap consumers,
Linux qualification tests, Worker capability and documentation in the unmerged
candidate, with the reporting fix separately identifiable.

The prerequisite must avoid every production coordinator and Pages push path.
Changes under `worker/` (except `worker/test/`) automatically deploy the
production coordinator on main; changes under `docs/` or `skills/` automatically
publish Pages. Review the complete current workflow filters before any future
merge. Do not change production deployment controls to make the split work.
The existing qualification reaper definition consumes protected main tooling;
all modes and the reaper require the same admitted isolation record and do not
dispatch a qualification on merge. Confirm an enabled independent recovery route before live
admission; source merge does not enable a disabled workflow. No merge or deployment is authorized by this preparation.

At dispatch, `candidate_sha` must equal the open candidate PR's exact head. The
protected workflow SHA must equal both the current default-branch SHA and the
PR's base SHA; select `first-walk` from that default-branch workflow. Candidate
build inputs must match protected Go modules, Worker dependencies and Wrangler
configuration. Candidate Worker source is data for the protected build.

The diagnostic ownership recipe remains in the candidate. Protected build
staging reads it as bounded regular-file data without executing it, rejects
symlink ancestors, and requires the reviewed SHA256
`eb26f129e9a7cb77ef2197688fcfb3b05356be86b889473e9808287cf016bbf4`.
The immutable candidate manifest seals it at
`candidate/recipes/bootstrap/v1/linuxOwnershipRepair.sh`. Protected admission
verifies that artifact, and the credentialless executor checks the same pin
before relay or guest activity. The executor and guest helper come from the
protected checkout; the recipe executes only in the synthetic guest. Protected
finalization requires the same recipe pin and protected runner digest. The
copy under `scripts/fixtures/` is inert test data, never the live recipe source.
A future diagnostic recipe change requires review and a new protected pin.

Keep the candidate PR open through execution and finalization. After the
prerequisite is eventually merged with separate authorization, reconcile the
candidate onto the actual new main SHA, refresh all source/build identities,
and obtain any required review. If the PR head, protected workflow, or default
branch changes, repeat identity verification before admission. A branch
workflow dispatch, raw Worker upload, locally rebuilt binary or closed PR cannot
substitute for this contract. Source merge, authority deployment and credential
custody, and one finite synthetic run each require their own authorization.

## Shared-account isolation admission

Resource-level isolation may use shared accounts. Before cloud-writing jobs can
operate, the protected `image-qualification` environment must carry a reviewed
`CRABBOX_IMAGE_QUALIFICATION_ISOLATION_JSON` record and its canonical SHA256 in
`CRABBOX_IMAGE_QUALIFICATION_ISOLATION_SHA256`. Candidate inputs cannot select
this record. Missing admission, drift, or overlap fails before mutation.

The record binds the Cloudflare account, fixed private qualification authority,
its single active Cloudflare version, source revision, logical version, policy
hash, full public binding digest, and exact run/registry namespace IDs with their
owning script and class. It also binds the AWS account, qualification role ARN,
Region, subnet, security group, base AMI and root-volume bound. Explicit production
Worker, namespace, role and security-group exclusion inventories must be
disjoint. Exclusive-subnet admission also requires disjoint subnet identities.
Operators must verify and admit actual live identities; credential
availability or discovery alone does not admit a deployment configuration.

### Explicit shared-subnet admission

Version 1 records retain their exclusive-subnet behavior. Version 2 requires an
explicit `aws.network` object: `{ "mode": "exclusive" }` keeps those exclusions,
or `mode: "shared-subnet"` selects bounded network verification. Shared mode does
not assert a complete exclusive production inventory: `production.inventoryVerified`
must be `false`, and `production.observation` must contain `verified: true`,
`scope: "qualification-identities-and-network"`, an `observedAt` timestamp and
the reviewed observation's `receiptSha256`. All existing identity arrays remain
required; only subnet overlap is allowed. These are protected operator assertions,
not permissions for candidates to choose or edit network policy.

A shared network object binds `vpcId`, `vpcCidr`, `subnetCidr`, `routeTableId`,
`internetGatewayId`, `networkAclId`, `securityGroupSha256` and `aclSha256`. Set the
private authority's nonsecret `CRABBOX_AWS_QUALIFICATION_NETWORK_JSON` binding to
the canonical JSON of that exact object. The protected admission digest, authority
binding digest and enrolled authority policy hash all cover it. An omitted binding
retains the old policy hash and behavior; it cannot satisfy a shared record.

The supported shared topology is deliberately narrow: one IPv4 VPC CIDR, one
main route table with only its local route and an internet-gateway default route,
one exact available subnet without IPv6 assignment, and the bound ACL and dedicated
group. Additional tables, associations, local CIDRs, private routes, truncated
inventories, denied reads and malformed responses stop admission. No shared route
or ACL is changed. The authority needs regional `DescribeSubnets`,
`DescribeRouteTables`, `DescribeNetworkAcls` and `DescribeSecurityGroups` reads;
unavailable permissions are a deployment dependency, never a reason to skip a check.

The group may allow IPv4 SSH on TCP22 only. Egress must cover exactly IPv4 outside
the local VPC CIDR on separate TCP80 and TCP443 permissions, with no IPv6, group
references, prefix lists or additional groups. Security groups are allow-only:
remove broad rules rather than adding a narrower rule alongside them. The
`qualificationEgress` helper in `scripts/image-qualification-network.ts` generates
the positive CIDR complement, capped at 60 single-CIDR/single-port allowances.
A `/16` needs 16 CIDRs on each port, or 32 rules. Verify the effective quota and
actual installed rules before dispatch; source preparation does not install them.

Use that same module's `qualificationGroupShape`, `qualificationAclShape` and
`networkDigest` on the authority's parsed EC2 Query response records to calculate
the two hashes. They bind security coordinates, sort unordered sets, and exclude
tags, descriptions and request IDs. Do not hash differently shaped CLI JSON and
assume it is interchangeable. The protected Node tooling imports this module with
Node 24's type stripping; keep it with the protected checkout, never candidate code.
The helper lives outside `worker/src`, which the build overlays with candidate
source. It is an immutable protected build input: a candidate changing it is
rejected before overlay, and subsequent protected commands import only the
protected copy. Shared admission therefore needs these tooling changes on the
protected workflow revision before qualification; a runtime PR alone is insufficient.

The authority checks the network at enrollment and before forward mutations,
including first-walk copy/attachment operations. Successful launch and instance
discovery responses, including cached replays, require an exact guest readback:
account, VPC, subnet, one interface, the dedicated group only, no IPv6 or IAM
profile, and the required IMDS settings. Effects enter the existing durable ledger
before a post-launch refusal. Cleanup, cancellation and finalization do not depend
on the shared network checks passing; ordinary ownership and account checks remain.

This permits shared-subnet use by trusted maintainers running reviewed synthetic
code without guest cloud credentials. It does not isolate the guest from public
HTTP/HTTPS endpoints, AWS DNS or platform services. Other private destinations have
no admitted forwarding route; network drift requires reassessment. Reads and
dispatch are not atomic with independent administrator changes: coordinate those
writers during the bounded run. No claim of permanent future subnet non-use is
required, and a different subnet in the same VPC is not itself isolation.

Protected tooling reads all namespace inventory pages and checks each bound
namespace ID against its owning script and class. The authority is private and
may have no service or production broker binding. Candidates have only their
own Fleet namespace, the fixed qualification transport, ephemeral run tokens,
and the reviewed bounded settings. Relays target only their run-derived
candidate. Their names and the controller name must be absent before creation;
existing or uncertain names cannot be adopted. Cloudflare creation is a
read-then-write operation, not an atomic exclusion of independent account
administrators; coordinate administrative writers during admitted operations.

Created resources carry admission and binding ownership markers. The reaper
requires the same protected admission record, validates the controller's source
and authority entrypoint before enabling or invoking it, and rejects changed
versions, unowned resources, external namespaces, extra broker bindings and
mismatched run-derived names. Recovery preserves the transport identity through
candidate tombstoning, so a lost deletion response can be retried without
replaying its namespace migration. Unmarked legacy resources require operator
review rather than automatic adoption. The scheduled workflow remains subject
to its separately managed enabled/disabled state.

Build and execution runners receive no cloud administration credentials.
First-walk CLI children receive a narrow environment and a private HOME,
configuration directory and working directory, with only the qualification relay
and ephemeral executor token. The trusted authority retains signing credentials;
its policy and ownership ledger constrain candidate requests and cleanup to the
admitted resource identities and run-owned IDs. Qualification credential custody,
private authority deployment, infrastructure admission and one finite synthetic
execution remain separate approval steps. Production bootstrap changes remain
in the unmerged candidate, whose eventual merge also triggers coordinator and
documentation publication workflows.

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
ambiguous response. The authority revalidates both the configured STS account and the exact
qualification assumed-role principal before every mutation, reconciliation and
cleanup. The required `CRABBOX_AWS_QUALIFICATION_ROLE_ARN` participates in the
enrolled policy hash. A production role in the same account is rejected. A
replacement credential must resolve to the enrolled role, account and fixed
Region policy. This is an immediate identity
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
