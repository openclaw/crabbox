# macOS VNC

Read when:

- launching managed AWS EC2 Mac desktop leases;
- preparing a static Mac for Crabbox VNC;
- debugging Screen Sharing credentials or EC2 Mac host requirements.

Crabbox supports macOS in two ways:

- managed AWS EC2 Mac leases on an operator-provided Dedicated Host;
- static Macs reached through `provider: ssh`.

## Managed AWS EC2 Mac

```sh
crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id silver-squid --open
crabbox screenshot --id silver-squid --output macos.png
```

EC2 Mac requirements:

- an allocated EC2 Mac Dedicated Host in the selected region;
- an optional `CRABBOX_HOST_ID` or `hostId` when you want to pin the lease to a
  specific Dedicated Host; `CRABBOX_AWS_MAC_HOST_ID` and `aws.macHostId` remain
  AWS compatibility aliases;
- On-Demand capacity;
- default fallback across current Apple silicon EC2 Mac families, then
  `mac1.metal`, unless `--type` is set.

Bootstrap enables Screen Sharing for `ec2-user`, sets a generated per-lease
password, stores it at `/var/db/crabbox/vnc.password`, and keeps access behind
the SSH tunnel. Managed EC2 Mac leases use `/Users/ec2-user/crabbox` as the
default work root because the macOS system volume is read-only. `crabbox vnc`
prints:

```text
macos username: ec2-user
macos password: ...
```

`crabbox screenshot` captures the same Screen Sharing/VNC framebuffer used by
WebVNC. It does not use `screencapture`, which is not reliable from EC2 Mac
non-interactive SSH sessions.

AWS EC2 Mac has a provider-level lifecycle constraint: Mac instances run on
allocated Dedicated Hosts with a 24-hour minimum host allocation period.
Crabbox launches onto an available host or the host id you provide. Warmup does
not allocate a host implicitly, but trusted operators can manage hosts
explicitly:

```sh
crabbox admin hosts list --provider aws --target macos --region eu-west-1
crabbox admin hosts offerings --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --dry-run
crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --force
crabbox admin hosts release h-0123456789abcdef0 --provider aws --target macos --region eu-west-1 --force
```

Promoted AWS images are scoped by target, architecture, and region. Use
`crabbox image promote <ami-id> --target macos --region <aws-region>` when
promoting a macOS AMI that was not created through `crabbox image create`.

## Static Mac

Static Mac targets are existing machines:

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.tailnet-name.ts.net
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

```sh
crabbox vnc --provider ssh --target macos --static-host mac-studio.tailnet-name.ts.net --host-managed --open
```

The Mac must already have SSH, `git`, `rsync`, `tar`, and Screen Sharing or a
VNC-compatible service. Credentials are host-managed. `--open` requires
`--host-managed` because the visible login prompt belongs to that Mac, not to a
Crabbox-created cloud lease.

Static Macs work well over Tailscale: put the MagicDNS name or 100.x address in
`static.host` and keep Screen Sharing limited to trusted networks.

## Troubleshooting

Missing host capacity

Use `--market on-demand` and verify an available EC2 Mac Dedicated Host is
allocated in the selected AWS region. Set `CRABBOX_HOST_ID` or `hostId` only
when you want to pin to a specific host. Trusted operators can check host
offerings with `crabbox admin hosts offerings --provider aws --target macos --region <region>`,
quota with `crabbox admin hosts quota --provider aws --target macos --region <region>`,
and allocated hosts with `crabbox admin hosts list --provider aws --target macos --region <region>`.

VNC prompt asks for host credentials

If `managed: false`, you opened a static Mac. Use the Mac's own Screen Sharing
credentials. Managed AWS EC2 Mac leases print the generated `ec2-user`
password.

Related docs:

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [AWS](aws.md)
- [vnc command](../commands/vnc.md)
- [screenshot command](../commands/screenshot.md)
