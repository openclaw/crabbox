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
