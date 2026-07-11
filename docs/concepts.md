# Concepts

Read when:

- you encounter a Crabbox term you do not recognize;
- you are writing docs and want to stay consistent with existing usage;
- you need a single page that lays out the vocabulary.

This page is a glossary. It defines the nouns and verbs Crabbox uses across the
CLI, broker, providers, and docs. When two synonyms exist, the preferred form is
in **bold**.

## Compute vocabulary

**Lease** - a time-bounded reservation of a remote runner that Crabbox created
or resolved. Has a canonical ID (`cbx_...`), a friendly slug, an idle timeout, a
TTL, and a state (`active`, `released`, `expired`, `failed`). Leases are the unit
of cost accounting and cleanup.

**Runner** - the remote machine itself. Provisioned by the provider, prepared by
the bootstrap step, and used for one or more runs. Crabbox does not distinguish
between a Hetzner cloud server, an AWS EC2 instance, and a static SSH host beyond
what the provider backend reports; all are runners.

**Box** / **testbox** - informal synonym for runner, used in the README and some
feature docs. Prefer "runner" in new docs unless the surrounding context talks
about leases as a product, in which case "box" reads more naturally (for example
"warm box").

**Pool** - the set of currently active runners visible to a user, org, or the
whole fleet. `crabbox list` and the admin `GET /v1/pool` endpoint both expose it.

**Pond** - an emergent peer group of leases that share a `pond=<name>` provider
label. A pond is not a central cluster object; it exists for as long as at least
one active lease carries the label. Use `crabbox pond peers`, `crabbox pond
connect`/`disconnect`, and `crabbox pond release` to operate on the group. See
[Pond](features/pond.md).

**Lease ID** - the canonical machine-friendly identifier, `cbx_` plus 12
lowercase hex characters (16 characters total, e.g. `cbx_abcdef123456`). Used in
labels, logs, claims, and broker APIs. See [Identifiers](features/identifiers.md).

**Slug** - the friendly name for a lease, drawn from a crustacean wordlist and
looking like `swift-crab`. Generated from a stable hash of the lease ID;
collisions append a 4-hex suffix. A requested `--slug` is normalized to
`[a-z0-9-]` (max 41 chars). Most commands accept either the ID or the slug via
`--id`.

**Run** - a single `crabbox run` invocation recorded by the broker. Has a `run_`
ID, an owning lease, a command, an exit code, and a record in run history. See
[History and logs](features/history-logs.md).

**Workspace** - the repo checkout plus runtime state inside a lease or delegated
sandbox: synced files, hydrated dependencies, the provider-owned working
directory, command output, and optional desktop/browser state. Prefer
"workspace" when the user-visible product is more than the raw runner.

## Roles

**CLI** - the local Go binary `crabbox`. Owns config, sync, command execution,
output streaming, and per-lease SSH keys. See [Architecture](architecture.md).

**Broker** / **coordinator** - the authenticated shared control plane. It owns
provider credentials, lease state, expiry, cleanup scheduling, run history,
usage, and cost caps. It runs on Cloudflare Workers with a Fleet Durable Object
or on Node.js with PostgreSQL and pg-boss. The two terms are interchangeable;
"coordinator" is preferred in docs that emphasize the service, "broker" when
emphasizing the trust boundary between the CLI and the provider.

**Provider** - a Crabbox adapter that knows how to acquire, resolve, list, and
release runners on a backing service. Crabbox ships adapters for managed clouds
(AWS, Azure, Google Cloud, Hetzner), self-hosted and local hosts (Proxmox,
Parallels, Local Container, static SSH), managed dev sandboxes reached over SSH
(Daytona, KubeVirt, External, Namespace Devbox, Semaphore, Sprites, exe.dev, RunPod, Railway), and
delegated execution sandboxes (Blacksmith Testbox, E2B, Modal, Islo, Tensorlake,
[Upstash Box](https://upstash.com/docs/box/overall/quickstart), Cloudflare, Azure Dynamic Sessions, W&B). See the
[Provider reference](providers/README.md).

**Backend** - the Go interface a provider implements: `SSHLeaseBackend` for
providers that hand Crabbox a real SSH target it provisions and connects to, and
`DelegatedRunBackend` for providers that own command execution themselves. See
[Provider backends](provider-backends.md).

**Operator** - a person with broker-side access (admin token, Cloudflare
config). Operators run `crabbox admin` commands and image bake/promote flows.

**Agent** - an LLM-backed process invoking Crabbox through the CLI. Agents are
first-class users of Crabbox; the docs are written for both humans and agents.
Crabbox gives an agent a governed workspace with sync, logs, artifacts, sharing,
cleanup, and review evidence.

## Modes

**Brokered mode** / **coordinator mode** - the path where the CLI talks to the
coordinator for lease creation, state, and cleanup, while still doing SSH,
rsync, and command execution directly to the runner. Provider secrets stay
coordinator-side.
Chosen when the provider supports the coordinator (AWS, Azure, Google Cloud,
Hetzner) and a broker URL is configured via `CRABBOX_COORDINATOR` or
`config set-broker`.

**Direct mode** / **direct-provider mode** - the CLI talks straight to the
provider API with no broker, no central run history, and no spend caps. This is
the only mode for every provider outside the brokerable four, and the fallback
for those four when no broker URL is configured.

**Static mode** - lease behavior for `provider: ssh`. The host is operator-owned;
Crabbox neither provisions nor deletes it. Always direct, never brokered.

**Delegated mode** - the path used by delegated-run providers (Blacksmith, E2B,
Modal, Islo, and others). The provider owns command execution and streams output
back to Crabbox. Crabbox-owned local sync options (`--sync-only`, `--checksum`)
are rejected; sync timing reports `sync=delegated`.

## Commands

**warmup** - acquire a lease and keep it ready (`--keep`, default true). No
command runs yet; the result is a warm box awaiting work.

**run** - acquire or reuse a lease, sync the checkout, run a command, stream
output, and release unless told to keep the lease.

**stop** / **release** - end a lease and delete its backing provider resources.

**cleanup** - sweep direct-provider leftovers based on labels and local state.
Intended for direct mode.

**reuse** - using `--id` (or a slug) to pick an existing lease instead of
creating a new one. Both `warmup` and `run` accept `--id`.

**reclaim** - move a local claim from one repo checkout to another so a lease
created in repo A can be reused from repo B (`--reclaim`). Required because
Crabbox binds leases to repos by default.

**hydrate** - prepare a runner with project dependencies from a repo-owned
GitHub Actions workflow. The default path executes supported setup steps; the
`--github-runner` fallback registers the box as a self-hosted runner and
dispatches a real Actions job when full GitHub semantics are required. See
[Actions hydration](features/actions-hydration.md).

## State

**Idle timeout** - how long a lease may go without a heartbeat before the broker
auto-releases it. Default 1800s (30m). Reset by every heartbeat or explicit
touch.

**TTL** - the absolute maximum wall-clock lifetime of a lease. Default 5400s
(90m), capped at 86400s (24h). Cannot be extended by heartbeats.
`expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`.

**Heartbeat** - a `POST /v1/leases/{id}/heartbeat` call sent by the CLI during
long-running commands. Bumps `lastTouchedAt`, can ship telemetry samples, and can
update the idle timeout when explicitly requested. Heartbeats at or after
`expiresAt` are rejected; once the deadline passes, cleanup owns the lease.

**Touch** - lower-level synonym for "update lease state and idle". The provider's
`Touch` method handles direct-provider state updates; the heartbeat is the
brokered equivalent.

**Reserved cost** - the worst-case TTL cost the broker reserves for a lease at
creation time (`hourlyRate × ttl`). Charged against the monthly spend cap until
the lease ends; freed on release.

**Estimated cost** - elapsed-runtime cost for a lease, computed from the hourly
rate and time spent `active`. What `crabbox usage` reports as a billing
approximation. See [Cost and usage](features/cost-usage.md).

## Sync

**Manifest** - the NUL-delimited list of paths Crabbox will sync, built from
`git ls-files --cached` plus `git ls-files --others --exclude-standard`, with
deletions tracked separately.

**Fingerprint** - a hash of the commit, dirty file metadata, and manifest. When
the local fingerprint matches the remote one, Crabbox skips rsync entirely.

**Git seeding** - the optional first-sync step where Crabbox fetches the
configured origin/base ref into the runner's Git directory before rsync, so
changed-file diffs are available remotely.

**Base ref** - the Git ref Crabbox seeds and hydrates against. Configurable per
repo via `sync.baseRef`.

**Sanity check** - a guardrail run after rsync that detects mass tracked
deletions, missing manifest entries, and other suspicious sync outcomes. See
[Sync](features/sync.md).

## Capabilities

**Desktop** - lease capability (`--desktop`) that adds a visible session:
resize-capable TigerVNC + XFCE on managed cloud Linux, Xvfb + XFCE + x11vnc in
local containers, or Wayland/GNOME. Required for `crabbox
vnc`, `crabbox webvnc`, and most `--browser` UI runs.

**Browser** - lease capability (`--browser`) that installs Chrome/Chromium and
exposes it through environment variables. Useful for Playwright/Vitest without a
full QA harness.

**Code** - lease capability (`--code`) that installs code-server bound to
loopback (port 8080). Used by `crabbox code` and the portal `/code/` bridge;
managed Linux only.

**Tailscale** - optional reachability layer for managed Linux leases. Joins the
lease to the configured tailnet so clients can reach the runner without its
public IP. Distinct from the network mode (`--network tailscale`) that selects
which plane the CLI uses. See [Capabilities](features/capabilities.md).

## Coordinator Runtime

**FleetCoordinator** - the runtime-neutral TypeScript control-plane behavior.
Both deployment runtimes use the same routes, auth, provider adapters, records,
cost controls, cleanup rules, portal, and bridge protocols.

**Durable Object** - the Cloudflare primitive that stores coordinator state and
serializes fleet decisions. Crabbox uses one Fleet Durable Object
(`idFromName("default")`) in the Cloudflare runtime.

**PostgreSQL runtime** - the portable Node.js deployment. The `crabbox` schema
stores coordinator key/value records; pg-boss uses `crabbox_jobs` for exact
maintenance jobs and periodic reconciliation.

**Alarm** - a future cleanup deadline. Cloudflare uses Durable Object alarms;
Node/PostgreSQL uses pg-boss jobs. Both schedule idle-timeout, TTL, cleanup
retry, and provider maintenance work.

**Portal** - the server-rendered, authenticated web UI hosted by the
coordinator under `/portal/...`. Surfaces lease detail, run logs and events,
and the live VNC/Code panes. See [Browser portal](features/portal.md).

**Bridge** - a coordinator endpoint that proxies traffic to a loopback service
on the lease over a WebSocket: WebVNC (VNC on 5900), code-server (8080), and egress.
Bridges authenticate against the portal session, then talk to the lease over the
SSH plane.

## Identity

**Owner** - the email address that owns a lease. Resolved from the signed GitHub
login token, `CRABBOX_OWNER`, Git env, or `git config user.email`.

**Org** - the GitHub-style organization namespace for a lease. Resolved from the
signed token or `CRABBOX_ORG`. Used for usage scoping and multi-tenant cost caps.

**Allowed org** - the GitHub org membership the broker requires before issuing a
signed login token. Configured per Worker.

**Admin token** - the separately scoped token required for `GET /v1/pool`, admin
lease routes, and fleet-wide listing. Held more closely than the shared
automation token.

**Cloudflare Access** - optional protection layer in front of the Worker. When
configured, the Worker trusts the verified `cf-access-jwt-assertion` header for
identity instead of raw client headers.

## Storage

**State directory** - where the CLI keeps local state (claims, per-lease keys,
known_hosts, checkpoints). Defaults to `$XDG_STATE_HOME/crabbox`, falling back to
the platform-specific user config directory.

**Claim** - a JSON file under the state directory binding a lease to a repo
checkout. Required for `crabbox run --id` to resolve slugs and to refuse
cross-repo reuse without `--reclaim`.

**Workdir** / **work root** - the directory on the runner where Crabbox syncs the
repo. Default `/work/crabbox` on Linux; provider- and target-specific on Windows
(`C:\crabbox` in normal mode) and macOS.

## Evidence and artifacts

**Run history** - the broker's record of past runs (`crabbox history`, `logs`,
`events`, `results`). Distinct from a lease's live state. See
[History and logs](features/history-logs.md).

**Test results** - parsed JUnit outcomes for a recorded run, surfaced by
`crabbox results <run-id>` (suites, tests, failures, errors, skipped). See
[Test results](features/test-results.md).

**Telemetry** - lightweight Linux host-health samples (load, memory, disk,
uptime) captured around a run. Per-run resource snapshots, not product analytics.
See [Telemetry](features/telemetry.md).

**Artifacts** - QA output bundles produced by `crabbox artifacts` (screenshots,
video, GIF, logs, status, metadata) that can be published and commented on a PR.
See [Artifacts](features/artifacts.md).

**Egress** - mediated outbound network for a lease: `crabbox egress` bridges the
box's traffic through the operator's machine over a WebSocket. See
[Egress](features/egress.md).

**Checkpoint** - saved VM-or-workspace state you can restore or fork from, with
IDs `chk_...`. Kinds include workspace archives, recipes, and provider-native
snapshots/images. See [Checkpoints](features/checkpoints.md).

**Cache volume** - provider-backed persistent mount point for speed-only state,
keyed by repo/runtime/platform inputs and mounted at an absolute cache path.
Unlike a checkpoint, it is not a forkable scenario handle and the worktree
remains the source of truth. See [Cache Controls](features/cache.md).

**Capsule** - a portable, replayable failure bundle captured from a GitHub
Actions run (`crabbox capsule from-actions`) and re-run with `crabbox capsule
replay`. See [Capsules](features/capsules.md).

## Documentation

**Source map** - the doc page that points each user-visible behavior at the
implementation file behind it. See [Source map](source-map.md).

**Feature page** - a doc under `docs/features/<name>.md` describing one capability
area. Owns the conceptual story; commands and providers cross-link from here.

**Command page** - a doc under `docs/commands/<name>.md` describing the flags,
behavior, and exit codes of one CLI command, kept in sync with `--help`.

**Provider page** - a doc under `docs/providers/<name>.md` describing one
provider's targets, config keys, env vars, sync behavior, and expected failures.

Related docs:

- [How Crabbox Works](how-it-works.md)
- [Architecture](architecture.md)
- [CLI](cli.md)
- [Configuration](features/configuration.md)
- [Provider backends](provider-backends.md)
