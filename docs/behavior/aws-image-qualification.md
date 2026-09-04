# AWS Image Qualification Authority

This is a pre-merge test boundary for a dedicated AWS sandbox account. It lets an
exact candidate coordinator execute Crabbox Fleet authentication, serialization,
AWS provider lifecycle, image catalog compare-and-swap, revision retirement, and
publisher rollback without receiving AWS or Cloudflare credentials.

It is not a general AWS proxy and is not enabled by default.

## Trust boundary

There are three Workers:

1. A protected binding selects the named `AWSQualificationController` entrypoint
   to enroll and finalize runs.
2. The exact candidate Worker has only `CRABBOX_AWS_QUALIFICATION_TRANSPORT`,
   bound to the authority's default transport entrypoint.
3. The non-public authority Worker holds the sandbox AWS credentials and signs
   admitted calls.

The candidate binding has immutable `ctx.props` containing `runId`, `owner`,
`candidateSha`, `deploymentHash`, and `expiresAt`. The controller binding carries
the same deployment hash. Enrollment stores the complete fixed policy and its
hash; candidate operations fail closed if the deployed policy later drifts.
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
    "runId": "image-qualification-<run>",
    "owner": "<reviewed-owner>",
    "candidateSha": "<40-hex-same-repo-sha>",
    "deploymentHash": "<64-hex-candidate-deployment-hash>",
    "expiresAt": "<absolute-iso8601-within-120m>"
  }
}
```

The candidate configuration must use the same fixed Region, AMI, subnet,
security group, and root size; request `on-demand`; select only `t3.small` or
`t3a.small`; leave `CRABBOX_AWS_INSTANCE_PROFILE` empty; and leave
`CRABBOX_AWS_FAST_SNAPSHOT_RESTORE_AZS` unset. It must also disable routes,
workers.dev, preview URLs, and cron.

## Replay and teardown

Each run has one Durable Object. Before a mutation, it persists the operation ID,
canonical normalized-request hash, and pending intent. A completed receipt is
replayed only for the same hash. `RunInstances` receives an authority-derived
deterministic `ClientToken` derived from the run and operation IDs. Uncertain
`CreateImage` and `ImportKeyPair` results retain their intents and use bounded
read reconciliation; they are never blindly reissued. Image reconciliation uses
authority-injected operation tags. Imported key names are authority-generated;
ownership is recorded only after ID, public key, and run tags are read back.
If bounded reconciliation proves neither success nor a definitive error, the
intent remains until final inventory and cleanup prove that no run-owned
resource remains; only then is it retired.
Finalization never redispatches a pending `RunInstances` request. It performs
bounded read-only discovery by the authority-injected run and operation tags,
then cleans any discovered instance; a no-effect launch intent is retired only
after the same zero-residue proof.

The ledger learns only IDs created by, or discovered beneath, the registered
run. Candidate reads and mutations are restricted to those IDs, except the fixed
base AMI and security group. Lifecycle deletes remove current ledger ownership
instead of accumulating stale IDs; terminating instances remain active until a
read confirms `terminated` or absent. Before and after teardown, bounded
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
cleanup and retries incomplete teardown.

Cloud setup, enrollment, candidate deployment, and paid execution remain
explicit maintainer actions. Merging this seam creates no Worker, secret,
Durable Object namespace, AWS resource, or spend.
