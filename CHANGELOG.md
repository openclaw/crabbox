# Changelog

## 0.2.0 - Unreleased

### Added

- Native OpenClaw plugin package exposing Crabbox run, warmup, status, list, and stop tools.
- Added GitHub browser login for `crabbox login`, including signed user tokens, polling-based CLI completion, `--no-browser`, and JSON output support.
- Added coordinator OAuth routes for GitHub login: `/v1/auth/github/start`, `/v1/auth/github/callback`, and `/v1/auth/github/poll`.
- Added signed non-admin user-token auth in the Worker while keeping the shared operator token for admin routes.
- Added GitHub org membership enforcement before minting browser-login tokens.
- Added the canonical coordinator endpoint configured for OAuth callback generation.
- Added Blacksmith Testbox workflow flags for `crabbox warmup` and `crabbox run`, enabling one-command Testbox runs without repo YAML or environment variables.

### Changed

- Updated CLI defaults, docs, examples, and auth guidance to prefer `https://crabbox.openclaw.ai`.
- Clarified that Cloudflare Access OAuth and Crabbox CLI OAuth are separate GitHub OAuth apps with separate callback URLs.
- Scoped normal GitHub-login users to their own leases, run history, logs, and usage; shared-token admin auth remains required for pool and fleet-wide operator views.
- AWS coordinator-created security groups now allow SSH only from configured CIDRs or the request source IP instead of adding world-open SSH ingress.

### Fixed

- Cleaned up Blacksmith Testbox local lease claims and per-lease SSH keys after failed warmups, explicit stops, and one-shot runs.
- Restricted Worker admin routes to shared-token admin auth so GitHub browser-login users cannot call admin endpoints.
- Fixed `whoami` reporting for GitHub browser-login tokens.
- Fixed exact `cbx_...` lookups bypassing owner-scoped slug authorization checks.
- Added cleanup and a pending-login cap for unauthenticated GitHub OAuth login starts.

## 0.1.0 - 2026-05-01

Crabbox 0.1.0 is the first public release: a Go CLI, Cloudflare Worker coordinator, and OpenClaw plugin for leasing fast remote Linux machines, syncing dirty worktrees, running commands, and releasing or reusing warm boxes safely.

### Highlights

- Lease remote Linux test boxes from the CLI, sync the current checkout, run a command over SSH, stream output locally, and return the remote exit code.
- Use stable canonical lease IDs such as `cbx_...` for APIs, scripts, paths, SSH keys, provider labels, and compatibility.
- Use friendly crustacean slugs such as `blue-lobster`, `swift-hermit`, and `amber-krill` anywhere a lease ID is accepted.
- Keep warm boxes ergonomic without runaway cost: kept leases auto-release after an idle timeout, defaulting to `30m`, while `--ttl` remains a maximum wall-clock cap.
- Hydrate a leased box through a project-owned GitHub Actions workflow so repositories define their own runtimes, services, secrets, caches, and readiness.
- Keep runner bootstrap intentionally tiny: SSH, Git, rsync, curl, jq, `/work/crabbox`, and cache directories only. Go, Node, pnpm, Docker, databases, and services belong to the repo setup layer.
- Drive Crabbox from OpenClaw through native plugin tools for run, warmup, status, list, and stop.
- Install via Homebrew with `brew install openclaw/tap/crabbox`, or download GoReleaser archives for macOS, Linux, and Windows.

### CLI

- Added `crabbox run` for one-shot remote command execution with automatic acquire, sync, heartbeat, command streaming, result collection, and release.
- Added `crabbox warmup` for reusable kept leases.
- Added `crabbox status`, `inspect`, `list`, `ssh`, `stop`, and compatibility aliases `release`, `pool list`, and `machine cleanup`.
- Added `crabbox cleanup` for direct-provider cleanup of expired machines.
- Added `crabbox init` to generate `.crabbox.yaml`, `.github/workflows/crabbox.yml`, and `.agents/skills/crabbox/SKILL.md`.
- Added `crabbox doctor`, `config`, `login`, `logout`, and `whoami` for local setup, broker auth, and identity checks.
- Added `crabbox admin leases`, `admin release`, and `admin delete` for trusted operator control of coordinator leases.
- Added `crabbox usage` for estimated runtime and cost reporting by user, org, fleet, or JSON output.
- Added `crabbox history` and `logs` for coordinator-recorded runs and retained log tails.
- Added `crabbox results` plus `run --junit` for JUnit summaries.
- Added `crabbox cache stats`, `cache warm`, and `cache purge`.
- Added `crabbox sync-plan` to inspect sync candidates, largest files, and largest directories without leasing a machine.
- Added `--json` output on inspection/status/history-style commands where machines or runs need scriptable output.

### Leases

- Added canonical immutable lease IDs with per-lease SSH keys under the Crabbox config directory.
- Added deterministic crustacean-style slug generation with collision suffixes when needed.
- Added slug-aware lookup for active leases while preserving exact `cbx_...` lookup precedence.
- Added provider-visible names and runner labels based on slugs while retaining canonical lease labels for cleanup.
- Added owner-scoped slug allocation in the coordinator and collision-safe slug allocation in direct-provider mode.
- Added `lastTouchedAt`, `idleTimeoutSeconds`, and recomputed `expiresAt` metadata.
- Added heartbeat/touch behavior for active operations, including `run`, `ssh`, cache commands, Actions hydration, and `status --wait`.
- Kept plain `status` read-only so status polling does not extend a lease forever.
- Added local claim files under the Crabbox state directory so reused leases stay associated with the repository that acquired them.
- Added `--reclaim` for intentionally moving a local lease claim between repositories.

### Coordinator

- Added a Cloudflare Worker API backed by a Fleet Durable Object for serialized lease state.
- Added brokered Hetzner and AWS provisioning so normal clients do not need provider API credentials.
- Added Durable Object alarms for lease expiry and cleanup.
- Added bearer-token coordinator auth for automation and local users.
- Added create, get, heartbeat/touch, release, admin lease, usage, run history, run log, and health endpoints.
- Added coordinator-owned slug allocation, idle expiry math, TTL caps, and provider metadata storage.
- Added cost guardrails for active leases and monthly reserved spend.
- Added provider-backed pricing from AWS Spot price history and Hetzner server-type prices, with static fallback rates.
- Added bounded HTTP dial/TLS timeouts and local `curl` fallback for coordinator transport failures.

### Providers

- Added Hetzner provisioning with SSH key import/reuse, class fallback, labels, server deletion, and direct debug mode.
- Added AWS EC2 Spot provisioning with signed EC2 Query API calls in the Worker, SSH key-pair import/reuse, security-group setup, Spot instance launch, tag propagation, and direct debug mode.
- Added AWS class fallback across broad C/M/R instance families.
- Added AWS direct-mode Spot placement score support across configured regions.
- Added provider labels/tags for canonical lease ID, slug, state, keep flag, created/touched/expiry timestamps, idle timeout, TTL, class, profile, and provider key.
- Added Hetzner-safe label encoding using Unix seconds and compact duration seconds.
- Added per-lease provider SSH key/key-pair cleanup when machines are deleted.

### Sync And Execution

- Added Git-backed sync manifests so Crabbox transfers tracked files plus nonignored untracked files instead of the full local tree.
- Added default sync excludes for `.git`, dependency folders, build caches, and other local-only directories.
- Added rsync checksum/delete options, sync timeouts, quiet-rsync heartbeats, and no-change fingerprint skips.
- Added sync preflight estimates and large-sync guardrails for file count and byte size.
- Added remote sanity checks for mass tracked deletions.
- Added remote Git seeding and shallow base-ref hydration for changed-test workflows.
- Stored sync metadata under `.git/crabbox` when the remote directory is a Git worktree, keeping the working tree clean.
- Added remote workdir creation for `--no-sync` runs.
- Added concise sync and command timing summaries for warmup, run, and Actions hydration.
- Added per-lease `known_hosts` files to avoid host-key conflicts when cloud providers reuse ephemeral IPs.

### GitHub Actions

- Added `crabbox actions register` to register leased machines as ephemeral GitHub Actions runners.
- Added `crabbox actions dispatch` to dispatch repository workflows.
- Added `crabbox actions hydrate` to register, dispatch, wait for readiness, and capture the hydrated workspace.
- Added workflow-dispatch input inspection so Crabbox skips optional inputs that older workflow refs do not declare.
- Added hydrated workspace detection so later `crabbox run --id <slug>` syncs into `$GITHUB_WORKSPACE`.
- Added non-secret environment handoff from the hydration workflow to later Crabbox commands.
- Added stop-marker writing so `crabbox stop` can ask the waiting Actions job to exit cleanly.
- Runner labels include `crabbox`, canonical lease labels, readable slug labels, and profile/class labels.

### OpenClaw Plugin

- Added a native OpenClaw plugin package at the repository root.
- Added `crabbox_run`, `crabbox_warmup`, `crabbox_status`, `crabbox_list`, and `crabbox_stop` tools.
- Added plugin tests that verify command construction and disabled-tool behavior.

### Results, Cache, And History

- Added JUnit XML parsing and summaries for remote test result files.
- Added stored result summaries in coordinator run history.
- Added bounded run-log tails so history remains useful without storing unbounded output.
- Added cache stats, warm, and purge helpers for pnpm, npm, Docker, and Git cache directories.
- Cache commands honor configured cache-kind toggles.

### Configuration And Docs

- Added YAML config loading from user config plus repo-local `crabbox.yaml` or `.crabbox.yaml`.
- Added environment overrides for coordinator, provider, class, server type, AWS, Hetzner, lease durations, sync behavior, Actions, results, cache, and env allowlists.
- Added scoped `lease.ttl` and `lease.idleTimeout` config.
- Removed pre-release JSON config compatibility before shipping.
- Added workflow-first top-level help with common flows, grouped commands, config pointers, environment variables, and aliases.
- Added command documentation under `docs/commands/`.
- Added feature docs for coordinator, providers, sync, lifecycle cleanup, Actions hydration, cache, test results, SSH keys, cost usage, auth/admin, and runner bootstrap.
- Added architecture, how-it-works, operations, performance, infrastructure, troubleshooting, security, CLI, orchestrator, and MVP docs.
- Added a dependency-free GitHub Pages docs builder and Pages deployment workflow.

### Release And CI

- Added GoReleaser configuration for macOS, Linux, and Windows archives.
- Added Homebrew tap publishing configuration for `openclaw/homebrew-tap`.
- Added release workflow hardening that skips Homebrew tap publication when the tap token is missing or invalid instead of failing after publishing release assets.
- Added CI for Go formatting, `go vet`, race tests, build, Worker formatting/lint/typecheck/tests/build, and snapshot release checks.
- Added strict local Go toolchain selection with `toolchain go1.26.2`, `GOTOOLCHAIN=local` in CI, and readonly trimmed builds.
- Added a Go core coverage gate enforcing at least `85%`; current coverage is above that threshold.
- Updated Worker dependencies to current Cloudflare Workers types, Wrangler, and TypeScript.
- Updated GitHub Pages actions to current major versions.

### Fixed

- Touch-only coordinator heartbeats no longer overwrite an existing lease idle timeout unless explicitly requested.
- Direct-provider slugs are collision-checked against active machines before provisioning.
- Direct-provider expiry is capped by the shorter of idle timeout and TTL.
- Direct-provider reuse refreshes `last_touched_at`, `expires_at`, and idle timeout labels.
- Slug lookup no longer lets malformed noncanonical `lease` labels shadow real slug labels.
- Direct Hetzner labels no longer contain invalid timestamp or duration characters.
- Coordinator slug and idle metadata are stored and returned through public lease routes.
- `crabbox-ready` now waits for a Crabbox bootstrap marker and writable work root so base-image tools cannot make machines look ready too early.
- Config-writing commands honor `CRABBOX_CONFIG`, keeping isolated login/logout tests out of the normal user config.
- Boolean flags for `logs` and admin lease actions work after positional IDs, such as `crabbox logs run_... --json`.
- `actions hydrate` retries without optional `crabbox_job` when an older workflow ref rejects the input.
- `cache warm` uses the hydrated GitHub Actions workspace and env handoff when a lease was prepared by `actions hydrate`.
- `doctor` accepts per-lease SSH keys as the default posture and validates explicit `CRABBOX_SSH_KEY` only when set.
- Local per-lease SSH keys move with coordinator-renamed lease IDs.
- Stored test-result summaries are bounded before run history persistence.
