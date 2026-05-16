# admin

`crabbox admin` contains trusted operator controls for coordinator-backed leases.

```sh
crabbox admin leases
crabbox admin leases --state active --json
crabbox admin lease-audit --state expired --provider aws
crabbox admin lease-audit --fail-on-live
crabbox admin providers identity --provider aws --region eu-west-1
crabbox admin providers policy --provider aws --target macos
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

Release/delete accept a canonical `cbx_...` ID or an active slug; use the canonical ID when an admin slug lookup is ambiguous. Add `--json` to print the updated lease record.

Admin commands require a configured coordinator and a separate admin bearer token
stored as `broker.adminToken` or `CRABBOX_COORDINATOR_ADMIN_TOKEN`. The shared
operator token is not enough for admin routes.

## leases

List coordinator lease records.

Flags:

```text
--state <state>     filter by active, released, expired, or failed
--owner <email>     filter by owner
--org <name>        filter by org
--limit <n>         default 100, maximum 500
--json              print JSON
```

## lease-audit

Check expired coordinator lease records against the backing cloud provider.
The audit currently supports AWS leases and reports whether each expired
`cloudID` is still present, missing, or could not be checked.

Flags:

```text
--state <state>     default expired
--provider <name>   default aws
--owner <email>     filter by owner
--org <name>        filter by org
--limit <n>         default 100, maximum 500
--fail-on-live      exit non-zero for live cloud instances or audit errors
--json              print JSON
```

## providers

Inspect provider identity and IAM policy requirements through provider-scoped
commands. `providers identity` is a read-only diagnostic for attaching policy
updates to the right cloud principal. `providers policy` prints the baseline
provider policy, or a target-specific combined policy when `--target` is set.

Flags:

```text
identity:
  --provider <provider>   currently aws
  --region <region>       provider region used by the identity endpoint
  --json                  print JSON

policy:
  --provider <provider>   currently aws
  --target <target>       optional; use macos for provider plus host lifecycle
```

## hosts

Inspect and manage host lifecycle resources through provider- and target-scoped
commands. `admin hosts --provider aws --target macos` maps to AWS EC2 Mac
Dedicated Host operations today; the command shape is intentionally generic so
other providers can add macOS host backends without introducing new top-level
admin nouns. `policy`, `offerings`, `quota`, `list`, and `allocate --dry-run`
are read-only. Real `allocate` and `release` require `--force` because host
resources are billed separately from Crabbox leases and can have provider
lifecycle constraints.

Flags:

```text
policy:
  --provider <provider>   currently aws
  --target <target>       currently macos
  prints copy-pasteable provider IAM JSON for host lifecycle permissions

list:
  --provider <provider>   currently aws
  --target <target>       currently macos
  --region <region>       provider region
  --type <type>           provider host type, for example mac1.metal or mac2.metal
  --state <state>         filter by provider host state
  --json                  print JSON

offerings:
  --provider <provider>   currently aws
  --target <target>       currently macos
  --region <region>       provider region
  --type <type>           default mac2.metal
  --json                  print JSON

quota:
  --provider <provider>   currently aws
  --target <target>       currently macos
  --region <region>       provider region
  --type <type>           default mac2.metal
  --json                  print JSON

allocate:
  --provider <provider>        currently aws
  --target <target>            currently macos
  --region <region>            provider region
  --availability-zone <az>     optional; omitted means discover and try offered AZs
  --type <type>                default mac2.metal
  --dry-run                    validate the request without allocating a host
  --force                      confirm host allocation
  --json                       print JSON

release:
  --id <host-id> or positional host id
  --provider <provider>   currently aws
  --target <target>       currently macos
  --region <region>
  --force                 confirm host release
  --json                  print JSON
```

## aws-identity

Compatibility alias for `crabbox admin providers identity --provider aws`.

## aws-policy

Compatibility alias for `crabbox admin providers policy --provider aws`.
Use `crabbox admin providers policy --provider aws --target macos` for the
combined AWS provider plus EC2 Mac Dedicated Host lifecycle policy.

The AWS provider policy covers key pairs, instance launch and termination,
managed security groups, image creation/promotion, snapshot cleanup, and
optional Service Quotas reads. If `CRABBOX_AWS_INSTANCE_PROFILE` is set, add a
separate scoped `iam:PassRole` grant for that role with
`iam:PassedToService=ec2.amazonaws.com`.

`--mac-hosts` remains supported as a legacy spelling for the combined macOS
policy.

For coordinator remediation, save the combined policy and attach it to the AWS
principal returned by `admin providers identity --provider aws`:

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

The helper dry-runs by default. With `--profile auto`, it scans local AWS
profiles and selects the one whose account matches the coordinator account.
With an explicit `--profile`, it verifies that profile directly. It writes the
inline role or user policy only when `--apply` is present. For assumed-role
identities, it attaches the policy to the underlying role name, not the session
name. JSON output includes
`policyTarget.type` and `policyTarget.name` when the coordinator ARN is an IAM
user, IAM role, or STS assumed-role ARN.

Flags:

```text
--mac-hosts    include EC2 Mac Dedicated Host lifecycle permissions
```

## mac-hosts

Compatibility alias for `crabbox admin hosts --provider aws --target macos`.
Prefer `admin hosts` in new scripts and runbooks.

The coordinator AWS identity must allow `ec2:DescribeInstanceTypeOfferings`,
`ec2:DescribeHosts`, `ec2:AllocateHosts`, `ec2:ReleaseHosts`, and
`ec2:CreateTags` for host lifecycle work, plus
`servicequotas:ListServiceQuotas` for the quota preflight. `AllocateHosts` uses
create-time tags, so `CreateTags` should be allowed only when the EC2 create
action is `AllocateHosts`:

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
      "Action": "servicequotas:ListServiceQuotas",
      "Resource": "*"
    }
  ]
}
```

This policy is intentionally limited to EC2 Mac Dedicated Host lifecycle
operations. End-to-end macOS image validation also needs the normal brokered
AWS provider permissions for key pairs, security groups, `RunInstances`,
`TerminateInstances`, image creation/promotion, snapshot cleanup, and baseline
Service Quotas reads. See [Infrastructure](../infrastructure.md#aws-ec2) before
running the paid macOS image smoke.

## release

Mark a lease released. Add `--delete` to delete the backing server while releasing.

Flags:

```text
--id <lease-id-or-slug>
--delete
--json
```

## delete

Delete the backing server for an active lease and mark it released. Requires `--force`.

Flags:

```text
--id <lease-id-or-slug>
--force
--json
```

Related docs:

- [Operations](../operations.md)
- [Auth and admin](../features/auth-admin.md)
