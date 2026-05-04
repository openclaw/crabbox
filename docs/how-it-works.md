# How Crabbox Works

Read when:

- you are new to Crabbox;
- you want the end-to-end mental model;
- you need to know which component owns a behavior before changing code.

## TL;DR

Crabbox is a remote testbox system. Two sides cooperate:

- The **CLI** keeps the developer story simple: lease a machine, sync the dirty checkout, run a command, stream output, clean up.
- The **broker** (a Cloudflare Worker plus one Durable Object) keeps shared capacity safe: it owns provider credentials, lease state, expiry, cleanup, usage, and cost guardrails.

Cloud machines are vanilla Ubuntu runners that hold no broker secrets. They are leaves: provisioned, used, deleted.

## The Pieces

```text
+----------------------+    HTTPS / JSON     +--------------------------+
| your laptop          |  ------------------> | Cloudflare Worker        |
| -------------        |   bearer + owner     | ------------------       |
| crabbox CLI          |                      | Fleet Durable Object     |
| repo checkout        |                      | provider creds           |
| per-lease SSH key    |                      | lease + usage state      |
+----------+-----------+                      +------------+-------------+
           |                                                | provider API
           |                                                v
           |                                +------------------------------+
           |       SSH (primary + fallback) | Hetzner Cloud / AWS EC2      |
           +----------- rsync ------------> | Ubuntu runner                |
                                            | /work/crabbox/<lease>/<repo> |
                                            +------------------------------+
```

The CLI talks to the broker over HTTPS, then talks **directly** to the leased runner over SSH and rsync. The runner never calls the broker; that path stays one-way.

## Ownership

| Layer       | Owns                                                                                                                                                            |
|:------------|:----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **CLI** | config + flags; per-lease SSH key; SSH readiness; Git seeding + rsync; sync fingerprints + sanity checks; remote command + streaming; heartbeats; release |
| **Broker** | request auth + identity; serialized lease state; provider credentials; machine create/delete; lease expiry; pool/status/inspect; usage; spend caps |
| **Provider** | raw compute: Hetzner Cloud servers or AWS EC2 instances |
| **Runner** | nothing durable for brokered boxes: Linux prepared by cloud-init with SSH, Git, rsync, curl, jq, `/work/crabbox`; AWS Windows/WSL2/macOS targets have provider-specific bootstrap; static targets are existing SSH hosts; project runtimes come from repo-owned setup |

## What `crabbox run` does

A single `crabbox run` command walks through five phases:

**1. Plan.** Load config (flags -> env -> repo -> user -> defaults). Mint a temporary lease ID and per-lease SSH key.

**2. Lease.** `POST /v1/leases` to the broker with class, provider, TTL, idle timeout, slug, bootstrap options, and the SSH public key. Worker authenticates, then forwards to the Fleet Durable Object. Durable Object enforces active-lease and monthly spend caps, asks the provider for live pricing, reserves the worst-case TTL cost, provisions the machine, and returns host / SSH user / port / work root / expiry / lease ID / slug. CLI re-keys its local key dir if the broker assigned a different final lease ID.

**3. Sync.** Wait for SSH and the target readiness probe. Seed remote Git when possible. Compare local and remote sync fingerprints; skip rsync if nothing changed. Otherwise rsync the dirty checkout into `/work/crabbox/<lease>/<repo>` for POSIX targets, or send a manifest tar archive for native Windows, then run sanity checks and hydrate the configured base ref when supported.

**4. Run.** Start heartbeats in the background. Run the requested command over SSH and stream stdout/stderr.

**5. Release.** Release the lease unless `--keep` is set. The broker terminates the runner and frees provider-side state. If bootstrap never reached SSH readiness on a fresh non-kept lease, `crabbox run` retries once with a new machine; it never duplicates commands on kept or explicitly reused leases.

## Warm Machines And Reuse

`crabbox warmup` follows the same lease creation path, then keeps the box ready for later use. Reuse is explicit:

```sh
crabbox warmup --profile project-check
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

While the CLI is using a lease it sends heartbeats; the Durable Object updates `lastTouchedAt` and recomputes idle expiry. If a warm lease goes untouched for the idle timeout, the alarm releases it.

## Brokered vs Direct Provider

The **brokered path** is normal operation:

```text
CLI -> Cloudflare Worker -> Durable Object -> provider API
CLI -> runner over SSH/rsync
```

Use it when maintainers or agents share infrastructure. Provider secrets stay off local machines; cleanup, usage, and cost control all flow through the broker.

The **direct path** is a debug fallback:

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

Direct mode needs local provider credentials (AWS SDK chain or `HCLOUD_TOKEN`). It has no central usage history and no brokered heartbeat. It is handy for diagnosing the broker itself, not for day-to-day work.

Static SSH targets use `provider: ssh` and bypass the broker even when a broker
URL exists in config. macOS and Windows WSL2 use the POSIX rsync contract;
native Windows uses PowerShell plus tar archive sync.

## Auth And Identity

The broker accepts signed GitHub login tokens for normal users and shared bearer tokens for trusted automation. Fallback routes can also sit behind Cloudflare Access before the Worker sees the request. Bearer-token CLI requests send:

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

Owner is resolved from the signed GitHub token for `crabbox login` users. In shared-token mode, owner comes from `CRABBOX_OWNER`, the Git email env, or `git config user.email`; `CRABBOX_ORG` sets the org. Raw Cloudflare Access identity headers are ignored; only a verified Access JWT email can become the bearer-token owner.

## Sync Model

Crabbox sync is intentionally local-first: it does not require a clean checkout. The sync layer:

- seeds remote Git from the configured origin/base ref when possible;
- overlays local dirty files with rsync (with a checksum mode for reliability);
- skips no-op syncs via fingerprints;
- excludes heavy directories from repo config;
- guards against suspicious mass tracked deletions;
- hydrates base-ref history for changed-test workflows.

Same loop for agents and humans: edit locally, run remotely.

## Cost And Usage

The broker tracks two cost numbers per lease:

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

Hourly price source order:

```text
1. CRABBOX_COST_RATES_JSON  explicit override
2. provider live pricing    EC2 Spot history / Hetzner server-type prices
3. built-in fallback rates  last-resort defaults
```

Hetzner pricing converts via `CRABBOX_EUR_TO_USD`. `crabbox usage` queries `GET /v1/usage` and groups by user, org, provider, and server type.

## Failure And Cleanup

Crabbox assumes failures are normal: CLIs crash, SSH disconnects, cloud-init fails, provider calls partially succeed, Cloudflare retries, machines outlive local processes.

The design response:

- one Durable Object serializes fleet decisions;
- lease creation is idempotent where practical;
- provider resources carry Crabbox tags/labels;
- release is safe to call repeatedly;
- stale leases expire by alarm;
- direct cleanup is conservative;
- brokered cleanup is broker-owned.

`crabbox cleanup` refuses to sweep provider resources when a coordinator is configured, because brokered cleanup belongs to the Durable Object.

## Where To Go Next

- [Architecture](architecture.md): component model, API, state, and failure model.
- [Orchestrator](orchestrator.md): broker responsibilities, lifecycle, cleanup, cost, and usage.
- [CLI](cli.md): command surface, config, output, and exit codes.
- [Features](features/README.md): one page per feature area.
- [Commands](commands/README.md): one page per command.
- [Infrastructure](infrastructure.md): Cloudflare, DNS, Hetzner, and AWS setup.
- [Security](security.md): auth, secrets, SSH, cleanup, and trust boundaries.
