# admin

`crabbox admin` groups trusted operator controls for coordinator-backed leases and the cloud resources behind them. Use it to inspect every lease the broker tracks, reconcile expired leases against live cloud state, force-release or delete a backing server, print provider IAM policies, and manage host lifecycle resources.

## Requirements

Coordinator-backed `admin` commands require server-authorized admin access. An explicit `broker.adminToken` or `CRABBOX_COORDINATOR_ADMIN_TOKEN` takes precedence over the normal broker credential and token command. Otherwise, the CLI uses its configured broker authentication, including a GitHub login whose immutable owner has an existing `CRABBOX_GITHUB_ADMIN_OWNERS` grant. The coordinator still rejects shared tokens and GitHub users without that grant; the CLI does not grant privileges or retry a denied explicit admin token with another credential. Policy-printing commands run locally without authentication.

## At a glance

```sh
crabbox admin leases
crabbox admin retained-reconcile --json
crabbox admin leases --state active --json
crabbox admin lease-audit --state expired --provider aws
crabbox admin lease-audit --fail-on-live
crabbox admin extend-lifetime --id cbx_abcdef123456 --owner alice@example.com --reason 'finish test' --expires-at 2026-10-12T12:00:00Z
crabbox admin lifetime-readback --id cbx_abcdef123456 --json
crabbox admin providers identity --provider aws --region eu-west-1
crabbox admin providers policy --provider aws --region eu-west-1 --target macos
crabbox admin hosts policy --provider aws --target macos
crabbox admin hosts offerings --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts list --provider aws --target macos --region eu-west-1
crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --dry-run
crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --force
crabbox admin hosts release h-0123456789abcdef0 --provider aws --target macos --region eu-west-1 --force
crabbox admin release blue-lobster
crabbox admin release blue-lobster --delete
crabbox admin delete cbx_... --force
```

`release` and `delete` accept either a canonical `cbx_...` lease ID or an active slug, given positionally or via `--id`. Prefer the canonical ID when a slug lookup could be ambiguous. Add `--json` to any subcommand to print the structured record instead of the human-readable line.

## leases

List coordinator lease records.

Flags:

```text
--state <state>     filter by active, released, expired, or failed
--owner <owner>     filter by owner identity
--org <name>        filter by org
--limit <n>         maximum leases (default 100)
--json              print JSON
```

The text output prints one line per lease with ID, slug, provider, state, server type, host, owner, org, idle timeout, and expiry. Enrolled leases append their lifetime mode, canary marker, effective stop and confirmed-stop deletion deadlines, retention, status, readiness and warnings. JSON retains the complete `lifetimePolicy`.

## extend-lifetime

Extend an enrolled lease's compute deadline before stopping or deletion starts.
This requires admin authorization and records the responsible owner, reason,
granting administrator, grant time, and expiry. It applies to explicit canaries
as well as default-cohort leases when their global mode allows mutation.

```text
--id <canonical-lease-id>   enrolled lease (required)
--owner <owner>             responsible owner (required)
--reason <reason>           audit reason (required)
--expires-at <RFC3339>      new compute deadline (required)
--json                      print the updated lease record
```

The expiry must advance the effective deadline and be no more than seven days
away. The coordinator rejects extensions once stopping starts, including a
stop request whose outcome is still uncertain. An extension cannot restore a
stopped instance or recover a deleted root disk. Ordinary lease owners cannot
authorize an extension themselves; ask an administrator before the warning's
deadline.

## lifetime-readback

Read the coordinator's current observation of the exact owned AWS instance and
its root volume, without local AWS credentials or any resource mutation:

```sh
crabbox admin lifetime-readback --id cbx_abcdef123456 --json
```

`--id` must be a canonical lease ID; `--json` is optional. JSON contains
`leaseId`, `provider`, `region`, `instanceId`, `instanceState`, `rootVolumeId`,
`rootVolumePresent`, and `observedAt`. A confirmed absent instance reports
`instanceState: "terminated"`; a confirmed absent retained root volume reports
`rootVolumePresent: false`. Missing or failed observations are not proof of
deletion. The coordinator verifies recorded ownership and resource identity
before reporting provider state. This operation is admin-only and read-only.

## lease-audit

Check coordinator lease records against the backing cloud provider. The audit currently reconciles AWS and Azure leases and reports, for each matching lease, whether its `cloudID` is still `found`, `missing`, or could not be checked (`error`). Each line also surfaces cleanup attempt counts and errors recorded by the broker's expiry sweep.

Flags:

```text
--state <state>     filter by state (default expired)
--provider <name>   provider to audit (default aws)
--owner <owner>     filter by owner identity
--org <name>        filter by org
--limit <n>         maximum leases (default 100)
--fail-on-live      exit non-zero when expired leases still have live cloud instances or audit errors
--json              print JSON
```

Use `--fail-on-live` in CI or cron jobs to turn unreconciled instances into a failing exit code.

## providers

Inspect provider identity and IAM policy requirements through provider-scoped subcommands.

```sh
crabbox admin providers identity --provider aws --region eu-west-1
crabbox admin providers policy --provider aws --region eu-west-1 --target macos
```

### providers identity

A read-only diagnostic that reports the cloud principal the coordinator authenticates as, so you can attach policy updates to the right identity. Currently supports `--provider aws`.

```text
--provider <provider>   provider (currently aws)
--region <region>       provider region used for the identity endpoint
--json                  print JSON
```

JSON output includes `policyTarget.type` and `policyTarget.name` when the coordinator ARN resolves to an IAM user, IAM role, or STS assumed-role ARN.

### providers policy

Print the baseline brokered AWS IAM policy. With `--target macos` (or `--host-lifecycle`), the output is combined with the EC2 Mac Dedicated Host lifecycle statements. Currently supports `--provider aws`.

```text
--provider <provider>   provider (currently aws)
--region <region>       region for owned-instance stop and termination permissions
--account <account-id>  AWS account ID (default: the IAM principal's account)
--target <target>       optional; macos combines provider plus host lifecycle policy
--host-lifecycle        include provider host lifecycle permissions
--mac-hosts             legacy alias for --host-lifecycle
```

The baseline AWS provider policy covers key pairs, instance launch, managed security groups, image creation/promotion, snapshot cleanup, instance/volume inspection, and Service Quotas reads. Stop and termination share an account-, region-, and ownership-tag-scoped statement (`crabbox=true`, `created_by=crabbox`). Supply `--region` for an installable policy; without it, replace the literal `<aws-region>` placeholder before installation. The account defaults to the IAM `${aws:PrincipalAccount}` policy variable; `--account` binds it to a specific 12-digit account. These are policy-generation options, not changes to coordinator configuration. If you set `CRABBOX_AWS_INSTANCE_PROFILE`, add a separate scoped `iam:PassRole` grant for that role with `iam:PassedToService=ec2.amazonaws.com`.

## hosts

Inspect and manage host lifecycle resources through provider- and target-scoped subcommands. Today `--provider aws --target macos` maps to AWS EC2 Mac Dedicated Host operations; the command shape is intentionally generic so other providers can add macOS host backends without introducing new top-level admin nouns.

`policy`, `list`, `offerings`, `quota`, `reservation`, and `allocate --dry-run` are read-only. Real `allocate` and `release` require `--force`, because host resources are billed separately from leases and carry provider lifecycle constraints.

All subcommands share the scope flags:

```text
--provider <provider>   host provider (default aws; currently aws)
--target <target>       host target OS (default macos; currently macos)
```

### hosts reservation / clear

```sh
crabbox admin hosts reservation h-0123456789abcdef0 --region eu-west-1 --json
crabbox admin hosts clear h-0123456789abcdef0 --region eu-west-1
```

`reservation` reads coordinator host associations, with their storage keys,
credential-free record summaries, canonical lease state (or `null`), and stale
reason. `clear` removes those host references atomically and reports the number
cleared. It preserves lease history, instance identity, and cleanup obligations;
it does not terminate instances or release Dedicated Hosts. Active/provisioning
and potentially retained associations require `clear --force`. A running create
may restore its association, so inspect the lease and provider before forcing.
Clear uses POST so older coordinators reject the unsupported operation without
dispatching a Dedicated Host release. Both commands require admin credentials and support the shared scope flags,
`--region`, and `--json`. They also work through `admin mac-hosts`.

### hosts policy

Print copy-pasteable IAM JSON for host lifecycle permissions.

### hosts list

```text
--region <region>   provider region
--type <type>       filter by host type, for example mac1.metal or mac2.metal
--state <state>     filter by provider host state
--json              print JSON
```

### hosts offerings

```text
--region <region>   provider region
--type <type>       host type (default mac2.metal)
--json              print JSON
```

### hosts quota

```text
--region <region>   provider region
--type <type>       host type (default mac2.metal)
--json              print JSON
```

### hosts allocate

```text
--region <region>            provider region
--availability-zone <az>     optional; if omitted, discover and try offered AZs
--type <type>                host type (default mac2.metal)
--dry-run                    validate the request without allocating a host
--force                      confirm host allocation
--json                       print JSON
```

`allocate` refuses to run unless either `--dry-run` or `--force` is set. The dry-run path summarizes the provider's validation result (for example, a `DryRunOperation` success or an `UnauthorizedOperation` permission gap) without billing a host.

### hosts release

```text
--id <host-id>      host id (or pass it positionally)
--region <region>   provider region
--force             confirm host release
--json              print JSON
```

Requires `--force` and an exact retained Crabbox lease binding for the selected
host, AWS account, and region. Crabbox verifies the current AWS account through
STS, rechecks the host's Crabbox ownership tags, and clears the durable host
binding after successful release; administrative confirmation does not
authorize releasing an unclaimed or externally adopted host. The host id may be given positionally
(for example, `crabbox admin hosts release h-0123456789abcdef0 --force`) or via
`--id`.

## release

Mark a lease released. Add `--delete` to also delete the backing server while releasing.

```text
--id <lease-id-or-slug>   lease to release (or pass it positionally)
--delete                  delete the backing server while releasing
--json                    print JSON
```

## delete

Delete the backing server for a lease and mark it released. Requires `--force`.

```text
--id <lease-id-or-slug>   lease to delete (or pass it positionally)
--force                   confirm deletion
--json                    print JSON
```

## Compatibility aliases

These spellings remain for existing scripts and runbooks. Prefer the provider- and target-scoped forms in new work.

- `crabbox admin aws-identity` — alias for `crabbox admin providers identity --provider aws`.
- `crabbox admin aws-policy` — alias for `crabbox admin providers policy --provider aws`; supports `--mac-hosts` for the combined macOS policy.
- `crabbox admin mac-hosts <list|offerings|quota|allocate|release|reservation|clear|policy>` — alias for `crabbox admin hosts --provider aws --target macos`.

## Applying a macOS IAM policy for coordinator remediation

To fix coordinator permissions for paid macOS image work, save the combined policy and attach it to the AWS principal returned by `admin providers identity`:

```bash
crabbox admin providers identity --provider aws --region eu-west-1 --json > /tmp/crabbox-provider-identity.json
crabbox admin providers policy --provider aws --target macos > /tmp/crabbox-macos-image-policy.json

scripts/apply-macos-image-iam-policy.sh \
  --identity /tmp/crabbox-provider-identity.json \
  --policy /tmp/crabbox-macos-image-policy.json \
  --profile auto

scripts/apply-macos-image-iam-policy.sh \
  --identity /tmp/crabbox-provider-identity.json \
  --policy /tmp/crabbox-macos-image-policy.json \
  --profile <aws-profile> \
  --apply
```

The helper dry-runs by default. With `--profile auto`, it scans local AWS profiles and selects the one whose account matches the coordinator account; with an explicit `--profile`, it verifies that profile directly. It writes the inline role or user policy only when `--apply` is present. For assumed-role identities, it attaches the policy to the underlying role name, not the session name.

The EC2 Mac Dedicated Host lifecycle policy is intentionally limited to host operations and looks like this:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeHosts",
        "ec2:DescribeInstanceTypeOfferings"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:AllocateHosts",
        "ec2:ReleaseHosts"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "ec2:CreateTags",
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:CreateAction": "AllocateHosts"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": [
        "servicequotas:GetServiceQuota",
        "servicequotas:ListServiceQuotas"
      ],
      "Resource": "*"
    }
  ]
}
```

`AllocateHosts` uses create-time tags, so `CreateTags` is scoped to the `AllocateHosts` create action. End-to-end macOS image validation also needs the normal brokered AWS provider permissions (key pairs, security groups, `RunInstances`/`TerminateInstances`, image creation/promotion, snapshot cleanup, and baseline Service Quotas reads); the combined `providers policy --target macos` output includes both sets. See [Infrastructure](../infrastructure.md#aws-ec2) before running the paid macOS image smoke.

## Related docs

- [Operations](../operations.md)
- [Auth and admin](../features/auth-admin.md)

## retained-reconcile

`crabbox admin retained-reconcile [--json]` reads the admin-only
`GET /v1/admin/retained-reconcile` status endpoint. It reports effective settings,
cumulative counters by provider/outcome, last pass time, the current-index cursor,
completed cycles, records scanned, and provider backoff deadlines. Counters count
observations across passes, not distinct leases. `present` identifies real retained
machines; only a bounded sample of their lease IDs appears in each structured
`coordinator.retained_reconcile` log line. The command does not start a pass or
change deletion settings.

See [Retained record reconciliation](../features/lifecycle-cleanup.md#retained-record-reconciliation)
for provider coverage and unresolved classes.
