# macOS VNC

Read this when you:

- launch a managed AWS EC2 Mac desktop lease;
- prepare an existing Mac for Crabbox VNC over `provider: ssh`;
- debug Screen Sharing credentials or EC2 Mac Dedicated Host requirements.

Crabbox reaches macOS desktops two ways:

- **managed AWS EC2 Mac** leases, provisioned onto an operator-allocated
  Dedicated Host;
- **static Macs** you already own, reached through `provider: ssh`.

Both expose the desktop over Screen Sharing (Apple's VNC service) on the box's
loopback `127.0.0.1:5900`, and Crabbox tunnels that port to your machine over
SSH. Nothing listens on a public VNC port.

## Managed AWS EC2 Mac

```sh
crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id silver-squid --open
crabbox screenshot --id silver-squid --output macos.png
```

### Bootstrap behavior

When the lease comes up, Crabbox bootstraps the box for SSH and Screen Sharing:

- enables Remote Login (SSH) for `ec2-user` and installs the per-lease public
  key;
- generates a 16-character VNC password, sets it as the `ec2-user` account
  password, and stores it at `/var/db/crabbox/vnc.password` (mode `0600`);
- enables and starts `com.apple.screensharing`, bound to loopback and reached
  only through the SSH tunnel.

The default work root is `/Users/ec2-user/crabbox`, because the macOS system
volume is read-only. `crabbox vnc` reads the stored password back over SSH and
prints:

```text
macos username: ec2-user
macos password: ...
```

### Screenshots

`crabbox screenshot --target macos` captures the live Screen Sharing
framebuffer over the same SSH-tunneled VNC connection (an RFB frame grab), the
same surface WebVNC bridges. It does **not** shell out to `screencapture`, which
is unreliable from a non-interactive EC2 Mac SSH session.

### Instance types and the Dedicated Host

EC2 Mac instances run on **Dedicated Hosts** with AWS's 24-hour minimum host
allocation period, so the lifecycle differs from regular Linux/Windows leases:

- a macOS lease needs an allocated EC2 Mac Dedicated Host in the selected
  region;
- capacity is **On-Demand** only — pass `--market on-demand`;
- unless you set `--type`, Crabbox tries the current Apple silicon families in
  order (`mac2.metal`, `mac2-m2.metal`, `mac2-m2pro.metal`, `mac-m4.metal`,
  `mac-m4pro.metal`, `mac-m4max.metal`, `mac2-m1ultra.metal`, `mac-m3ultra.metal`)
  and finally `mac1.metal`;
- to pin the lease to a specific host, set `CRABBOX_HOST_ID` or `hostId` in
  config. Brokered pinning requires admin authentication unless the host belongs
  to the same owner and organization's released lease; other users rely on
  automatic discovery. `CRABBOX_AWS_MAC_HOST_ID` and `aws.macHostId`
  remain accepted aliases.

`crabbox warmup` does not allocate a Dedicated Host implicitly. Trusted
operators manage hosts explicitly:

```sh
crabbox admin hosts list      --provider aws --target macos --region eu-west-1
crabbox admin hosts offerings --provider aws --target macos --region eu-west-1 --type mac2.metal
crabbox admin hosts allocate  --provider aws --target macos --region eu-west-1 --type mac2.metal --dry-run
crabbox admin hosts allocate  --provider aws --target macos --region eu-west-1 --type mac2.metal --force
crabbox admin hosts release h-0123456789abcdef0 --provider aws --target macos --region eu-west-1 --force
```

Promoted AWS images are scoped by target, architecture, and region. Use
`crabbox image promote <ami-id> --target macos --region <aws-region>` to promote
a macOS AMI that was not created through `crabbox image create`.

## Static Mac

A static Mac is an existing machine; Crabbox does not provision or manage it.

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
crabbox vnc --provider ssh --target macos \
  --static-host mac-studio.tailnet-name.ts.net --host-managed --open
```

The Mac must already provide SSH, `git`, `rsync`, `tar`, and Screen Sharing (or
another VNC-compatible service on `127.0.0.1:5900`). Credentials stay
host-managed — Crabbox does not set or read a password — so `crabbox vnc` prints
`credentials: host-managed` and you log in with that Mac's own account or Screen
Sharing password.

`--open` requires `--host-managed`, because opening the client lands you on that
host's own OS login prompt rather than a Crabbox-created cloud desktop; the flag
is your acknowledgement of that. Static Macs cannot be screenshotted with
`crabbox screenshot --target macos` for the same reason — they are existing host
machines, not Crabbox desktops.

Static Macs work well over Tailscale: put the MagicDNS name or `100.x` address in
`static.host` and keep Screen Sharing limited to trusted networks.

## Troubleshooting

**No host capacity (managed AWS).** Use `--market on-demand` and confirm an EC2
Mac Dedicated Host is allocated in the region. Set `CRABBOX_HOST_ID` / `hostId`
only to pin a specific host; brokered pinning requires admin authentication
unless the host belongs to the same owner and organization's released lease.
Operators can inspect capacity:

```sh
crabbox admin hosts offerings --provider aws --target macos --region <region>
crabbox admin hosts quota     --provider aws --target macos --region <region>
crabbox admin hosts list      --provider aws --target macos --region <region>
```

**`target does not expose VNC on 127.0.0.1:5900`.** Screen Sharing is not
listening on loopback yet. On a managed lease, wait for bootstrap to finish; on
a static Mac, enable Screen Sharing and confirm it binds `127.0.0.1:5900`.

**VNC prompts for host credentials.** If the output shows `managed: false`, you
opened a static Mac — use that host's own Screen Sharing credentials. Managed
EC2 Mac leases print the generated `ec2-user` password.

Related docs:

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [AWS](aws.md)
- [vnc command](../commands/vnc.md)
- [screenshot command](../commands/screenshot.md)
