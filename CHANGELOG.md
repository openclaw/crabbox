# Changelog

## Unreleased

### Added

- Azure Sandbox: optionally restore fresh independently owned leases from pinned clean snapshots, verify source and lifecycle settings, and preserve safe recovery and exact-owned cleanup. [PR 2850](https://github.com/openclaw/crabbox/pull/2850). Thanks @galiniliev.
- Serve: report per-machine lease counts, runtime, and allocated CPU-hours; machine listings and the portal use durable completion rollups starting at deployment without a history scan or backfill, while usage retains full recorded history. [PR 2864](https://github.com/openclaw/crabbox/pull/2864).
- Coordinator: allow an explicit legacy AWS account attestation to reconcile pre-cutover retained records without enabling resource deletion. [PR 2861](https://github.com/openclaw/crabbox/pull/2861).

### Fixed

- Coordinator: reconcile retained AWS records with filtered batches of at most 200 unique instance IDs, avoiding missing-instance fallback storms and isolating transient chunk failures while preserving successful observations and bounded backoff. [PR 2866](https://github.com/openclaw/crabbox/pull/2866).
- Serve: reuse invocation-owned relay SSH connections across readiness, sync, commands, and cleanup while preserving pinned host keys and independent workspace ownership. [PR 2865](https://github.com/openclaw/crabbox/pull/2865).
- Sync: keep finalization fetches blob-filtered when completing shallow Git history, and explain live-child owner release refusals with safe lease recovery guidance. [PR 2854](https://github.com/openclaw/crabbox/pull/2854).

## 0.78.1 - 2026-10-15

### Highlights

- **Put busy-but-usable machines back in the pool.** Heavily loaded Macs now report real availability instead of staying `unknown` and never being offered for new leases. [PR 2845](https://github.com/openclaw/crabbox/pull/2845).
- **Capture AWS checkpoints before returning success.** Direct AWS `checkpoint create --wait=false` now establishes every backing snapshot before returning, so subsequent work cannot race an unstarted capture. [PR 2834](https://github.com/openclaw/crabbox/pull/2834).

### Fixed

- Azure Sandbox: report original allocation expiry without discarding cleanup ownership, and expose safe operation-specific transport failure categories without leaking private causes. [PR 2849](https://github.com/openclaw/crabbox/pull/2849). Thanks @galiniliev.
- Coordinator Node image: re-pin the AWS RDS global CA bundle after AWS added the `sa-west-1` root CAs (114 Amazon RDS certificates, all self-signed Amazon roots). [PR 2853](https://github.com/openclaw/crabbox/pull/2853).
- Checkpoints: establish every direct AWS backing snapshot before non-waiting success, expose provider-owned `captureEstablished` attestations while preserving other providers' success semantics, and reduce native availability and AWS public-IP polling slack. [PR 2834](https://github.com/openclaw/crabbox/pull/2834).
- Serve: preserve CPU and user-idle readings on heavily loaded Macs with independent sampling deadlines and interval-idle CPU measurement, accept CPU totals above 100%, fall back to raw host CPU when container stats fail, and add opt-in per-signal diagnostics. [PR 2845](https://github.com/openclaw/crabbox/pull/2845).

## 0.78.0 - 2026-10-14

### Highlights

- **Donate spare capacity without slowing down your own work.** `crabbox serve` offers new leases only when host CPU, memory, user activity, and power policies allow; running leases continue, and the CLI and portal explain why a machine is busy. [PR 2838](https://github.com/openclaw/crabbox/pull/2838).
- **Tell accepted release from confirmed cleanup.** Run history preserves retained or unconfirmed lease outcomes in the `cleanup` phase instead of claiming the machine has stopped. [PR 2819](https://github.com/openclaw/crabbox/pull/2819). Thanks @vincentkoc.
- **Get the configured runtimes and clearer Testbox failures.** Native commands use the selected Node and Go versions, readiness waits for successful setup, and settlement and cleanup failures retain safe, phase-specific diagnostics. [PR 2818](https://github.com/openclaw/crabbox/pull/2818), [PR 2831](https://github.com/openclaw/crabbox/pull/2831), [PR 2835](https://github.com/openclaw/crabbox/pull/2835), [PR 2837](https://github.com/openclaw/crabbox/pull/2837). Thanks @vincentkoc.
- **Verify release installs more reliably.** Native-host notarization checks, bounded lookup retries, and attributable diagnostics improve macOS verification; Homebrew verification accepts the current egress runtime packs. [PR 2829](https://github.com/openclaw/crabbox/pull/2829), [PR 2836](https://github.com/openclaw/crabbox/pull/2836), [PR 2842](https://github.com/openclaw/crabbox/pull/2842), [PR 2823](https://github.com/openclaw/crabbox/pull/2823), [PR 2828](https://github.com/openclaw/crabbox/pull/2828), [PR 2840](https://github.com/openclaw/crabbox/pull/2840). Thanks @vincentkoc.

### Upgrade notes

- Automation reading `lease.released` must use the `cleanup` phase and `leaseStopped=false` to distinguish an accepted release request from confirmed machine cleanup. [PR 2819](https://github.com/openclaw/crabbox/pull/2819). Thanks @vincentkoc.
- Serve availability gating is enabled by default. Set `--availability always` only when a donor should accept new leases regardless of host activity; laptop defaults also require ten minutes of user idle and AC power. [PR 2838](https://github.com/openclaw/crabbox/pull/2838).

### Changed

- Coordinator: remove per-owner lease lifetime overrides from the production configuration; every owner uses the global lease policy, and the deploy guard now protects the complete live override document generically. [PR 2830](https://github.com/openclaw/crabbox/pull/2830).

### Added

- Serve: offer machines only while host CPU, memory, user activity, and power policies allow, with availability reasons, portal measurements, and a read-only local probe; running leases continue. [PR 2838](https://github.com/openclaw/crabbox/pull/2838).

### Fixed

- Release verification: retry fully captured transient online notarization-daemon errors for up to four strict checks with bounded backoff, retaining every failure diagnostic and preserving rejection and interruption guards. [PR 2836](https://github.com/openclaw/crabbox/pull/2836), [PR 2842](https://github.com/openclaw/crabbox/pull/2842).
- Release verification: accept provenance schema 4 (egress runtime packs) in the installed-Homebrew verifier, checking it against the same exact filesystem-layout archive inventory as schema 3. [PR 2840](https://github.com/openclaw/crabbox/pull/2840).
- CI: Go CLI race shards keep the restored Go build cache, so the race build's cache trim no longer deletes the non-race packages the one-time CLI test build reuses; that build drops from about 135 s to about 30 s per shard. [PR 2833](https://github.com/openclaw/crabbox/pull/2833).
- Testbox: announce readiness only after setup succeeds, so failed preparation does not keep an unprepared runner in its idle loop. [PR 2831](https://github.com/openclaw/crabbox/pull/2831). Thanks @vincentkoc.
- CI: skip production coordinator deployment for changes limited to Worker tests while retaining runtime, configuration, and deployment-helper triggers. [PR 2839](https://github.com/openclaw/crabbox/pull/2839). Thanks @vincentkoc.
- CLI: distinguish accepted release from confirmed cleanup in run events, preserving retained or unconfirmed outcomes in the `cleanup` phase. [PR 2819](https://github.com/openclaw/crabbox/pull/2819). Thanks @vincentkoc.
- Release verification: check online notarization on each binary's native macOS host while retaining foreign-architecture static checks and a manual online opt-in. [PR 2829](https://github.com/openclaw/crabbox/pull/2829).
- Release verification: report bounded, attributable macOS notarization diagnostics without changing failed checks or executing rejected binaries. [PR 2823](https://github.com/openclaw/crabbox/pull/2823). Thanks @vincentkoc.
- CI: assign Go CLI race and coverage shards by test-name hash, with a checked-in weight list spreading known-slow tests, so adding a test no longer moves hundreds of existing tests between shards and pushes one past its 10-minute race timeout. [PR 2826](https://github.com/openclaw/crabbox/pull/2826).
- CI: Go CLI race shards stream test events like the coverage shards, print only failed or unfinished test output, and end with listed and unlisted slow-test timings; the slow-test weights now come from those linux race timings. [PR 2832](https://github.com/openclaw/crabbox/pull/2832).
- Release verification: retain attributable notarization errors after unrelated log floods, while bounding streamed records and sanitized diagnostics. [PR 2828](https://github.com/openclaw/crabbox/pull/2828). Thanks @vincentkoc.
- Blacksmith Testbox: expose the configured Node and Go runtimes to native commands instead of using the runner image defaults. [PR 2818](https://github.com/openclaw/crabbox/pull/2818). Thanks @vincentkoc.
- Blacksmith Testbox: report typed command causes and remaining cleanup budget when GitHub settlement fails, while keeping private error details out of CLI output. [PR 2835](https://github.com/openclaw/crabbox/pull/2835). Thanks @vincentkoc.
- Blacksmith Testbox: distinguish native status or stop failures from later cleanup phases, preserving command timing and typed causes with display-safe error diagnostics. Native stop output remains unchanged. [PR 2837](https://github.com/openclaw/crabbox/pull/2837). Thanks @vincentkoc.

## 0.77.0 - 2026-10-13

### Highlights

- **Stop leases without waiting on cloud cleanup.** `crabbox stop --wait=false` hands confirmed deletion to the coordinator and keeps local recovery state until cleanup completes; plain `stop` still waits. [PR 2809](https://github.com/openclaw/crabbox/pull/2809).
- **Scripted runs start faster.** No-sync runs batch SSH workspace preparation and script upload and overlap telemetry and event delivery, with idempotent retries after transient transport failures. [PR 2809](https://github.com/openclaw/crabbox/pull/2809).
- **Egress works from installed CLIs.** Setup wrappers receive verified architecture-specific runtime-pack helpers, public destinations are opt-in, and cleanup errors fail the run without hiding the workload outcome. [PR 2812](https://github.com/openclaw/crabbox/pull/2812).
- **Lifetime cleanup and Blacksmith failures are more trustworthy.** Ordinary TTL deletion takes precedence over maximum-lifetime retention, and Blacksmith Testbox failures keep their native diagnostics through cleanup and reporting. [PR 2795](https://github.com/openclaw/crabbox/pull/2795), [PR 2820](https://github.com/openclaw/crabbox/pull/2820). Thanks @vincentkoc.

### Changed

- CLI: add `stop --wait=false` for coordinator-owned queued cleanup while preserving pending local recovery state; ordinary stop still waits for confirmed deletion. [PR 2809](https://github.com/openclaw/crabbox/pull/2809).
- CLI: batch no-sync SSH workspace preparation and script upload, overlap independent telemetry and event delivery, and preserve retained scripts and terminal run records. [PR 2809](https://github.com/openclaw/crabbox/pull/2809).
- Coordinator: deployments refuse to overwrite live lifetime-policy overrides that are missing from source. [PR 2816](https://github.com/openclaw/crabbox/pull/2816). Thanks @vincentkoc.

### Added

- Egress: provision verified architecture-specific runtime-pack helpers for installed setup wrappers, add opt-in public destinations, and fail runs on redacted environment cleanup errors while preserving primary workload and owner outcomes. [PR 2812](https://github.com/openclaw/crabbox/pull/2812).

### Fixed

- Coordinator deployment: the lease-policy preservation guard accepts authorized maximum-TTL changes without discarding other live overrides. [PR 2822](https://github.com/openclaw/crabbox/pull/2822).
- Blacksmith Testbox: preserve native failures and safe diagnostics through cleanup and reporting, finish independent proof and failure captures, and keep lease recovery handles accurate. [PR 2820](https://github.com/openclaw/crabbox/pull/2820). Thanks @vincentkoc.
- Coordinator: preserve ordinary TTL deletion ahead of maximum-lifetime retention, reject conflicting delete requests during lifetime work, and reconcile shared AWS ingress after lifetime deletion. [PR 2795](https://github.com/openclaw/crabbox/pull/2795). Thanks @vincentkoc.
- CLI: retain idempotent SSH retries for batched workspace preparation and script upload after transient transport failures. [PR 2809](https://github.com/openclaw/crabbox/pull/2809).

## 0.76.0 - 2026-10-13

### Highlights

- **Boxes now die on time by default.** Every lease defaults to a 30-minute lifetime and 30-minute idle timeout with a 4-hour maximum that also clamps running leases. Coordinator policies set defaults, maximums, and extension rules globally or per organization and owner; `crabbox extend` and in-run auto-extension are budget-checked. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- **Direct-provider machines reclaim themselves.** Shared-bootstrap Linux providers install an on-box expiry guard (AWS instances terminate on shutdown), expired `keep=true` machines are no longer exempt from cleanup, and `directLeases: deny` requires coordinator allocation on a machine. [PR 2806](https://github.com/openclaw/crabbox/pull/2806).
- **`crabbox list` answers in seconds.** Listings stream live leases first and show only live leases by default; `--retained` adds kept history, `--jsonl` streams machine-readable rows, and `--all` keeps orphaned provider inventory visible without a 1,000-row cap. [PR 2802](https://github.com/openclaw/crabbox/pull/2802).
- **The coordinator cleans up stale lease records itself.** Retained AWS records are reconciled against their exact instances in bounded, resumable passes, with progress in `crabbox admin retained-reconcile`. [PR 2803](https://github.com/openclaw/crabbox/pull/2803).
- **Serve machines are reachable from anywhere.** Agents advertise LAN and tailnet front doors with pinned-key, LAN-first selection, fall back to an end-to-end encrypted coordinator relay, and appear in the portal with live capacity. [PR 2794](https://github.com/openclaw/crabbox/pull/2794), [PR 2796](https://github.com/openclaw/crabbox/pull/2796), [PR 2797](https://github.com/openclaw/crabbox/pull/2797), [PR 2807](https://github.com/openclaw/crabbox/pull/2807), [PR 2811](https://github.com/openclaw/crabbox/pull/2811).

### Upgrade notes

- Leases default to a 30-minute lifetime and 30-minute idle timeout, and coordinators clamp any requested or existing lease to 4 hours unless a lifetime policy allows more; on upgrade, running leases older than the maximum expire at the next maintenance pass. `crabbox run` auto-extends while its command executes; request a longer `--ttl` or use `crabbox extend` for interactive work. Daytona and other creation-fixed providers cannot be extended after creation. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- `crabbox list` and `crabbox list --json` now return only active and provisioning leases; add `--retained` where scripts relied on kept terminal records. [PR 2802](https://github.com/openclaw/crabbox/pull/2802).
- `crabbox cleanup` for direct providers now deletes expired machines labeled `keep=true` and gives running or provisioning machines a 1-hour grace instead of 12 hours. [PR 2806](https://github.com/openclaw/crabbox/pull/2806).
- Lease extensions reserve their additional cost; an extension that would exceed a fleet, owner, or organization budget is refused with `lease_extension_budget_exceeded`. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).

### Changed

- Default lease lifetime to 30 minutes with a 4-hour maximum and coordinator global, organization, and owner lifetime policies that also clamp live leases. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- Direct lease allocations and explicit idle-timeout updates report policy clamps and persist the effective timeout consistently; coordinator heartbeats also report clamped idle overrides. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- Lease extensions atomically reserve their additional cost against fleet, owner, and organization budgets; running commands report when exhausted budgets prevent extension. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- Registered external leases retain their renewable heartbeat deadline when lifetime policy is tightened. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- CLI: `crabbox list` streams and shows live leases by default; `--retained` includes kept terminal leases. Add `--jsonl` streaming and uncapped `--all` lease lookups while preserving the buffered `--json` array. [PR 2802](https://github.com/openclaw/crabbox/pull/2802).
- Direct cleanup now expires `keep=true` leases and reduces running/provisioning expiry grace from 12 hours to 1 hour; unowned machines remain untouched. [PR 2806](https://github.com/openclaw/crabbox/pull/2806).
- Direct leases: preserve provider user-data limits with compressed Tencent Cloud bootstrap, retain renewed leases during cleanup, and normalize Sealos ownership labels. [PR 2806](https://github.com/openclaw/crabbox/pull/2806).
- Coordinator monthly reserved-USD budgets are raised to $100,000 fleet-wide, $50,000 per owner and $100,000 per org; reservations from finished leases had exhausted the per-owner cap at roughly a quarter of the estimated spend. [PR 2800](https://github.com/openclaw/crabbox/pull/2800).
- CI: run CLI coverage in three isolated shards, retaining atomic whole-suite coverage and the 90% core gate; report the slowest top-level tests without summing nested timings. [PR 2810](https://github.com/openclaw/crabbox/pull/2810). Thanks @vincentkoc.

### Added

- `crabbox extend` requests bounded lifetime extensions; running commands can auto-extend when policy and provider lifetime capabilities allow. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- Direct Linux guest expiry guards across shared-bootstrap providers, plus `directLeases: deny` and trusted provider exceptions to require coordinator allocation. [PR 2806](https://github.com/openclaw/crabbox/pull/2806).
- Coordinator: reconcile retained terminal AWS leases from the current index with scoped cleanup proof, bounded resumable passes, opt-in owned deletion, and `crabbox admin retained-reconcile` status. [PR 2803](https://github.com/openclaw/crabbox/pull/2803).
- Serve agents advertise LAN and tailnet front-door candidates, with pinned-key LAN-first client selection and per-lease route caching. [PR 2796](https://github.com/openclaw/crabbox/pull/2796).
- Serve leases can fall back to an owner-authorized, end-to-end encrypted coordinator relay when LAN and tailnet routes are unavailable. [PR 2797](https://github.com/openclaw/crabbox/pull/2797), [PR 2807](https://github.com/openclaw/crabbox/pull/2807).
- Portal: show owners their donated serve machines, online status, capacity, allocated leases, and runtime details. [PR 2794](https://github.com/openclaw/crabbox/pull/2794).
- Blacksmith Testbox: configure the native total readiness wait (queue and hydration) through `blacksmith.waitTimeout`, `CRABBOX_BLACKSMITH_WAIT_TIMEOUT`, or `--blacksmith-wait-timeout`, preserving the native 15-minute default when unset. [PR 2805](https://github.com/openclaw/crabbox/pull/2805). Thanks @vincentkoc.

### Fixed

- Direct lease extension refreshes file-backed guest expiry guards after saving provider labels; failed guest writes report both deadlines and can be retried without adding lifetime twice. [PR 2804](https://github.com/openclaw/crabbox/pull/2804).
- AWS: honor the requested region before coordinator and capacity fallbacks, using a regional promoted image or the normal stock image without redirecting to another region's promotion. [PR 2808](https://github.com/openclaw/crabbox/pull/2808).
- Serve: allow relay pairing and both SSH handshakes to complete during bootstrap and sync; add bounded, payload-free client and agent diagnostics with `CRABBOX_SERVE_RELAY_DEBUG=1`. [PR 2811](https://github.com/openclaw/crabbox/pull/2811).
- Blacksmith Testbox: cancel the exact owned GitHub workflow after native stop leaves it active, releasing concurrency slots without waiting for the SSH idle timer; retain recovery state until both sides confirm completion. [PR 2813](https://github.com/openclaw/crabbox/pull/2813). Thanks @vincentkoc.
- AWS deployment smoke: pin Linux/amd64 `t3.small` with class `tiny` and On-Demand capacity so inherited defaults cannot launch a larger instance. [PR 2814](https://github.com/openclaw/crabbox/pull/2814). Thanks @vincentkoc.
- Preserve worst-case `reservedUSD` in usage reports and expose enforced cost as `committedUSD`, with compatible CLI output and conservative reservations for retained legacy VMs and unresolved provisioning. [PR 2801](https://github.com/openclaw/crabbox/pull/2801).
- Settle terminal lease budget reservations at elapsed compute cost capped by the admission reservation, preserve observed runtime through late release and cleanup retries, and charge confirmed never-allocated leases zero compute runtime. [PR 2799](https://github.com/openclaw/crabbox/pull/2799).
- Coordinator deployments fail when credentials are missing or the public health endpoint does not confirm the active version and requested commit. [PR 2798](https://github.com/openclaw/crabbox/pull/2798). Thanks @vincentkoc.

## 0.75.0 - 2026-10-12

### Highlights

- **Turn spare machines into private container capacity.** `crabbox serve` offers owner-only leases through one authenticated SSH front door, with pinned end-to-end container SSH, warm package caches, and opt-in capacity fallback. [PR 2768](https://github.com/openclaw/crabbox/pull/2768), [PR 2770](https://github.com/openclaw/crabbox/pull/2770), [PR 2789](https://github.com/openclaw/crabbox/pull/2789).
- **Keep coordinator requests moving as lease history grows.** Separate current and maintenance indexes avoid repeated history scans; bounded reads reduce memory pressure, while structured 503 responses and CLI backoff recover transient overload and resets. [PR 2769](https://github.com/openclaw/crabbox/pull/2769), [PR 2774](https://github.com/openclaw/crabbox/pull/2774), [PR 2777](https://github.com/openclaw/crabbox/pull/2777), [PR 2782](https://github.com/openclaw/crabbox/pull/2782), [PR 2786](https://github.com/openclaw/crabbox/pull/2786), [PR 2788](https://github.com/openclaw/crabbox/pull/2788).
- **Set an optional safety net for retained AWS leases.** Default-off maximum-age policies expose stop and deletion deadlines, allow bounded extensions, and verify stop permissions before enforcement. [PR 2759](https://github.com/openclaw/crabbox/pull/2759), [PR 2771](https://github.com/openclaw/crabbox/pull/2771). Thanks @vincentkoc.
- **Scan and filter the lease portal more easily.** Clickable fleet readouts, provider activity, and compact state filters bring a refreshed layout to the home, admin, and detail pages. [PR 2773](https://github.com/openclaw/crabbox/pull/2773).
- **Sync native Jujutsu working copies directly.** Git-backed `jj` workspaces now sync their exact `@` tree, including working-copy edits and octopus merges, to ordinary SSH leases. [PR 2780](https://github.com/openclaw/crabbox/pull/2780), [Issue 1081](https://github.com/openclaw/crabbox/issues/1081). Thanks @atimmer.

### Upgrade notes

- Automation using `crabbox run` should allow up to 30 seconds for run admission when the coordinator is unavailable, increased from 10 seconds; shorter caller deadlines still win. [PR 2777](https://github.com/openclaw/crabbox/pull/2777).
- `crabbox list --all` now paginates active-lease lookups, so results can include active leases beyond the former single-page cap. Older coordinators retain the legacy fallback with a warning when its limit is reached. [PR 2788](https://github.com/openclaw/crabbox/pull/2788).

### Added

- Sync: support native Git-backed Jujutsu workspaces by syncing the exact `@` tree, including octopus merges and working-copy edits, with explicit origin-bookmark seeding and fail-closed revision checks. [PR 2780](https://github.com/openclaw/crabbox/pull/2780), [Issue 1081](https://github.com/openclaw/crabbox/issues/1081). Thanks @atimmer.
- Proxmox: add opt-in prepared XFCE/TigerVNC and browser templates with per-clone credentials, fail-closed readiness and capability-safe adapter replay. [PR 2781](https://github.com/openclaw/crabbox/pull/2781), [Issue 2549](https://github.com/openclaw/crabbox/issues/2549). Thanks @ahkohd.
- Docs: translate the landing page and core introductions into Simplified Chinese, Japanese, Korean, Spanish, German, French, and Brazilian Portuguese, with language navigation, English fallback, preserved examples, and source-hash staleness notices. [PR 2784](https://github.com/openclaw/crabbox/pull/2784), [Issue 1065](https://github.com/openclaw/crabbox/issues/1065). Thanks @alexandre-leng.
- Coordinator: add default-off maximum-age policies and explicit admin rollout canaries for retained AWS leases, with verified stop permissions, visible stop/deletion deadlines, bounded extensions, and root-disk cleanup. [PR 2759](https://github.com/openclaw/crabbox/pull/2759). Thanks @vincentkoc.
- Serve: run `crabbox serve` on trusted machines to donate spare CPU as owner-only container leases (`--provider serve`), with outbound coordinator polling, local deadline enforcement, a single authenticated SSH front door (`--ssh-port`) with pinned end-to-end container SSH, warm package caches, per-user service install, and opt-in capacity fallback. Coordinators enable it per owner with `CRABBOX_SERVE_ALLOWED_OWNERS` (off by default). [PR 2768](https://github.com/openclaw/crabbox/pull/2768), [PR 2770](https://github.com/openclaw/crabbox/pull/2770), [PR 2789](https://github.com/openclaw/crabbox/pull/2789).

### Changes

- Coordinator: enable Cloudflare Workers Logs with full head sampling so Worker requests, Durable Object alarms, and structured coordinator failure lines are retained for incident attribution, with query strings redacted from logged URLs. [PR 2783](https://github.com/openclaw/crabbox/pull/2783), [PR 2785](https://github.com/openclaw/crabbox/pull/2785).
- Portal: redesign the lease portal with clickable fleet readouts, a per-provider activity bar, a segmented state filter with compact filter chips, calmer text-only state labels, and a refreshed dark/light skin across admin and detail pages. [PR 2773](https://github.com/openclaw/crabbox/pull/2773).
- Portal: omit stale external runners from the home page while preserving runner list API and detail access. [PR 2775](https://github.com/openclaw/crabbox/pull/2775).
- Docs: consolidate provider selection, class catalogs, and lifecycle references; correct stale provider support, sizing, and transport guidance. [PR 2763](https://github.com/openclaw/crabbox/pull/2763).

### Fixes

- Coordinator/CLI: use independent current and maintenance indexes for lease listings and paginate active admin lookups, preserving filters, history fallback and cursor source checks. [PR 2788](https://github.com/openclaw/crabbox/pull/2788).
- AWS: resolve automatic runner images in each fallback region, recover missing promotions with stock images, and let exact-type Spot requests promptly use enabled On-Demand fallback. [PR 2787](https://github.com/openclaw/crabbox/pull/2787). Thanks @vincentkoc.
- Coordinator: index leases with pending maintenance so alarm reconciliation, discovery, provider access, slug lookup, and registration avoid scanning settled history, with bounded backfill and daily repair audits. [PR 2786](https://github.com/openclaw/crabbox/pull/2786).
- Coordinator/CLI: return structured retryable 503 responses with Retry-After for Durable Object overload/reset instead of Cloudflare 1101, and retry reads and run admission with bounded backoff. [PR 2777](https://github.com/openclaw/crabbox/pull/2777).
- Coordinator: stop alarm reconciliation from issuing a storage read per retained historical lease, and keep read-only lifetime readbacks out of the lifecycle queue, reducing request queueing and overload errors on long-lived fleets. [PR 2782](https://github.com/openclaw/crabbox/pull/2782).
- Coordinator: allow explicitly configured capacity admins to use their owner allowance when shared fleet or org caps are full, while preserving spending limits and ordinary admission. [PR 2778](https://github.com/openclaw/crabbox/pull/2778). Thanks @vincentkoc.
- Coordinator: preserve deployment-managed capacity-admin membership across deploys without replacing secret-backed members or raising ordinary owner limits. [PR 2776](https://github.com/openclaw/crabbox/pull/2776). Thanks @vincentkoc.
- Coordinator: reconcile org-wide external runner inventories across owners, deduplicate visible runners with two bounded streaming scans, hide observations older than two hours, and prune rows last seen more than seven days ago. [PR 2779](https://github.com/openclaw/crabbox/pull/2779).
- Coordinator: preserve leading zero bytes in generated Ed25519 SSH host keys so the public identity and injected private key remain a valid pair. [PR 2772](https://github.com/openclaw/crabbox/pull/2772). Thanks @vincentkoc.
- Bound coordinator storage scans for pools, image catalogs, checkpoints, and dynamic runner indexes to uncached pages. [PR 2774](https://github.com/openclaw/crabbox/pull/2774).
- Coordinator: prevent the portal from exhausting coordinator memory when many external runner records are stored by bounding reads and pruning old stale records. [PR 2769](https://github.com/openclaw/crabbox/pull/2769).
- Serve: stop spurious `serve_rate_limited` heartbeat errors by pacing each heartbeat at least `--poll-interval` after the previous response. [PR 2770](https://github.com/openclaw/crabbox/pull/2770).
- Serve: prepare bootstrap packages in the background without delaying restart heartbeats or reconciliation; prepared leases start in about a second instead of ~30s, recover automatically after shared-image pruning, and retain unique SSH host keys. [PR 2770](https://github.com/openclaw/crabbox/pull/2770).
- Coordinator: confirm lifetime-cap stops every 30 seconds for the first five minutes instead of waiting for the five-minute retry, and retire a lease's lifetime policy when an explicit stop deletes it; harden the lifetime canary live proof. [PR 2771](https://github.com/openclaw/crabbox/pull/2771).
- Sync: explain incomplete shallow sources and Git overlay conflicts during local Git seeding, with actionable configuration fixes. [PR 2662](https://github.com/openclaw/crabbox/pull/2662), related [Issue 2661](https://github.com/openclaw/crabbox/issues/2661). Thanks @shunkakinoki.
- CLI: resolve explicit provider, target, Windows mode, architecture, OS, class, and type flags before validating inherited configuration, preserving unrelated defaults and recorded-lease identity checks. [PR 2767](https://github.com/openclaw/crabbox/pull/2767).
- Jobs: apply a selected job's provider and platform selectors before validating inherited defaults, including dry runs and existing-lease reuse. [PR 2767](https://github.com/openclaw/crabbox/pull/2767).

## 0.74.0 - 2026-10-10

### Highlights

- **Recover GitHub Codespaces leases safely after interrupted requests.** Caller-supplied lease IDs now bind recovery to the exact account and creation intent, with receipts that prevent accidental recreation. [PR 2752](https://github.com/openclaw/crabbox/pull/2752), [Issue 2660](https://github.com/openclaw/crabbox/issues/2660). Thanks @jcxmt125.
- **Start exe.dev leases from a prepared VM.** Use `--exe-dev-from` to copy a ready workspace into a separately owned lease with fresh ownership tags and a persistent work root. [PR 2757](https://github.com/openclaw/crabbox/pull/2757), [PR 2754](https://github.com/openclaw/crabbox/pull/2754). Thanks @salmonumbrella.
- **See what AWS capacity checks actually know.** Doctor distinguishes sufficient quota from unchecked regional usage instead of reporting available capacity from the quota ceiling alone. [PR 2753](https://github.com/openclaw/crabbox/pull/2753). Thanks @vincentkoc.
- **Run more concurrent coordinator leases.** Fleet, org, and capacity-admin active-lease caps rise from 20 to 50; the ordinary owner cap and monthly budgets remain unchanged. [PR 2756](https://github.com/openclaw/crabbox/pull/2756).

### Upgrade notes

- Automation parsing AWS doctor capacity output should handle `capacity=unknown quota_limit=sufficient usage=unchecked region=...`: a sufficient quota no longer implies available regional capacity. [PR 2753](https://github.com/openclaw/crabbox/pull/2753). Thanks @vincentkoc.

### Added

- exe.dev: start separately claimed leases from prepared VMs with `--exe-dev-from`, fresh ownership tags, and persistent work roots while preserving existing claim paths. [PR 2757](https://github.com/openclaw/crabbox/pull/2757), [PR 2754](https://github.com/openclaw/crabbox/pull/2754). Thanks @salmonumbrella.
- GitHub Codespaces: support replay-safe caller-supplied lease IDs with account- and intent-bound recovery, exact-resource cleanup, and terminal receipts. [PR 2752](https://github.com/openclaw/crabbox/pull/2752), [Issue 2660](https://github.com/openclaw/crabbox/issues/2660). Thanks @jcxmt125.

### Changes

- Coordinator: raise fleet, org, and capacity-admin active-lease caps from 20 to 50, preserving the ordinary owner cap and monthly budgets. [PR 2756](https://github.com/openclaw/crabbox/pull/2756).

### Fixes

- AWS: stop reporting available capacity from quota ceilings alone; mark regional usage unchecked and clarify that class fallback still requires quota headroom. [PR 2753](https://github.com/openclaw/crabbox/pull/2753). Thanks @vincentkoc.
- DigitalOcean: prefer the first available region near San Francisco when no region is configured, preserve explicit regions, and reuse the recorded region on fixed-lease replay. [PR 2758](https://github.com/openclaw/crabbox/pull/2758).
- Azure: refuse retained network-companion cleanup when provider-key or fixed-attempt tags conflict with the original claim, even when immutable resource IDs still match. [PR 2751](https://github.com/openclaw/crabbox/pull/2751).

## 0.73.0 - 2026-10-08

### Highlights

- **Start AWS Windows leases on Server 2025.** The promoted eu-west-1 developer image and stock fallback now use Windows Server 2025; an explicit OS selector keeps Server 2022 available. [PR 2748](https://github.com/openclaw/crabbox/pull/2748), [PR 2734](https://github.com/openclaw/crabbox/pull/2734), [Issue 2717](https://github.com/openclaw/crabbox/issues/2717). Thanks @saftall.
- **Run shell workloads in Azure sandboxes and recover failed Azure VMs safely.** The new Azure Container Apps Sandboxes provider supports kept Linux workloads, uploads, idle resume, and verified cleanup. Azure VM leases retain cleanup ownership after VM loss, with `crabbox hold` for salvage. [PR 2738](https://github.com/openclaw/crabbox/pull/2738), [PR 2733](https://github.com/openclaw/crabbox/pull/2733). Thanks @galiniliev.
- **Keep Windows command arguments intact.** Native programs and uploaded PowerShell scripts receive embedded quotes, empty arguments, backslashes, and Unicode without the legacy argument binder losing or merging them. [PR 2736](https://github.com/openclaw/crabbox/pull/2736).
- **Recover interrupted creates after a coordinator reset.** Bounded history reads, streamed log downloads, and capped caches reduce memory pressure; interrupted creates fail promptly while preserving provider cleanup ownership. [PR 2745](https://github.com/openclaw/crabbox/pull/2745).
- **Know how to stop every retained lease.** Runs print the exact stop command and retention reason, Cloudflare retries failed container cleanup, and stop respects cleanup already owned by expiry. [PR 2741](https://github.com/openclaw/crabbox/pull/2741), [PR 2735](https://github.com/openclaw/crabbox/pull/2735), [PR 2747](https://github.com/openclaw/crabbox/pull/2747), [Issue 2740](https://github.com/openclaw/crabbox/issues/2740), [Issue 2746](https://github.com/openclaw/crabbox/issues/2746). Thanks @coygeek and @shakkernerd.

### Upgrade notes

- AWS Windows leases now boot Server 2025 by default. Pin `--os windows-server:2022` to keep Server 2022. [PR 2748](https://github.com/openclaw/crabbox/pull/2748), [PR 2734](https://github.com/openclaw/crabbox/pull/2734), [Issue 2717](https://github.com/openclaw/crabbox/issues/2717). Thanks @saftall.
- After an Azure VM disappears, automatic cleanup resumes only previously admitted deletion. Otherwise choose `crabbox stop` or `crabbox hold` explicitly, even for an expired lease; retain local claims for recovery. Holds do not expire automatically and need explicit finalization after salvage and disposal. [PR 2733](https://github.com/openclaw/crabbox/pull/2733). Thanks @galiniliev.

### Added

- Azure Sandbox: run kept Linux shell workloads in existing Azure Container Apps Sandbox groups with fixed lease recovery, explicit uploads, idle resume, and verified cleanup that can interrupt active requests. [PR 2738](https://github.com/openclaw/crabbox/pull/2738). Thanks @galiniliev.

### Changes

- AWS Windows leases: default the stock AMI fallback to Server 2025 in direct and brokered mode while preserving explicit Server 2022 selection and promoted-image precedence. [Issue 2717](https://github.com/openclaw/crabbox/issues/2717). [PR 2748](https://github.com/openclaw/crabbox/pull/2748). Thanks @saftall.
- AWS Windows images: enable guarded Server 2025 bakes from stock with source-only OS selection, guest-version proof, and matching Server Core containers; explicit Server 2022 leases retain stock selection. [Issue 2717](https://github.com/openclaw/crabbox/issues/2717). [PR 2734](https://github.com/openclaw/crabbox/pull/2734). Thanks @saftall.

### Fixes

- Stop: observe expiry-owned cleanup and reconcile its confirmed completion without duplicate provider deletion, preserving local recovery material while pending. [PR 2747](https://github.com/openclaw/crabbox/pull/2747). [Issue 2746](https://github.com/openclaw/crabbox/issues/2746). Thanks @shakkernerd.
- Coordinator: bound historical lease reads, log downloads, cleanup scans, and bridge history caches; fail interrupted creates promptly before dispatch or after resource-ID publication while retaining provider cleanup custody. [PR 2745](https://github.com/openclaw/crabbox/pull/2745).
- Windows developer images: reboot through validated SSH without racing workspace ownership release, require bounded SSH down/up transitions before resuming prep, and fail on rejected shutdown commands. [PR 2744](https://github.com/openclaw/crabbox/pull/2744).
- Windows developer images: retry and verify container pulls and native package downloads, report the failing prep step, exception and longer log tail, and distinguish accepted release from unconfirmed cleanup after prep failure. [PR 2743](https://github.com/openclaw/crabbox/pull/2743).
- AWS developer images: wait up to `CRABBOX_IMAGE_CAPACITY_WAIT` (default 45 minutes per acquisition) for fleet, owner, or org active-lease capacity, preserving cleanup and excluding rejected attempts from measured timings. [PR 2742](https://github.com/openclaw/crabbox/pull/2742).
- Windows developer images: keep staged TruffleHog binaries executable on stock Server 2025 and report validation exit codes, stdout, stderr, and launch errors before atomic installation. [PR 2739](https://github.com/openclaw/crabbox/pull/2739).
- Run: print the exact provider-scoped stop command and retention reason when ownership checks fail closed or automatic cleanup cannot confirm the lease stopped, including timing JSON. [Issue 2740](https://github.com/openclaw/crabbox/issues/2740). [PR 2741](https://github.com/openclaw/crabbox/pull/2741). Thanks @coygeek.
- Validate OS selector compatibility after lease flags resolve the provider and target, so a configured or environment Windows Server selector accepts `--target windows` while incompatible final selections still fail. [PR 2737](https://github.com/openclaw/crabbox/pull/2737).
- Windows: preserve embedded quotes, empty arguments, backslashes, and Unicode when running native executables or passing arguments to uploaded PowerShell scripts. [PR 2736](https://github.com/openclaw/crabbox/pull/2736).
- Azure: preserve original fixed-lease companion identities before readiness and across endpoint refresh, resume automatic cleanup only after durable deletion admission, and validate current attachments across the full companion set before deletion; reject reassigned or replacement resources and verify every resource slot before completion. [PR 2733](https://github.com/openclaw/crabbox/pull/2733). Thanks @galiniliev.
- Azure: retain failed leases for salvage with durable holds that block reuse and deletion, including observation-only holds when the original local claim is lost; finalize holds explicitly after read-only verification of operator cleanup. [PR 2733](https://github.com/openclaw/crabbox/pull/2733). Thanks @galiniliev.
- Azure: report fixed-lease capacity rejections as settled only after exact companion cleanup and local claim removal. [PR 2733](https://github.com/openclaw/crabbox/pull/2733). Thanks @galiniliev.
- Cloudflare: retain cleanup alarms after container destruction fails and retry terminal leases before reporting stopped or expired status. [PR 2735](https://github.com/openclaw/crabbox/pull/2735).
- Warmup: report definitive coordinator quota rejection in timing output while preserving recovery for interrupted or ambiguous creation. [PR 2732](https://github.com/openclaw/crabbox/pull/2732). [Issue 2731](https://github.com/openclaw/crabbox/issues/2731). Thanks @shakkernerd.
- Scaleway: create fixed leases from public images without snapshot access, recover the exact server after lost responses, and revalidate root-disk attempt ownership before cleanup. [PR 2728](https://github.com/openclaw/crabbox/pull/2728).

## 0.72.0 - 2026-10-06

### Highlights

- **Start cached Cloudflare containers in about 1–2 seconds, then checkpoint and fork them.** Native container execution speeds up startup, while checkpoints capture reusable workspaces and forks start new leases from them. [PR 2625](https://github.com/openclaw/crabbox/pull/2625), [PR 2713](https://github.com/openclaw/crabbox/pull/2713). Thanks @altaywtf.
- **Use class-sized AWS disks without a 400 GB image minimum.** The promoted eu-west-1 Linux developer image now has a 32 GB root snapshot, so `tiny` leases request 40 GB; a live lease reported a 38 GB root filesystem. [PR 2668](https://github.com/openclaw/crabbox/pull/2668), [PR 2704](https://github.com/openclaw/crabbox/pull/2704), [PR 2714](https://github.com/openclaw/crabbox/pull/2714).
- **Get clearer AWS acquisition and Windows readiness results.** Reclaimed instances fail promptly, recorded Windows leases use Windows readiness, and `--os windows-server:2022|2025` selects the server version explicitly. [PR 2721](https://github.com/openclaw/crabbox/pull/2721), [PR 2722](https://github.com/openclaw/crabbox/pull/2722), [PR 2720](https://github.com/openclaw/crabbox/pull/2720), [Issue 2716](https://github.com/openclaw/crabbox/issues/2716), [Issue 2715](https://github.com/openclaw/crabbox/issues/2715), [Issue 2717](https://github.com/openclaw/crabbox/issues/2717). Thanks @saftall.
- **Keep run output through transient coordinator append failures.** Atomic, idempotent event appends and bounded retries preserve event order and recover lost acknowledgements without duplicating output. [PR 2723](https://github.com/openclaw/crabbox/pull/2723), [Issue 1561](https://github.com/openclaw/crabbox/issues/1561).
- **Find the right on-demand computer and access workflow more easily.** The README, crabbox.sh home page, provider catalog, and desktop/VNC/browser guides now explain the current agent workflow and supported providers together. [PR 2697](https://github.com/openclaw/crabbox/pull/2697), [PR 2699](https://github.com/openclaw/crabbox/pull/2699), [PR 2698](https://github.com/openclaw/crabbox/pull/2698). Thanks @shakkernerd.

### Upgrade notes

- Stop kept Cloudflare leases before redeploying the runner: the one-time cutover deletes the old classes' Durable Object state. Then run `crabbox cleanup` to retire old claims and delete the six legacy container applications as described in [the Cloudflare upgrade guide](docs/providers/cloudflare.md). `--type basic` now maps to the higher-cost `standard-1` with a warning; `--type lite` is rejected because the bundled image cannot start on it. [PR 2625](https://github.com/openclaw/crabbox/pull/2625). Thanks @altaywtf.
- Explicit target or Windows-mode flags that contradict a recorded lease now fail before guest access. Remove the conflicting flags or select the lease's recorded platform; release-only resolution remains available for cleanup. [PR 2722](https://github.com/openclaw/crabbox/pull/2722), [Issue 2715](https://github.com/openclaw/crabbox/issues/2715). Thanks @saftall.
- Repository contributors must install the shared autoreview skill from `openclaw/agent-skills`; the repository no longer carries its own executable helper. Existing copy-mode consumers should reinstall to receive updates. [PR 2663](https://github.com/openclaw/crabbox/pull/2663). Thanks @vincentkoc.

### Added

- AWS Windows leases accept `--os windows-server:2022` or `windows-server:2025` (also `os` / `CRABBOX_OS`) in direct and brokered mode; the default remains Server 2022 and promoted Windows AMIs still win without a Windows selector. [PR 2720](https://github.com/openclaw/crabbox/pull/2720). [Issue 2717](https://github.com/openclaw/crabbox/issues/2717). Thanks @saftall.
- **Checkpoint and fork Cloudflare containers.** `crabbox checkpoint create` captures a running Cloudflare lease as a `cloudflare-container-snapshot` in about 8 seconds, and `checkpoint fork` starts new leases from it in seconds, retrying while a fresh snapshot propagates. Delegated-run providers can now implement native checkpoints through `DelegatedCheckpointBackend`. Cloudflare has no snapshot delete API; snapshots expire after 30 days and `checkpoint delete --local-only` removes the record. Cloudflare lease commands keep using the runner URL and workdir the lease was created with, and a create or fork whose runner response is lost keeps its claim so `status` and `stop` can still reach the container. Stopping a lease while it starts keeps it stopped; unresolved creates retain cleanup custody regardless of elapsed time or a temporary stopped status during snapshot retries, and retries reuse containers already started by concurrent requests. [PR 2713](https://github.com/openclaw/crabbox/pull/2713), [PR 2626](https://github.com/openclaw/crabbox/pull/2626). Thanks @altaywtf.
- Add optional vCPU and memory minimums for new AWS Linux leases while preserving existing class fallback behavior. [PR 2710](https://github.com/openclaw/crabbox/pull/2710). [Issue 2709](https://github.com/openclaw/crabbox/issues/2709). Thanks @shakkernerd.

### Changes

- Use one shared autoreview installation from `openclaw/agent-skills`; repository entrypoints receive upstream fixes without copied helpers or tests. [PR 2663](https://github.com/openclaw/crabbox/pull/2663). Thanks @vincentkoc.
- AWS Linux leases now get their class root-disk size (40 GB for `tiny` instead of 400 GB): the promoted eu-west-1 developer image was rebaked from stock Ubuntu with a 32 GB root. [PR 2668](https://github.com/openclaw/crabbox/pull/2668), [PR 2704](https://github.com/openclaw/crabbox/pull/2704), [PR 2714](https://github.com/openclaw/crabbox/pull/2714).
- **Cached Cloudflare containers start in about 1–2 seconds.** The `cloudflare` runner moves to the Containers `durable_object` scheduling policy: one Durable Object class starts each lease's image and instance type through `ctx.container`, commands and uploads use native `exec()`, and the in-container Go runner, six per-type classes, and `max_instances: 4` cap are gone. `--type basic` now uses `standard-1` with a warning, `--type lite` is rejected because the bundled image cannot start on it, `--cloudflare-image` selects a named runner image, canceled commands stop their process group, and a lease whose container stops ends instead of continuing in an empty workspace. Upgrading is a one-time cutover: stop kept leases first, because redeploying deletes the old classes' Durable Object state. [PR 2625](https://github.com/openclaw/crabbox/pull/2625). Thanks @altaywtf.
- Rewrite the README around on-demand computers for agents, with every supported provider listed. [PR 2697](https://github.com/openclaw/crabbox/pull/2697).
- Redesign the crabbox.sh home page around on-demand computers for agents, with a provider wall generated from the catalog, a new crab-in-a-box icon, and a tokenized visual system shared by the docs and Features pages. [PR 2699](https://github.com/openclaw/crabbox/pull/2699). Thanks @shakkernerd.
- Refresh the docs against the current CLI and provider catalog, including a single guide to desktop, VNC, browser and portal access. [PR 2698](https://github.com/openclaw/crabbox/pull/2698).

### Fixes

- Accept native Tenki SSH certificates on the first connection and during refresh by comparing the decoded session extension directly, while retaining key, signature, and expiry checks. [PR 2727](https://github.com/openclaw/crabbox/pull/2727).
- DigitalOcean: check size availability before creating resources and retain definite fixed-lease rejections as retryable, locally releasable attempts. [PR 2725](https://github.com/openclaw/crabbox/pull/2725).
- Scaleway: accept the same SSH public key after API comment normalization, allowing first fixed-lease acquisition and safe cleanup of stranded key-only attempts. [PR 2724](https://github.com/openclaw/crabbox/pull/2724).
- Boat (ASCII Box): release absent sandboxes without waiting for blocked background data purges, while preserving the purge operation ID in fixed-lease terminal receipts. [PR 2726](https://github.com/openclaw/crabbox/pull/2726).
- Probe resolved SSH leases with their recorded platform, preserve Linux bootstrap gates, and reject contradictory explicit target flags. [Issue 2715](https://github.com/openclaw/crabbox/issues/2715), [PR 2722](https://github.com/openclaw/crabbox/pull/2722), [PR 2718](https://github.com/openclaw/crabbox/pull/2718). Thanks @saftall.
- Fail AWS acquire promptly when EC2 terminates an instance or closes its Spot request during network, SSH, Windows, or coordinator SSM readiness, preserving the reclaim reason and existing cleanup ownership. [PR 2721](https://github.com/openclaw/crabbox/pull/2721), [Issue 2716](https://github.com/openclaw/crabbox/issues/2716). Thanks @saftall.