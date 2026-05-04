# AWS

Read when:

- choosing AWS as the Crabbox provider;
- debugging EC2 capacity, quotas, AMIs, security groups, or EC2 Mac hosts;
- changing AWS provisioning code in the CLI or Worker.

AWS is Crabbox's broad managed provider. It supports Linux, native Windows,
Windows WSL2, and EC2 Mac targets. Brokered mode keeps AWS credentials in the
Cloudflare Worker; direct mode uses the local AWS credential chain for provider
debugging.

## Targets

| Target | Managed | Notes |
| --- | --- | --- |
| Linux | Yes | Spot by default; On-Demand optional; cloud-init bootstrap. |
| Windows native | Yes | EC2Launch, OpenSSH, Git for Windows, TightVNC, archive sync. |
| Windows WSL2 | Yes | Nested virtualization on C8i/M8i/R8i families; POSIX sync through WSL. |
| macOS | Yes | EC2 Mac Dedicated Host id required; On-Demand only. |

Examples:

```sh
crabbox warmup --provider aws --class beast
crabbox run --provider aws --class beast --market on-demand -- pnpm check
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider aws --target windows --windows-mode wsl2
CRABBOX_AWS_MAC_HOST_ID=h-... crabbox warmup --provider aws --target macos --desktop --market on-demand
```

## Capacity

AWS Linux defaults to Spot. Use `--market on-demand` for one lease when Spot is
blocked or when an account only has On-Demand quota. `capacity.fallback` can
fall back to On-Demand after Spot capacity/quota failures when configured.

Crabbox tries ordered instance candidates for the requested class. Explicit
`--type` is exact: if EC2 rejects it, Crabbox fails clearly instead of silently
choosing another type.

Current class defaults:

```text
AWS Linux
standard  c7a.8xlarge, c7i.8xlarge, m7a.8xlarge, m7i.8xlarge, c7a.4xlarge
fast      c7a.16xlarge, c7i.16xlarge, m7a.16xlarge, m7i.16xlarge, c7a.12xlarge, c7a.8xlarge
large     c7a.24xlarge, c7i.24xlarge, m7a.24xlarge, m7i.24xlarge, r7a.24xlarge, c7a.16xlarge, c7a.12xlarge
beast     c7a.48xlarge, c7i.48xlarge, m7a.48xlarge, m7i.48xlarge, r7a.48xlarge, c7a.32xlarge, c7i.32xlarge, m7a.32xlarge, c7a.24xlarge, c7a.16xlarge

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
all       mac2.metal unless `--type` is set
```

## Broker Secrets And Env

Worker secrets:

```text
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN optional
CRABBOX_AWS_MAC_HOST_ID optional; required for brokered target=macos
```

CLI/direct env and config:

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
CRABBOX_AWS_MAC_HOST_ID
```

## Security And Networking

Crabbox imports or reuses an EC2 key pair, creates or reuses the
`crabbox-runners` security group when no security group is supplied, and opens
only SSH ports to configured CIDRs or the detected request source. VNC stays
behind the SSH tunnel. Supplying `CRABBOX_AWS_SECURITY_GROUP_ID` makes ingress
policy your responsibility.

## Images

Linux resolves the latest Ubuntu 24.04 x86_64 AMI unless overridden. Windows
resolves the latest Windows Server 2022 English Full Base AMI unless overridden.
Operators can create and promote trusted AWS images with `crabbox image`.

Related docs:

- [Providers](providers.md)
- [Linux VNC](vnc-linux.md)
- [Windows VNC](vnc-windows.md)
- [macOS VNC](vnc-macos.md)
- [Infrastructure](../infrastructure.md)
- [image command](../commands/image.md)
