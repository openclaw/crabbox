# Network And Reachability

Read this when you are:

- choosing between `--network auto`, `--network tailscale`, or `--network public`;
- debugging "Crabbox can SSH but my browser cannot reach the desktop";
- deciding whether a lease should join a tailnet, and how the CLI falls back
  between the public address and the tailnet address;
- tuning SSH port fallbacks for restrictive operator networks.

A Crabbox lease can be reachable on more than one network plane. A managed
Linux lease created with `--tailscale` joins a Tailscale tailnet and also keeps
its public address; AWS/Azure Windows and EC2 Mac leases stay public-only;
static SSH targets are reachable however the operator configured the host. Each
command picks one plane and prints which one it used.

## Network modes

The `--network` flag (or `network:` in config) selects the plane:

```text
auto       prefer the tailnet when it is reachable, otherwise fall back to public
tailscale  require tailnet reachability; fail if the tailnet cannot be reached
public     ignore tailnet metadata and use the public address
```

`auto` is the default. It optimizes for "do not surprise me": use the tailnet
when both client and runner are on it, and fall back transparently to the
public path otherwise.

`tailscale` is the strict mode. Use it to verify tailnet reachability, or when
the public address is firewalled to a CI runner your local machine cannot
reach.

`public` is the escape hatch. Use it when tailnet metadata is stale, when you
are debugging public-network behavior, or when the client cannot reach the
tailnet for unrelated reasons.

The mode applies to every lease-acting command that connects over SSH or
bridges a lease, including `crabbox ssh`, `crabbox run`, `crabbox status`,
`crabbox inspect`, `crabbox vnc`, `crabbox webvnc`, `crabbox code`,
`crabbox screenshot`, the `crabbox desktop` subcommands, and `crabbox egress`.
Because `crabbox status` resolves through the same path, the address it prints
matches what later commands will use.

Only `auto`, `tailscale`, and `public` are valid; any other value exits with
code 2 and the message `network must be auto, tailscale, or public`.

## How `auto` picks a plane

For a lease that carries tailnet metadata, `auto`:

1. reads `tailscale_fqdn`, `tailscale_ipv4`, and `tailscale_hostname` from the
   server labels, in that order, and takes the first non-empty value;
2. probes that target over SSH with a 5-second TCP transport probe;
3. uses the tailnet target if the probe succeeds;
4. otherwise falls back to the public address and prints
   `network fallback tailscale_unreachable` to stderr.

For a lease with no tailnet metadata, `auto` is just public mode.

Static SSH targets behave the same way when the static host name is a MagicDNS
name or a `100.x` address: `auto` defers to whatever `static.host` resolves to.

The planned local Incus testbed is a special case worth keeping explicit:
Crabbox does not reach the Incus-managed guest until the operator exposes that
guest back out of the Linux host in a Mac-reachable way. On this Apple Silicon
testbed, that usually means direct bridge reachability or an Incus-managed proxy
or network-forward rule from the Linux host to guest port `22`. The opt-in
Incus live smoke (`CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=incus ... scripts/live-smoke.sh`)
assumes that route already exists; the paired live doctor smoke
(`CRABBOX_LIVE_DOCTOR_PROVIDERS=incus scripts/live-doctor-smoke.sh`) proves only
daemon/control-plane readiness, not Mac-to-guest SSH reachability.

## Public reachability

Brokered AWS Linux/Windows/Mac, Azure Linux and native Windows, Google Cloud,
Hetzner Linux, Proxmox, and the various managed-sandbox providers all expose at
least one dialable address. Crabbox stores the public address on the server
record and uses it whenever the resolved mode is `public`.

Public addresses are gated by the provider's firewall or security group:

- AWS managed leases use a dedicated security group with SSH ingress limited to
  configured CIDRs or the request source IP for active leases.
- Hetzner managed leases use the project cloud firewall, kept limited to the
  operator's IPs.
- Azure managed leases use the configured network security group plus
  `azure.sshCIDRs`.
- Proxmox uses the first non-loopback IPv4 address reported by the QEMU guest
  agent, so the address can be private if the selected bridge is private.

If your client IP changes during a long warmup, the existing firewall rule may
not include the new IP. Re-running `crabbox status` refreshes provider SSH
access for the active lease when source CIDRs are in play, restoring
reachability.

## Tailnet reachability

When a managed Linux lease is created with `--tailscale`, the runner bootstrap:

- installs Tailscale;
- joins the tailnet with the configured tags (default `tag:crabbox`);
- writes non-secret metadata under `/var/lib/crabbox/tailscale-*`;
- extends the `crabbox-ready` contract with a bounded check that a `100.x`
  address was assigned;
- discards the auth key after `tailscale up` so it never persists on the
  runner.

`--tailscale` only provisions **managed Linux** leases. The CLI rejects it for
non-Linux targets, for static/BYO providers, and for Blacksmith (which owns its
own machine connectivity), with an exit-code-2 message pointing you at
`--network tailscale` plus a tailnet `static.host` instead.

The metadata Crabbox stores on the lease record:

```text
tailscale=true
tailscale_hostname=blue-lobster
tailscale_fqdn=blue-lobster.tail-scale.ts.net
tailscale_ipv4=100.64.0.5
tailscale_state=ready
tailscale_tags=tag:crabbox
tailscale_exit_node=...
tailscale_exit_node_allow_lan_access=true|false
```

Brokered leases receive a one-shot auth key minted by the Worker via Tailscale
OAuth. Direct-provider leases use a key read from the environment variable named
by `--tailscale-auth-key-env`. Bootstrap pipes the key to `tailscale up` through
stdin, so it is neither placed in process arguments nor stored on the runner.

When the metadata says the lease is on the tailnet but the client cannot reach
it, the usual causes are:

- the client is not joined to the tailnet (check `tailscale status` locally);
- ACLs block the tag pair from reaching the `100.x` address;
- the runner's `tailscaled` died (rare; readiness probes normally catch this
  before the lease is handed back).

`crabbox status --id <lease> --network tailscale` is the fastest way to test
tailnet reachability after lease creation.

### Exit nodes

`--tailscale-exit-node <name-or-100.x>` routes the runner's outbound traffic
through a chosen exit node, and `--tailscale-exit-node-allow-lan-access` keeps
LAN reachable while doing so (the latter requires an exit node to be set, or the
config fails validation). When an exit node is configured, `auto` mode prefers
the tailnet path during bootstrap so traffic uses the node, and Crabbox runs a
remote egress check: it confirms the exit node is selected in `tailscale debug
prefs` and that the runner can fetch its public IP. If the node is joined but
internet egress fails, the command exits with code 5 and a message naming the
exit node.

## SSH port and fallback

Crabbox runs SSH on a non-standard port by default to keep noise out of provider
firewall logs:

```yaml
ssh:
  port: "2222"
  fallbackPorts:
    - "22"
```

`ssh.port` is the primary port the bootstrap binds. `ssh.fallbackPorts` is an
ordered list of additional ports the CLI tries when the primary is unreachable,
typically because the operator's egress is restricted, sshd has not bound the
new port yet, or cloud-init is still mid-flight.

Fallback behavior:

- the CLI builds an ordered, de-duplicated candidate list of `ssh.port`
  followed by each fallback port;
- it tries the primary first, then each fallback in order;
- the first port that connects wins for that operation;
- the probe is repeated per operation, so a later command re-evaluates the
  candidates from scratch.

Set `ssh.fallbackPorts: []` or `CRABBOX_SSH_FALLBACK_PORTS=none` to disable
fallback entirely. Some networks prefer this so a misconfigured `2222` rule
fails loudly instead of quietly falling through to `22`. `CRABBOX_SSH_PORT`
overrides the primary port; `CRABBOX_SSH_FALLBACK_PORTS` (comma-separated, or
`none`) overrides the fallback list.

## Loopback-bound capabilities

Lease capabilities such as the desktop VNC server and code-server bind to
loopback on purpose, so they need no provider firewall changes:

```text
VNC          127.0.0.1:5900   reached via SSH tunnel
code-server  127.0.0.1:8080   reached via portal bridge
```

The network mode does not change loopback bindings. `--network` only selects
which interface the SSH tunnel or portal bridge uses to reach the lease;
loopback is reachable from the runner regardless of which plane you picked.

## Static hosts

Static SSH targets (`provider=ssh`) honor the same modes:

- `--network public` dials `static.host` as configured;
- `--network tailscale` requires `static.host` to be a MagicDNS name or `100.x`
  address and probes it for SSH reachability (6-second transport probe), failing
  with exit code 5 if it cannot connect;
- `--network auto` defers to the resolved address: tailnet if `static.host` is
  on the tailnet, otherwise public.

Tailscale-managed bootstrap (`--tailscale`) is rejected for static providers;
the host is operator-owned and Crabbox does not install Tailscale on it. To use
the tailnet, point `static.host` at a tailnet address and pass
`--network tailscale`.

## Failure surface

When a strict network mode cannot be satisfied, the CLI exits with code 5 and a
message naming the mode and the lease or host:

```text
network=tailscale requested but lease cbx_... has no tailnet address
network=tailscale requested for static host mac-studio but SSH is not reachable; is this client joined to the tailnet?
network=tailscale requested but blue-lobster.tail-scale.ts.net is not reachable over SSH; is this client joined to the tailnet?
```

`auto` mode never fails on a tailnet probe; it falls back to public and writes
`network fallback tailscale_unreachable` to stderr. That line is the diagnostic
signal that the tailnet plane is unhealthy even though the command kept working;
the corresponding `network=public` token in the `ready`/`status` output confirms
which plane was actually used.

## Related docs

- [Tailscale](tailscale.md)
- [Runner bootstrap](runner-bootstrap.md)
- [SSH keys](ssh-keys.md)
- [vnc command](../commands/vnc.md)
- [ssh command](../commands/ssh.md)
- [doctor command](../commands/doctor.md)
