# Tailscale

Read when:

- adding or debugging tailnet reachability;
- deciding whether a host is provider-owned or only network-reachable;
- changing SSH, VNC, or coordinator bootstrap behavior.

Tailscale is an optional Crabbox reachability layer. It is not a provider.
Providers still own machines: Hetzner, AWS, Azure, static SSH hosts, and Blacksmith
Testbox. Tailscale only changes which host Crabbox dials for SSH-backed work.

V1 support:

- managed Linux leases can join a tailnet with `--tailscale`;
- static hosts can use MagicDNS names or 100.x addresses in `static.host`;
- managed Windows and EC2 Mac Tailscale provisioning is not enabled yet;
- Blacksmith Testbox connectivity remains Blacksmith-owned.

## Commands

Create a managed Linux lease that joins the configured tailnet:

```sh
crabbox warmup --tailscale
crabbox run --tailscale -- pnpm test
crabbox run --tailscale --desktop --browser -- pnpm test:e2e
```

Choose the connection path for SSH, VNC, screenshots, WebVNC, status, inspect,
and reused `run --id` leases:

```sh
crabbox ssh --id blue-lobster --network auto
crabbox ssh --id blue-lobster --network tailscale
crabbox vnc --id blue-lobster --network tailscale --open
crabbox run --id blue-lobster --network public -- pnpm test
```

Network modes:

- `auto`: prefer Tailscale when lease metadata exists and SSH is reachable,
  otherwise use the provider/public host;
- `tailscale`: require a tailnet host and fail clearly when this client cannot
  reach it;
- `public`: force the provider/public host for debugging.

When `auto` falls back to the public host, Crabbox prints the selected network
in ready/status output instead of silently hiding the path.

## Config

```yaml
tailscale:
  enabled: true
  network: auto
  tags:
    - tag:crabbox
  hostnameTemplate: crabbox-{slug}
  authKeyEnv: CRABBOX_TAILSCALE_AUTH_KEY
  exitNode: mac-studio.example.ts.net
  exitNodeAllowLanAccess: true
```

Environment overrides:

```text
CRABBOX_TAILSCALE=1
CRABBOX_NETWORK=auto|tailscale|public
CRABBOX_TAILSCALE_TAGS=tag:crabbox,tag:ci
CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE=crabbox-{slug}
CRABBOX_TAILSCALE_AUTH_KEY=<direct-provider only>
CRABBOX_TAILSCALE_EXIT_NODE=mac-studio.example.ts.net
CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS=1
```

`tailscale.enabled` and `--tailscale` request tailnet join for newly created
managed Linux leases. `tailscale.network` and `--network` choose target
resolution for SSH-backed commands. Hostname templates support `{id}`, `{slug}`,
and `{provider}`.

Direct-provider mode reads the one-off auth key from `tailscale.authKeyEnv`.
Brokered mode does not require a local Tailscale key.

`tailscale.exitNode` asks the lease to route outbound internet through a
tailnet exit node after it joins Tailscale. Use a MagicDNS name or 100.x address
for an approved exit node. `tailscale.exitNodeAllowLanAccess` maps to
Tailscale's LAN-access flag and requires `tailscale.exitNode`. In `network:
auto`, exit-node leases bootstrap over the tailnet host once it appears because
the public/provider SSH path can become asymmetric after the lease selects the
exit node.

## Brokered Mode

The Worker mints a fresh auth key per requested lease using Tailscale OAuth.
Secrets live in Worker configuration:

```text
CRABBOX_TAILSCALE_CLIENT_ID
CRABBOX_TAILSCALE_CLIENT_SECRET
CRABBOX_TAILSCALE_TAILNET optional, defaults to -
CRABBOX_TAILSCALE_TAGS default/allowed comma-separated tags
CRABBOX_TAILSCALE_ENABLED set 0 to disable
```

Flow:

1. The CLI sends `tailscale`, `tailscaleTags`, `tailscaleHostname`, and optional
   exit-node settings in `CreateLease`.
2. The Worker validates requested tags against `CRABBOX_TAILSCALE_TAGS`.
3. The Worker uses OAuth to mint a one-off, ephemeral, pre-approved, tagged auth
   key.
4. The key is injected only into cloud-init user-data.
5. The runner installs Tailscale, runs `tailscale up`, and writes non-secret
   metadata under `/var/lib/crabbox`.
6. After SSH readiness, the CLI reads that metadata and posts it back to the
   coordinator.

The auth key is never stored in lease records, provider labels, run logs, or
local config. User-data can still contain the short-lived key at the provider,
so use one-off ephemeral keys and avoid long-lived reusable keys.

## Exit Nodes

Exit-node egress is opt-in per lease:

```sh
crabbox warmup --tailscale --tailscale-exit-node mac-studio.example.ts.net --tailscale-exit-node-allow-lan-access
crabbox run --tailscale --tailscale-exit-node 100.100.100.100 -- curl -4 https://ifconfig.me
```

The exit node must already advertise exit-node capability and be approved in
Tailscale admin. ACLs/grants must allow the lease's tags, such as
`tag:crabbox`, to access `autogroup:internet` through exit nodes.

After the lease is reachable, Crabbox verifies that the selected exit node can
reach the public internet. If that check fails, the run stops before sync or the
remote command and reports the exit-node egress failure. This usually means the
exit node is not approved for internet routing, the tailnet policy does not
grant `autogroup:internet` to the lease tag, or the exit-node machine itself is
not forwarding traffic.

## VNC And SSH

Crabbox continues to use OpenSSH and per-lease SSH keys. Tailscale SSH is not
enabled in v1.

Managed VNC remains loopback-bound:

```text
local localhost:5901 -> SSH -> remote 127.0.0.1:5900
```

Tailscale only changes the SSH endpoint from the public/provider host to the
tailnet host. Crabbox does not bind managed VNC to 100.x addresses, and does
not use Tailscale Serve, Funnel, or noVNC for managed leases.

## Static Hosts

Static hosts are operator-managed. Point `static.host` at a MagicDNS name or a
100.x address:

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.example.ts.net
  user: steipete
  port: "22"
  workRoot: /Users/steipete/crabbox
```

For static hosts, `--network tailscale` is a reachability assertion. Crabbox
does not install or join Tailscale on the host.

## Tailscale References

- [Auth keys](https://tailscale.com/kb/1085/auth-keys)
- [Ephemeral nodes](https://tailscale.com/docs/features/ephemeral-nodes)
- [OAuth clients](https://tailscale.com/kb/1215/oauth-clients)
- [ACL tags](https://tailscale.com/kb/1068/acl-tags)
- [Secure auth-key CLI usage](https://tailscale.com/kb/1595/secure-auth-key-cli)
- [tailscale up flags](https://tailscale.com/kb/1241/tailscale-up)

Related docs:

- [Providers](providers.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Security](../security.md)
- [Troubleshooting](../troubleshooting.md#tailscale-path-fails)
