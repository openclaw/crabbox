# inspect

`crabbox inspect` prints the full record for a single lease: state, provider,
server identity, the resolved SSH command, idle/expiry timing, Tailscale
metadata, and the provider labels attached to the box. Reach for it when
something looks wrong and you want every detail in one place.

```sh
crabbox inspect --id blue-lobster
crabbox inspect --id blue-lobster --network tailscale
crabbox inspect --id blue-lobster --json
crabbox inspect --provider namespace-devbox --id blue-lobster
crabbox inspect --provider ssh --target windows --windows-mode wsl2 --static-host win-dev.local
```

You can also pass the lease id or slug as a positional argument instead of
`--id`:

```sh
crabbox inspect blue-lobster
```

When `--provider` is omitted, an exact local claim or an unambiguous claimed
slug selects its recorded provider before provider initialization. An explicit
`--provider` remains authoritative; ambiguous claimed slugs require a canonical
lease ID or `--provider`, while missing claims keep the configured-provider
fallback.

## Output

Human output prints one `key=value` line per field, followed by any Tailscale
metadata (when the lease has Tailscale enabled) and one `label.<name>=<value>`
line per provider label.

```text
id=cbx_abcdef123456
slug=blue-lobster
provider=aws
target=linux
windows_mode=-
state=active
server=i-0abcdef0123456789
host=203.0.113.10
network=public
ssh=~/.config/crabbox/testboxes/cbx_abcdef123456/id_ed25519 -p 2222 crabbox@203.0.113.10
ssh_fallback_ports=22
idle_for=12m4s
idle_timeout=30m0s
last_touched=2026-05-07T07:55:12Z
expires=2026-05-07T08:25:12Z
tailscale.state=ok
tailscale.hostname=blue-lobster
tailscale.fqdn=blue-lobster.tail-scale.ts.net
tailscale.ipv4=100.64.0.5
tailscale.tags=tag:crabbox
label.target=linux
label.state=active
```

The `ssh=` line shows the connection for the selected `--network` mode (the
key path, port, user, and host). Empty fields render as `-`.

`--json` prints the structured status record (the same shape returned by
[`status`](status.md)), including non-secret Tailscale metadata and the full
label map. Secrets such as broker tokens, provider keys, and VNC passwords are
never included in either output mode.

For brokered leases, labels describe the current coordinator lease record,
including allocation identity, target, capacity, timing, and capability facts.
They are not a fresh read of provider tags. Only the documented
`providerMetadata` fields below are refreshed from the provider during
coordinator inspection.

Brokered JSON records also expose the coordinator's provider-cleanup state:

- `cleanupStatus`: for released leases, `pending`, `failed`, `complete`, or
  `retained`. This is computed from the current lifecycle record, not persisted
  as a separate state or used as provider deletion authority.
- `cleanupStartedAt`: cleanup has started but is not yet terminal.
- `cleanupError`: cleanup remains unconfirmed; this can include legacy diagnostics
  for pending creation as well as observed failures.
- `cleanupRetryAt`: the coordinator scheduled another cleanup attempt.
- `releaseDeletesServer`: whether release is intended to delete the provider
  resource.

Cleanup is terminal under Crabbox's coordinator predicate only when `state` is
`released`, `cleanupStartedAt`, `cleanupError`, and `cleanupRetryAt` are all
absent, and `releaseDeletesServer` is either omitted or `true`. An explicit
`releaseDeletesServer: false` means the provider resource was intentionally
retained and must not be treated as deletion-confirmed. Omitted and `false` are
therefore distinct states.

`pending` includes an allocation response or cleanup attempt still being
observed. An explicit stop keeps observing that state within its existing
five-minute bound instead of treating it as a provider failure. A real cleanup
failure or uncertain abandoned allocation remains `failed`; local claims and
SSH files are retained. Older coordinators omit `cleanupStatus`, and clients
continue using the existing conservative metadata checks. The original
diagnostics remain available so older clients also fail closed.

These fields report the coordinator's lifecycle observation. They are not an
independent provider inventory check. `complete` does not override remaining
cleanup metadata or an explicit retained-resource flag.

For coordinator leases whose provider can inject an SSH host key before first
boot, JSON also includes `sshHostKey`. Its value is exactly the public host-key
algorithm and base64 payload, without a hostname or comment:

```json
{
  "sshHostKey": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA..."
}
```

The key is the authoritative public half generated for provisioning, not a key
learned later through `known_hosts` or `ssh-keyscan`. The field is omitted when
the provider cannot inject a host key before boot.

When the provider assigns a resource an immutable identifier separate from its
name, JSON also includes `providerResourceId`:

```json
{
  "serverId": "my-app-7f3a91",
  "providerResourceId": "0195f3d2-5c1a-7c39-9c1e-6f0f2b7a41cd"
}
```

`serverId` is the resource's name, which identifies a slot in the provider's
namespace rather than one specific resource: it stops resolving as soon as the
resource is deleted, and it can be occupied by a record other than the one you
created. `providerResourceId` pins the exact resource, so automation that must
act on one specific resource should key off it. The field is omitted for
providers whose only identity is the name.

When a lease's provider cannot be reached by its recorded resource id and the
lookup falls back to the name, `labels.islo_resource_id_mismatch` is set to
`true` and `labels.islo_claimed_resource_id` reports the id the lease claims,
so `providerResourceId` is never silently attributed to a different resource.

AWS leases also include authoritative provider metadata sourced from EC2
`DescribeInstances`. Brokered inspection requests a fresh coordinator-side
lookup; direct inspection uses the local AWS client:

```json
{
  "providerMetadata": {
    "instanceProfileAttached": false
  }
}
```

Consumers can use this boolean to fail closed when a workload must not receive
an IAM instance profile. The field is omitted when the backend cannot attest
the association state.

## Flags

```text
--id <lease-id-or-slug>      lease to inspect (required); also accepted as a positional argument
--provider <name>            override the configured provider (e.g. aws, hetzner, ssh, namespace-devbox)
--target linux|macos|windows target OS
--windows-mode normal|wsl2   Windows execution mode
--static-host <host>         static SSH host (provider=ssh)
--static-user <user>         static SSH user override
--static-port <port>         static SSH port override
--static-work-root <path>    static target work root
--network auto|tailscale|public  which address the resolved SSH line prints
--json                       print the structured JSON record
```

## inspect vs status vs list

- `inspect` is the long-form record for one lease, including provider labels
  and the resolved SSH command.
- [`status`](status.md) is the shorter "is this lease healthy right now"
  check, with optional `--wait` and bounded telemetry.
- [`list`](list.md) is the table view across many leases, scoped by owner/org
  or fleet-wide for admins.

Use `inspect` when something is unexpected and you want all the detail at once.
Use `status` when an automation needs a quick liveness check. Use `list` when
you are hunting for a specific lease across the pool.

Related docs:

- [status](status.md)
- [list](list.md)
- [ssh](ssh.md)
- [Identifiers](../features/identifiers.md)
- [Network and reachability](../features/network.md)
