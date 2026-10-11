# Serve provider

`serve` is a brokered Linux SSH-lease provider. A trusted machine runs
`crabbox serve`, advertises spare CPU and memory, and polls its configured
coordinator every two seconds for container assignments. The coordinator never
opens a connection to the machine. Both the Cloudflare and Node/PostgreSQL
coordinators use the same placement and lifecycle module.

## Coordinator enablement

Serve is disabled by default. The coordinator operator must set
`CRABBOX_SERVE_ALLOWED_OWNERS` to a comma-separated list of exact recorded owner
IDs, for example `github:12345,github:67890`. Whitespace and empty entries are
ignored; `*` allows every authenticated owner. Unset or empty disables serve
for everyone, on both coordinator runtimes. All serve endpoints and new serve
lease requests return HTTP 403 `serve_not_enabled` for unlisted owners without
exposing machine or lease data. `crabbox doctor --provider serve` and
`crabbox serve status` report this rejection.

Removing an owner prevents new placements and rejects its agent heartbeats.
The agent logs `serve_not_enabled` and continues enforcing local TTL and idle
deadlines; the container watchdog also remains active. Existing containers
therefore expire even while access is disabled.

## Quick start

Log in to the same coordinator on the donating machine and client using the
normal `crabbox login` workflow, then run:

```sh
# Donating machine, with Docker/OrbStack or Podman running:
crabbox serve --cpus 24 --memory 48g

# Client with network access to the donating machine:
crabbox run --provider serve -- pnpm test
crabbox serve machines --json
crabbox doctor --provider serve
```

The default advertised address is a local IPv4 address in `100.64.0.0/10`,
commonly supplied by a tailnet. Without one, specify `--advertise-address`.
Clients must reach this address directly: SSH is not relayed by the coordinator.
Normal sync, run, logs, results, SSH, list, status, inspect, and stop work through
the existing brokered SSH-lease path.

## Trust and limits

Placement is owner-only, with matching organization scope. A machine registered
by one owner cannot receive another owner's lease. There is no sharing option. The coordinator currently accepts up to 64 registered
machines per owner and organization, each machine accepts at most 32 leases,
and each owner in an organization can have at most 64 live serve leases
(`serve_lease_limit`, HTTP 429). Lease SSH public keys must be `ssh-ed25519`
(at most 256 bytes) or `ssh-rsa` (at most 4096 bytes), including any comment.
Shared coordinator maintenance processes at most 16 machines and reads at most
256 reserved leases per alarm, resuming from a persisted cursor within 15 seconds
when more work remains. Ordinary fleet expiry and orphan cleanup run between batches.
Containers and shared package caches are for the owner's trusted workloads;
they are not a hostile-tenant boundary. The agent uses the existing broker login
and stores no new token type. Agent state includes its persistent machine UUID,
public lease keys, deadlines, and status, outside the repository.

Every container gets a hard CPU limit, memory limit, and low CPU shares. The
agent and coordinator enforce the offered CPU, memory, and `--max-leases` caps.
CPU shares reduce contention but do not reserve idle CPU for the host.
The agent starts only its configured local-container image; a different image
in an assignment fails with `serve_image_mismatch`.
Containers have no host Docker socket, privileged mode, host network, or host
bind mounts. Only managed named cache volumes are mounted. The image and SSH
bootstrap come from the shared local-container implementation.

| Class | CPU | Memory |
| --- | ---: | ---: |
| standard | 8 | 16 GiB |
| fast | 16 | 32 GiB |
| large | 32 | 64 GiB |
| beast | Whole offered capacity | Whole offered capacity |

The discovery class catalog is `unmapped` with no static profiles: the
coordinator resolves these four sizes, and `beast` depends on the selected
machine's offer. This does not disable `--class`; `tiny` and `small` are not
supported served sizes.

`--type cpu4` requests four CPUs and 8 GiB. Requests must fit the offer and
remaining reservations. Placement prefers the most free CPU; use
`--serve-machine <name-or-UUID>` to pin a machine. No capacity fails immediately
with `serve_capacity_unavailable`, including owner-visible machine capacity.

Optional fallback applies only to that explicit capacity rejection:

```yaml
provider: serve
serve:
  machine: builder
  fallback: aws
```

`--serve-fallback aws` overrides configuration. Acquisition retries once using
the fallback provider; other errors do not trigger fallback. The actual provider
is retained for run reporting and cleanup. Fixed-ID acquisition does not switch
providers. `CRABBOX_SERVE_MACHINE` and `CRABBOX_SERVE_FALLBACK` provide environment
overrides.

## Deadlines, shutdown, and cleanup

The coordinator considers machines online for 30 seconds after their last
heartbeat. After 90 seconds without a heartbeat, live leases become failed/lost.
Machine records offline for more than seven days are pruned once all their
leases have confirmed cleanup. On return, the agent removes only containers
labeled as belonging to its own machine UUID when the coordinator no longer recognizes their leases.

Both the agent and an in-container watchdog enforce the hard TTL and idle
deadline. The watchdog keeps running when the agent exits or cannot contact the
coordinator, and automatic runtime removal deletes the container when it ends.
Client activity refreshes idle deadlines through the normal coordinator
heartbeat. An unreachable coordinator cannot extend a local deadline.

SIGINT/SIGTERM marks the agent draining and stops accepting work. By default,
running containers remain until their last received deadlines; restart the
agent to resume heartbeats. `--drain-on-exit` removes its containers immediately.
Release reserves capacity until the agent confirms actual container absence.
Never manually reuse an agent state directory concurrently on two machines.

## Cache volumes and macOS runtimes

The agent reuses machine-scoped named volumes for the pnpm store, npm cache,
Go build cache, and Go module cache. Existing `cache.volumes` configuration
customizes these paths; names remain scoped to the machine. Use
`--no-cache-volumes` to disable or `crabbox serve cache prune` while the agent is
stopped to remove its cache volumes. Runtime refusal to remove in-use volumes
is reported, not forced.

SSH binds only the advertised address by default. Ports are runtime-assigned
unless `--ssh-ports` is set, for example `--ssh-ports 8443` or
`--ssh-ports 2200,2205-2207`. This also accepts config `serve.sshPorts` and
environment variable `CRABBOX_SERVE_SSH_PORTS`; the flag has highest priority.
Ports must be in 1–65535, ranges must be ascending, and the pool may contain at
most 1024 distinct ports. Busy ports are skipped. Advertised lease capacity is
limited by usable pool ports and `--max-leases`. Exhaustion during assignment
fails that lease with `serve_ssh_ports_exhausted`; it never uses an ephemeral
port outside the pool. Permit these ports in the tailnet ACL and host firewall.

Some macOS VM-backed Docker or Podman runtimes cannot publish a host tailnet address. Select a reachable
`--advertise-address` and a runtime-supported `--publish-host`. Binding
`--publish-host 0.0.0.0` is an explicit opt-in and prints a warning; it is never a
fallback. Check host firewall rules and runtime port forwarding yourself.

macOS and Windows guest targets, GPU requests, desktop/WebVNC, and cross-owner
sharing are unsupported. See [serve command](../commands/serve.md) for service
installation and agent flags.
