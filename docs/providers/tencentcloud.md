# Tencent Cloud CVM Provider

Read this when you are:

- choosing `provider: tencentcloud`;
- validating a direct Tencent Cloud CVM SSH lease;
- changing `internal/providers/tencentcloud` or live smoke coverage.

Tencent Cloud is registered as a Linux-only **SSH lease** provider. Crabbox
creates a CVM instance, injects the standard Crabbox cloud-init bootstrap
through CVM user data, writes Crabbox ownership metadata as Tencent Cloud tags,
waits for public IPv4 and SSH readiness, then uses the normal Crabbox
SSH/sync/run/stop/cleanup path.

The provider is **direct-only** in this release. It does not run through the
coordinator, so the local CLI must have Tencent Cloud API credentials and direct
cleanup remains the operator's responsibility.

## When To Use It

Use Tencent Cloud CVM when you want Crabbox Linux leases in a Tencent Cloud
account and local direct credentials are acceptable. Prefer AWS, Azure, GCP, or
Hetzner when you need a brokered team path, coordinator-side credentials, or
existing coordinator cost accounting.

## Commands

Auth and inventory can be checked without creating resources:

```sh
crabbox doctor --provider tencentcloud
```

The provider is available through the normal SSH-lease command surface:

```sh
crabbox warmup --provider tencentcloud --tencentcloud-image img-xxxxxxxx
crabbox run --provider tencentcloud --tencentcloud-image img-xxxxxxxx -- pnpm test
crabbox ssh --provider tencentcloud --id my-app
crabbox stop --provider tencentcloud my-app
crabbox cleanup --provider tencentcloud --dry-run
```

`--id` accepts the canonical lease id (`cbx_...`), friendly slug, or Tencent
Cloud instance id (`ins-...`). `--type` is the generic Crabbox server type
override; `--tencentcloud-type` is the CVM instance type override.

## Configuration

```yaml
provider: tencentcloud
target: linux
class: standard
tencentcloud:
  region: ap-shanghai
  zone: ap-shanghai-2
  image: img-xxxxxxxx
  type: SA5.MEDIUM2
  vpcId: ""
  subnetId: ""
  securityGroupId: ""
  rootGB: 50
  internetChargeType: TRAFFIC_POSTPAID_BY_HOUR
  internetMaxBandwidthOut: 5
  sshCIDRs: []
```

Config keys under `tencentcloud:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `region` | `cfg.TencentCloud.Region` | `ap-shanghai` | Tencent Cloud API region. |
| `zone` | `cfg.TencentCloud.Zone` | `ap-shanghai-2` | CVM availability zone. |
| `image` | `cfg.TencentCloud.Image` | empty | Required CVM image ID, such as a public Ubuntu image or a custom image. |
| `type` | `cfg.TencentCloud.Type` | `SA5.MEDIUM2` | CVM instance type. |
| `vpcId` | `cfg.TencentCloud.VPCID` | empty | Optional VPC ID. Most current CVM instance families, including the default `SA5.MEDIUM2`, require VPC networking. |
| `subnetId` | `cfg.TencentCloud.SubnetID` | empty | Optional subnet ID. Set this with `vpcId` when the selected instance type requires VPC networking. |
| `securityGroupId` | `cfg.TencentCloud.SecurityGroupID` | empty | Optional existing security group ID. Crabbox does not create or mutate security groups in Phase 1. |
| `rootGB` | `cfg.TencentCloud.RootGB` | `50` | System disk size in GiB. |
| `internetChargeType` | `cfg.TencentCloud.InternetChargeType` | `TRAFFIC_POSTPAID_BY_HOUR` | Public bandwidth charge type passed to CVM. |
| `internetMaxBandwidthOut` | `cfg.TencentCloud.InternetMaxBandwidthOut` | `5` | Public outbound bandwidth in Mbps. |
| `sshCIDRs` | `cfg.TencentCloud.SSHCIDRs` | empty | Reserved for future security-group mutation. Non-empty values fail fast. |
| `apiEndpoint` | `cfg.TencentCloud.APIEndpoint` | `https://cvm.tencentcloudapi.com` | Optional CVM API endpoint override. Use `*.intl.tencentcloudapi.com` for international endpoint families when needed. |

Provider-specific flags:

```text
--tencentcloud-region <region>
--tencentcloud-zone <zone>
--tencentcloud-image <image-id>
--tencentcloud-type <instance-type>
--tencentcloud-vpc-id <vpc-id>
--tencentcloud-subnet-id <subnet-id>
--tencentcloud-security-group-id <security-group-id>
--tencentcloud-root-gb <gib>
--tencentcloud-internet-charge-type <charge-type>
--tencentcloud-internet-max-bandwidth-out <mbps>
--tencentcloud-api-endpoint <url-or-host>
```

Tencent Cloud leases default to SSH user `ubuntu` on port `22` with no fallback
port. Use an Ubuntu-compatible image, or set generic `ssh.user` if your image
expects another account. Explicit generic `ssh.user` and `ssh.port` values
remain authoritative.

Environment overrides:

```text
TENCENTCLOUD_SECRET_ID                      Tencent Cloud API SecretId
TENCENTCLOUD_SECRET_KEY                     Tencent Cloud API SecretKey
TENCENTCLOUD_TOKEN                          Optional temporary credential token
TENCENTCLOUD_ACCOUNT_ID                     Optional account UIN override for tag resource names
CRABBOX_TENCENTCLOUD_REGION                 Override the region
CRABBOX_TENCENTCLOUD_ZONE                   Override the zone
CRABBOX_TENCENTCLOUD_IMAGE                  Override the image ID
CRABBOX_TENCENTCLOUD_TYPE                   Override the CVM instance type
CRABBOX_TENCENTCLOUD_VPC_ID                 Override the VPC ID
CRABBOX_TENCENTCLOUD_SUBNET_ID              Override the subnet ID
CRABBOX_TENCENTCLOUD_SECURITY_GROUP_ID      Override the security group ID
CRABBOX_TENCENTCLOUD_ROOT_GB                Override the system disk size
CRABBOX_TENCENTCLOUD_INTERNET_CHARGE_TYPE   Override the public bandwidth charge type
CRABBOX_TENCENTCLOUD_INTERNET_MAX_BANDWIDTH_OUT
CRABBOX_TENCENTCLOUD_API_ENDPOINT           Override the CVM API endpoint
```

Do not pass Tencent Cloud secrets as command-line arguments. Keep them in the
environment, a local shell profile, or a secret manager.

## Required Tencent Cloud Permissions

The direct provider signs Tencent Cloud API v3 requests and uses CVM, Tag, and
STS APIs. A minimal custom policy should allow:

```text
cvm:DescribeInstances
cvm:RunInstances
cvm:TerminateInstances
tag:ModifyResourceTags
sts:GetCallerIdentity
```

Add VPC, image, or security-group permissions required by your account policy
when using custom images, subnets, or existing security groups. `doctor` calls
STS and `DescribeInstances`; it does not create resources.

## Lifecycle

1. Generate a per-lease SSH key under the Crabbox testbox key directory.
2. Create one CVM instance with configured region, zone, image, type, network,
   optional security group, public bandwidth, cloud-init user data, and Crabbox
   tags.
3. Wait for public IPv4 and Crabbox SSH bootstrap readiness.
4. Replace Crabbox tags with ready-state ownership metadata and claim the lease
   locally.
5. Run normal Crabbox sync/run/ssh workflows over SSH.
6. Update timeout/Tailscale tags on touch.
7. Terminate the CVM instance on `stop`; `cleanup` deletes only expired
   resources whose live tags still prove Crabbox ownership.

## Network And Security Groups

Phase 1 does not create or mutate Tencent Cloud security groups. Operators own
the selected VPC, subnet, default public IP behavior, and security-group
policy. If `tencentcloud.securityGroupId` is set, Crabbox attaches that
existing security group during `RunInstances`. If `tencentcloud.sshCIDRs` is
non-empty, acquire fails fast because Crabbox does not yet create ingress rules.

Use a preconfigured security group that allows SSH from trusted sources, short
TTLs, and `cleanup --dry-run` during validation.

## Guarded Live Smoke

The repeatable live check is opt-in:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=tencentcloud scripts/live-smoke.sh
```

or directly:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=tencentcloud scripts/live-tencentcloud-smoke.sh
```

The script builds `bin/crabbox` unless `CRABBOX_BIN` points at an existing
binary, verifies the Crabbox-owned Tencent Cloud inventory starts empty, creates
one short-lived CVM lease, runs `echo ok`, verifies `list --json`, stops the
lease, runs dry-run cleanup, and verifies the final inventory is empty.

Required inputs:

```text
TENCENTCLOUD_SECRET_ID
TENCENTCLOUD_SECRET_KEY
CRABBOX_LIVE_TENCENTCLOUD_IMAGE or CRABBOX_TENCENTCLOUD_IMAGE or tencentcloud.image
CRABBOX_TENCENTCLOUD_VPC_ID and CRABBOX_TENCENTCLOUD_SUBNET_ID for VPC-only instance types
```

Optional smoke overrides:

```text
CRABBOX_LIVE_TENCENTCLOUD_TYPE
CRABBOX_TENCENTCLOUD_REGION
CRABBOX_TENCENTCLOUD_ZONE
CRABBOX_TENCENTCLOUD_SECURITY_GROUP_ID
```

Final classifications include:

```text
classification=live_tencentcloud_smoke_passed
classification=environment_blocked
classification=quota_blocked
classification=validation_failed
classification=cleanup_failed
```

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Tailscale**: yes through the standard Linux cloud-init path when a direct
  Tailscale auth key is configured.
- **Desktop / browser / code**: not advertised in Phase 1.
- **Cleanup**: yes, tag-owned only.
- **Coordinator**: never; direct CLI only.

## Gotchas

- `tencentcloud.image` is required. Tencent Cloud image IDs can vary by region
  and account, so Crabbox does not guess one.
- Tag updates need the account UIN. Crabbox calls `sts:GetCallerIdentity`, or
  uses `TENCENTCLOUD_ACCOUNT_ID` when set.
- Instances are deleted only after live tags match the local claim's lease,
  slug, provider, and provider key.
- This provider does not yet support coordinator/broker mode, native
  checkpointing, pause/resume, or managed security-group rule creation.
