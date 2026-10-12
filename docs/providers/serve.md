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

## Prepared container images

After the first heartbeat and reconciliation, Serve probes the configured image
in the background for `sshd`, Git, rsync,
curl, sudo, and Python 3. If any are missing and `apt-get` is available, it
installs the shared bootstrap package set once in a temporary container, then
commits a local prepared image. This uses ordinary Docker/Podman `run` and
`commit`; it requires no buildx or Dockerfile. Preparation output goes to stderr.
Images that already have the tools, or have no `apt-get`, keep the existing
bootstrap behavior. Heartbeats and existing-lease reconciliation continue during
preparation; new leases use the configured image with lease-time bootstrap until
the prepared image is ready. If preparation fails, the agent prints a warning and uses
the configured image, so leases install their tools at startup as before.
Failure to pull or inspect the configured image still stops agent startup.

The tag is `crabbox-serve-prepared:<hash>`, where the hash is the first 16 hex
digits of SHA-256 over the base runtime image ID, package list, and recipe
version. Subsequent agent starts reuse that tag. The image has labels
`crabbox.serve.prepared=true`, `crabbox.serve.base-reference`, and
`crabbox.serve.base-image-id`. Preparation preserves the base command,
entrypoint, user, and environment, and removes SSH host keys and apt lists.
Each served container generates its own host keys, including when the
configured image already contained keys.

The advertised image, assignment checks, container `image` label, and image
evidence's `ConfiguredReference` remain the configured reference. Evidence's
`RuntimeImageID` identifies the image actually running. When that ID matches
the agent's prepared image, `RepositoryDigests` and `RepositoryDigestStatus`
come from the configured base ID recorded at startup, since a locally committed
image has no repository digests. Otherwise digests are observed from the running
image itself; container labels cannot select the source. Ordinary
`local-container` provisioning and evidence are unchanged.

## Deadlines, shutdown, and cleanup

`--poll-interval` is a minimum pacing interval: the next poll starts only after
both the previous poll start plus the interval and the previous heartbeat
response (including errors) plus the interval. Slow container work cannot
compress successive heartbeat requests below that interval.

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
stopped to remove its cache volumes and local prepared images that are not
used by running containers. Prepared images are shared by agents using the
same base and recipe, so prune checks running containers across the runtime.
Runtime refusal to remove in-use volumes or images is reported, not forced.
Prune cannot discover idle agents with separate state directories. If another
agent prunes a prepared image, the next lease detects its absence, uses the
configured image with lease-time bootstrap, and starts background re-preparation.

## Single-port SSH reachability

Allow one TCP port per donating machine: the agent's SSH front door, default
`2222`. `--ssh-port 8443` (config `serve.sshPort`, environment
`CRABBOX_SERVE_SSH_PORT`) changes it. The flag takes precedence; ports must be
in 1–65535. The agent binds its advertised address and fails with an actionable
error if that address or port is unavailable. Capacity is controlled solely by
`--max-leases` and the CPU/memory offer.

For example, a tailnet grant can allow clients to reach only that front door:

```json
{
  "grants": [
    {"src": ["group:developers"], "dst": ["tag:crabbox-builder"], "ip": ["tcp:2222"]}
  ]
}
```

On macOS hosts with the Application Firewall enabled, the front door listens in
the `crabbox` process itself, so that binary must be allowed to accept incoming
connections. Notarized release builds (Homebrew or GitHub Releases) qualify
automatically when "Automatically allow downloaded signed software" is on.
Locally built binaries, including Developer ID-signed but unnotarized builds,
are blocked silently: the agent registers and leases provision, but clients on
other machines time out waiting for the SSH transport. Allow that binary in
System Settings → Network → Firewall → Options, or use a notarized build.
Connections from the donating machine to its own address are not filtered.

Containers publish sshd on runtime-assigned **127.0.0.1-only** ports. Those ports
are agent-side implementation details; clients never connect to them directly
or need tailnet grants for them. This also works with macOS VM-backed Docker or
Podman runtimes that cannot bind a host tailnet address.

The front door accepts only public-key authentication as `crabbox`. Each live
lease key authorizes forwarding to that lease ID on port 22 only; shell, exec,
agent forwarding, and arbitrary TCP forwarding are rejected. The client runs
an inner SSH session end to end with the container, using the same per-lease
key. Its private `known_hosts` pins both the persistent agent host key and the
per-container host key supplied through the authenticated coordinator.

The ed25519 agent host key is created once with mode 0600 in the agent state
directory and reused after restart. Keep that directory private and persistent.
Only assignments in the latest successful heartbeat are eligible; saved state
does not authorize clients before the first heartbeat. Release, reconciliation,
deadline expiry, and drain revoke access and close existing tunnels. Deadlines
are enforced independently of coordinator polling. Each machine admits at most
32 pending SSH handshakes (10-second timeout, three authentication attempts),
and each lease at most 16 connections and 16 forwarded streams.

macOS and Windows guest targets, GPU requests, desktop/WebVNC, and cross-owner
sharing are unsupported. See [serve command](../commands/serve.md) for service
installation and agent flags.
