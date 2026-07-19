# admin

`crabbox admin` groups trusted operator controls for coordinator-backed leases and the cloud resources behind them. Use it to inspect every lease the broker tracks, reconcile expired leases against live cloud state, force-release or delete a backing server, print provider IAM policies, and manage host lifecycle resources.

## Requirements

Every `admin` subcommand needs both a configured coordinator and a separate admin bearer token. The token is read from `broker.adminToken` in config or the `CRABBOX_COORDINATOR_ADMIN_TOKEN` environment variable. The ordinary operator/shared token (`broker.token` / `CRABBOX_COORDINATOR_TOKEN`) is not sufficient for admin routes — commands fail with a configuration error when only the shared token is present.

## At a glance

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

The text output prints one line per lease with ID, slug, provider, state, server type, host, owner, org, idle timeout, and expiry.

## lease-audit

Check coordinator lease records against the backing cloud provider. The audit currently reconciles AWS leases and reports, for each matching lease, whether its `cloudID` is still `found`, `missing`, or could not be checked (`error`). Each line also surfaces cleanup attempt counts and errors recorded by the broker's expiry sweep.

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
crabbox admin providers policy --provider aws --target macos
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
--target <target>       optional; macos combines provider plus host lifecycle policy
--host-lifecycle        include provider host lifecycle permissions
--mac-hosts             legacy alias for --host-lifecycle
```

The baseline AWS provider policy covers key pairs, instance launch and termination, managed security groups, image creation/promotion, snapshot cleanup, and Service Quotas reads. If you set `CRABBOX_AWS_INSTANCE_PROFILE`, add a separate scoped `iam:PassRole` grant for that role with `iam:PassedToService=ec2.amazonaws.com`.

## hosts

Inspect and manage host lifecycle resources through provider- and target-scoped subcommands. Today `--provider aws --target macos` maps to AWS EC2 Mac Dedicated Host operations; the command shape is intentionally generic so other providers can add macOS host backends without introducing new top-level admin nouns.

`policy`, `list`, `offerings`, `quota`, and `allocate --dry-run` are read-only. Real `allocate` and `release` require `--force`, because host resources are billed separately from leases and carry provider lifecycle constraints.

All subcommands share the scope flags:

```text
--provider <provider>   host provider (default aws; currently aws)
--target <target>       host target OS (default macos; currently macos)
```

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

Requires `--force`. The host id may be given positionally (for example, `crabbox admin hosts release h-0123456789abcdef0 --force`) or via `--id`.

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
- `crabbox admin mac-hosts <list|offerings|quota|allocate|release|policy>` — alias for `crabbox admin hosts --provider aws --target macos`.

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
