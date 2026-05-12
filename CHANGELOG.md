# Changelog

## 0.12.0 - Unreleased

### Added

- Added `crabbox run --script <file>` and `--script-stdin` so larger remote commands can be uploaded and executed as files instead of quoted shell strings.
- Added `crabbox run --env-from-profile <file>` and repeatable `--allow-env <name>` for redacted, first-class live-secret forwarding from local profile files.
- Added `crabbox run --fresh-pr <owner/repo#number>` for fresh remote GitHub PR checkouts, with optional `--apply-local-patch`.
- Added `crabbox run --preflight`, `--capture-stderr`, automatic failure bundles, env-forwarding summaries, and `CRABBOX_PHASE:<name>` timing markers for easier live/provider run debugging.
- Added `crabbox run --keep-on-failure` so failed one-shot runs can leave the exact lease available for SSH inspection until idle/TTL expiry.
- Added `provider: proxmox` for direct Proxmox VE Linux QEMU VM leases, including template clone, cloud-init SSH key injection, guest-agent bootstrap, docs, and cleanup support.
- Added `provider: tensorlake` delegated runs for Tensorlake Firecracker sandboxes through the `tensorlake` CLI, including archive sync, env allowlist forwarding, docs, and live-provider coverage. Thanks @zozo123.
- Added `scripts/proxmox-build-template.sh` to build a Crabbox-ready Ubuntu 24.04 Proxmox template from a public cloud image. Thanks @VACInc.
- Added Azure native Windows desktop/VNC and Windows WSL2 lease support, matching the AWS Windows capability boundary. Thanks @jwmoss.
- Added `crabbox azure login` so direct Azure users can persist the active `az login` subscription, tenant, and location without manually exporting service-principal environment variables. Thanks @galiniliev.
- Added `azure.network` / `CRABBOX_AZURE_NETWORK` so Azure direct leases can SSH through private VNet addresses when using VPN/private-network access. Thanks @galiniliev.

### Changed

- Expanded default sync excludes for common generated churn such as `.ignored`, `.vite`, `playwright-report`, `test-results`, and local `.crabbox` log/capture directories, and added top-directory hints for large sync candidates.
- Changed sync guardrails to count the dirty delta when local changes are present while still printing the full candidate size, making dirty-worktree iteration less noisy.
- Changed automatic failure-bundle stdout/stderr capture to cap implicit temp logs while still allowing explicit `--capture-stdout` / `--capture-stderr` files for full local streams.
- Documented `--fresh-pr ... --apply-local-patch` as the preferred fast path for PR iteration from noisy local checkouts.
- Clarified that Blacksmith delegated runs cannot forward CLI-side `--env-from-profile` values and should use workflow-side secrets.
- Documented Islo's `islo ssh --setup` host-alias flow for ad-hoc SSH access to Islo sandboxes. Thanks @zozo123.
- Documented Azure CLI login setup, private-network SSH selection, and regional constraints for reused Azure VNet/subnet/NSG resources. Thanks @galiniliev.

### Fixed

- Fixed repo-local `env.allow: ["*"]` so it no longer forwards every local environment variable to remote commands. Thanks @Hinotoi-agent.
- Fixed Sprites list pagination so missing or repeated continuation tokens fail instead of spinning or accepting malformed pages. Thanks @stainlu.
- Fixed Sprites and Namespace Devbox work-root validation so broad roots are rejected before create/prepare flows. Thanks @stainlu.
- Fixed Namespace Devbox prepare error reporting so prepare failures are not hidden behind earlier SSH config fallback errors. Thanks @stainlu.
- Fixed shared-token coordinator auth so caller-supplied `X-Crabbox-Owner` and `X-Crabbox-Org` headers cannot select the authenticated owner/org. Thanks @Hinotoi-agent.
- Fixed Code, WebVNC, and Egress bridge ticket creation so `use`-shared lease users cannot mint lease-side bridge-agent tickets without manage access. Thanks @Hinotoi-agent.
- Fixed Windows SSH sync by disabling unsupported OpenSSH ControlMaster multiplexing and preferring WSL rsync/path conversion when available. Thanks @galiniliev.

## 0.11.0 - 2026-05-11

### Added

- Added `crabbox job list/run` and repo-local `jobs:` config for named warmup → Actions hydrate → run → cleanup workflows.
- Added Daytona and Namespace Devbox lanes to `scripts/live-smoke.sh` so delegated live smoke coverage can run through the shared harness.
- Added `provider: gcp` for Google Cloud Compute Engine Linux SSH leases, including direct ADC auth, brokered service-account auth, class fallback, Spot/on-demand fallback, docs, and cleanup support.
- Added `crabbox cleanup --provider namespace-devbox` to remove Crabbox-owned Namespace SSH snippets and keys.
- Added `scripts/openclaw-wsl2-tests.sh` for one-command OpenClaw full-suite runs on AWS Windows WSL2 Crabbox leases.

### Changed

- Aligned direct GCP provisioning with Google's official Compute Go SDK (`cloud.google.com/go/compute/apiv1`) and project-wide aggregated instance discovery.
- Moved OpenClaw Blacksmith Testbox run safeguards into Crabbox, including one-shot slug reporting and stalled sync termination.
- Improved `crabbox media preview` and `artifacts collect --gif` defaults to generate higher-quality 1000px/24fps GIFs with Floyd-Steinberg palette dithering and optional gifsicle optimization. Thanks @obviyus.

### Fixed

- Fixed the Blacksmith Testbox sync-stall guard to match current `blacksmith` CLI sync start and completion messages.
- Fixed GCP leases so exact `--type` requests still use configured zone and Spot-to-on-demand fallback, aliases derive GCP class defaults, explicit brokered tags replace Worker default tags, custom networks and ingress policies get separate SSH firewall rules, and brokered pool views include instances outside the Worker's default zone.
- Fixed `crabbox actions hydrate/register` so AWS Windows WSL2 leases can use Linux GitHub Actions hydration instead of being rejected as Windows targets, including root-runner and stale apt-list handling.
- Fixed `scripts/openclaw-wsl2-tests.sh` so follow-up hydrate/run/cleanup commands keep the AWS Windows WSL2 target configuration and warmup failures print captured output.
- Fixed `scripts/openclaw-wsl2-tests.sh` so dirty-sync package graph changes refresh workspace dependencies before the full OpenClaw test command runs.
- Fixed first `crabbox run` syncs after GitHub Actions hydration so tracked checkout files are not treated as stale remote files before the initial dirty-worktree sync.
- Fixed `crabbox run` history finish recording to allow large final log payloads enough time to reach the coordinator.
- Fixed Namespace Devbox release-only resolution so `crabbox stop --provider namespace-devbox --namespace-delete-on-release <name>` deletes without re-preparing SSH.
- Fixed Namespace Devbox release cleanup so stopping a Crabbox Devbox removes its local `~/.namespace/ssh/crabbox-*` snippet and key files.
- Fixed `crabbox webvnc daemon start` so it starts with a fresh bridge log and waits briefly for the bridge-ready marker before returning.

## 0.10.0 - 2026-05-10

### Added

- Added `crabbox run --capture-stdout <path>` and repeatable `--download remote=local` for binary-safe proof capture without streaming arbitrary bytes into the terminal or run-log previews.
- Added `crabbox desktop terminal` for visible terminal smokes, including Sixel-friendly Git-for-Windows `mintty` launch defaults on native Windows.
- Added `crabbox desktop record` plus `desktop terminal --screenshot/--record` for one-command visual proof capture, including native Windows MP4 recording through interactive desktop frames.
- Added automatic contact-sheet PNGs for desktop recordings, `crabbox desktop proof` for one-shot visual proof bundles, recorder diagnostics, and direct PR publishing from terminal/proof captures.

### Changed

- Updated docs for output capture, desktop terminal/proof capture, Windows desktop bootstrap, artifact contact sheets, and managed-provider readiness checks.
- Reworked the WebVNC share dialog into an inline Google-style sharing flow with add-user, org access, copy-link, and done actions.

### Fixed

- Fixed delegated run providers so unsupported `--capture-stdout` and `--download` requests fail instead of streaming stdout and skipping downloads.
- Fixed E2B sandbox creation so Crabbox caps default lease timeouts to E2B's one-hour API limit instead of failing live smoke warmups.
- Fixed `crabbox run` output capture validation so malformed `--download` specs, bad download destinations, and bad `--capture-stdout` paths fail before leasing, syncing, or running remotely.
- Fixed interrupt handling so a second `Ctrl-C` can terminate slow cleanup after the first signal starts graceful cancellation.
- Fixed `crabbox doctor --provider ...` so coordinator secret readiness checks only run for managed brokered providers.
- Fixed `crabbox desktop terminal --provider ssh -- ...` so static SSH command arguments are not consumed as lease IDs.
- Fixed `crabbox run --capture-stdout` so local capture write failures report as capture errors instead of remote command exits.
- Fixed brokered provider preflight so `crabbox doctor --provider azure` reports missing Worker secrets and lease creation returns `provider_not_configured` instead of a coordinator `500`.
- Fixed interrupted one-shot runs so `SIGINT`/`SIGTERM` cancel through the CLI context and still run best-effort lease cleanup.
- Fixed SSH readiness progress logs to include per-port probe state in timeout errors.
- Fixed managed AWS Windows desktop bootstrap so WebVNC/screenshot targets start TightVNC reliably and screenshots are not covered by Windows' first-network flyout.
- Fixed Windows `desktop launch` argument handling so terminal commands such as `bash -lc '...'` and other quoted GUI launches are passed losslessly.
- Fixed the source-built CLI version so unreleased local builds no longer report the previous release.

## 0.9.0 - 2026-05-10

### Added

- Added `provider: sprites` for Sprites microVM SSH leases through the `sprite` CLI/API, including Crabbox sync/run, `crabbox ssh`, and live smoke docs.
- Added `provider: namespace-devbox` for Namespace Devbox SSH leases through the `devbox` CLI, with Crabbox sync/run layered on the returned SSH endpoint.
- Added live smoke checklists and script coverage for direct E2B and Semaphore provider validation. Thanks @stainlu.

### Changed

- Updated Worker runtime dependencies and Go provider SDKs, including noVNC, fast-xml-parser, AWS EC2, Daytona, Islo, and related Go runtime libraries.

### Fixed

- Fixed signed portal user tokens so caller-provided admin claims are rejected instead of granting admin access. Thanks @Hinotoi-agent.
- Fixed Islo workdir containment so absolute paths and parent-directory escapes are rejected before sandbox creation, sync, or run. Thanks @Hinotoi-agent.
- Fixed Islo archive sync uploads to use the API's multipart file contract instead of falling back after server-side `500` responses.
- Fixed Semaphore host configuration so dashboard URLs normalize to hosts while API paths, query strings, fragments, and user info are rejected. Thanks @stainlu.
- Fixed WebVNC portal input focus so controller typing stays in the remote desktop and right-clicks no longer open the browser context menu.
- Fixed Semaphore list output so locally claimed jobs show their lease slugs.
- Fixed E2B relative workdirs so they resolve under the configured E2B user's home instead of always `/home/user`.
- Fixed E2B workspace guardrails so broad roots such as `/`, `/home`, and `/tmp` are rejected before sync creates, deletes, or extracts files.
- Fixed E2B sandbox creation so unsafe workdirs are rejected before the API call. Thanks @stainlu.
- Fixed E2B user validation so path-like users are rejected before sandbox or process calls. Thanks @stainlu.
- Fixed stale Code, WebVNC, and egress bridge clients so expired or missing leases stop polling/restarting after terminal coordinator responses. Thanks @vincentkoc.
- Fixed `crabbox desktop paste` for terminal windows so symbol-heavy text falls back to direct typing instead of sending a literal `Ctrl+V` into xterm-like sessions.
- Removed the vulnerable transitive `fast-xml-builder` Worker dependency by updating fast-xml-parser.

## 0.8.0 - 2026-05-09

### Added

- Added `provider: azure` for managed Azure Linux and native Windows SSH leases, including direct and brokered provisioning, shared Azure networking, SKU fallback, Azure docs, and cleanup support. Thanks @jwmoss.
- Added `provider: e2b` for delegated E2B sandbox runs using E2B sandbox REST/envd APIs. Thanks @zozo123.
- Added `provider: semaphore` for direct Semaphore CI testbox leases over SSH. Thanks @loadez.
- Added an authenticated coordinator control WebSocket for low-latency run attach streams and lease heartbeats, with HTTP polling/heartbeat fallback for older brokers. Thanks @vincentkoc.
- Added rescue-first desktop/WebVNC failure output that names the failing layer and prints exact `rescue:` or native VNC fallback commands when bridges, viewers, browser launches, VNC targets, or input stacks hang.
- Added collaborative WebVNC observer mode, with one active controller, read-only observers, and a portal takeover button that shows who is controlling the session.
- Added first-class `crabbox artifacts` commands for desktop screenshots, MP4 recordings, trimmed GIFs, logs, metadata, Mantis/OpenClaw QA templates, and PR-ready publishing through broker-owned artifact storage, AWS S3, or Cloudflare R2.

### Changed

- Expanded Semaphore and E2B documentation across provider, configuration, CLI, and command pages so direct providers have first-class setup, auth, lifecycle, and troubleshooting guidance.
- Changed `crabbox attach` to prefer the coordinator control WebSocket, drain retained backlog pages, and then stream live run output with less polling latency.
- Changed WebVNC portal sharing to open as an in-session modal, added a standalone share-page back action, and simplified collaboration controls into a single stateful control button.
- Raised the Go core coverage gate to 90% and added regression coverage around provider claims, config parsing, bootstrap defaults, run-log previews, and slug fallbacks.

### Fixed

- Fixed the portal provider filters so Azure leases show their own filter badge and provider icon. Thanks @stainlu.
- Fixed Azure broker SSH security rules so repeated primary/fallback SSH ports are de-duplicated before writing network security group rules.
- Fixed `crabbox run` transport chatter by keeping SSH multiplexers alive longer, retrying fallback SSH ports for streaming commands, and batching stdout/stderr preview events into larger coordinator chunks. Thanks @vincentkoc.
- Fixed macOS WebVNC cursor visibility by enabling noVNC's dot-cursor fallback when Screen Sharing sends a transparent or zero-sized cursor.
- Fixed managed AWS macOS bootstrap so VNC password generation does not abort under `pipefail` before Screen Sharing readiness is installed.
- Fixed WebVNC daemon start-by-slug so coordinator-backed leases use the resolved target OS in the background bridge command.
- Fixed coordinator-backed `crabbox list` so a stale admin token no longer blocks normal logged-in users; the CLI now falls back to active user-visible leases instead of failing with `401 unauthorized`.
- Fixed desktop, screenshot, VNC, and WebVNC SSH helpers so they retry live fallback ports when a coordinator lease advertises an SSH port that is not ready yet.

### Fixed

- Fixed stale Code, WebVNC, and egress bridge clients so expired or missing leases stop polling/restarting after terminal coordinator responses. Thanks @vincentkoc.

### Fixed

- Fixed Blacksmith Testbox shell command rendering so multiline `--shell` payloads with trailing blank whitespace do not produce a spurious shell syntax failure after the remote command succeeds.

## 0.7.0 - 2026-05-07

### Added

- Added mediated egress commands and browser wiring so Linux desktop leases can proxy selected app traffic through the operator machine via the coordinator bridge.
- Added WebVNC portal clipboard controls for sending local clipboard text into the remote session and copying remote clipboard text back to the local browser.
- Added lease sharing for individual users or the owning org, including `crabbox share`, `crabbox unshare`, API access checks, and a portal share control on lease detail pages.

### Fixed

- Fixed `egress start --coordinator` so live public-route egress starts work when the local default coordinator is Cloudflare Access-protected.
- Fixed Tailscale exit-node bootstrap paths to prefer tailnet metadata and fail clearly when remote exit-node egress is not active.
- Fixed `run --no-sync` timing summaries so they report `sync_skipped=true`.
- Fixed native Windows command output so first-use PowerShell progress records do not leak CLIXML into run logs.
- Fixed Islo provider sync so `crabbox run --provider islo` uploads the local workspace, uses the correct `/workspace/<workdir>`, and falls back to chunked exec upload while the archive API returns server errors.
- Fixed Code and WebVNC bridge websocket auth so upgraded brokers receive short-lived bridge tickets in the `Authorization` header instead of logging them in URL query strings, while preserving query fallback for older brokers.
- Fixed managed AWS macOS desktop leases so readiness and WebVNC use a writable `ec2-user` work root, call `crabbox-ready` by absolute path, and read the generated Screen Sharing password via sudo.

## 0.6.0 - 2026-05-07

### Added

- Added `provider: daytona` for Daytona sandbox leases using Daytona's SDK/toolbox for sync and command execution, with short-lived SSH access available through `crabbox ssh`.
- Added Daytona CLI profile auth fallback so `daytona login --api-key ...` can satisfy Crabbox Daytona auth without duplicating `DAYTONA_API_KEY`.
- Added `provider: islo` for delegated Islo sandbox runs using the Islo Go SDK.
- Added a provider backend registry and authoring guide so delegated and SSH-backed providers can live in provider-owned packages while core keeps command parsing, rendering, and capability validation.
- Added `--tailscale-exit-node` and `--tailscale-exit-node-allow-lan-access` so managed Linux leases can route egress through an approved tailnet exit node.
- Added broker capacity hints for AWS leases, including selected market, attempted regions, quota/capacity advice, and configurable high-pressure class warnings.
- Added `crabbox code` and per-lease `/code/` portal URLs for authenticated code-server access on `--code` Linux leases.
- Added per-lease portal detail pages with bridge status, access-panel copy commands, recent run links, and a stop action.
- Added portal run detail pages with command metadata, result summaries, dense viewport-fitted portal tables, provider/OS badges, active/ended/provider/target filters, sticky portal chrome, and copyable retained log previews.
- Added latest lease telemetry snapshots for coordinator-backed Linux leases, including load, memory, disk, and uptime in `status --json` and the portal detail view.
- Added bounded lease telemetry history with portal sparklines and stale/high-resource badges on lease detail pages.
- Added run-level telemetry summaries with start/end Linux resource snapshots in run history JSON, human history output, and portal run tables/details.
- Added live run telemetry samples for longer Linux commands, including bounded coordinator storage and portal load/memory/disk trend lines on run detail pages.
- Added portal visibility for external Blacksmith Testbox runners synced from `crabbox list --provider blacksmith-testbox`, with owner-scoped runner rows, stale markers, GitHub Actions links, status badges, stuck filters, detail pages, and copyable local stop commands.
- Added admin portal visibility for non-owned runner leases, including `mine`/`system` filters and matching detail/code/VNC drilldowns for operator sessions.
- Added `crabbox desktop launch --webvnc --open` to launch a desktop browser/app and immediately bridge the same lease into the WebVNC portal.
- Added `crabbox webvnc --daemon`/`--background` plus `--status`/`--stop` for background WebVNC bridges without tmux.
- Added `crabbox media preview` for creating motion-trimmed GIF previews and optional trimmed MP4 clips from desktop recordings.
- Documented the prebaked runner image boundary: provider-owned AMIs/snapshots hold machine capabilities while repo/runtime caches stay in QA workflows or warm leases.

### Changed

- Changed AWS capacity fallback to route configured `CRABBOX_CAPACITY_REGIONS` across both brokered and direct AWS launches, with the deployed coordinator defaulting to a wider multi-region pool for better headroom.
- Changed coordinator lease requests to omit the default capacity block, preserving mixed-version broker compatibility while still sending explicit market, strategy, fallback, multi-region, availability-zone, or hint opt-out settings.
- Changed coordinator-backed CLI lease output to print broker capacity hints when AWS routing, quota, Spot fallback, or configured high-pressure classes are involved.
- Changed the portal lease table to merge external Blacksmith Testbox runners into the main grid as muted, disabled rows instead of rendering a separate external-runners table.
- Refactored built-in provider backend implementations into `internal/providers/<name>` packages while keeping command orchestration and rendering core-owned.

### Fixed

- Fixed Daytona SDK sync so tar creation and Daytona toolbox upload stream from disk instead of buffering large archives in memory.
- Fixed Daytona resource override handling so snapshot-only sandboxes reject generic `--class` and `--type` flags instead of accepting no-op compute settings.
- Fixed Islo delegated runs so shell-mode commands preserve raw shell strings and truncated exec streams fail instead of silently reporting success.
- Fixed provider-owned flags and target/capability validation to run through registered provider specs while preserving script-facing list JSON compatibility for coordinator and Blacksmith backends.
- Fixed Blacksmith Testbox queued/outage failures so users see the upstream queue state and practical fallback guidance instead of an opaque timeout.
- Fixed Blacksmith Testbox repo inference for mirrored repositories and portal runner sync for stale or external Testbox rows.
- Fixed managed Linux desktop/browser leases to preinstall video capture and native addon build helpers, avoiding per-scenario apt installs in browser QA runs.
- Fixed managed Linux desktop leases to use a slim XFCE session instead of bare Openbox, preserving a real panel/window-manager desktop while avoiding the full XFCE meta package.
- Fixed SSH readiness progress logs to distinguish open TCP ports, failed SSH authentication, and failed Crabbox ready checks.
- Fixed auto-shell command reconstruction so arguments with spaces stay quoted when shell operators such as `&&` are present.
- Fixed managed Linux bootstrap ordering so SSH is reachable before slow desktop/browser package setup while readiness still waits for the full desktop/browser contract.
- Fixed managed desktop/browser warmups so slow cloud-init bootstraps get a longer readiness window, retry once after SSH timeout, and clean up failed leases instead of leaking unusable VMs.
- Fixed brokered cloud server names so friendly-slug collisions with stale provider VMs do not block new leases.
- Fixed human WebVNC desktop launches to keep browser windows windowed by default and reserve fullscreen for explicit capture/video workflows.
- Fixed WebVNC portal status text and bridge commands so waiting/reset states explain the exact local bridge command to run.
- Fixed the Code portal waiting state so it shows bridge status, copy/reload controls, and automatically opens the workspace once the local bridge connects.
- Fixed `crabbox webvnc --stop` so daemon shutdown terminates the active child bridge, not only the supervisor.
- Fixed portal command rows so their copy affordance copies the matching local command instead of only labelling the section.
- Fixed portal Windows target badges to show compact `win` and `win (wsl2)` labels instead of `windows / normal`.
- Fixed portal access and time columns to use compact capability icons, relative time labels, and sortable time metadata instead of wide action buttons and Zulu timestamps.
- Fixed lease detail layout so local commands live inside the access panel instead of forcing a separate full-width commands section above recent runs.
- Fixed portal run detail layout density, responsive action alignment, and run telemetry readability so long-lived run pages fit operator viewports cleanly.
- Fixed generated docs-site navigation so the sidebar scroll position is preserved while moving between pages.
- Fixed Windows WebVNC credential handling so generated portal links preserve special characters and managed TightVNC sessions copy service passwords into the logged-in user's registry profile.
- Fixed managed Linux browser setup so Chrome/Chromium launches skip first-run and default-browser prompts.
- Fixed managed Linux browser cloud-init setup so Chrome/Chromium policy and wrapper generation cannot break YAML parsing.
- Fixed WebVNC portal passwords with escaped special characters and kept the bridge alive across viewer resets and transient coordinator EOFs.

## 0.5.1 - 2026-05-05

### Added

- Added `.crabboxignore` for repo-local sync-only exclude patterns shared by `run` and `sync-plan`.
- Added WebVNC portal controls for reconnect, fullscreen, and clipboard-ready bridge commands.

### Fixed

- Fixed managed AWS Windows WSL2 bootstrap by using the current Ubuntu WSL rootfs URL, downloading large rootfs files through `curl.exe`, and retrying empty or partial rootfs downloads instead of reusing a poisoned tarball. Thanks @vincentkoc.
- Fixed AWS Windows WSL2 mode overrides so they refresh the default instance type to a nested-virtualization-capable family. Thanks @steipete.
- Fixed AWS Windows WSL2 runs so mode overrides also refresh the default work root to `/work/crabbox` while keeping WSL2 sync on the fast rsync path.
- Fixed remote git seeding so an unfetchable local commit cannot leave an empty `.git` worktree that makes sync sanity report every tracked file as deleted.
- Skipped remote git seeding for local commits that are not present in any remote-tracking ref, avoiding slow doomed clone/fetch attempts before rsync.
- Fixed WebVNC bridge reconnects so reloading or reconnecting the browser no longer requires restarting the local bridge.
- Fixed Windows archive sync from macOS so Apple extended attributes do not spam remote tar warnings.
- Fixed the Homebrew formula test command so GoReleaser emits the expected formula syntax.

## 0.5.0 - 2026-05-04

### Added

- Added `--desktop`, `--browser`, and `crabbox vnc` for optional Linux UI/browser leases, including loopback-only VNC with per-lease passwords and headless browser support without a desktop.
- Added authenticated WebVNC portal support with `crabbox webvnc`, which bridges a desktop lease into the coordinator portal with short-lived bridge tickets and without exposing the remote VNC port.
- Added managed AWS Windows desktop leases with OpenSSH, Git for Windows, loopback TightVNC, per-lease VNC passwords, and `crabbox vnc`.
- Added managed AWS Windows WSL2 support for Linux command execution inside brokered Windows leases.
- Added AWS macOS desktop lease plumbing for EC2 Mac Dedicated Hosts, including Screen Sharing setup and per-lease credentials.
- Added `crabbox vnc --open` to start the SSH tunnel and launch the local VNC client for managed desktop leases.
- Added `crabbox desktop launch` to open a browser or app inside a visible desktop lease, including native Windows scheduled-task launch for the logged-in console session.
- Added `crabbox screenshot` to save a PNG from a desktop lease without opening a VNC client.
- Added optional Tailscale reachability for managed Linux leases with `--tailscale`, `--network auto|tailscale|public`, brokered OAuth auth-key minting, and non-secret tailnet metadata in status/inspect output.
- Added static macOS/Windows VNC endpoint discovery, including SSH-tunneled loopback VNC and trusted static direct VNC on `host:5900`.
- Added generated Windows console login details and auto-logon for managed AWS Windows desktop leases.
- Added a minimal XFCE desktop profile with panel/window manager for managed VNC leases.
- Added generated command help for grouped commands so `crabbox actions --help`, `crabbox cache --help`, `crabbox desktop --help`, and similar entrypoints exit cleanly.

### Changed

- Clarified static macOS/Windows VNC as existing-host access, not Crabbox-created boxes, so `--open` no longer launches an OS credential prompt unless `--host-managed` is passed.
- Switched top-level CLI routing to Kong while preserving existing per-command flags, passthrough remote commands, aliases, and exit-code behavior.

### Fixed

- Fixed WebVNC portal login redirects by canonicalizing broker origins before starting the browser login flow.
- Fixed AWS desktop provisioning and Windows SSH bootstrap issues that could leave managed desktop leases unreachable.
- Fixed passthrough command help such as `crabbox run --help` so it prints local usage instead of provisioning a remote lease.
- Fixed `crabbox desktop launch --browser` on freshly warmed desktop leases by creating the remote workdir before launching the app.
- Fixed failed Blacksmith Testbox warmups so printed, newly listed, or delayed `tbx_...` boxes are stopped instead of being left queued after an upstream workflow error.
- Fixed `crabbox run --junit` so all-passing JUnit files record results instead of leaving the coordinator run stuck when the failure list is empty.
- Fixed native Windows `--shell` runs so multi-statement PowerShell scripts keep their quotes instead of being re-parsed by a nested PowerShell process.
- Removed the static macOS managed-login path so static host VNC cannot be mistaken for a Crabbox-created external instance.
- Excluded macOS AppleDouble `._*` sidecar files from default sync manifests so native Windows archives do not transfer invalid TypeScript/package sidecars.
- Quoted `crabbox vnc` tunnel key paths so macOS `Application Support` lease keys can be pasted directly into a shell.
- Skipped Linux-only GitHub Actions hydration stop markers on native Windows static targets.
- Fixed brokered Tailscale requests on coordinators without OAuth secrets so they fail as disabled instead of entering the auth-key minting path.
- Fixed Worker deploy smoke to prefer the Crabbox-scoped Cloudflare token when it is present in the environment or local profile.

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
