# AWS Provider

Read when:

- choosing `provider: aws`;
- debugging EC2 capacity, quotas, AMIs, security groups, or EC2 Mac hosts;
- changing `internal/providers/aws` or brokered AWS provisioning.

AWS is the broad managed provider. It supports Linux, native Windows, Windows
WSL2, and EC2 Mac leases. The backend is an SSH lease provider: after
provisioning, Crabbox owns SSH readiness, sync, command execution, results,
desktop tunnels, and cleanup.

## When To Use

Use AWS when you need:

- managed Windows or WSL2 test machines on EC2 capacity;
- EC2 Mac desktops through a configured Dedicated Host;
- broad Linux capacity with Spot and On-Demand fallback;
- coordinator-owned cloud credentials and cost accounting.

Use Hetzner for cheaper Linux-only capacity. Use Static SSH when a known host
already exists.

## Commands

```sh
crabbox warmup --provider aws --class standard
crabbox run --provider aws --class fast -- pnpm test
crabbox run --provider aws --market on-demand -- pnpm check
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox warmup --provider aws --target macos --desktop --market on-demand
```

`--type` is exact. If EC2 rejects that type, Crabbox fails instead of silently
choosing another instance. Use `--class` when fallback is desired.

## Config

```yaml
provider: aws
target: linux
class: beast
market: spot
aws:
  region: us-east-1
  ami: ""
  securityGroupId: ""
  subnetId: ""
  instanceProfile: ""
  rootGB: 120
  sshCIDRs: []
```

Important direct-mode environment:

```text
AWS_PROFILE
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN
CRABBOX_AWS_REGION
CRABBOX_AWS_AMI
CRABBOX_AWS_SECURITY_GROUP_ID
CRABBOX_AWS_SUBNET_ID
CRABBOX_AWS_INSTANCE_PROFILE
CRABBOX_AWS_ROOT_GB
CRABBOX_AWS_SSH_CIDRS
CRABBOX_HOST_ID
CRABBOX_AWS_MAC_HOST_ID legacy AWS alias
CRABBOX_CAPACITY_REGIONS
CRABBOX_CAPACITY_AVAILABILITY_ZONES
CRABBOX_CAPACITY_HINTS
CRABBOX_CAPACITY_LARGE_CLASSES
```

Brokered AWS credentials belong in the Worker, not on developer machines.

## Targets

| Target | Notes |
| --- | --- |
| Linux | Ubuntu bootstrap, SSH, rsync, optional desktop/browser/code. |
| Windows native | EC2Launch, OpenSSH, Git for Windows, archive sync; optional TightVNC/autologon with `--desktop`. |
| Windows WSL2 | Nested virtualization families; POSIX sync and commands through WSL. |
| macOS | Requires an available EC2 Mac Dedicated Host in the selected region; On-Demand only. `CRABBOX_HOST_ID` or `hostId` can pin a specific host; `CRABBOX_AWS_MAC_HOST_ID` and `aws.macHostId` remain AWS compatibility aliases. |

## Lifecycle

1. Import or reuse the lease SSH key.
2. Select region, market, instance type, subnet, AMI, and security group.
3. Launch EC2 instance, Spot request, Windows instance, or EC2 Mac host-backed
   instance.
4. Tag instance, volumes, and Spot requests with Crabbox lease labels.
5. Wait for SSH readiness, and for `crabbox-ready` on POSIX targets.
6. Let core sync and run over SSH.
7. Terminate on release, cleanup, or coordinator expiry.

Brokered cleanup is coordinator-owned. Direct cleanup is best-effort through
provider labels and `crabbox cleanup`.

## Capabilities

- SSH: yes.
- Crabbox sync: yes.
- Desktop/browser/code: yes, target-dependent.
- Tailscale: Linux managed leases.
- Actions hydration: Linux SSH leases only.
- Coordinator: yes.

## Gotchas

- Spot capacity and quota errors are normal. Prefer classes over exact `--type`
  when you want fallback.
- Brokered leases include `capacityHints` unless disabled with
  `capacity.hints: false` or `CRABBOX_CAPACITY_HINTS=0`.
- During capacity pressure, prefer `standard` or `fast` plus multiple
  `CRABBOX_CAPACITY_REGIONS`; `beast` starts at 48xlarge candidates and can
  consume 192 vCPUs per request.
- Windows WSL2 needs nested virtualization instance families. If you pass an
  exact `--type`, use the listed C8i/M8i/M8i Flex/R8i families; M7/T3-style
  Windows types are rejected before leasing.
- EC2 Mac needs an allocated Dedicated Host in the selected region.
- VNC stays behind SSH tunnels; do not expose VNC ports directly.

Related docs:

- [Feature: AWS](../features/aws.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
- [Provider backends](../provider-backends.md)
