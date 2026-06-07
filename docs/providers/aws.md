# AWS Provider

Read this when you are:

- choosing `provider: aws`;
- debugging EC2 capacity, Service Quotas, AMIs, security groups, or EC2 Mac
  Dedicated Hosts;
- changing `internal/providers/aws` or brokered AWS provisioning in the Worker.

AWS is the broad managed provider. It is an SSH-lease backend: Crabbox
provisions an EC2 instance, then owns SSH readiness, sync, command execution,
results, desktop tunnels, and cleanup. It supports Linux, native Windows,
Windows under WSL2, and EC2 Mac. AWS is one of the four providers that can run
through the Worker broker (alongside Hetzner, Azure, and GCP); without a broker
URL configured it runs direct-from-CLI against the EC2 API.

## When to use AWS

Reach for AWS when you need:

- managed Windows or WSL2 test machines on EC2 capacity;
- EC2 Mac desktops backed by a Dedicated Host;
- broad Linux capacity with Spot and On-Demand fallback;
- broker-owned cloud credentials and cost accounting.

Prefer [Hetzner](./hetzner.md) for cheaper Linux-only capacity, or
[Static SSH](./ssh.md) when a known host already exists.

## Commands

```sh
crabbox warmup --provider aws --class standard
crabbox warmup --provider aws --arch arm64 --class fast
crabbox run --provider aws --class fast -- pnpm test
crabbox run --provider aws --market on-demand -- pnpm check
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox warmup --provider aws --target macos --desktop --market on-demand
```

`--type` is exact: if EC2 rejects the requested type, Crabbox fails rather than
silently substituting another instance. Use `--class` when you want capacity
fallback across instance families.

### Instance classes

When you pass `--class` instead of `--type`, Crabbox tries an ordered list of
instance types and falls back across families on capacity or quota errors. For
Linux the classes resolve to (first candidate shown):

| Class | First candidate | vCPUs |
| --- | --- | --- |
| `standard` | `c7a.8xlarge` | 32 |
| `fast` | `c7a.16xlarge` | 64 |
| `large` | `c7a.24xlarge` | 96 |
| `beast` (default) | `c7a.48xlarge` | 192 |

Windows and macOS targets use their own candidate lists (Windows WSL2 uses
nested-virtualization families; macOS uses `mac*.metal` types). The default
class is `beast`.

## Configuration

```yaml
provider: aws
target: linux
architecture: amd64
class: beast
market: spot
aws:
  region: eu-west-1        # default eu-west-1
  ami: ""                  # override the auto-selected AMI
  securityGroupId: ""      # reuse an existing security group
  subnetId: ""             # pin a subnet (and its VPC)
  instanceProfile: ""      # IAM instance profile name to attach
  rootGB: 400              # default 400
  sshCIDRs: []             # allowed SSH source ranges
  macHostId: ""            # pin an EC2 Mac Dedicated Host
```

Set `architecture: arm64` or pass `--arch arm64` for Linux Graviton leases.
Crabbox switches class fallback to C7g/M7g/R7g families and resolves Canonical
Ubuntu ARM64 AMIs unless `aws.ami` is pinned. ARM64 is not supported for managed
Windows or WSL2 targets.

### Environment variables (direct mode)

```text
AWS_PROFILE
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN
AWS_REGION
CRABBOX_AWS_REGION                  # overrides AWS_REGION / aws.region
CRABBOX_AWS_AMI
CRABBOX_AWS_SECURITY_GROUP_ID
CRABBOX_AWS_SUBNET_ID
CRABBOX_AWS_INSTANCE_PROFILE
CRABBOX_AWS_ROOT_GB
CRABBOX_AWS_SSH_CIDRS               # comma-separated
CRABBOX_HOST_ID                     # pin a Dedicated Host (EC2 Mac)
CRABBOX_AWS_MAC_HOST_ID             # legacy alias for the Mac host id
CRABBOX_CAPACITY_REGIONS            # comma-separated fallback regions
CRABBOX_CAPACITY_AVAILABILITY_ZONES
CRABBOX_CAPACITY_HINTS              # 0 disables brokered capacity hints
```

Notes:

- The standard AWS SDK credential chain applies in direct mode (`AWS_PROFILE`,
  static keys, SSO, etc.). `aws.instanceProfile` is the **IAM instance profile**
  attached to the launched instance, not the local AWS CLI profile.
- `CRABBOX_AWS_REGION` wins over `AWS_REGION` and `aws.region`; the built-in
  default region is `eu-west-1`.
- For brokered AWS, the cloud credentials live in the Worker, not on developer
  machines. See `crabbox config set-broker --provider aws` and the brokered
  IAM policy from `crabbox admin aws-policy`.

## Targets

| Target | Notes |
| --- | --- |
| Linux | Ubuntu bootstrap, SSH, rsync sync, optional desktop/browser/code, Tailscale, Actions hydration. |
| Windows native | EC2Launch bootstrap, OpenSSH, Git for Windows, archive sync; optional desktop with `--desktop`. |
| Windows WSL2 | `--windows-mode wsl2`; launches on nested-virtualization families (`c8i`/`m8i`/`m8i-flex`/`r8i`); POSIX sync and commands run inside WSL. |
| macOS | Requires an available EC2 Mac Dedicated Host in the region; On-Demand only. Pin a host with `CRABBOX_HOST_ID` / `aws.macHostId` (`CRABBOX_AWS_MAC_HOST_ID` is a legacy alias). |

## Lifecycle

1. Import or reuse the per-lease SSH key (RSA for native Windows, ed25519
   otherwise).
2. Select region, market, instance type, subnet, AMI, and security group.
3. Launch the EC2 instance — Spot request, On-Demand instance, native Windows
   instance, or EC2 Mac host-backed instance.
4. Tag the instance, volumes, and Spot requests with Crabbox lease labels.
5. Wait for SSH readiness, plus the `crabbox-ready` marker on POSIX targets.
6. Hand off to core for sync and command execution over SSH.
7. Terminate on release, cleanup, or broker expiry.

Brokered cleanup is owned by the Worker (lease expiry plus an AWS orphan
sweep). Direct cleanup is best-effort via provider labels and
`crabbox cleanup --provider aws`.

## Checkpoints

AWS supports provider-native checkpoints in addition to workspace archives:

- Linux/macOS, image strategy or macOS target → AWS AMI
  (`checkpoint --kind native`, kind `aws-ami`).
- Linux disk-snapshot strategy → EBS snapshot (`aws-ebs-snapshot`).
- Native Windows targets do not support native checkpoints.

In brokered mode you can promote and warm AMIs:

- `crabbox image promote` promotes a brokered AMI for a target/region.
- `crabbox image fsr-status --provider aws` reports Fast Snapshot Restore state.

## Capabilities

- SSH: yes.
- Crabbox sync: yes.
- Desktop / browser / code: yes, target-dependent.
- Tailscale: Linux managed leases.
- Actions hydration: Linux SSH leases only.
- Coordinator (broker): supported.

## Gotchas

- Spot capacity and quota errors are normal. Prefer `--class` over an exact
  `--type` when you want fallback.
- Run `crabbox doctor --provider aws` before the first warmup in a new or
  unfunded account. Doctor reads EC2 vCPU Service Quotas for the effective
  class/type and recommends a smaller class/type when `beast` would exceed the
  account cap.
- `beast` starts at 48xlarge candidates and can consume up to 192 vCPUs per
  request. Under capacity pressure, prefer `standard` or `fast` plus several
  `CRABBOX_CAPACITY_REGIONS`.
- Brokered leases include capacity hints unless disabled with
  `capacity.hints: false` or `CRABBOX_CAPACITY_HINTS=0`.
- Windows WSL2 requires nested-virtualization families. An exact `--type` must
  be a `c8i`/`m8i`/`m8i-flex`/`r8i` instance; `m7`/`t3`-style Windows types are
  rejected before leasing.
- EC2 Mac requires an allocated Dedicated Host in the selected region and is
  On-Demand only.
- VNC stays behind SSH tunnels; never expose VNC ports directly.

## Related docs

- [AWS feature notes](../features/aws.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
- [Provider backends](../provider-backends.md)
