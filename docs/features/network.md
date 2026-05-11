# Network And Reachability

Read when:

- choosing between `--network auto`, `tailscale`, or `public`;
- debugging "Crabbox can SSH but my browser can't reach the desktop";
- changing how Crabbox falls back between the public IP and the tailnet IP;
- adjusting SSH port fallbacks for restrictive operator networks.

A Crabbox lease can be reachable through more than one network plane.
Brokered Linux leases can join a Tailscale tailnet, brokered AWS Windows and
EC2 Mac leases stay public, and static SSH targets can be on either depending
on how the operator configured them. The CLI picks one plane per command and
prints which it picked.

## Modes

```text
--network auto       prefer tailnet when reachable, otherwise fall back to public
--network tailscale  require tailnet reachability; fail otherwise
--network public     ignore tailnet metadata and use the public address
```

`auto` is the default. It optimizes for "do not surprise me": prefer tailnet
when both client and runner are on the tailnet, fall back transparently to
the public path when the client is off-tailnet.

`tailscale` is the strict mode. Use it when you specifically want to verify
tailnet reachability or when the public IP is firewalled to a CI runner that
your local box cannot reach.

`public` is the escape hatch. Use it when the tailnet metadata is stale, when
you are debugging public-network issues, or when the client cannot reach the
tailnet for unrelated reasons.

The mode applies to `crabbox ssh`, `crabbox run`, `crabbox vnc`, and
`crabbox webvnc`. `crabbox status --network auto` also resolves through this
path so the printed address matches what later commands will use.

## How `auto` Picks A Plane

For a lease with tailnet metadata, `auto` mode:

1. reads `tailscale_fqdn`, `tailscale_ipv4`, and `tailscale_hostname` from the
   server labels;
2. probes the first non-empty option over SSH with a 5-second TCP transport
   probe;
3. uses that target if the probe succeeds;
4. falls back to the public IP and prints `network=public` with the reason
   `tailscale_unreachable`.

For a lease with no tailnet metadata, `auto` is just public mode.

Static SSH targets behave the same way when the static host name is a
MagicDNS or `100.x` address. If the operator points `static.host` at a
MagicDNS name, `--network tailscale` works without any other configuration -
the address is already on the tailnet.

## Public Reachability

Brokered AWS Linux, AWS Windows, AWS Mac, Azure Linux, Azure native Windows,
Google Cloud, Hetzner Linux, Proxmox, Daytona, and Islo leases all expose at
least one dialable address.
Crabbox stores the public address on the server record and uses it whenever
the network mode resolves to `public`.

Public addresses are gated by the provider's security group / firewall. AWS
managed leases use the `crabbox-runners` security group with SSH ingress
limited to the configured CIDRs or the request source IP. Hetzner managed
leases use the cloud firewall attached to the project; the broker keeps it
limited to the operator's IPs. Azure managed leases use the configured network
security group and `azure.sshCIDRs`.
Proxmox uses the first non-loopback IPv4 address reported by the QEMU guest
agent, so the address can be private if the selected bridge is private.

If your client IP changes during a long warmup, the existing security group
rule may not include the new IP. Re-running `crabbox status` adds the
current IP back and updates the rule.

## Tailnet Reachability

When a managed Linux lease is created with `--tailscale`, cloud-init:

- installs the Tailscale package;
- joins the tailnet with the configured tags (default `tag:crabbox`);
- writes non-secret metadata to `/var/lib/crabbox/tailscale-*`;
- extends `crabbox-ready` with a bounded check that a `100.x` address has
  been assigned;
- discards the auth key after `tailscale up` so it never persists.

The metadata Crabbox stores on the lease record:

```text
tailscale=true
tailscale_hostname=blue-lobster
tailscale_fqdn=blue-lobster.tail-scale.ts.net
tailscale_ipv4=100.64.0.5
tailscale_state=ok
tailscale_tags=tag:crabbox
tailscale_exit_node=...
tailscale_exit_node_allow_lan_access=true|false
```

Brokered leases get a one-shot auth key minted by the Worker via Tailscale
OAuth (`worker/src/tailscale.ts`). Direct-provider leases use a key from
`CRABBOX_TAILSCALE_AUTH_KEY`. The auth key is never stored on the runner.

When the metadata says the lease is on the tailnet but the client cannot
reach it, the most common reasons are:

- the client is not joined to the tailnet (`tailscale status` on the client);
- ACLs block the tag pair from reaching `100.x`;
- the runner's `tailscaled` process died (rare; readiness probes catch it
  before the lease is handed back).

`crabbox status --id <lease> --network tailscale` is the fastest way to test
tailnet reachability after lease creation.

## SSH Port And Fallback

Crabbox runs SSH on a non-standard port by default to keep noise out of the
provider firewall logs:

```yaml
ssh:
  port: "2222"
  fallbackPorts:
    - "22"
```

`ssh.port` is the primary port the bootstrap binds to. `ssh.fallbackPorts` is
an ordered list of additional ports the CLI will try when the primary port
is unreachable - typically because the operator's egress is restricted, the
sshd has not bound the new port yet, or cloud-init is still mid-flight.

Fallback rules:

- the CLI tries primary first, then each fallback in order;
- the first port that opens a TCP connection wins for that command;
- success is sticky for the run; the next command repeats the probe;
- the CLI prints `ssh-port-fallback=22` when fallback was used.

Set `ssh.fallbackPorts: []` or `CRABBOX_SSH_FALLBACK_PORTS=none` to disable
fallback entirely. Some networks prefer this so a misconfigured `2222` rule
fails loud instead of quietly using `22`.

## Loopback-Bound Capabilities

Lease capabilities (desktop, code) are bound to loopback on purpose so they
do not need provider firewall changes:

```text
VNC          127.0.0.1:5900   reached via SSH tunnel
code-server  127.0.0.1:8080   reached via portal bridge
```

The network mode does not change loopback bindings. `--network` only changes
which interface the SSH tunnel or portal bridge uses to talk to the lease.
Loopback is loopback; it is reachable from the runner regardless.

## Static Hosts

Static SSH targets honor the same modes:

- `--network public` uses `static.host` as configured;
- `--network tailscale` requires `static.host` to be a MagicDNS name or
  `100.x` address, then probes for SSH reachability;
- `--network auto` defers to the resolved address: if `static.host` is on
  the tailnet, that is what `auto` uses; otherwise it is public.

Tailscale-managed bootstrap (`--tailscale`) is rejected for static providers.
Static hosts are operator-owned; Crabbox does not install Tailscale on them.
Set `static.host` to a tailnet address and select `--network tailscale`
explicitly.

## Failure Surface

When a network mode cannot be satisfied, the CLI exits with code 5 and a
message that names the mode and the lease:

```text
network=tailscale requested but lease cbx_... has no tailnet address
network=tailscale requested for static host mac-studio but SSH is not reachable
network=tailscale requested but blue-lobster.tail-scale.ts.net is not reachable over SSH
```

`auto` mode never fails on a tailnet probe; it falls back to public and
records the reason. The `network=public reason=tailscale_unreachable` log
line is the diagnostic signal that the tailnet plane is unhealthy even
though the command kept working.

Related docs:

- [Tailscale](tailscale.md)
- [Runner bootstrap](runner-bootstrap.md)
- [SSH keys](ssh-keys.md)
- [vnc command](../commands/vnc.md)
- [ssh command](../commands/ssh.md)
- [doctor command](../commands/doctor.md)
