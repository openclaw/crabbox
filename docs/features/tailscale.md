# Tailscale

Read when:

- adding or debugging tailnet reachability for a lease;
- deciding whether a host is provider-owned or only network-reachable;
- changing SSH, VNC, or coordinator bootstrap behavior over a tailnet.

Tailscale is an optional reachability layer, not a provider. Providers still own
the machines (Hetzner, AWS, Azure, Google Cloud, Proxmox, static SSH hosts, and
the rest). Tailscale only changes which host Crabbox dials when it runs SSH-backed
work against a lease.

## Scope

- Managed Linux leases can join a tailnet at creation time with `--tailscale`.
  Tailscale provisioning is offered by the providers that declare the capability:
  Hetzner, AWS, Azure, and Google Cloud.
- Static (`provider=ssh`) hosts are not joined by Crabbox; point `static.host` at a
  MagicDNS name or `100.x` address and assert reachability with `--network tailscale`.
- `--tailscale` is rejected for non-Linux targets, for Blacksmith Testbox (Blacksmith
  owns connectivity), for static hosts, and for sandbox providers such as Sprites that
  expose SSH through their own proxy.

## Commands

Create a managed Linux lease that joins the configured tailnet:

```sh
crabbox warmup --tailscale
crabbox run --tailscale -- pnpm test
crabbox run --tailscale --desktop --browser -- pnpm test:e2e
```

Choose the connection path when acting on an existing lease (`ssh`, `vnc`,
`screenshot`, `webvnc`, `status`, `inspect`, and reused `run --id` leases):

```sh
crabbox ssh --id swift-crab --network auto
crabbox ssh --id swift-crab --network tailscale
crabbox vnc --id swift-crab --network tailscale --open
crabbox run --id swift-crab --network public -- pnpm test
```

### Network modes

`--network` (config key `network`, default `auto`) selects how the SSH endpoint is
resolved:

- `auto`: prefer the tailnet host when the lease carries Tailscale metadata and SSH
  is reachable there; otherwise fall back to the provider/public host.
- `tailscale`: require a tailnet host and fail clearly when this client cannot reach
  it over SSH.
- `public`: force the provider/public host (useful for debugging).

When `auto` falls back to the public host, Crabbox prints `network fallback
tailscale_unreachable` and reports the selected path in `ready`/`status` output
(`ready ssh=… network=public …`) instead of silently switching.

## Config

```yaml
network: auto
tailscale:
  enabled: true
  tags:
    - tag:crabbox
  hostnameTemplate: crabbox-{slug}
  authKeyEnv: CRABBOX_TAILSCALE_AUTH_KEY
  exitNode: build-host.example.ts.net
  exitNodeAllowLanAccess: true
```

`tailscale.enabled` (or `--tailscale`) requests a tailnet join for newly created
managed Linux leases. `tailscale.network` is an alias that sets the top-level
`network` mode used for target resolution on SSH-backed commands. Hostname templates
support `{id}`, `{slug}`, and `{provider}`; the rendered value is sanitized to a DNS
label. When `enabled` is set, `tags` must contain at least one valid `tag:` value and
`hostnameTemplate` must be non-empty.

Environment overrides:

```text
CRABBOX_TAILSCALE=1
CRABBOX_NETWORK=auto|tailscale|public
CRABBOX_TAILSCALE_TAGS=tag:crabbox,tag:ci
CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE=crabbox-{slug}
CRABBOX_TAILSCALE_AUTH_KEY_ENV=CRABBOX_TAILSCALE_AUTH_KEY
CRABBOX_TAILSCALE_AUTH_KEY=<direct-provider only>
CRABBOX_TAILSCALE_EXIT_NODE=build-host.example.ts.net
CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS=1
```

Flag equivalents: `--tailscale`, `--network`, `--tailscale-tags`,
`--tailscale-hostname-template`, `--tailscale-auth-key-env`, `--tailscale-exit-node`,
and `--tailscale-exit-node-allow-lan-access`.

In direct-provider mode the auth key is read from the variable named by
`tailscale.authKeyEnv` (default `CRABBOX_TAILSCALE_AUTH_KEY`). Managed VM
providers normally use a one-off key. Islo requires a reusable, ephemeral key
because its snapshot-safe memory-only node identity must re-enroll after daemon
loss. Brokered mode does not need a local key; the coordinator mints one per lease
(see below).

## Exit nodes

Exit-node egress is opt-in per lease. The lease routes its outbound internet through
an approved tailnet exit node after it joins Tailscale:

```sh
crabbox warmup --tailscale --tailscale-exit-node build-host.example.ts.net --tailscale-exit-node-allow-lan-access
crabbox run --tailscale --tailscale-exit-node 100.100.100.100 -- curl -4 https://ifconfig.me
```

`exitNodeAllowLanAccess` maps to Tailscale's LAN-access flag and requires `exitNode`.
The exit node must already advertise exit-node capability and be approved in the
Tailscale admin console, and the tailnet policy must grant the lease's tags (for
example `tag:crabbox`) access to `autogroup:internet` through exit nodes.

When an exit node is selected, `network: auto` prefers the tailnet host for bootstrap
because the provider/public SSH path can become asymmetric once the lease routes
outbound traffic through the exit node. Before sync or the remote command, Crabbox
verifies on the box that an exit node is selected in `tailscale prefs` and that the
node can reach the public internet. If that check fails, the run stops and reports the
exit-node egress failure (`tailscale exit node … joined but remote internet egress
failed`), which usually means the exit node is not approved, the policy does not grant
`autogroup:internet` to the lease tag, or the exit-node machine is not forwarding.

## Brokered mode

When a lease is created through the coordinator, it mints a fresh auth key per
lease using Tailscale OAuth. Secrets live in coordinator configuration:

```text
CRABBOX_TAILSCALE_CLIENT_ID
CRABBOX_TAILSCALE_CLIENT_SECRET
CRABBOX_TAILSCALE_TAILNET    optional, defaults to "-"
CRABBOX_TAILSCALE_TAGS       default/allowed comma-separated tags (default tag:crabbox)
CRABBOX_TAILSCALE_ENABLED    set 0 to force-disable, 1 to force-enable
CRABBOX_TAILSCALE_INSTALL_MODE package or pinned (default package)
CRABBOX_TAILSCALE_VERSION       pinned static build version
CRABBOX_TAILSCALE_SHA256_AMD64  pinned amd64 archive checksum
CRABBOX_TAILSCALE_SHA256_ARM64  pinned arm64 archive checksum
```

The OAuth client's tags and each requested lease tag must also satisfy Tailscale's
ownership rules:

- **Single tag:** give the OAuth client `tag:crabbox` and set
  `CRABBOX_TAILSCALE_TAGS=tag:crabbox`. The tag sets match exactly.
- **Multi-tag exact match:** an OAuth client with `tag:ci,tag:staging` can mint a key
  requesting both tags together without extra ownership rules.
- **Multi-tag subsets:** requesting only `tag:ci` from that two-tag client requires
  `tag:ci` to own itself in `tagOwners`; do the same for every subset tag.
- **Deployment-owner pattern:** preferably give the OAuth client one narrow owner tag,
  such as `tag:crabbox-deployer`, and make that tag an owner of each workload tag that
  Crabbox may request.

Example deployment-owner policy:

```json
{
  "tagOwners": {
    "tag:crabbox-deployer": ["autogroup:admin"],
    "tag:ci": ["tag:crabbox-deployer"],
    "tag:staging": ["tag:crabbox-deployer"]
  }
}
```

Then assign only `tag:crabbox-deployer` to the OAuth client and set
`CRABBOX_TAILSCALE_TAGS=tag:ci,tag:staging`. This keeps the client least-privilege
without requiring it to carry every workload tag. Do not broaden the OAuth client to
unrelated tags just to resolve an ownership error.

Flow:

1. The CLI sends the requested Tailscale settings (enabled, tags, hostname, optional
   exit node) in `CreateLease`.
2. The coordinator validates the requested tags against `CRABBOX_TAILSCALE_TAGS` and
   rejects any tag outside that set.
3. The coordinator exchanges the OAuth client for a token and mints a one-off auth key
   that is non-reusable, ephemeral, pre-approved, tagged, and expires in 10 minutes.
4. The key is injected only into the runner's cloud-init user-data.
5. The runner installs Tailscale, runs `tailscale up` with the requested hostname and
   tags, and writes non-secret metadata under `/var/lib/crabbox` (`tailscale-ipv4`,
   `-hostname`, `-fqdn`, `-version`, `-device-id`, and exit-node markers).
6. After SSH readiness, the CLI reads that metadata and posts it back to the
   coordinator so the lease record carries the tailnet address.

The auth key is never stored in lease records, provider labels, run logs, or local
config. The short-lived key can still appear in user-data at the provider, so the
Worker only mints one-off ephemeral keys — never long-lived reusable keys.

The default installer mode runs Tailscale's package install script. Set
`CRABBOX_TAILSCALE_INSTALL_MODE=pinned` to download a static Tailscale archive,
verify the configured SHA-256 checksum, install the `tailscale` and `tailscaled`
binaries, and record the client version. The built-in pinned defaults track the
Islo Tailscale build so both bootstrap paths use the same binary version.

On release, `crabbox stop` attempts a best-effort remote `tailscale logout` before
provider cleanup when SSH is still reachable. Coordinator cleanup also attempts a
best-effort Tailscale device delete when the lease record has a `deviceID`; missing
device ids or Tailscale API failures are recorded in lease metadata without blocking
provider deletion.

If Tailscale rejects a requested subset or unowned tag, the coordinator returns
`invalid_tailscale_tags` with exact-match/`tagOwners` guidance and preserves the raw
Tailscale HTTP status and response body for diagnosis.

Preflight the coordinator without leasing a machine:

```sh
CRABBOX_TAILSCALE_ENABLED=0 scripts/live-tailscale-smoke.sh --json
CRABBOX_LIVE=1 CRABBOX_COORDINATOR=https://broker.example.com CRABBOX_ADMIN_TOKEN=replace-me scripts/live-tailscale-smoke.sh --json
```

## SSH and VNC

Crabbox continues to use OpenSSH with per-lease keys; Tailscale SSH is not used.
Tailscale only changes the SSH endpoint from the public/provider host to the tailnet
host.

Managed VNC remains loopback-bound and tunneled over SSH:

```text
local localhost:5901 -> SSH -> remote 127.0.0.1:5900
```

Crabbox does not bind managed VNC to `100.x` addresses and does not use Tailscale
Serve, Funnel, or noVNC for managed leases.

## Static hosts

Static hosts are operator-managed. Point `static.host` at a MagicDNS name or `100.x`
address and use `--network tailscale` to assert reachability:

```yaml
provider: ssh
target: macos
static:
  host: build-host.example.ts.net
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

For static hosts, `--network tailscale` only asserts reachability and probes SSH;
Crabbox does not install or join Tailscale on the host.

## Tailscale references

- [Auth keys](https://tailscale.com/kb/1085/auth-keys)
- [Ephemeral nodes](https://tailscale.com/docs/features/ephemeral-nodes)
- [OAuth clients](https://tailscale.com/kb/1215/oauth-clients)
- [ACL tags](https://tailscale.com/kb/1068/acl-tags)
- [Secure auth-key CLI usage](https://tailscale.com/kb/1595/secure-auth-key-cli)
- [tailscale up flags](https://tailscale.com/kb/1241/tailscale-up)

## Related docs

- [Network modes](network.md)
- [Providers](providers.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Security](../security.md)
- [Troubleshooting](../troubleshooting.md#tailscale-path-fails)
