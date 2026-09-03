# AWS Image Qualification Authority

This is a pre-merge test boundary for a dedicated AWS sandbox account. It lets an
exact candidate coordinator execute Crabbox Fleet authentication, serialization,
AWS provider lifecycle, image catalog compare-and-swap, revision retirement, and
publisher rollback without receiving AWS or Cloudflare credentials.

It is not a general AWS proxy and is not enabled by default.

## Trust boundary

There are three Workers:

1. A protected control plane enrolls and finalizes runs through a service binding.
2. The exact candidate Worker has only `CRABBOX_AWS_QUALIFICATION_TRANSPORT`, bound
   to the authority's `AWSQualificationTransport` entrypoint.
3. The non-public authority Worker holds the sandbox AWS credentials and signs
   admitted calls.

The candidate binding has immutable `ctx.props` containing `runId`, `owner`,
`candidateSha`, and `expiresAt`. The authority requires an enrolled exact match.
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
8-20. Launches are one on-demand `t3.small` or `t3a.small`, with an encrypted
`gp3` root volume, no instance profile, IMDSv2 required, and authority-injected
owner/run/SHA/expiry/operation tags.

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
ec2:CreateSnapshot
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
`RunInstances` to the configured AMI, subnet, and security group ARNs and the
two allowed instance types. Keep these IAM constraints even though the
authority duplicates them at runtime.

## Enrollment and binding

Deploy the authority from
`worker/wrangler.aws-qualification-authority.jsonc`. That config has no route,
workers.dev URL, preview URL, or cron. Supply AWS credentials only to this Worker.

Before deploying a candidate, call the protected control-plane `enroll` RPC. Add
a candidate service binding whose props exactly match the enrolled identity:

```json
{
  "binding": "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
  "service": "crabbox-aws-qualification-authority",
  "entrypoint": "AWSQualificationTransport",
  "props": {
    "runId": "image-qualification-<run>",
    "owner": "<reviewed-owner>",
    "candidateSha": "<40-hex-same-repo-sha>",
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
canonical request hash, and pending intent. A completed receipt is replayed only
for the same hash. `RunInstances` receives an authority-derived deterministic
`ClientToken`. Lost image or snapshot responses reconcile through
authority-injected operation tags.

The ledger learns only IDs created by, or discovered beneath, the registered
run. Candidate reads and mutations are restricted to those IDs, except the fixed
base AMI and security group. `finalize` deregisters images, deletes snapshots,
terminates instances, deletes imported key pairs, and verifies zero run-owned
instance/volume/key-pair/image/snapshot residue. The expiry alarm runs the same
cleanup and retries incomplete teardown.

Cloud setup, enrollment, candidate deployment, and paid execution remain
explicit maintainer actions. Merging this seam creates no Worker, secret,
Durable Object namespace, AWS resource, or spend.
