# How Crabbox Works

Read this when you are new to Crabbox, want the end-to-end mental model, or
need to know which component owns a behavior before you change code.

## TL;DR

Crabbox is a remote software testing and execution control plane built around
short-lived testboxes and sandboxes. Two sides cooperate:

- The **CLI** (`cmd/crabbox`, `internal/cli`) keeps the developer story simple:
  lease a box, sync your dirty checkout, run a command, stream output, clean up.
- The **coordinator** (`worker/src`) keeps shared capacity safe: it holds
  provider credentials and owns lease state, expiry, cleanup, usage accounting,
  and cost guardrails. It runs on Cloudflare Workers with a Durable Object or
  as a Node.js service with PostgreSQL and pg-boss.

The leased machines are vanilla runners that hold no broker secrets. They are
leaves: provisioned, used, deleted. Durable evidence — run records, logs,
telemetry, screenshots, artifacts — stays with Crabbox, not on the box.

## The pieces

```text
┌──────────────────────┐   HTTPS / JSON   ┌───────────────────────────┐
│ your machine         │ ───────────────► │ coordinator runtime       │
│ crabbox CLI          │  bearer + owner  │ Cloudflare + Durable Obj. │
│ repo checkout        │                  │ or Node.js + PostgreSQL   │
│ per-lease SSH key    │                  │ provider creds + state    │
└──────────┬───────────┘                  └────────────┬──────────────┘
           │                                         │ provider API
           │                                         ▼
           │                          ┌──────────────────────────────┐
           │ SSH (primary + fallback) │ managed cloud or sandbox     │
           └────────── rsync ────────►│ runner host                   │
                                      │ /work/crabbox/<lease>/<repo> │
                                      └──────────────────────────────┘
```

For normal CLI leases, the CLI talks to the coordinator over HTTPS, then talks
**directly** to the leased runner over SSH and rsync. The runner never calls the
coordinator for ordinary command execution; that path stays one-way. The
coordinator manages leases, not that data plane — your files, commands, and
output never traverse it. The dedicated
[private AWS workspace service](features/aws-private-workspaces.md) is a
separate SSM-only API-managed path with no SSH data plane.

For long-lived interactions the CLI may also open one authenticated WebSocket to
the coordinator at `/v1/control`. That socket carries run-event attach
streams and lease heartbeats, so high-latency links do less request polling. The
HTTPS endpoints remain canonical storage and a compatibility fallback, so older
CLIs and coordinators still interoperate.

## Ownership

| Layer        | Owns                                                                                                                                                                                                                              |
| :----------- | :-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **CLI**      | config + flags; per-lease SSH key; SSH readiness; Git seeding + rsync; sync fingerprints and sanity checks; remote command + streaming; the control WebSocket when available, with HTTP fallback; release                         |
| **Broker**   | request auth + identity; serialized lease state; provider credentials; machine create/delete; lease expiry; pool/status/inspect; run records, logs, events, telemetry; usage; spend caps                                          |
| **Provider** | raw compute: the per-provider adapter creates and deletes hosts (Hetzner servers, AWS EC2 instances, Azure VMs, GCP instances, and others)                                                                                        |
| **Runner**   | nothing durable on a brokered box: Linux is prepared with SSH, Git, rsync, and a work root; non-Linux targets get provider-specific bootstrap; static targets are existing SSH hosts; project runtimes come from repo-owned setup |

## What `crabbox run` does

A single `crabbox run` walks through five phases.

**1. Plan.** Load config in precedence order (flags → env → repo config → user
config → defaults). Mint a temporary lease ID (`cbx_` + 12 hex chars) and a
per-lease SSH key under `<user-config>/crabbox/testboxes/<lease>/id_ed25519`
(RSA for AWS/Azure Windows targets).

**2. Lease.** `POST /v1/leases` to the broker with class, provider, target, TTL,
idle timeout, slug, requested capabilities, and the SSH public key. The
coordinator authenticates the request, serializes fleet mutations, enforces
active-lease and monthly spend caps, asks the provider for live pricing,
reserves the worst-case TTL cost, provisions the machine (with region and
market fallback), and returns the host, SSH user/port, work root, expiry, final
lease ID, and slug. If the coordinator assigned a different final lease ID, the
CLI re-keys its local key directory to match.

**3. Sync.** Wait for SSH and the readiness probe. Seed remote Git from the
configured origin and base ref when possible. Compare local and remote sync
fingerprints and skip rsync if nothing changed. Otherwise rsync the dirty
checkout into the work root (`/work/crabbox/<lease>/<repo>` on Linux) for POSIX
targets, or send a manifest tar archive for native Windows. Then run sanity
checks and hydrate the configured base ref where supported.

**4. Run.** Start heartbeats in the background. Run the requested command over
SSH and stream stdout/stderr back. When brokered, mirror progress to the broker
as a run record with phased events.

**5. Release.** Release the lease unless `--keep` is set. The broker deletes the
runner and frees provider-side state. If bootstrap never reached SSH readiness on
a fresh, non-kept lease, `crabbox run` replaces the machine once and retries; it
never duplicates the command on kept or explicitly reused leases.

## Warm boxes and reuse

`crabbox warmup` follows the same lease-creation path, then keeps the box ready
for later use instead of running a command. Reuse is explicit, by slug or ID:

```sh
crabbox warmup --profile project-check
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

Every lease gets a friendly slug (an `<adjective>-<noun>` pair derived from the
lease ID, e.g. `blue-lobster` or `swift-crab`) alongside its canonical `cbx_…`
ID; most commands accept either via `--id`. While the CLI is using a lease it
sends heartbeats, and the coordinator updates `lastTouchedAt` and recomputes
idle expiry. If a warm lease goes untouched past its idle timeout, the runtime's
durable scheduler releases it.

## Brokered vs. direct provider

The **brokered path** is normal operation:

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

Use it when people or agents share infrastructure. Provider secrets stay off
local machines; cleanup, usage, and cost control all flow through the broker.
Brokering is available for the managed cloud providers (`hetzner`, `aws`,
`azure`, `daytona`, `gcp`) and is engaged only when a broker URL is configured
(`CRABBOX_COORDINATOR` or `config set-broker`).

The **direct path** is the fallback when no broker is configured:

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

Direct mode needs local provider credentials (for example the AWS SDK chain or a
Hetzner token). It keeps no central usage history and no brokered heartbeat. It
is useful for diagnosing the broker itself, and it is the only mode for the many
direct-only adapters (sandbox runners, static SSH hosts, local containers, and
the rest of the provider set).

With `broker.mode: registered`, the provider path stays direct but the CLI
mirrors owner-scoped lease metadata and heartbeats to the coordinator. That adds
portal inventory, opt-in sharing, and outbound WebVNC without moving provider
credentials or direct provider cleanup authority into the broker. Release
removes only metadata by default; an explicitly bound outbound runtime adapter
can perform a user-confirmed workspace delete. Registered records do not count
toward managed provider quotas, costs, images, pools, or maintenance.

Static SSH targets (`provider: ssh`) point at a preexisting machine and bypass
the broker even when a broker URL exists in config. macOS and Windows WSL2
targets use the POSIX rsync contract; native Windows uses a PowerShell plus tar
archive sync.

## Auth and identity

The coordinator accepts signed GitHub login tokens for normal users and shared
bearer tokens for trusted automation. Cloudflare routes may additionally sit
behind Cloudflare Access; Node deployments may trust an identity header only
from configured reverse-proxy CIDRs. Bearer-token CLI requests send:

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

For `crabbox login` users, the owner is resolved from the signed GitHub token. In
shared-token mode the owner comes from `CRABBOX_OWNER`, the Git email env, or
`git config user.email`, and `CRABBOX_ORG` sets the org. Raw Cloudflare Access
identity headers are ignored; only a verified Access JWT email can become the
bearer-token owner. See [Security](security.md) for the full trust model.

## Sync model

Crabbox sync is intentionally local-first and does not require a clean checkout.
The sync layer:

- seeds remote Git from the configured origin and base ref when possible;
- overlays local dirty files with rsync (with an optional checksum mode);
- skips no-op syncs via fingerprints;
- excludes heavy directories from repo config;
- guards against suspicious mass deletions of tracked files;
- hydrates base-ref history for changed-test workflows.

The same loop serves agents and humans: edit locally, run remotely. Use
`crabbox sync-plan` for a read-only preview of the manifest before a run.

## Cost and usage

The broker tracks two cost numbers per lease:

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

Hourly price source order:

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner pricing converts to USD via `CRABBOX_EUR_TO_USD`. Cost caps are set with
the `CRABBOX_MAX_*` env vars (active-lease counts and monthly USD, globally and
per owner/org); exceeding them fails creation with HTTP 429. `crabbox usage`
queries `GET /v1/usage` and groups spend by user, org, provider, and server
type.

## Failure and cleanup

Crabbox assumes failure is normal: CLIs crash, SSH disconnects, cloud-init
fails, provider calls partially succeed, coordinator requests retry, and
machines outlive local processes. The design response:

- each supported runtime serializes fleet decisions;
- lease creation is idempotent where practical;
- provider resources carry Crabbox tags or labels;
- release is safe to call repeatedly;
- stale leases expire through Durable Object alarms or pg-boss jobs, with
  periodic reconciliation;
- direct cleanup is conservative;
- brokered cleanup is broker-owned.

`crabbox cleanup` refuses to sweep provider resources when a coordinator is
configured, because brokered cleanup belongs to the coordinator.

## Where to go next

- [Architecture](architecture.md): component model, API, state, and failure model.
- [Orchestrator](orchestrator.md): broker responsibilities, lifecycle, cleanup, cost, and usage.
- [CLI](cli.md): command surface, config, output, and exit codes.
- [Features](features/README.md): one page per feature area.
- [Commands](commands/README.md): one page per command.
- [Infrastructure](infrastructure.md): coordinator runtimes, ingress, secrets,
  DNS, and provider setup.
- [Security](security.md): auth, secrets, SSH, cleanup, and trust boundaries.
