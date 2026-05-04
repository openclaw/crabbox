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
CRABBOX_AWS_MAC_HOST_ID=h-... \
  crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id silver-squid --open
crabbox screenshot --id silver-squid --output macos.png
```

EC2 Mac requirements:

- an allocated EC2 Mac Dedicated Host in the selected region;
- `CRABBOX_AWS_MAC_HOST_ID` or `aws.macHostId`;
- On-Demand capacity;
- the default `mac2.metal` instance type unless `--type` is set.

Bootstrap enables Screen Sharing for `ec2-user`, sets a generated per-lease
password, stores it at `/var/db/crabbox/vnc.password`, and keeps access behind
the SSH tunnel. `crabbox vnc` prints:

```text
macos username: ec2-user
macos password: ...
```

AWS EC2 Mac has a provider-level lifecycle constraint: Mac instances run on
allocated Dedicated Hosts with a 24-hour minimum host allocation period.
Crabbox launches onto a host id you provide; it does not allocate, scrub, or
retire Mac hosts for you.

## Static Mac

Static Mac targets are existing machines:

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.tailnet-name.ts.net
  user: steipete
  port: "22"
  workRoot: /Users/steipete/crabbox
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

Missing host id

Set `CRABBOX_AWS_MAC_HOST_ID` or `aws.macHostId`, use `--market on-demand`, and
verify the Dedicated Host is allocated in the selected AWS region.

VNC prompt asks for host credentials

If `managed: false`, you opened a static Mac. Use the Mac's own Screen Sharing
credentials. Managed AWS EC2 Mac leases print the generated `ec2-user`
password.

Related docs:

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [AWS](aws.md)
- [vnc command](../commands/vnc.md)
- [screenshot command](../commands/screenshot.md)
