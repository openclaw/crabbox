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
owner/run/attempt/SHA/expiry/operation tags. The protected workflow constructs
`image-qualification-<workflow-run-id>-<attempt>`; the signer derives
`crabbox_qualification_attempt` from the positive decimal attempt suffix, never
from candidate request tags. Enrollment rejects a run ID without that suffix.

The run may own one physical key pair and one active AMI with at most one child
snapshot. Candidate traffic is capped at 64 operations, eight unresolved
intents, and the 120-minute expiry. The authority accepts only the five request
fields in the service binding contract, bounds the complete envelope to 64 KiB
before normalization, and requires a flat map of at most 256 string parameters.
Responses are also capped at 64 KiB. Cleanup and inventory calls do not consume
candidate capacity.

The preprovisioned security group is read-only to the candidate and authority
signer. A separate protected network owner admits one TCP port 22 IPv4 `/32` for
the actual executor, then removes that exact run-tagged rule. Neither candidate
nor signer receives security-group write permissions.

### Executor admission and network recovery

After deployment, `execute` and protected `arm` start concurrently. On the same
executor VM that will run the candidate, trusted preflight code registers through
the controller before downloading the candidate artifact or invoking its Worker.
The controller uses only Cloudflare's observed `CF-Connecting-IP`, rejects Worker
subrequests and address override paths, and accepts no caller-selected CIDR,
account, Region, or security group. Missing edge metadata fails closed.

A distinct random run-bound preflight capability permits one immutable address
registration and bounded readiness polling. Its digest, registration, network
intent, rule receipt, and execution-arm state live in the existing run Durable
Object. The successful readiness response consumes the capability durably before
candidate bytes run; a lost response fails closed. No privileged OIDC permission,
AWS session, Cloudflare token, or reusable controller capability enters `execute`.

The protected arm job reads the fixed policy from the controller, verifies the
AWS account, persists the network intent and dispatch fence, then authorizes only
the observed `/32`. The group must contain no foreign ingress. AWS rule tags bind
the run, numeric workflow attempt, owner, candidate SHA, expiry, and network
operation digest (`crabbox_qualification_network`). Exact readback precedes
`armExecution`; the candidate transport rejects requests before this durable
gate. A lost authorization response is reconciled by those tags, never retried
blindly and never recovered by adopting a foreign duplicate.

Finalization fences execution first. Relay, compute, network, and candidate
cleanup each receive an independent attempt. The network owner waits out bounded
in-flight admission, reconciles run-owned rules, revokes exact rule IDs, and
verifies absence before recording completion. A changed or foreign rule is not
deleted. Compute cleanup may complete while network recovery remains pending;
overall finalization and registry retirement wait for both. The controller is
retained on incomplete teardown. The authority alarm fences and cleans compute;
only the protected finalizer/reaper owns network writes.

### Hosted network credentials

The private infrastructure owner must establish the sandbox account, fixed group,
network role, permissions boundary, state backend, and recovery custody before
cloud execution. Source changes do not provision these resources or credentials.
The network role needs only `sts:GetCallerIdentity`,
`ec2:DescribeSecurityGroupRules`, `ec2:AuthorizeSecurityGroupIngress`,
`ec2:RevokeSecurityGroupIngress`, and the tag-on-create `ec2:CreateTags` permission.
Its IAM policies must restrict writes to the fixed sandbox group, approved Region,
and qualification request/resource tags. Do not add these permissions to the
candidate authority role.

Only protected `arm`, `finalize`, and reaper steps receive the environment secrets
`CRABBOX_IMAGE_QUALIFICATION_NETWORK_ACCESS_KEY_ID`,
`CRABBOX_IMAGE_QUALIFICATION_NETWORK_SECRET_ACCESS_KEY`,
`CRABBOX_IMAGE_QUALIFICATION_NETWORK_SESSION_TOKEN`, and
`CRABBOX_IMAGE_QUALIFICATION_NETWORK_EXPIRES_AT`. These must be one short-lived STS
session owned by the approved network principal, with its real ISO-8601 expiration;
never IAM-user keys, an SSO cache, or candidate-controlled values. Admission
requires validity through the authority's `cleanupNotAfter`: run expiry plus
30 minutes. The reaper runs every ten minutes with a twenty-minute job bound;
both fit this explicit cleanup window. A three-hour network session leaves
thirty minutes for provisioning before the maximum two-hour run. Scheduling
delays or approval gates must not be treated as a guarantee of cleanup.
The native AWS CLI uses only those environment credentials, with profile files,
metadata credentials, retries, and credential logging disabled.
The authority returns its persisted absolute ingress-dispatch deadline. Before
authorization, the helper requires the entire 25-second native-call budget to
fit inside both that fence and the run expiry. A delayed acknowledgement after
finalization cannot start a new write.

The provisioning owner must verify the unattended reaper can obtain this session,
refresh it before expiry if recovery remains incomplete, and remove the temporary
secret material after confirmed teardown. Expired or missing credentials leave
the durable recovery record intact and fail visibly; compute cleanup still runs.
A successful local SSO login is not proof of this hosted credential delivery.
Cloud qualification remains blocked until this custody and recovery route is
established and verified separately.

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
`crabbox_qualification_attempt`, `crabbox_qualification_sha`, and
`crabbox_qualification_expiry` request tags.
Mutation of existing resources must require the same run resource tag. Restrict
`RunInstances` to the configured base AMI ARN or an AMI carrying the complete
authority-injected qualification tag set, plus the configured subnet and
security group ARNs and the two allowed instance types. Do not grant a wildcard
AMI resource. IAM provides the outer tagged-image boundary; the authority then
requires the exact derived AMI ID to remain active in the enrolled run ledger,
so another run's or an unrelated tagged image is still rejected. Keep these IAM
constraints even though the authority duplicates them at runtime.

## Enrollment and binding

Deploy the authority from
`worker/wrangler.aws-qualification-authority.jsonc`. That config has no route,
workers.dev URL, preview URL, or cron. Supply AWS credentials only to this Worker.

Before deploying a candidate, bind the protected caller to
`AWSQualificationController` with the reviewed deployment hash and call
`enroll`. Add a candidate service binding whose props exactly match the enrolled
identity:

```json
{
  "binding": "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
  "service": "crabbox-aws-qualification-authority",
  "props": {
    "runId": "image-qualification-<run>-<attempt>",
    "owner": "<reviewed-owner>",
    "candidateSha": "<40-hex-same-repo-sha>",
    "candidateWorker": "<isolated-candidate-worker-name>",
    "deploymentHash": "<64-hex-candidate-deployment-hash>",
    "expiresAt": "<absolute-iso8601-within-120m>"
  }
}
```

The candidate configuration must use the same fixed Region, AMI, subnet,
security group, and root size; request `on-demand`; select only `t3.small` or
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
match that exact registry record while its state remains `claimed`. Finalization
persists an irreversible per-run `finalizingAt` fence before transitioning the
registry to `finalizing` or starting cleanup I/O, so an already-admitted
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
every mutation, so credential rotation cannot move a run into another account.
The public AWS release path waits for that terminal instance state only when the
qualification transport binding is active; ordinary AWS releases keep their
existing fire-and-forget behavior.
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
