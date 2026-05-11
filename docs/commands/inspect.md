# inspect

`crabbox inspect` prints detailed lease and provider metadata. Use it for
debugging coordinator state, provider labels, expiry, SSH target details,
and Tailscale metadata.

```sh
crabbox inspect --id blue-lobster
crabbox inspect --id blue-lobster --network tailscale
crabbox inspect --id blue-lobster --json
crabbox inspect --provider namespace-devbox --id blue-lobster
crabbox inspect --provider semaphore --id blue-lobster
crabbox inspect --provider sprites --id blue-lobster
crabbox inspect --provider ssh --target windows --windows-mode wsl2 --static-host win-dev.local
```

## Output

Human output prints lease state, provider, server type, public IP, work
root, owner, org, idle timeout, TTL, expiry, last touched, the resolved
SSH command for the selected network mode, and any Tailscale metadata the
lease carries.

```text
lease=cbx_abcdef123456 slug=blue-lobster
state=active provider=aws server=i-0abcdef0123456789 type=c7a.48xlarge
host=203.0.113.10 user=crabbox port=2222 work_root=/work/crabbox
owner=alex@example.com org=openclaw
idle_timeout=30m0s ttl=90m0s
created_at=2026-05-07T07:42:18Z last_touched=2026-05-07T07:55:12Z expires_at=2026-05-07T08:25:12Z
ssh: ssh -i ~/.config/crabbox/testboxes/cbx_abcdef123456/id_ed25519 -p 2222 crabbox@203.0.113.10
tailscale: state=ok ipv4=100.64.0.5 fqdn=blue-lobster.tail-scale.ts.net tags=tag:crabbox
```

JSON output returns the structured record, including non-secret Tailscale
metadata. Secrets (broker tokens, provider keys, VNC passwords) are never
included.

## Flags

```text
--id <lease-id-or-slug>      lease to inspect; required for managed providers
--provider hetzner|aws|azure|gcp|proxmox|ssh|namespace-devbox|semaphore|sprites|daytona   override the configured provider
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>         static SSH host for provider=ssh
--static-user <user>         static SSH user override
--static-port <port>         static SSH port override
--static-work-root <path>    static target work root
--network auto|tailscale|public  select which address inspect prints
--json                       print JSON
```

## Inspect vs Status vs List

- `inspect` is the long-form record for one lease, including provider
  metadata, label state, and the resolved SSH command;
- `status` is the shorter "is this lease healthy right now" check, with
  optional `--wait` and bounded telemetry;
- `list` is the table view across many leases, scoped by owner/org or
  fleet-wide for admins.

Use `inspect` when something is unexpected and you want all the detail in
one place. Use `status` when an automation needs a quick liveness check.
Use `list` when you are looking for a specific lease across the pool.

Related docs:

- [status](status.md)
- [list](list.md)
- [ssh](ssh.md)
- [Identifiers](../features/identifiers.md)
- [Network and reachability](../features/network.md)
