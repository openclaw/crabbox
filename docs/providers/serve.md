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

The agent advertises up to four IPv4 front-door candidates: a local tailnet
address in `100.64.0.0/10`, followed by private LAN addresses. Clients try
same-subnet LAN candidates first, then tailnet, then other candidates.
If no direct candidate is reachable, new agents and clients use a coordinator-brokered
WebSocket relay. Released 0.75.0 agents retain their direct-only behavior.
Normal sync, run, logs, results, SSH, list, status, inspect, and stop work through
the existing brokered SSH-lease path.

The [browser portal](../features/portal.md#index-portal) shows your donated
machines below the fleet, with online/offline and draining status, heartbeat age,
capacity, allocated lease links, runtime/agent versions, and SSH front doors.
It also shows served leases, runtime, and allocated CPU-hours for leases created
in the current UTC month, matching `crabbox serve machines`. These summaries combine
current allocations with durable monthly rollups recorded once on agent-confirmed
cleanup. Rollups start at deployment with no historical backfill; cross-month leases
remain attributed to their creation month. Listings read only each machine's rollup
and current allocations, never the full lease history.
The section appears only for enabled owners with machines; even admins see only
their own machines in the current organization.

[`crabbox usage`](../commands/usage.md) also groups serve leases by stable machine
ID with the display name when available, including full recorded history. It uses the selected reporting month and
normal usage authorization. CPU-hours are runtime times assigned CPUs, not actual
CPU utilization; serve leases continue to carry zero estimated cost.

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
Availability sampling starts after the first reconciliation and image-preparation
kickoff, and reuses reconciliation's container inventory. Busy or unknown
availability does not delay image preparation or existing lease deadlines.
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

## Availability

By default, a machine accepts new leases only while the owner is not using it.
The agent samples host signals approximately every ten seconds, independently
of heartbeats and deadline enforcement. Busy or unknown machines advertise zero
free capacity; existing leases continue at their existing low CPU priority.
Availability never kills a running lease.

| Policy | Desktop/server default | Laptop default (battery detected) |
| --- | --- | --- |
| `--max-host-cpu` | `0.5` | `0.5` |
| `--require-user-idle` | `0` (disabled) | `10m` |
| `--require-ac-power` | Ignored | `true` |
| `--max-memory-pressure` | `critical` | `critical` |

CPU is a time-weighted 60-second average of total host utilization minus this
agent's owned, labeled containers' runtime CPU usage, normalized by host logical
cores and clamped at zero. On macOS the agent uses
Mach host CPU counter deltas (or interval `top` samples in builds without cgo), the kernel memory-pressure
level, IOHIDSystem console idle time, and `pmset` power information. On Linux it
uses `/proc/stat`, `MemAvailable` (warning below 10%, critical below 5%), and
sysfs power information. Linux console idle time is unknown: enabling an idle
requirement there conservatively prevents availability. Unsupported or failed
required measurements are unknown, never treated as idle. Unknown form factor
uses laptop defaults.

If the agent cannot read its container inventory or CPU stats, it uses raw host
CPU without subtracting container usage. This conservatively overestimates load;
`serve container CPU unknown` is an informational reason, not an unknown host
measurement. Host signals are sampled concurrently with independent 45-second
deadlines. A slow or failed signal does not consume another signal's deadline,
and sampling never blocks heartbeats. Samples start at least ten seconds apart;
reads that take longer begin the next sample immediately after completion.
Readings and gaps older than one minute are treated as unknown; the longer
budget allows CPU sampling to complete on heavily loaded hosts without treating
those bounded reads as stale.

CPU above the threshold, memory pressure at or above the selected level, or
battery power must persist for at least 60 seconds before becoming busy.
Recent console activity makes the machine busy immediately. All required
conditions must be clear for five minutes before availability returns, including
at startup after the first complete CPU window. Unknown measurements immediately
prevent new placement. `--availability always` explicitly disables gating.

Configuration uses `serve.availability.mode`, `maxHostCPU`, `requireUserIdle`,
`requireACPower`, and `maxMemoryPressure`. Flags override environment variables,
which override configuration and detected defaults. The corresponding environment
variables are `CRABBOX_SERVE_AVAILABILITY`, `CRABBOX_SERVE_MAX_HOST_CPU`,
`CRABBOX_SERVE_REQUIRE_USER_IDLE`, `CRABBOX_SERVE_REQUIRE_AC_POWER`, and
`CRABBOX_SERVE_MAX_MEMORY_PRESSURE`.

```yaml
serve:
  availability:
    mode: auto
    maxHostCPU: 0.5
    requireUserIdle: 10m
    requireACPower: true
    maxMemoryPressure: critical
```

`crabbox serve status` and `crabbox serve machines` show availability and reasons.
The portal shows the same state, CPU, console idle time, and power source; its
machine tile counts online, non-draining available machines. Legacy agents
without availability data retain their previous placement behavior. Capacity
errors include owner-visible machine states and reasons.

For a read-only local measurement, run `crabbox serve status --probe --json`.
It samples for seven minutes by default to include the startup and recovery windows,
without registering, changing services, starting containers, or writing state.
`--probe-duration 1m` through `1h` changes the observation period; a short probe
may correctly report unknown while warming up. Use the agent's `--state-dir`
and `--runtime` to subtract its own containers.

Set `CRABBOX_SERVE_AVAILABILITY_DEBUG=1` on the agent or probe to log bounded
per-signal outcomes and elapsed times to stderr, including errors and timeouts.
These diagnostics omit command arguments, output, and error text; JSON probe
results remain on stdout.

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
in 1–65535. The agent binds each advertised IPv4 address separately on that
same port, never a wildcard address. If some binds fail, it logs the failures
and advertises only the addresses that bound; no bound addresses is fatal.
The agent re-detects candidates on each heartbeat and rebinds when addresses
change, including DHCP and Wi-Fi changes. Capacity is controlled by availability,
`--max-leases`, and the CPU/memory offer.

Automatic LAN candidates come from up, non-loopback, broadcast-capable
interfaces in `10/8`, `172.16/12`, or `192.168/16`. Tunnel and runtime bridge
interfaces are excluded. `--no-lan-candidates` (config
`serve.noLANCandidates`, environment `CRABBOX_SERVE_NO_LAN_CANDIDATES`) disables
LAN discovery. LAN traffic is **not governed by tailnet policy**: the front door
is reachable from the LAN and permits only the live lease's key authentication.
Host firewall rules still apply.

Repeat `--advertise-address` to replace automatic detection entirely, keeping
up to four explicit IPv4 addresses in the given order. Configuration uses
`serve.advertiseAddresses` (a YAML list), or comma-separated
`CRABBOX_SERVE_ADVERTISE_ADDRESSES`. The released singular environment variable
`CRABBOX_SERVE_ADVERTISE_ADDRESS` remains a fallback. Explicit addresses may be
public or link-local, but never wildcard, loopback, or multicast.

The client spends at most about six seconds probing candidates. It accepts an
address only after an SSH key exchange proves possession of the pinned agent
host key; a wrong host on a reused LAN subnet is skipped without prompting or
learning its key. Both SSH hops retain their own pinned host key. The chosen
route is printed as `serve route: lan 192.168.1.20:8443` (or `tailnet`/`explicit`)
and included in run timing JSON as `sshRoute`. Private per-lease state caches the
choice; subsequent invocations validate only that endpoint's pinned key and
re-probe candidates if it fails. Existing single-address agents and clients
remain supported.

If direct candidates fail, the client prints `serve route: relay`. Relay works
from off-network clients and without any tailnet-policy changes: both the client
and donating agent connect outbound to the coordinator. A dedicated relay session
forwards the end-to-end SSH transport; both front-door and container host keys
remain pinned. The relay never terminates either SSH hop.

Relay admission remains owner-only and requires an active lease on an online
machine. Each owner may have eight concurrent sessions, each lease four (Node
also caps the process at 1,024 sessions). Admission tokens are single use; the
agent token is stored only as a hash in coordinator storage and delivered from
the live session's memory on the next heartbeat. Parties have 60 seconds to
connect, and the first socket waits at most 30 seconds for its peer. Closing
either side or ending the lease closes the relay; sessions cannot outlive the
lease's hard deadline. Sessions do not survive a relay restart; start a fresh
invocation to retry.

Relay adds coordinator latency. Each direction allows one acknowledged 32 KiB
frame at a time, bounding buffering with backpressure at the cost of throughput
on high-latency links. On macOS and Linux clients, foreground commands reuse one
relay-backed front-door SSH connection for the invocation. Readiness, sync, and
workload commands also reuse their inner SSH connection; workspace ownership
uses a separate inner connection over that same relay so renewals and release
remain independent of the workload. Both host keys remain pinned, and the front
door still restricts every forwarding channel to the authenticated lease.

Control sockets live in private, owner-only directories unique to the invocation
and lease identity. Their foreground masters are joined and the directories
removed before release or when the invocation exits, including cancellation and
`--keep`. They are never shared between invocations or coordinator logins.
Exported background forwards retain their independent transport lifetime.
Windows clients retain the non-multiplexed relay transport.

A fresh invocation always retries direct candidates, even
if its previous route was relay, so returning to the LAN restores direct access.
The relay runs on both Cloudflare (one dedicated Durable Object per session) and
Node/PostgreSQL. Fleet handles admission and lifecycle, never the data frames.
Printed SSH commands reuse your normal coordinator login and refuse to connect
if the configured coordinator has changed since the command was generated.

Relay SSH connections allow up to 60 seconds for admission, pairing, and the
two SSH handshakes, including bootstrap probes and sync. A shorter caller
deadline or cancellation still stops the operation. Direct routes keep their
existing connection timeouts.

Set `CRABBOX_SERVE_RELAY_DEBUG=1` on the client and agent to diagnose relay
connections. Client stderr (including bootstrap probes) and the agent log show
session IDs, bytes sent/received, ACK counts, outstanding bytes, the window
limit, and close reasons. Output is limited to the first 16 events per session,
then one event every five seconds and a close summary. Payloads, credentials,
and peer-provided close text are never logged.

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
connections. Signed release builds (Homebrew or GitHub Releases) are allowed
automatically when "Automatically allow downloaded signed software" is on;
otherwise allow the binary in System Settings → Network → Firewall → Options.
If leases provision but clients on other machines time out waiting for the SSH
transport, first confirm plain TCP reachability of the front door from a client
machine (for example `nc -z <address> <port>`) before changing the agent: tailnet
policy and host routing problems produce the same symptom.

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
