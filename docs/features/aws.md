# AWS

Read when:

- choosing AWS as the Crabbox provider;
- debugging EC2 capacity, quotas, AMIs, security groups, or EC2 Mac hosts;
- changing AWS provisioning code in the CLI (`internal/cli/aws.go`) or coordinator
  (`worker/src/aws.ts`).

AWS is Crabbox's broadest managed provider. It leases full SSH-reachable EC2
boxes and supports four targets: Linux, native Windows, Windows WSL2, and EC2
Mac. AWS is one of the four brokerable providers, so it can run two ways:

- **Brokered** — the coordinator holds the AWS credentials and provisions
  on your behalf. The CLI still talks SSH/rsync directly to the runner.
- **Direct** — no broker configured. The CLI uses the local AWS credential chain
  (`AWS_PROFILE`, env keys, or shared config) and provisions itself. This is the
  usual path for provider debugging.

## Targets

| Target | Notes |
| --- | --- |
| Linux | Spot by default, On-Demand optional; cloud-init bootstrap. |
| Windows native | EC2Launch, OpenSSH, Git for Windows, archive sync; optional desktop with `--desktop`. |
| Windows WSL2 | Nested virtualization on C8i/M8i/R8i families; POSIX sync through WSL. |
| macOS | Requires an available EC2 Mac Dedicated Host. Brokered mode can discover one; direct mode needs a host id. On-Demand only. |

```sh
crabbox warmup --provider aws --class beast
crabbox warmup --provider aws --arch arm64 --class fast
crabbox run --provider aws --class beast --market on-demand -- pnpm check
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox warmup --provider aws --target macos --desktop --market on-demand
```

## Capacity And Fallback

AWS Linux defaults to Spot. Use `--market on-demand` for a single lease when
Spot is blocked or when an account only has On-Demand quota. Set
`capacity.fallback: on-demand` (or `CRABBOX_CAPACITY_FALLBACK=on-demand`) to fall
back to On-Demand automatically after Spot capacity/quota rejections.

Crabbox tries an ordered list of instance candidates for the requested class
(below). An explicit `--type` is exact: if EC2 rejects it, Crabbox fails clearly
instead of silently picking another type. During capacity incidents, prefer
`standard` or `fast`; `beast` starts at 48xlarge candidates and can request up to
192 vCPUs before fallback.

Give AWS more regional headroom with `CRABBOX_CAPACITY_REGIONS` or
`capacity.regions`. Both brokered and direct launches try the primary region
first, then each configured region in order. Pin specific zones with
`CRABBOX_CAPACITY_AVAILABILITY_ZONES`. The public coordinator defaults its pool
to:

```sh
CRABBOX_CAPACITY_REGIONS=eu-west-1,eu-west-2,eu-central-1,us-east-1,us-west-2
```

Brokered AWS leases return capacity hints in the lease payload and CLI output:
selected region/market, failed-attempt regions, quota pressure, Spot-to-On-Demand
fallback, and high-pressure class warnings. Suppress them with
`capacity.hints: false` or `CRABBOX_CAPACITY_HINTS=0`. Set
`CRABBOX_CAPACITY_LARGE_CLASSES=beast,large` to change which classes raise
high-pressure warnings.

These response fields are wire-compatible across mixed CLI/broker versions:
upgraded brokers add optional fields older clients ignore, and upgraded clients
keep the lease request sparse (they omit default hint/routing fields and send no
capacity block at all for broker defaults) unless an operator configures a
non-default market/fallback, a multi-region pool, pinned zones, or
`capacity.hints: false`.

See [Capacity and fallback](capacity-fallback.md) for the shared model across
providers.

### Class candidates

```text
AWS Linux
standard  c7a.8xlarge, c7i.8xlarge, m7a.8xlarge, m7i.8xlarge, c7a.4xlarge
fast      c7a.16xlarge, c7i.16xlarge, m7a.16xlarge, m7i.16xlarge, c7a.12xlarge, c7a.8xlarge
large     c7a.24xlarge, c7i.24xlarge, m7a.24xlarge, m7i.24xlarge, r7a.24xlarge, c7a.16xlarge, c7a.12xlarge
beast     c7a.48xlarge, c7i.48xlarge, m7a.48xlarge, m7i.48xlarge, r7a.48xlarge, c7a.32xlarge, c7i.32xlarge, m7a.32xlarge, c7a.24xlarge, c7a.16xlarge

AWS Linux ARM64 (--arch arm64)
standard  c7g.8xlarge, m7g.8xlarge, r7g.8xlarge, c7g.4xlarge
fast      c7g.16xlarge, m7g.16xlarge, r7g.16xlarge, c7g.12xlarge, c7g.8xlarge
large     c7g.16xlarge, m7g.16xlarge, r7g.16xlarge, c7g.12xlarge
beast     c7g.16xlarge, m7g.16xlarge, r7g.16xlarge, c7g.12xlarge

AWS Windows
standard  m7i.large, m7a.large, t3.large
fast      m7i.xlarge, m7a.xlarge, t3.xlarge
large     m7i.2xlarge, m7a.2xlarge, t3.2xlarge
beast     m7i.4xlarge, m7a.4xlarge, m7i.2xlarge

AWS Windows WSL2
standard  m8i.large, m8i-flex.large, c8i.large, r8i.large
fast      m8i.xlarge, m8i-flex.xlarge, c8i.xlarge, r8i.xlarge
large     m8i.2xlarge, m8i-flex.2xlarge, c8i.2xlarge, r8i.2xlarge
beast     m8i.4xlarge, m8i-flex.4xlarge, c8i.4xlarge, r8i.4xlarge, m8i.2xlarge

AWS macOS
all       mac2.metal, mac2-m2.metal, mac2-m2pro.metal, mac-m4.metal, mac-m4pro.metal, mac-m4max.metal, mac2-m1ultra.metal, mac-m3ultra.metal, then mac1.metal unless --type is set
```

Windows WSL2 needs nested-virtualization families. An explicit `--type` must come
from the C8i/M8i/M8i-flex/R8i families; Crabbox rejects unsupported families
(such as M7 or T3) before asking AWS or the coordinator for a lease. Omit
`--type` to let class fallback choose.

## Images

- **Linux** resolves the latest Ubuntu 26.04 AMI from Canonical for the selected
  architecture. Pass `--arch arm64` for Graviton/ARM64 capacity and
  `--os ubuntu:24.04` for the previous LTS. Supported selectors:
  `ubuntu:26.04` and `ubuntu:24.04`.
- **Windows** resolves the latest Windows Server 2022 English Full Base AMI.
- **macOS** resolves the matching Amazon EC2 macOS AMI for the chosen instance
  family (arm64 for Apple silicon, x86_64 for `mac1.metal`).

Override the resolved image with `CRABBOX_AWS_AMI` / `aws.ami`. Operators can
also bake and promote trusted AWS images with `crabbox image` (see the
[image command](../commands/image.md)).

## Security And Networking

Crabbox imports or reuses an EC2 key pair, creates or reuses the
`crabbox-runners` security group when none is supplied, and opens only the SSH
ports to the configured CIDRs or the detected request source. The default root
volume is 400 GB gp3, encrypted; override with `CRABBOX_AWS_ROOT_GB` /
`aws.rootGB`. VNC stays behind the SSH tunnel.

Supplying `CRABBOX_AWS_SECURITY_GROUP_ID` makes ingress policy your
responsibility. Set `CRABBOX_AWS_SUBNET_ID` to launch into a non-default VPC.

Account-level AWS guardrails should cover every region Crabbox can allocate in:

- **S3 Block Public Access** is an account-wide control. Enable all four settings
  once per account; AWS propagates them across regions.
- **The IAM account password policy** is global IAM state. Set it once per
  account when IAM users are present.
- **IAM Access Analyzer external-access analyzers** are regional. Create one in
  every region Crabbox can launch in, not only the primary `CRABBOX_AWS_REGION`:

```sh
for region in eu-west-1 eu-west-2 eu-central-1 us-east-1 us-west-2; do
  if ! aws accessanalyzer get-analyzer \
    --region "$region" \
    --analyzer-name crabbox-external-access >/dev/null 2>&1; then
    aws accessanalyzer create-analyzer \
      --region "$region" \
      --analyzer-name crabbox-external-access \
      --type ACCOUNT
  fi
done
```

### Orphan sweep

The brokered coordinator can sweep stray AWS instances itself. When
`CRABBOX_AWS_ORPHAN_SWEEP_ENABLED` is not disabled and AWS broker credentials are
present, the Fleet Durable Object alarm periodically scans `CRABBOX_AWS_REGION`
plus `CRABBOX_CAPACITY_REGIONS` for Crabbox-tagged EC2 instances; the Worker cron
handler bootstraps the alarm for idle fleets after a deploy or config change. The
sweep uses equivalent pg-boss scheduling and reconciliation on Node/PostgreSQL. The
sweep only terminates candidates with an exact retained coordinator lease
binding when `CRABBOX_AWS_ORPHAN_SWEEP_DELETE=1`; provider tags alone remain
report-only. Otherwise it stores the latest report for admin inspection. Tune
cadence and grace with
`CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS` and
`CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS`.

## EC2 Mac Hosts

EC2 Mac instances run only on Dedicated Hosts, and host lifecycle is explicit
operator work. Admin-authenticated brokered mode can pin a host with
`CRABBOX_HOST_ID` (legacy alias `CRABBOX_AWS_MAC_HOST_ID`). A normal broker user
can pin a host only when a released AWS macOS lease proves that the same owner
and organization previously used it; other host pins remain admin-only. Pinned
launches wait through the bounded capacity handoff after the previous Mac
instance terminates instead of failing immediately. Unpinned launches use
automatic available-host discovery in the region.

```sh
crabbox admin providers identity --provider aws --region eu-west-1
crabbox admin providers policy --provider aws --target macos
crabbox admin hosts list --provider aws --target macos --region eu-west-1
crabbox admin hosts offerings --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --dry-run
crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --force
crabbox admin hosts release h-0123456789abcdef0 --provider aws --target macos --region eu-west-1 --force
```

The coordinator AWS identity needs `ec2:DescribeInstanceTypeOfferings`,
`ec2:DescribeHosts`, `ec2:AllocateHosts`, `ec2:ReleaseHosts`, and
`ec2:CreateTags` for host lifecycle work, plus `servicequotas:GetServiceQuota`
for known Mac host quota checks and `servicequotas:ListServiceQuotas` as a
fallback for future Mac host families. `CreateTags` is required because Crabbox
tags hosts during `AllocateHosts`; scope it with
`ec2:CreateAction=AllocateHosts`. Run `admin hosts allocate --dry-run` first to
validate the request path without creating a host, and use
`admin providers identity --provider aws` to confirm which AWS principal needs
the policy.

IAM is not the only preflight. AWS tracks Dedicated Mac host capacity through
separate EC2 Service Quotas such as
[Running Dedicated mac2 Hosts](https://docs.aws.amazon.com/ec2/latest/instancetypes/ec2-instance-quotas.html),
which `admin hosts quota --provider aws --target macos` inspects through the
coordinator. If `allocate --dry-run` succeeds, check quota before treating a real
allocation failure as a runtime bug.

Host lifecycle policy is not the full macOS image policy. Later warmup, WebVNC,
AMI create, candidate boot, promotion, and cleanup phases also need the normal
brokered AWS provider permissions documented in
[Infrastructure](../infrastructure.md#aws-ec2) (launch/list/tag/terminate, key
pair, security group, image, snapshot, baseline Service Quotas). Print the
combined provider plus Dedicated Host policy with
`crabbox admin providers policy --provider aws --target macos`, or print the two
grants separately with `crabbox admin providers policy --provider aws` and
`crabbox admin hosts policy --provider aws --target macos`.

## Credentials And Configuration

### Broker secrets

```text
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN              optional
CRABBOX_HOST_ID               optional; admin-only except owner reuse of a released Mac host
CRABBOX_AWS_MAC_HOST_ID       optional legacy alias for CRABBOX_HOST_ID
```

### CLI / direct env and config

```text
AWS_PROFILE
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN
AWS_REGION                    fallback for CRABBOX_AWS_REGION
CRABBOX_AWS_REGION            default eu-west-1
CRABBOX_AWS_AMI
CRABBOX_AWS_SECURITY_GROUP_ID
CRABBOX_AWS_SUBNET_ID
CRABBOX_AWS_INSTANCE_PROFILE
CRABBOX_AWS_ROOT_GB           default 400
CRABBOX_AWS_SSH_CIDRS
CRABBOX_HOST_ID
CRABBOX_AWS_MAC_HOST_ID       legacy alias
CRABBOX_AWS_ORPHAN_SWEEP_ENABLED
CRABBOX_AWS_ORPHAN_SWEEP_DELETE
CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS
CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS
CRABBOX_CAPACITY_MARKET
CRABBOX_CAPACITY_FALLBACK
CRABBOX_CAPACITY_REGIONS
CRABBOX_CAPACITY_AVAILABILITY_ZONES
CRABBOX_CAPACITY_HINTS
CRABBOX_CAPACITY_LARGE_CLASSES
```

The same values are available as `aws.*` keys (`region`, `ami`,
`securityGroupId`, `subnetId`, `instanceProfile`, `rootGB`, `sshCIDRs`,
`macHostId`) and `capacity.*` keys (`market`, `fallback`, `regions`,
`availabilityZones`, `hints`) in the config file.

Related docs:

- [Providers](providers.md)
- [Capacity and fallback](capacity-fallback.md)
- [Linux VNC](vnc-linux.md)
- [Windows VNC](vnc-windows.md)
- [macOS VNC](vnc-macos.md)
- [Infrastructure](../infrastructure.md)
- [image command](../commands/image.md)
