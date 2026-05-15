# admin

`crabbox admin` contains trusted operator controls for coordinator-backed leases.

```sh
crabbox admin leases
crabbox admin leases --state active --json
crabbox admin lease-audit --state expired --provider aws
crabbox admin lease-audit --fail-on-live
crabbox admin aws-identity --region eu-west-1
crabbox admin aws-policy
crabbox admin mac-hosts policy
crabbox admin mac-hosts offerings --region eu-west-1 --type mac2.metal
crabbox admin mac-hosts list --region eu-west-1
crabbox admin mac-hosts allocate --region eu-west-1 --type mac2.metal --dry-run
crabbox admin mac-hosts allocate --region eu-west-1 --type mac2.metal --force
crabbox admin mac-hosts release h-0123456789abcdef0 --region eu-west-1 --force
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

## aws-identity

Show the AWS caller identity used by the coordinator. This is a read-only admin
diagnostic for attaching AWS IAM policy updates to the right principal.

Flags:

```text
--region <region>   AWS region used for the STS endpoint
--json              print JSON
```

## aws-policy

Print the baseline IAM policy for brokered AWS provider operations. This is a
local, read-only helper for operators configuring the Worker AWS principal.

The policy covers key pairs, instance launch and termination, managed security
groups, image creation/promotion, snapshot cleanup, and optional Service Quotas
reads. If `CRABBOX_AWS_INSTANCE_PROFILE` is set, add a separate scoped
`iam:PassRole` grant for that role with `iam:PassedToService=ec2.amazonaws.com`.
EC2 Mac Dedicated Host allocation and release are intentionally separate; use
`crabbox admin mac-hosts policy` for that grant.

Flags:

```text
no flags
```

## mac-hosts

Print the IAM policy, list offerings, list hosts, allocate hosts, or release
AWS EC2 Mac Dedicated Hosts through the coordinator. `policy`, `offerings`,
`list`, and `allocate --dry-run` are read-only. Real `allocate` and `release`
require `--force` because EC2 Mac Dedicated Hosts are billed separately from
Crabbox leases and have AWS lifecycle constraints.

The coordinator AWS identity must allow `ec2:DescribeInstanceTypeOfferings`,
`ec2:DescribeHosts`, `ec2:AllocateHosts`, `ec2:ReleaseHosts`, and
`ec2:CreateTags` for this command group. `AllocateHosts` uses create-time tags,
so `CreateTags` should be allowed only when the EC2 create action is
`AllocateHosts`:

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
    }
  ]
}
```

This policy is intentionally limited to EC2 Mac Dedicated Host lifecycle
operations. End-to-end macOS image validation also needs the normal brokered
AWS provider permissions for key pairs, security groups, `RunInstances`,
`TerminateInstances`, image creation/promotion, snapshot cleanup, and optional
Service Quotas reads. See [Infrastructure](../infrastructure.md#aws-ec2) before
running the paid macOS image smoke.

Flags:

```text
policy:
  prints copy-pasteable AWS IAM JSON for EC2 Mac host lifecycle permissions

list:
  --region <region>     AWS region
  --type <type>         filter by mac1.metal, mac2.metal, or another Mac type
  --state <state>       filter by host state
  --json                print JSON

offerings:
  --region <region>     AWS region
  --type <type>         default mac2.metal
  --json                print JSON

allocate:
  --region <region>             AWS region
  --availability-zone <az>      optional; omitted means discover and try offered AZs
  --type <type>                 default mac2.metal
  --dry-run                     validate the request without allocating a host
  --force                       confirm host allocation
  --json                        print JSON

release:
  --id <host-id> or positional host id
  --region <region>
  --force                       confirm host release
  --json                        print JSON
```

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
