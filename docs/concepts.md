# Concepts

Read when:

- you encounter a Crabbox term you do not recognize;
- you are writing docs and want to stay consistent with existing usage;
- you need a single page that lays out the vocabulary.

This page is a glossary. It defines the nouns and the verbs Crabbox uses
across the CLI, broker, providers, and docs. When two synonyms exist, the
preferred form is in **bold**.

## Compute Vocabulary

**Lease** - a time-bounded reservation of a remote runner that Crabbox
created or resolved. Has a canonical ID (`cbx_...`), a friendly slug, an
idle timeout, a TTL, and a state (`active`, `released`, `expired`,
`failed`). Leases are the unit of cost accounting and cleanup.

**Runner** - the remote machine itself. Provisioned by the provider,
prepared by cloud-init, used for one or more leases. Crabbox does not
distinguish between a Hetzner cloud server, an AWS EC2 instance, and a
static SSH host beyond what the provider backend tells it - all are
runners.

**Box** / **Testbox** - informal synonym for runner. Used in the README and
some early docs. Prefer "runner" in new docs unless the surrounding context
is talking about leases as a product (in which case "box" reads better).

**Pool** - the set of currently active runners visible to a user, org, or
the whole fleet. `crabbox list` and `/v1/pool` both expose it.

**Slug** - the friendly name for a lease. Looks like `blue-lobster`.
Generated from a stable hash of the lease ID; collisions append a 4-hex
suffix. See [Identifiers](features/identifiers.md).

**Lease ID** - the canonical machine-friendly identifier
(`cbx_abcdef123456`). Used in labels, logs, and APIs. Always 16 chars.

**Run** - a single `crabbox run` invocation against a coordinator. Has a
`run_...` ID, an owning lease, a command, an exit code, and a record in
coordinator history.

**Workspace** - the repo checkout plus runtime state inside a lease or delegated
sandbox. A workspace includes the synced files, hydrated dependencies,
provider-owned working directory, command output, and optional desktop/browser
state. New docs should use "workspace" when the user-visible product is more
than the raw runner.

## Roles

**CLI** - the local Go binary `crabbox`. Owns config, sync, command
execution, output streaming, and per-lease SSH keys. See
[Architecture](architecture.md).

**Broker** / **Coordinator** - the Cloudflare Worker plus Fleet Durable
Object. Owns provider credentials, lease state, expiry, cleanup alarms,
usage, and cost. Both terms are used interchangeably; "coordinator" is
preferred in feature docs that emphasize state, "broker" when emphasizing
the trust boundary between CLI and provider.

**Provider** - a Crabbox component that knows how to acquire, resolve,
list, and release runners on a backing service. Built-in providers: AWS,
Hetzner, Static SSH, Blacksmith Testbox, Daytona, Islo. See
[Provider reference](providers/README.md).

**Backend** - the Go interface a provider implements:
`SSHLeaseBackend` for providers that hand Crabbox a real SSH target,
`DelegatedRunBackend` for providers that own command execution
themselves. See [Provider backends](provider-backends.md).

**Operator** - a person with broker-side access (admin token, Cloudflare
config). Operators run `crabbox admin` commands and image bake/promote
flows.

**Agent** - an LLM-backed process invoking Crabbox through the CLI or the
OpenClaw plugin. Agents are first-class users of Crabbox; the docs
intentionally write for both humans and agents. Crabbox gives agents a governed
workspace with sync, logs, artifacts, sharing, cleanup, and review evidence.

## Modes

**Brokered mode** / **coordinator mode** - the normal path, where the CLI
talks to the Cloudflare Worker for lease creation, lease state, and
cleanup. Provider secrets stay broker-side. Used for shared team
infrastructure.

**Direct mode** / **direct-provider mode** - the local-debug fallback, where
the CLI talks straight to the provider API (AWS SDK, Hetzner API, Daytona
SDK, Islo SDK). No coordinator, no central history, no spend caps. Use
when you are debugging the broker itself.

**Static mode** - lease behavior for `provider: ssh`. The host is operator-
owned; Crabbox does not provision or delete it. Bypasses both broker and
direct provisioning paths.

**Delegated mode** - the path used by Blacksmith, Islo, and the Daytona
`run` flow. The provider owns command execution and streams output back to
Crabbox. Crabbox-owned sync (`--sync-only`, `--checksum`) is rejected;
sync timing reports `sync=delegated`.

## Commands

**warmup** - acquire a lease and keep it ready. No command runs yet.

**run** - acquire or reuse a lease, sync, run a command, stream output,
release.

**stop** - release a specific lease and delete its provider resources.

**cleanup** - sweep direct-provider leftovers based on labels. Refuses
when a coordinator is configured.

**reuse** - using `--id` (or a slug) to pick an existing lease instead of
creating a new one. Both `warmup` (idempotent) and `run` accept `--id`.

**reclaim** - move a local claim from one repo to another so a lease
created in repo A can be reused from repo B. Required because Crabbox
binds leases to repos by default.

**hydrate** - prepare a runner with project dependencies, usually by
dispatching a real GitHub Actions job that registers an ephemeral
self-hosted runner. The CLI then runs the local command in the hydrated
workspace. See [Actions hydration](features/actions-hydration.md).

## State

**Idle timeout** - the duration a lease may go without heartbeats before
the broker auto-releases it. Default 30m. Reset by every heartbeat or
explicit touch.

**TTL** - the absolute maximum wall-clock lifetime of a lease. Default
90m. Cannot be extended by heartbeats. `expiresAt = min(createdAt + ttl,
lastTouchedAt + idleTimeout)`.

**Heartbeat** - a `POST /v1/leases/{id}/heartbeat` call sent by the CLI
during long-running commands. Updates `lastTouchedAt`, can ship telemetry
samples, and can update idle timeout when explicitly requested.

**Touch** - lower-level synonym for "update lease state and idle". The
provider's `Touch` method is what handles direct-provider state updates;
heartbeat is the brokered equivalent.

**Reserved cost** - the worst-case TTL cost the broker reserves for a
lease at creation time (`hourlyRate × ttl`). Charged against the monthly
spend cap until the lease ends; freed on release. Distinct from elapsed
runtime cost, which is reported by `crabbox usage`.

**Estimated cost** - elapsed-runtime cost for a lease, computed from the
hourly rate and the time spent in `active`. What `crabbox usage` reports
as a billing approximation.

## Sync

**Manifest** - the NUL-delimited list of paths Crabbox will sync, built
from `git ls-files --cached` and `git ls-files --others --exclude-standard`.

**Fingerprint** - a hash of the commit, dirty file metadata, and manifest.
When the local fingerprint matches the remote one, Crabbox skips rsync.

**Git seeding** - the optional first-sync step where Crabbox fetches the
configured origin/base ref into the runner's Git directory before rsync,
so changed-file diffs are available remotely.

**Base ref** - the Git ref that Crabbox seeds and hydrates. Default
`main`. Configurable per repo in `sync.baseRef`.

**Sanity check** - a guardrail run after rsync that detects mass tracked
deletions, missing manifest entries, and other suspicious sync outcomes.

## Capabilities

**Desktop** - lease capability that adds Xvfb + XFCE + x11vnc. Required
for `crabbox vnc`, `crabbox webvnc`, and most `--browser` UI runs.

**Browser** - lease capability that installs Chrome/Chromium and exports
`BROWSER`/`CHROME_BIN`. Useful for Playwright/Vitest/etc. without a full
QA harness.

**Code** - lease capability that installs code-server bound to loopback.
Used by `crabbox code` and the portal `/code/` bridge.

**Tailscale** - optional reachability layer for managed Linux leases.
Joins the lease to the configured tailnet so clients on the tailnet can
reach the runner without the public IP. Distinct from the network mode
(`--network tailscale`) that selects which plane the CLI uses.

## Backplane

**Durable Object** - the Cloudflare Worker primitive that holds Crabbox
fleet state. Crabbox uses one fleet Durable Object so all scheduling
decisions are serialized.

**Alarm** - the Durable Object scheduling primitive that fires on a future
timestamp. Crabbox uses alarms for idle-timeout sweeps and TTL cleanup.

**Portal** - the server-rendered web UI hosted by the same Worker. Pages
under `/portal/...`. See [Browser portal](features/portal.md).

**Bridge** - a portal endpoint that proxies traffic to a loopback service
on the lease (VNC, code-server). Bridges authenticate against the portal
session, then talk to the lease over the internal SSH plane.

## Identity

**Owner** - the email address that owns a lease. Resolved from the signed
GitHub login token, `CRABBOX_OWNER`, Git env, or `git config user.email`.

**Org** - the GitHub-style organization namespace for a lease. Resolved
from the signed token or `CRABBOX_ORG`. Used for usage scoping and
multi-tenant cost caps.

**Allowed org** - the GitHub org membership the broker requires before
issuing a signed login token. Configured per Cloudflare Worker.

**Admin token** - the separately scoped token required for `/v1/pool`,
admin lease routes, and fleet-wide listing. Held more closely than the
shared automation token.

**Cloudflare Access** - optional protection layer in front of the Worker.
When configured, the Worker only trusts the `CF-Access-Jwt-Assertion`
header (verified upstream); raw identity headers from the client are
ignored.

## Storage

**State directory** - where the CLI keeps local state (claims, per-lease
keys, known_hosts). Defaults to `$XDG_STATE_HOME/crabbox`, falling back to
the platform-specific user config directory.

**Claim** - a JSON file under the state directory binding a lease to a
repo. Required for `crabbox run --id` to resolve slugs and to refuse
cross-repo reuse without `--reclaim`.

**Workdir** / **work root** - the directory on the runner where Crabbox
syncs the repo. Default `/work/crabbox` on Linux; provider-specific on
Windows and macOS.

## Documentation

**Source map** - the doc page that points each user-facing behavior at the
implementation file behind it. Updated when behavior changes. See
[Source map](source-map.md).

**Feature page** - a doc under `docs/features/<name>.md` describing what
Crabbox does in one capability area. Owns the conceptual story; commands
and providers cross-link from here.

**Command page** - a doc under `docs/commands/<name>.md` describing the
flags, behavior, and exit codes of one CLI command. One per top-level
command, kept in sync with `--help` by `scripts/check-command-docs.mjs`.

**Provider page** - a doc under `docs/providers/<name>.md` describing one
provider's targets, config keys, env vars, sync behavior, and expected
failures.

Related docs:

- [How Crabbox Works](how-it-works.md)
- [Architecture](architecture.md)
- [CLI](cli.md)
- [Configuration](features/configuration.md)
- [Provider backends](provider-backends.md)
