# Changelog

## 0.5.0 - Unreleased

### Added

- Added `--desktop`, `--browser`, and `crabbox vnc` for optional Linux UI/browser leases, including loopback-only VNC with per-lease passwords and headless browser support without a desktop.
- Added static macOS/Windows VNC endpoint discovery, including SSH-tunneled loopback VNC and trusted static direct VNC on `host:5900`.
- Added `crabbox vnc --open` to start the SSH tunnel and launch the local VNC client for managed desktop leases.
- Added a minimal XFCE desktop profile with panel/window manager for managed VNC leases.
- Clarified static macOS/Windows VNC as existing-host access, not Crabbox-created boxes, so `--open` no longer launches an OS credential prompt unless `--host-managed` is passed.

### Fixed

- Quoted `crabbox vnc` tunnel key paths so macOS `Application Support` lease keys can be pasted directly into a shell.
- Fixed native Windows `--shell` runs so multi-statement PowerShell scripts keep their quotes instead of being re-parsed by a nested PowerShell process.
- Skipped Linux-only GitHub Actions hydration stop markers on native Windows static targets.

## 0.4.0 - 2026-05-03

### Added

- Added static SSH macOS and Windows targets with `--target macos|windows`, `--windows-mode normal|wsl2`, and config/env support for reusable hosts.

### Changed

- Brokered Hetzner and AWS leases now reject non-Linux targets clearly; use `provider: ssh` for macOS or Windows hosts.

### Fixed

- Made Blacksmith live smoke explicit opt-in so the default live smoke works in repositories without a Testbox workflow.

## 0.3.1 - 2026-05-03

### Added

- Added `actions.fields` config support so repository-specific workflow inputs are sent on every Actions hydration, with CLI `-f key=value` overrides. Thanks @vincentkoc.
- Added a command-doc drift check to `npm run docs:check` so every top-level CLI command has a matching command page and index entry. Thanks @stainlu.

### Fixed

- Deferred run-history creation against legacy coordinators until a lease is known, avoiding noisy `invalid_lease_id` failures before command execution. Thanks @vincentkoc.
- Suppressed repeated run-event append warnings when a legacy coordinator does not support the newer run-event path. Thanks @vincentkoc.
- Fixed recorded run logs so long noisy commands are stored in bounded chunks instead of losing the failure evidence between the first output events and the final tail.
- Forced SSH to use Crabbox's per-lease identity file so local SSH-agent keys cannot exhaust server auth attempts before the runner key is tried.

## 0.3.0 - 2026-05-02

Crabbox 0.3.0 makes brokered runs much easier to observe and debug, adds
trusted AWS image lifecycle commands, improves AWS and Blacksmith reliability,
and tightens coordinator auth boundaries.

### Added

- Added early durable run session handles and append-only run events, plus `crabbox events <run-id>` for inspecting the coordinator event log.
- Added `crabbox attach <run-id>` for following recorded events from active runs, plus `--after` and `--limit` pagination for `crabbox events`. Thanks @stainlu.
- Added `--timing-json` for `warmup`, `actions hydrate`, and `run` so provider comparisons can read stable sync, command, total, exit-code, and Actions run timing from one JSON record.
- Added `--market spot|on-demand` to `warmup` and `run` so AWS capacity market choice no longer requires environment-only overrides.
- Added `crabbox image create --id <cbx_id> --name <ami-name> [--wait]` for trusted operators to create AWS AMIs from active brokered AWS leases.
- Added `crabbox image promote <ami-id>` for trusted operators to promote an available AMI as the coordinator default for future brokered AWS leases.
- Added JSON output and wait polling for image creation, including `--wait-timeout` and `--no-reboot` controls.
- Added best-effort AWS vCPU quota preflight for brokered launch fallback, with concise quota-code attempt metadata when a requested instance type cannot fit the applied quota.
- Added Blacksmith Testbox timing JSON output that reports delegated sync in the same schema as AWS and Hetzner runs.
- Added coordinator-orphan hints to human `crabbox list` output when provider machines carry no active coordinator lease.
- Added the Access-protected coordinator route `https://crabbox-access.openclaw.ai` for service-token proof and hardened automation.
- Added Cloudflare Access service-token headers for coordinator CLI requests. Thanks @stainlu.
- Added optional GitHub team allowlisting for browser-login tokens with `CRABBOX_GITHUB_ALLOWED_TEAMS`. Thanks @stainlu.
- Added separate coordinator admin-token auth so shared operator tokens no longer grant admin routes.
- Added Cloudflare Access JWT verification before Access identity can affect bearer-token ownership.
- Added coordinator image routes for admin-token callers: `POST /v1/images`, `GET /v1/images/{ami-id}`, and `POST /v1/images/{ami-id}/promote`.
- Added AWS provider support for `CreateImage` and `DescribeImages`, with Crabbox-owned AMI tags.
- Added `docs/commands/image.md` and linked the image command from the CLI docs, command index, docs site, and source map.
- Added `npm run docs:check` with internal Markdown link validation plus docs-site generation, and wired it into CI.
- Added `scripts/live-smoke.sh` for opt-in AWS, Hetzner, and Blacksmith Testbox live smoke coverage from a real repository checkout.
- Added `scripts/live-auth-smoke.sh` for opt-in live proof that shared tokens cannot call admin routes, admin tokens can, Access edge auth works, and raw Access identity headers are ignored.
- Added `scripts/deploy-worker-smoke.sh` to run the Worker gate, deploy the coordinator, verify public health routes, and optionally include a short AWS lease smoke.

### Changed

- Hydrated runs now skip the expensive Git base-ref hydration fetch when the remote base is already current enough for the local base SHA.
- Brokered AWS class requests now fall back through provider candidates, account-policy launch rejections, and a small burstable fallback instead of failing on the first Free Tier-ineligible high-core type.
- Brokered AWS fallback now skips known quota-impossible candidates before calling `RunInstances`, while preserving explicit `--type` failure semantics.
- Brokered lease records now keep the requested AWS instance type plus concise provisioning-attempt metadata when fallback chooses a different type.
- Coordinator run history now records the resolved lease provider/class/type when a lease exists, avoiding stale requested-type entries after fallback.
- Brokered AWS lease creation now uses the promoted AWS image when no explicit `awsAMI` or `CRABBOX_AWS_AMI` override is supplied.
- Moved the deployed coordinator route to the OpenClaw Cloudflare account at `https://crabbox.openclaw.ai` and scoped default broker org/auth settings to `openclaw`.
- User config writes now force `0600` permissions, and `crabbox doctor` reports overly broad config permissions.
- Image route validation now rejects noncanonical lease IDs, invalid AMI IDs, invalid AMI names, non-AWS leases, and promotion attempts before an image reaches `available`.

### Fixed

- Recorded durable `run.failed` events reliably for coordinator-backed pre-command failures such as lease claim, bootstrap, sync, and remote workdir errors.
- Fixed retained run-log tails under concurrent stdout/stderr writes so `crabbox logs` does not drop lines while run events are being recorded.
- Included the GitHub Actions hydration run URL in `crabbox run --timing-json` output when an Actions-hydrated workspace marker carries a run ID.
- Preserved explicit AWS `--type` requests as exact instance-type requests; Crabbox now fails clearly instead of silently falling back when the user asked for a specific type.
- Fixed AWS On-Demand launches by omitting Spot request tag specifications when no Spot request is created.
- Fixed Blacksmith Testbox JSON list output so the CLI returns an empty array when Blacksmith reports no active testboxes.
- Fixed brokered AWS security-group creation by sending EC2's required `GroupDescription` parameter, restoring first-run AWS provisioning in fresh accounts.
- Fixed coordinator warmup waits to keep touching the lease during slow bootstrap so short idle timeouts do not release a box while the foreground CLI is still waiting.
- Fixed SSH known-host handling for macOS config paths containing spaces, restoring per-lease known-host isolation under `Library/Application Support`.
- Scoped SSH ControlMaster sockets by per-lease key path so fast IP reuse across ephemeral machines cannot inherit a stale control connection.
- Fixed `crabbox list --provider blacksmith-testbox --json` to return parsed JSON instead of rejecting the shared `--json` flag.
- Prevented caller-supplied Access identity headers from overriding signed GitHub user token identity. Thanks @stainlu.
- Canceled SSH bootstrap waits when the coordinator lease disappears or becomes inactive, and made wait progress include elapsed and remaining time.
- Warned before running JavaScript package-manager commands on an unhydrated raw box when the repo declares an Actions hydration workflow.
- Fixed the generated docs-site mobile menu icon so the hamburger bars remain visible on narrow iOS/Safari viewports.
- Fixed responsive padding on the generated docs-site frontpage body content.
- Documented self-hosted GitHub OAuth setup so external coordinator deployments can avoid `Invalid redirect_uri` login failures.

## 0.2.0 - 2026-05-01

Crabbox 0.2.0 hardens the brokered runner path after real AWS and Blacksmith Testbox use: browser login is safer, AWS SSH ingress is no longer world-open by default, SSH readiness waits for the Crabbox bootstrap marker, and fallback SSH ports are configurable instead of being hidden port-22 magic.

### Added

- Added GitHub browser login for `crabbox login`, including signed user tokens, polling-based CLI completion, `--no-browser`, and JSON output support.
- Added coordinator OAuth routes for GitHub login: `/v1/auth/github/start`, `/v1/auth/github/callback`, and `/v1/auth/github/poll`.
- Added signed non-admin user-token auth in the Worker while keeping the shared operator token for admin routes.
- Added GitHub org membership enforcement before minting browser-login tokens.
- Added the canonical coordinator endpoint configured for OAuth callback generation.
- Added Blacksmith Testbox workflow flags for `crabbox warmup` and `crabbox run`, enabling one-command Testbox runs without repo YAML or environment variables.
- Added configurable SSH fallback ports via `ssh.fallbackPorts` and `CRABBOX_SSH_FALLBACK_PORTS`.

### Changed

- Updated CLI defaults, docs, examples, and auth guidance to prefer `https://crabbox.openclaw.ai`.
- Clarified that Cloudflare Access OAuth and Crabbox CLI OAuth are separate GitHub OAuth apps with separate callback URLs.
- Scoped normal GitHub-login users to their own leases, run history, logs, and usage; shared-token admin auth remains required for pool and fleet-wide operator views.
- AWS coordinator-created security groups now allow SSH only from configured CIDRs, the CLI-detected outbound IPv4 CIDR, or the request source IP instead of adding world-open SSH ingress.
- Direct AWS security groups now honor the configured AWS SSH source CIDRs when creating managed SSH ingress.
- Direct and brokered AWS now open the same configured SSH port candidates that the CLI will try.

### Fixed

- Cleaned up Blacksmith Testbox local lease claims and per-lease SSH keys after failed warmups, explicit stops, and one-shot runs.
- Fixed `status` and `inspect` readiness reporting so active leases with a host are not marked ready until SSH and `crabbox-ready` actually respond.
- Fixed remote sync sanity failures to include the remote deletion count and sample paths instead of hiding the useful stderr behind `exit status 66`.
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
