# Changelog

## Unreleased

### Added

- Added direct FastAPI Cloud application and deployment inspection through `status`, `list`, and `doctor`, including configured default application support. Thanks @zozo123.
- Added normalized provider runtime capabilities and `--runtime` filters to `crabbox providers` and `crabbox providers recommend`.
- Added `crabbox providers recommend fanout-testing` for forkable workspace and best-of-N test selection guidance.
- Added normalized provider reachability capabilities and `--reachability` filters to `crabbox providers` and `crabbox providers recommend`.
- Added `crabbox providers recommend offline-validation` for credentialless local, BYO SSH, and external-provider validation guidance.
- Added normalized provider lifecycle capabilities and `--lifecycle` filters to `crabbox providers` and `crabbox providers recommend`.
- Added `crabbox providers recommend failure-diagnostics` for failed-run triage guidance across proof, session, artifact, download, preview, and SSH-debuggable providers.
- Added `crabbox providers recommend warm-start` for low-latency repeated-run guidance across local runtimes, cache volumes, retained sessions, pause/resume, and workspace-state providers.
- Added `crabbox providers recommend resource-observability` for coordinator usage/cost visibility, SSH resource telemetry, retained run evidence, reusable sessions, and preview URL guidance.
- Added `crabbox providers recommend code-interpreter` for generated-code and script execution guidance across delegated and local sandbox providers.
- Added `crabbox providers recommend disposable-execution` for cleanup-capable temporary sandbox guidance across delegated and local sandbox providers.
- Added `crabbox providers recommend web-app-smoke` for app and service smoke guidance across provider URLs, SSH tunnels, tailnet reachability, browser/code/desktop access, sessions, and retained outputs.
- Added `crabbox providers recommend interactive-debug` for live inspection guidance across synced SSH, browser/code/desktop access, reusable sessions, provider URLs, and retained evidence.
- Added a provider live-smoke contract doc for opt-in validation of adapters that need credentials, quota, local runtimes, or private control planes.
- Tuned `crabbox providers recommend live-smoke` to keep local runtime smoke paths visible when cloud credentials are unavailable.
- Added Apple Container to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Local Container to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Docker Sandbox to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added SmolVM to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Superserve to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Vercel Sandbox to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Linode to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added DigitalOcean to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Nebius to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added OVHcloud to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added NVIDIA Brev to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Phala to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Anthropic Sandbox Runtime to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added OpenSandbox to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Proxmox to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added XCP-ng to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Agent Sandbox live-smoke documentation and dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added Scaleway live-smoke documentation and dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added KubeVirt live-smoke dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added Daytona live-smoke documentation and dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added Namespace Devbox live-smoke documentation and dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added Semaphore live-smoke documentation and dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added Sprites live-smoke documentation and dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added W&B live-smoke documentation and dispatch regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added Incus live-smoke documentation and preflight regression coverage for the guarded `scripts/live-smoke.sh` matrix.
- Added Multipass to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added Tart to the guarded `scripts/live-smoke.sh` provider smoke matrix.
- Added run-session metadata for Vercel Sandbox runs so retained and reused sandboxes can be written through `--lease-output`.
- Added run-session metadata for CodeSandbox runs so retained and reused sandboxes can be written through `--lease-output`.
- Added run-session metadata for OpenSandbox runs so retained and reused sandboxes can be written through `--lease-output`.
- Added run-session metadata for Upstash Box runs so retained and reused boxes can be written through `--lease-output`.
- Added run-session metadata for Azure Dynamic Sessions runs so retained and reused sessions can be written through `--lease-output`.
- Added run-session metadata for Freestyle runs so retained and reused VMs can be written through `--lease-output`.
- Added run-session metadata for Tensorlake runs so retained and reused sandboxes can be written through `--lease-output`.
- Added run-session metadata for Superserve runs so retained and reused sandboxes can be written through `--lease-output`.
- Added run-session metadata for SmolVM runs so retained and reused machines can be written through `--lease-output`.
- Added run-session metadata for OpenComputer runs so retained and reused sandboxes can be written through `--lease-output`.
- Added run-session metadata for Agent Sandbox runs so retained and reused claims can be written through `--lease-output`.
- Added run-session metadata for Apple Machine runs so retained and reused machines can be written through `--lease-output`.

### Fixed

- Fixed Tart SSH readiness on hosts where OpenSSH can reach the guest but Go's raw TCP probe cannot. Thanks @kmcquade.
- Revoked active WebVNC and Code viewers when their lease share access is removed while preserving owner, admin, and still-authorized sessions. Thanks @coygeek.
- Prevented E2B and Upstash Box credentials from following redirects outside each request's trusted origin while preserving same-origin redirects. Thanks @coygeek.
- Prevented SmolVM API credentials from following redirects outside the configured API origin while preserving same-origin redirects. Thanks @coygeek.
- Made direct and brokered Azure Windows desktop leases converge on working SSH/SFTP, first-logon readiness, terminal extension state, retryable disk cleanup, and actionable bootstrap diagnostics. Thanks @fcoury-oai.

## 0.33.0 - 2026-06-22

### Added

- Added `provider: nebius` for direct Nebius AI Cloud Linux SSH leases through the native CLI, with profile-owned authentication, managed networking and disks, and claim-backed lifecycle hardening. Thanks @coygeek.
- Added an opt-in local benchmark timing ledger with repeated provider runs and evidence-aware reports. Thanks @TurboTheTurtle.
- Added the Phala confidential Intel TDX CVM provider with default-on hardware attestation, exact Compose binding, TLS-authenticated SSH, and fail-closed claim-backed lifecycle cleanup. Thanks @anagnorisis2peripeteia.
- Added reusable E2B run-session handles and cleanup commands for `--keep --lease-output`. Thanks @kiranmagic7.
- Added reusable Modal run-session handles and cleanup commands for `--keep --lease-output`. Thanks @kiranmagic7.
- Added the Scaleway direct Linux SSH-lease provider with per-lease IAM keys, claim-backed lifecycle recovery, and guarded live smoke coverage. Thanks @coygeek.
- Added reusable W&B run-session handles and cleanup commands for `--keep --lease-output`.
- Added Linux CPU capacity to lease telemetry and portal status details.

### Changed

- Consolidated lifecycle cleanup, credential routing, artifact boundaries, and run-history recovery guarantees across the README and operational documentation.
- Refreshed the bundled Crabbox agent skill for current remote-proof, job, pool, artifact, desktop, and provider-boundary workflows. Thanks @coygeek.
- Defined Crabbox's supported single-user and cooperative-team security boundary, clarified repository configuration as trusted project automation, and separated vulnerability reporting from compatibility-preserving hardening.

### Fixed

- Verified pinned OpenSSH, Git for Windows, TightVNC, and versioned Ubuntu WSL bootstrap artifacts before privileged extraction, installation, or import. Thanks @coygeek.
- Preserved valid JUnit summaries when sibling reports are malformed, stopped silently truncating auto-discovered reports, and added opt-in failure status for parsed test failures. Thanks @coygeek.
- Redacted WebVNC viewer URLs, usernames, and passwords from command output by default while preserving explicit private-terminal reveal. Thanks @coygeek.
- Prevented repository-local KubeVirt config from selecting operator SSH key paths while preserving inline public keys. Thanks @coygeek.
- Restricted lease sharing rosters to owners, admins, and `manage` recipients while keeping shared leases visible to `use` recipients. Thanks @coygeek.
- Redacted credential-bearing Proxmox API URL userinfo from text and JSON `config show` output. Thanks @coygeek.
- Restricted EC2 Mac Dedicated Host inventory to admins or callers with a visible attached lease, and required admin authentication for explicit brokered host pinning. Thanks @coygeek.
- Restricted runtime-adapter service credentials to workspace lifecycle and desktop-connection routes, excluding interactive terminal attachment. Thanks @coygeek.
- Rejected cross-origin Azure Dynamic Sessions redirects before command, environment, upload, or management bodies can be replayed. Thanks @coygeek.
- Kept manual release publication on the reviewed default-branch GoReleaser configuration instead of allowing a selected tag to replace credentialed release behavior. Thanks @coygeek.
- Rejected cross-origin coordinator redirects before bearer, Access, or local identity headers can be replayed. Thanks @coygeek.
- Redacted configured Upstash Box API keys from HTTP and streamed error diagnostics. Thanks @coygeek.
- Redacted configured Semaphore API tokens from provider response diagnostics. Thanks @coygeek.
- Kept GitHub Actions runner registration tokens off remote SSH command arguments. Thanks @coygeek.
- Redacted Cloudflare runner bearer tokens from HTTP and streamed error diagnostics. Thanks @coygeek.
- Confined remote failure-bundle links to the generated archive subtree and omitted unsafe special entries. Thanks @coygeek.
- Required actual Islo sandbox identifiers to already be canonical before raw-ID recovery can reach provider operations. Thanks @coygeek.
- Required canonical generated Freestyle VM names before raw-ID recovery can reuse or delete provider resources. Thanks @coygeek.
- Rejected plaintext non-loopback E2B API endpoints before provider credentials can be attached. Thanks @coygeek.
- Rejected cross-origin RunPod REST redirects before bearer credentials or pod-create bodies can be replayed. Thanks @coygeek.
- Rejected non-canonical signed browser-session tokens so suffix changes cannot bypass Code portal logout revocation. Thanks @coygeek.
- Required a matching local claim before Cloudflare container reuse, status, or stop operations can reach the runner. Thanks @coygeek.
- Redacted configured Freestyle API keys from lifecycle, command, and file-operation error diagnostics. Thanks @coygeek.
- Redacted configured OpenComputer API keys from control-plane and upload error diagnostics. Thanks @coygeek.
- Rejected cross-origin Cloudflare runner redirects before command, environment, or upload bodies can be replayed. Thanks @coygeek.
- Validated AWS region inputs before building SigV4-signed service endpoints, preventing request-selected hostname escapes. Thanks @coygeek.
- Required run artifacts now reject dangling symlinks and symlinks to directories instead of treating them as proof files. Thanks @coygeek.
- Rejected symlinked and non-regular artifact bundle entries before publish side effects, preventing files outside the selected bundle from being uploaded. Thanks @coygeek.
- Kept `CRABBOX_ENV_ALLOW` authoritative over selected profile allowlists while preserving explicit `--allow-env` additions. Thanks @coygeek.
- Made desktop paste/type and POSIX launch/proof success depend on verified clipboard delivery or live/visible launch state, including clipboard-manager and wrapper handoffs. Thanks @coygeek.
- Released newly created SSH leases when prewarm hydration, probe, or ready-pool registration fails, preventing paid lease leaks. Thanks @coygeek.
- Preserved transient run-history creation retries until a replacement lease attaches successfully.
- Stopped lease-local mediated egress daemons during ordinary lease stop before provider release.
- Revoked isolated Code viewer sessions when their GitHub portal session logs out, preventing stale viewer cookies from retaining prior-owner lease access. Thanks @coygeek.
- Prevented unauthenticated Cloudflare Access key fetches and bounded key-set refresh work for invalid JWT key IDs. Thanks @coygeek.
- Blocked normalized empty-segment variants of internal coordinator routes and stripped caller-supplied internal headers before fleet dispatch. Thanks @coygeek.
- Source-bound Azure Dynamic Sessions bearer tokens to operator-approved endpoints instead of repository-selected destinations. Thanks @coygeek.
- Made coordinator-backed `crabbox list` query the user's active orchestrator leases directly, reserving admin-wide machine inventory for `--all` and avoiding stale admin-token warnings during ordinary listing.
- Let Islo use tenant defaults for implicit sandbox image and capacity while preserving every explicit config, environment, and flag override. Thanks @zozo123.
- Made new runtime-adapter ticket claims provisional until agent connection or lease registration, allowing authenticated recovery of expired inactive first claims while preserving all existing and confirmed adapter IDs.
- Separated shared automation tokens from signed user-token keys, preserving shared-token-only automation while requiring distinct session signing material for GitHub login.
- Required retained coordinator ownership records before orphan sweeps delete AWS or Azure machines or release EC2 Mac hosts, while keeping tag-only and legacy candidates visible in reports.
- Verified the pinned GitHub CLI release artifacts before installing them in the default Cloudflare sandbox image and preserved true AMD64/ARM64 target selection during cross-platform builds.
- Pinned and verified the default Proxmox template cloud image before conversion, while preserving custom image URLs with a required matching SHA256.
- Kept Code, WebVNC, and Egress bridge tickets out of WebSocket URLs while preserving ordinary coordinator authentication, older-coordinator bearer retries, and legacy-client compatibility.
- Added opt-in per-lease Code portal origins with one-time viewer bootstrap and lease-scoped browser sessions, isolating proxied workspace content from coordinator and other lease origins without changing existing Code URLs. Thanks @coygeek.
- Source-bound broker and direct-provider credentials to repository-configured endpoints, while preserving same-source custom deployments and explicit environment or CLI overrides.
- Restricted Crabbox-managed Windows credential files to the managed user, Administrators, and SYSTEM without changing desktop credential consumers. Thanks @coygeek.
- Created default artifact bundles and retained run logs/metadata with private local permissions while preserving explicit shared-output directories. Thanks @coygeek.

## 0.32.0 - 2026-06-15

### Added

- Documented the end-to-end runtime adapter topology, trust boundaries, request paths, startup order, and failure signals.
- Added `crabbox connect <lease-id-or-slug>` to open an interactive SSH session to key-, certificate-, and proxy-authenticated provider targets while keeping `crabbox ssh` as the print-only command surface for token-as-username providers.
- Added `crabbox adapter ingress` as a provider-neutral authenticated HTTP and WebSocket bridge for loopback fleet services.
- Added JSON API initiation of generation-fenced runtime-adapter workspace deletion through explicit registered lease release.
- Added reusable Cloudflare container run-session handles with exact cleanup commands for `--keep --lease-output`. Thanks @zozo123.

### Fixed

- Pinned GitHub Actions workflow dependencies to reviewed immutable commits and added CI enforcement against mutable references. Thanks @coygeek.
- Hardened XCP-Ng repository config so it cannot override trusted provider credentials. Thanks @coygeek.
- Replaced browser-native portal confirmation and clipboard prompts with themed, keyboard-accessible HTML dialogs.
- Hardened GCP operator inventory and workspace recovery by requiring deterministic Crabbox instance names plus canonical provider labels before accepting resources. Thanks @coygeek.
- Hardened shared-lease run auditability by preserving actor attribution while granting lease owners read-only access to runs, logs, events, telemetry, and portal history. Thanks @coygeek.
- Pinned shipped runtime container base images to reviewed multi-platform digests and enforced the pins in CI. Thanks @coygeek.
- Redacted manage-only WebVNC bridge commands and egress session details from `use` share viewers. Thanks @coygeek.
- Created run downloads, captures, proofs, and failure bundles with private POSIX permissions. Thanks @coygeek.
- Rejected broker-supplied GitHub login URLs that do not use the expected HTTPS GitHub authorization endpoint.
- Preserved single-use bridge tickets when presented to the wrong lease, role, or runtime-adapter endpoint. Thanks @coygeek.
- Required lease manage access before resetting another operator's WebVNC bridge. Thanks @coygeek.
- Aligned the `apple-container` provider fallback image with the portable OS default while preserving explicit image choices. Thanks @coygeek.
- Fixed `apple-container` inventory parsing for Apple container 1.0 object-form status and nested network addresses. Thanks @coygeek.
- Added a dedicated route-scoped service credential for Crabfleet workspace lifecycle requests without granting general coordinator access.
- Kept accepted workspace creates successful when post-persist prewarm maintenance is temporarily unavailable.

## 0.31.0 - 2026-06-14

### Added

- Added configurable organization-wide workspace prewarming with cross-owner adoption, immediate replenishment while busy, and automatic idle drain.
- Added `crabbox webvnc local` on macOS and Linux for token-gated browser access to an existing loopback VNC tunnel, with the VNC password accepted only through stdin and kept out of process arguments, environment variables, URLs, and viewer files.
- Added authenticated Crabfleet workspace terminals with bounded SSH/WebSocket bridging, durable tmux resume, and lifecycle revocation.
- Added `crabbox adapter connect`, an outbound ticket-authenticated relay for the narrow `crabfleet/v1` runtime-adapter API, with a current-user-owned peer-verified Unix-socket transport, per-request local-token reload, bounded bodies, configurable desktop request timeouts, and reconnecting coordinator login refresh.
- Added `crabbox adapter serve`, a generic authenticated Linux/macOS-hosted workspace lifecycle API with a no-follow descriptor-verified lock in a private current-user-owned state directory, read-only state validation, crash-owned lifecycle children including bounded provider discovery, fixed TTL/idle and machine-shape override policy, explicit idempotent fixed-ID provider contracts, immutable full-identity status adoption and full-identity pre-release validation even before claim persistence, per-attempt provider route/config scopes, exact fixed external identities with crash-reclaimable fully fsynced slug reservations, restart-safe gated provider-side-effect durability with immediate memory-retried credential-bridge revocation on failed terminal writes, adapter-only side-effect-free WebVNC restarts with ordinary daemon heartbeats preserved, scope/state/resource-bound daemon reuse, per-workspace daemon OS locking, verified WebVNC supervisor/process-tree revocation, exact remote websockify socket/process ownership plus authenticated noVNC WebSocket readiness, full-identity refreshed-absence cleanup, bounded process-tree orchestration, no-follow token loading, exact-owned non-forking loopback SSH tunnels on Linux/macOS/Windows, and a public open-source Linux desktop bootstrap with noVNC/websockify, private user-owned VNC credentials, and a narrowly privileged desktop reset helper.
- Added `provider: ovh` for direct OVHcloud Public Cloud Linux SSH leases with signed API authentication, local claim-backed ownership, guarded recovery, and live lifecycle coverage. Thanks @coygeek.
- Added `provider: codesandbox` for delegated CodeSandbox Linux environments with archive sync, retained lifecycle, pause/resume, preview URLs, exact SDK pinning, truthful running-state checks, command exit propagation, and live lifecycle coverage; archive-sync orchestration is now shared across CodeSandbox, OpenComputer, OpenSandbox, Superserve, and Vercel Sandbox. Thanks @coygeek.
- Added `provider: cloudflare-dynamic-workers` for authenticated Worker-runtime module execution through Cloudflare Dynamic Workers, including blocked-by-default egress, stable caching, durable run metadata, lifecycle commands, and isolated live smoke coverage. Thanks @coygeek.
- Added `provider: agent-sandbox` for delegated Linux runs through Agent Sandbox `v0.5.0rc1` `v1beta1` warm pools, using the operator's `kubectl` for dependency-light discovery, lifecycle, archive sync, exec, guarded ownership cleanup, and live smoke coverage. Thanks @coygeek.
- Added `provider: vercel-sandbox` for delegated Linux microVM runs through the official Vercel Sandbox SDK, including archive sync, streamed output, retained-session resume, ownership-guarded lifecycle operations, and guarded live smoke coverage. Thanks @coygeek.
- Added generic Job evidence fields plus bounded Islo single-file `--require-artifact` and `--download` support, with provider capability gating and secret-safe archive upload errors. Thanks @zozo123.
- Added owner-scoped outbound runtime-adapter relays so registered workspaces can be created and deleted through a provider-neutral lifecycle API without exposing the provider control plane, including confirmed Delete actions in the portal.

### Fixed

- Hardened Agent Sandbox repository-config workload and workdir selection, mount-safe replacement sync, pinned pod-container execution, absolute and multi-file kubeconfig handling, controller-enforced TTL expiry with retained exact-claim cleanup, warm-pool/lifecycle/downstream identity validation, one-shot cleanup arming, cleanup dry-run identity checks, root-rechecked missing-claim handling, downstream-missing claim retention, recoverable ambiguous-create reconciliation, terminal status detection, retained activity bookkeeping, local claim removal reporting, and UID-pinned recovery leases when failed-readiness cleanup cannot reach Kubernetes; thanks @coygeek.
- Added an explicit `webvnc local --security-type vnc` mode that forces standard VNC password authentication when a server advertises account authentication first.
- Fixed coordinator hibernation recovery to preserve unambiguous live bridges while rejecting duplicate or stale restored endpoints.
- Fixed portable Node coordinator startup when the production bundle loads the external CommonJS `ssh2` dependency.
- Fixed CodeSandbox ownership tags, one-shot SDK bridge shutdown, mount-safe root workspace replacement, runtime-only resume responses, and authenticated preview URLs, preventing lifecycle rejection, command hangs, archive-sync failures, and unusable private port links.
- Hardened runtime-adapter relays with end-to-end absolute deadlines, durable generation-scoped dispatch fences retained across ambiguous connector failures, atomic owner-only legacy cleanup, rejection of unfenced proxy deletes, per-owner in-flight quotas, post-cancellation accounting, response-delivery grace, connector-matched request validation, restart-safe TTL-first live-bridge revocation, retry-safe upstream rejection handling, generation-fenced confirmed-absence acknowledgments, and cleanup-fenced workspace bindings.
- Fixed Cloudflare Dynamic Workers lifecycle reads, compatibility identity, bundle validation, and live-smoke credential isolation.
- Fixed Windows local-container sync to avoid unusable WSL command shims, support Docker Desktop mount roots, and fall back to native rsync when WSL lacks native SSH tooling. Thanks @brokemac79.
- Fixed brokered Tailscale cleanup to avoid privileged deletion from client-posted device IDs, preserve connectivity across normal reboots, and fail live preflight on application-level errors.
- Fixed Crabfleet workspaces to use any configured brokered provider and route the OpenClaw deployment through its canonical OAuth host and verified AWS backend with isolated, ephemeral key-only SSH access, stock-image cloud-init, and readiness-gated, pinned, Workers-compatible terminal attachment.
- Kept controller-acknowledged post-acquire failures behind the durable provider-release gate, accepted coordinator token-command authentication in outbound adapters, dispatched relay requests concurrently with reserved delete capacity and disconnect cancellation, held auto-selected local WebVNC ports under host-wide lifetime reservations across workspace daemons, and made Windows controller sidecar replacement/removal write-through durable.
- Made controller create/delete durability acknowledgments retryable, durably gated the complete raw acquisition identity and exact returned coordinator adapter/workspace binding before readiness, retained started pre-acknowledgment attempts through stable-absence or exact-identity recovery cleanup, moved ready identity drift into expected-identity cleanup without first-adopting later resolve output, retained terminal desktop revocation intent until the stopping transition persists, deferred coordinator deregistration and claim/routing removal until stable provider absence, loaded exact persisted external routing for controller inspect/inventory/stop even without a claim, required raw external release attestations including declarative raw acquire/resolve `json-lease` output, complete declarative and protocol-command inventory, and an exact `cloudId` argument in every declarative release command, fsynced external routing temporaries before rename plus the installed directory and full ancestor chain afterward, made confirmed-absence claim/routing/reservation deletions directory-durable before terminal acknowledgment, boot-bound Linux slug-reservation owners to the kernel boot ID plus PID/start ticks, required full WebVNC provider identity checks, ignored unrelated partial inventory while failing closed on partial target matches, failed closed on oversized inventory without repeating successful release, gated startup child recovery on a directory-synced state snapshot, suppressed ordinary registered auto-WebVNC daemons during controller child warmup, honored controller policy flag precedence before validating environment duration fallbacks, namespaced direct-SSH WebVNC identities by a domain-separated public controller/provider owner ID while keeping raw owner tokens out of daemon argv, status, and logs, allocated their remote loopback ports under a host-wide lock with occupied-port and bind-collision retries plus exact chosen-port persistence, bound Linux controller and WebVNC process identities to the current boot plus PID/start/nonce, required exact local listener ownership before direct-SSH credential retrieval, authentication, or viewer URL emission, restricted remote reset termination to the complete persisted process identity, budgeted SSH tunnel readiness across the configured connect timeout plus listener verification, restarted WebVNC after foreground SSH tunnel death, installed noVNC, Websockify, and util-linux in generated Linux desktop bootstraps, honored absolute `XDG_CONFIG_HOME` overrides for external routing state on every platform while rejecting invalid values, used native Windows process APIs for daemon identity checks, and fixed the desktop reset helper to trusted absolute commands.

## 0.30.0 - 2026-06-13

### Added

- Added an idempotent workspace adapter over coordinator leases, with durable owner-scoped lifecycle mapping and truthful capability negotiation for external control planes.
- Added `provider: nvidia-brev` for direct Linux GPU workspaces through the Brev CLI and generated SSH config, including normal Crabbox sync/run access, guarded ownership cleanup, and live `nvidia-smi` smoke coverage. Thanks @coygeek.
- Added a generated provider decision matrix with checked metadata for execution model, access, substrate, GPU fit, lifecycle, cleanup, and provider caveats; docs validation now fails on provider drift. Thanks @coygeek.
- Added confirmed lifecycle actions to portal lease rows, with provider shutdown for coordinator-managed boxes and explicitly metadata-only deregistration for client-managed boxes.
- Added `provider: superserve` for delegated Linux sandbox runs through the Superserve control and data planes, including archive sync, retained leases, ownership-guarded lifecycle operations, and credentialed live smoke coverage. Thanks @coygeek.
- Added `provider: namespace-instance` (`namespace-compute`) for short-lived Namespace Compute Linux leases through `nsc`, including per-lease SSH keys, proxy-backed sync/run, duration safeguards, ownership-filtered cleanup, and guarded live smoke coverage. Thanks @coygeek.
- Added comprehensive guides for deploying the portable Node/PostgreSQL coordinator and integrating private control planes through generic external providers, registered inventory, sharing, and outbound WebVNC.
- Added `provider: linode` for direct Linux SSH leases with per-lease keys, account-bound cleanup, preserved operator tags, interface-aware existing firewalls, and guarded live smoke coverage. Thanks @coygeek.
- Added `provider: windows-sandbox` for disposable native Windows runs through Microsoft Windows Sandbox, including mapped workspace sync, streamed output, timeout and cancellation cleanup, and keep-on-failure inspection. Thanks @zozo123.
- Added `provider: smolvm` for delegated Linux microVM runs through the hosted smolfleet API, including archive sync, retained leases, status, cleanup, and repository-scoped ownership checks. Thanks @zozo123.
- Added guarded SmolVM live E2E coverage for retained reuse, archive replacement, environment forwarding, command exit propagation, diagnostics, and targeted cleanup.
- Added non-mutating Proxmox storage, bridge, pool, template, and cluster inventory readiness diagnostics plus guarded live lifecycle smoke coverage, with safer failed-create and cleanup claim handling. Thanks @coygeek.
- Added direct SSH login helpers for kept Islo sandboxes through the official Islo CLI proxy. Thanks @zozo123.
- Added a portable Node.js and PostgreSQL coordinator runtime with durable pg-boss maintenance jobs, WebSocket bridges, trusted reverse-proxy identity support, container packaging, and the existing Cloudflare Worker/Durable Object runtime preserved as an adapter over the same fleet implementation.
- Added refreshable coordinator bearer authentication through a shell-free JSON argv token command, including HTTP and reconnecting WebSocket bridges behind expiring upstream identity proxies.
### Fixed

- Fixed pond ACL bootstrap to preserve Tailscale HuJSON comments, ordering, trailing commas, and unrelated policy sections while failing closed on ambiguous shapes. Thanks @coygeek.
- Fixed Tailscale bootstrap and cleanup determinism with opt-in pinned static installs, recorded client/device metadata, coordinator preflight smoke coverage, and best-effort device cleanup on release.
- Fixed brokered Tailscale tag-ownership failures to return actionable exact-match and `tagOwners` guidance while preserving the raw API error.
- Fixed managed Linux Tailscale bootstrap to deliver auth keys through stdin instead of exposing them in `tailscale up` process arguments.
- Fixed trusted reverse-proxy identity deployments to support a secret-bound assertion when direct coordinator access cannot be network-isolated.
- Fixed direct VNC and WebVNC SSH forwards to bind explicitly to workstation loopback even when user SSH configuration enables gateway ports.
- Fixed the portal and connected WebVNC desktops to default to the current system appearance by migrating away from legacy two-state browser theme preferences.
- Fixed Cloudflare container runs to fail when streamed stdout or stderr cannot be written instead of silently reporting success after output loss.
- Fixed Proxmox bridge readiness on PVE 8 by falling back to its compatible local-bridge and SDN-vnet inventory filter.

## 0.29.0 - 2026-06-12

### Added

- Added repeatable `--local-container-volume host:container[:ro]` bind mounts for explicit local-container runs. Thanks @anagnorisis2peripeteia.
- Added provider-neutral coordinator registration for direct SSH leases, with owner-scoped inventory and sharing, outbound WebVNC, automatic bridge daemons for kept desktops, and coordinator-safe metadata-only release and expiry.
- Added provider-optional `crabbox pause` and `crabbox resume` lifecycle commands, with Islo sandbox pause/resume support that preserves local lease claims. Thanks @zozo123.
- Added `provider: opensandbox` for delegated Linux sandbox runs through the OpenSandbox API, including archive sync, retained lease reuse, off-argv environment forwarding, status, and cleanup. Thanks @coygeek.
- Added `provider: anthropic-sandbox-runtime` (`srt`) for local one-shot command execution through Anthropic Sandbox Runtime, including filesystem/network policy handoff, doctor checks, config overrides, and live enforcement coverage. Thanks @coygeek.
- Added `provider: hostinger` for direct Linux VPS leases with read-only catalog and payment-method discovery, explicit purchase opt-in, setup-time SSH keys, ambiguous-purchase recovery, stopped-VPS reuse, and stop-only billing-aware release. Thanks @coygeek.
- Added `provider: apple-vz` for full ARM64 Ubuntu VMs through Apple's `Virtualization.framework`, including verified cloud images, secret-safe signed URL handling, loopback VSOCK SSH, retained leases, native helper packaging, failure rollback, and live lifecycle coverage. Thanks @coygeek.
- Added `provider: digitalocean` for direct Linux SSH leases backed by DigitalOcean Droplets, including flat-tag ownership, per-lease SSH keys, docs, and guarded live smoke coverage. Thanks @coygeek.
- Added a delegated Freestyle provider that runs commands in Freestyle VMs through the Freestyle REST API, with env-only authentication, archive sync, and automatic VM cleanup. Thanks @zozo123.
- Added `provider: hyperv` for local Windows VM SSH leases through Microsoft Hyper-V, including differencing-disk provisioning, OpenSSH and MinGit bootstrap, password-less dev-image initialization, retained lease reuse, and cleanup. Thanks @anagnorisis2peripeteia.
- Added an opt-in Islo userspace Tailscale plane with tailnet-aware pond peers, proxy-routed tailnet traffic, and URL-bridge fallback for leases without `--tailscale`. Thanks @zozo123.
- Added `provider: xcpng` for SSH leases on XCP-ng pools through the XenAPI control plane, including template cloning, fresh ISO installs, retained lease reuse, cleanup, diagnostics, and guarded live E2E coverage. Thanks @coygeek.

### Fixed

- Fixed `stop` and `pond release` to preserve claims, SSH credentials, lifecycle metadata, and restart routing when providers intentionally retain reusable stopped resources.
- Fixed external lease commands to reuse each lease's persisted provider routing after the current external configuration changes.
- Fixed `local-container` stop cleanup when a Docker container was removed externally, including stale claim and stored-key removal. Thanks @hxy91819.
- Fixed Apple VZ release artifacts to target macOS 13, bounded guest serial logs without blocking noisy VMs, escaped terminal controls in diagnostics, and preserved retained lease state when helper inventory lookup fails.
- Fixed DigitalOcean capability-tag persistence, provider config visibility and precedence, account-scoped ambiguous Droplet/SSH-key create recovery, retryable cleanup, and unnecessary monitoring-agent installation.
- Fixed Namespace Devbox setup instructions to use the current browser workspace approval flow instead of obsolete token environment variables.
- Fixed XCP-ng XenAPI integer encoding, trusted endpoint configuration, template validation, HVM config-drive attachment, deterministic guest-network selection, retained-lease IP fallback, YAML-safe usernames, collision-resistant ISO runs, required networking for fresh ISO VMs, Windows 11 disk and vTPM requirements, bounded guest-network discovery, failure-recoverable VM ownership, copied-disk and local-key cleanup, generated Windows answer media, pre-boot answer attachment, and bounded ISO E2E cleanup.

## 0.28.0 - 2026-06-11

### Added

- Added `provider: opencomputer` for delegated Linux sandbox runs through the OpenComputer REST API, including archive sync, retained leases, optional burst capacity, status, and cleanup. Thanks @zozo123.
- Added local-container checkpoint forks that launch a fresh Docker lease from a committed checkpoint image while replaying and validating its recorded daemon scope. Thanks @anagnorisis2peripeteia.
- Added opt-in native Docker local-container checkpoints with immutable image identity, daemon-scope-aware verification and deletion, mounted-workspace guards, and live lifecycle coverage. Thanks @anagnorisis2peripeteia.
- Added `provider: morph` for Morph Cloud Linux SSH leases, including snapshot boot, Morph API key/config plumbing, per-instance SSH key retrieval, pause-on-release reuse, and provider docs. Thanks @coygeek.
- Added a built-in Incus provider for local or remote Linux containers and virtual machines, including socket, TLS, and OIDC control-plane authentication, optional SSH proxy devices, retained lease reuse, and live lifecycle verification. Thanks @coygeek.
- Added Tart macOS desktop leases with native Screen Sharing, a token-gated host-side WebVNC bridge, and documented local-network exposure boundaries. Thanks @anagnorisis2peripeteia.
- Added native Azure Windows ARM64 lease support with explicit Windows ARM64 images, Cobalt ARM64 SKU inference, and `CRABBOX_AZURE_WINDOWS_ARM64_IMAGE` broker configuration for ARM64 validation.
- Added persistent Apple Container 1.0 development machines through the local `apple-machine` provider.
- Added local Windows sandbox execution through Microsoft Execution Containers with explicit filesystem, network, DACL-fallback, and Win32k capability controls plus an execution-backed doctor check.

### Changed

- Removed the stale root OpenClaw plugin package and its npm publishing surface; Crabbox releases now version only the Worker package and Go CLI artifacts.
- Expanded release, smoke, installer, provider-contract, cleanup, and race coverage across the CLI, Worker, and provider adapters.

### Fixed

- Fixed kept Tart VMs stopping when the Crabbox command that launched them exited.
- Hardened provider lifecycle ownership, claims, retained-resource metadata, rollback, cleanup timeouts, and partial-failure reporting across Apple Container, ASCII Box, AWS, Azure, Azure Dynamic Sessions, Blacksmith Testbox, Cloudflare, Daytona, Docker Sandbox, E2B, exe.dev, external providers, GCP, Hetzner, Islo, Local Container, Modal, Multipass, Namespace, Parallels, Proxmox, Railway, RunPod, Semaphore, Sprites, SSH, Tart, Tenki, Tensorlake, Upstash Box, and Weights & Biases.
- Fixed static SSH requested slugs, delegated synthetic lease IDs, provider bridge targets, service inventory pagination, Windows share validation, and provider-specific configuration validation.
- Fixed Linux and macOS developer-tool installers, AWS account and orphan guards, image-minting and WSL2 smoke cleanup, coverage isolation, live-smoke JSON handling, and release workflow tag checkout ordering.
- Fixed CI deadcode, script sandboxing, and Cloudflare cleanup race failures found during release validation.

## 0.27.0 - 2026-06-09

### Added

- Added ordered declarative external lifecycle steps with optional acquire rollback, allowing multi-command private provider setup without shell wrappers.

## 0.26.1 - 2026-06-09

### Added

- Added declarative `external.lifecycle` command configuration, provider resource-name mapping, and coordinator-free WebVNC over SSH for deterministic private devbox CLIs.
- Added Podman runtime compatibility for `provider: local-container`, including runtime selection, provider flags on SSH commands, and Podman-safe local lease claim scopes. Thanks @sallyom.
- Added `sync.include` / `sync.includes` whitelists for root-relative sync plans, SSH sync, native Windows sync, local Actions hydration, and archive-sync providers. Thanks @anagnorisis2peripeteia.
- Added generic `kubevirt` SSH leases and a versioned `external` executable provider so private or proprietary VM/devbox control planes can integrate through configuration without provider-specific Crabbox forks.
- Added Tenki to the live provider smoke harness, including authenticated create/run coverage and a paused-session check that proves `status --wait` does not resume the sandbox.

### Changed

- Extended GitHub broker login user tokens to 180 days by default, exposed token expiry in login/doctor identity output, and made the lifetime configurable with `CRABBOX_USER_TOKEN_TTL_SECONDS`.
- Added optional GitHub user-token admin allowlists via `CRABBOX_GITHUB_ADMIN_OWNERS` and `CRABBOX_GITHUB_ADMIN_LOGINS`, and removed committed capacity-admin identities from the reusable Worker config.

### Fixed

- Fixed brokered provider doctor output so expired or rejected broker tokens tell maintainers to renew Crabbox login instead of misreporting AWS, Azure, GCP, or Hetzner credential failures.
- Fixed delegated run artifact collection so Blacksmith Testbox can satisfy `--require-artifact` and `--artifact-glob` before one-shot lease cleanup.
- Fixed malformed AWS, Azure, and GCP SSH CIDR configuration to fail closed instead of falling back to broad SSH access. Thanks @coygeek.
- Fixed local-container warmup on Windows by mounting the generated bootstrap directory instead of passing the script inline to Docker. Thanks @anagnorisis2peripeteia.
- Fixed SSH-backed status waits to honor `--wait-timeout` while allowing Tenki readiness probes without resuming paused sessions. Thanks @aki-luxor.
- Fixed Tenki JSON lease listings to expose the Crabbox lease ID instead of an unset numeric provider ID.
- Fixed brokered Azure lease creation to persist in-flight leases before VM provisioning, keep failed creates visible, and sweep orphaned Azure VMs from coordinator maintenance. Fixes https://github.com/openclaw/crabbox/issues/215.
- Fixed brokered lease release races so leases released while provisioning cannot be reactivated or lose cleanup retry state.
- Fixed Islo provider status, streaming exec, archive upload, share, and delete handling for the current Islo API contract. Thanks @zozo123.
- Restricted shared `use` viewers from mutating lease heartbeat or Tailscale metadata, and hardened archive sync for option-like filenames while preserving sync cancellation. Thanks @zozo123.

### Removed

## 0.26.0 - 2026-06-02

### Added

- Added `provider: multipass` for local Ubuntu VM SSH leases through Canonical Multipass, including cloud-init bootstrap, Crabbox sync/run lifecycle, cleanup, and cache-volume support. Thanks @jwmoss.

### Changed

### Fixed

- Fixed the README latest-release badge to use Badgen so GitHub release status does not depend on Shields' token pool. Thanks @zozo123.

### Removed

## 0.25.0 - 2026-06-01

### Added

- Added `provider: apple-container` for local Apple silicon macOS Linux leases, including SSH sync/run lifecycle and provider-backed cache volumes. Thanks @zozo123.
- Added a repo-local Blacksmith Testbox workflow and Crabbox config so delegated Testbox validation has workflow/job defaults.
- Added `crabbox prewarm` to lease and hydrate reusable test-ready boxes from configured GitHub Actions, with provider-owned handling for delegated runners such as Blacksmith Testbox.
- Added broker ready pools for hydrated reusable leases, including `prewarm --pool`, `run --pool`, `pool ready/register/borrow/return/ensure`, and the broker ready-pool API.
- Added `crabbox doctor --all --prepare-check` to report provider matrix readiness, resolved test machine types, and hydration workflow/job setup without creating leases.
- Added `crabbox webvnc daemon list` to show alive and stale local WebVNC helper daemons after agent runs.

### Changed

- Raised the coordinator fleet-wide and org-wide reserved monthly caps while keeping per-owner and active lease limits in place, so trusted operators are not blocked by stale reserved-cost accounting.
- Tuned XFCE/WebVNC desktops for smoother interactive use with low-latency `x11vnc`, 60fps WayVNC, and low-compression noVNC defaults.
- Updated Go and Worker dependencies, including Wrangler, Vitest, oxlint, Cloudflare Workers types, AWS SDK, Daytona SDK, Google API modules, OpenTelemetry, and the Go toolchain.

### Fixed

- Fixed GNOME desktop leases to follow the same persisted light/dark theme selection as XFCE, including GTK settings, panel restart, and browser color-scheme flags.
- Fixed GNOME theme toggles to restart the desktop panel inside the active session so the top and bottom bars stay visible.
- Fixed WebVNC GNOME theme switching on existing leases without the dynamic helper, including black GNOME Terminal profiles for dark mode.
- Fixed GNOME WebVNC terminal title bars to follow light/dark theme changes by updating labwc window decorations.
- Fixed GNOME WebVNC terminal menubars to follow light/dark theme changes and added a generated desktop background for GNOME sessions.
- Fixed XFCE desktop leases to drag and resize windows opaquely instead of using the wireframe destination box, with full move/resize opacity and XFWM compositing disabled for the Xvfb/VNC path.
- Fixed Apple Container bootstrap on hosts whose runtime does not inherit DNS by passing detected host resolvers while preserving explicit `--apple-container-extra-run-args --dns` overrides.
- Fixed Apple Container runs to fail as soon as the container exits during SSH bootstrap and include a short container log tail instead of waiting for the full SSH timeout.
- Classified Blacksmith Testbox cleanup, sync-marker, cancelled Actions, and post-ready stall failures as retryable infra stages instead of generic unknown failures.
- Fixed Azure VM provisioning so slow creates time out quickly, continue through SKU/region fallback, and use a Worker Azure region list separate from AWS regions.
- Fixed local Actions hydration after warmup SSH port fallback so prewarmed SSH-backed boxes reuse the resolved reachable endpoint instead of retrying the configured port.

### Removed

- Removed the stale root OpenClaw plugin package and its npm publish surface.

## 0.24.0 - 2026-05-31

### Added

- Added provider-backed cache volumes for rebuildable dependency caches, including `cache.volumes`, `CRABBOX_CACHE_VOLUMES`, repeatable `--cache-volume [name=]key:path`, `crabbox cache volumes`, Blacksmith Testbox sticky-disk forwarding, Local Container Docker volume mounts, and claim-backed required-volume checks for reused leases.

### Fixed

- Scoped the README Release badge to `?event=push` so it reflects tag-push release runs instead of cancelled `workflow_dispatch` runs. Fixes https://github.com/openclaw/crabbox/issues/189. Thanks @zozo123.

## 0.23.0 - 2026-05-30

### Added

- Added `provider: ascii-box` for [ASCII Box](https://box.ascii.dev) Ubuntu sandbox SSH leases, using the documented `box --json` CLI for create/list/status/stop/delete and standard Crabbox SSH sync/run. Thanks @zozo123.
- Added Azure `--azure-os-disk ephemeral-preview` / `azure.osDisk: ephemeral-preview` for opt-in ephemeral OS disk full caching through Azure Compute API `2025-04-01`. Thanks @jwmoss.
- Added configurable capacity-admin owner caps for coordinators that need elevated active lease limits for trusted operators.

### Changed

- Raised the default coordinator monthly budget caps so configured capacity pools are less likely to reject trusted brokered leases before provider quota is reached.

### Fixed

- Fixed brokered Azure Linux lease creation so a stalled coordinator request times out with a concrete cleanup/retry hint instead of sitting silently in the leasing phase for the full coordinator HTTP timeout.
- Fixed brokered Azure Spot VM fallback so `on-demand-after-*` windows bound VM create waits, on-demand retries use separate VM names, and timed-out Spot cleanup is retried from Fleet maintenance.

## 0.22.1 - 2026-05-29

### Added

- Added `--arch arm64` / `architecture: arm64` for Linux ARM leases on Azure and AWS, including Azure Dpsv6/Dpdsv6 and AWS Graviton class fallback plus matching Ubuntu ARM64 image resolution.

### Fixed

- Fixed brokered lease creation diagnostics so long coordinator requests print progress, timed-out create requests do not retry non-idempotent POSTs through curl, and Azure ARM errors preserve the useful conflict message.

## 0.22.0 - 2026-05-29

### Added

- Added `provider: azure-dynamic-sessions` for delegated Linux runs through Microsoft Azure Container Apps custom container Dynamic Sessions, including a Crabbox runner image, archive sync, streaming commands, local claims, status/list/stop, and provider docs. Thanks @zozo123.
- Added `crabbox pond` peer discovery, bridge, and SSH-mesh support for multi-lease networking, including bridge adapters for Cloudflare, E2B, Islo, Modal, Railway, and Tensorlake.
- Added Azure backend routing so `provider: azure` can select `azure.backend: dynamic-sessions` or `--azure-backend dynamic-sessions` while still reporting the canonical `azure-dynamic-sessions` provider.
- Added Islo delegated run session handles so `crabbox run --provider islo --keep --lease-output <file>` returns stable lease metadata and cleanup commands for orchestrators. Thanks @zozo123.
- Added `crabbox init --detect` to scan common Go, Node, Rust, and Makefile project markers and generate a repo-local `jobs.detected` remote check plus matching preflight tools. Thanks @zozo123.

### Fixed

- Fixed Azure VM provisioning to automatically use region-scoped shared VNet/NSG names when a Crabbox-managed base network already exists in another Azure region.
- Fixed brokered Azure regional fallback so region-scoped shared network names are computed per lease instead of mutating the Worker client's configured vnet/NSG names.
- Hardened Azure Dynamic Sessions endpoint validation, claim boundaries, token destinations, missing-response handling, lifecycle edges, shell string preservation, and runner image behavior.
- Fixed Islo run session handles to preserve resolved and claimed slugs, keep explicit lease IDs authoritative, return handles after lease creation, and quote cleanup commands safely.
- Fixed `crabbox stop` to accept `--id <lease>` like every other lease command, and updated the stop hint that `crabbox run` prints so it can be pasted back verbatim. Thanks @edihasaj.
- Fixed lease commands (`run`, `status`, `stop`, `ssh`, `inspect`, `screenshot`, `vnc`, `webvnc`, `actions`, `artifacts`, `checkpoint`, `egress`) to auto-route `--id static_<slug>` ids to `--provider ssh` and restore the original static host from the local lease claim, so static SSH leases no longer require repeating routing flags after `crabbox warmup`.
- Fixed `crabbox init --detect` to run nested detected package checks from the package directory and validate generated preflight tools.
- Fixed Blacksmith Testbox workflow fallback selection so generic Actions hydration workflows are not mistaken for Testbox workflows, and fixed native Windows wrapper commands so PowerShell-based Node bootstraps can run before JavaScript runtime preflight checks.
- Fixed brokered AWS provisioning to compact stale Crabbox SSH ingress after EC2 reports the security group rule limit, then retry the current source rule before failing.
- Fixed coordinator lease cleanup so expired AWS leases whose EC2 instance is already gone still clean provider keys before closing.
- Fixed AWS EC2 Mac host cleanup and selection so stale pending hosts are released by the orphan sweep and hosts with no reported launch capacity are skipped.
- Fixed Worker AWS Linux user-data compression and hardened command/security boundaries found by CodeQL.
- Fixed provider documentation tables to match the registered provider capabilities for Azure, GCP, and Railway.

## 0.21.0 - 2026-05-27

### Added

- Added `--desktop-env gnome` for a GNOME-apps desktop profile on labwc/WayVNC with GNOME Panel taskbars and Xwayland-backed app launches.
- Added native Windows support for GitHub-runner Actions hydration so workflows can prepare Windows leases before Crabbox attaches to the hydrated workspace.
- Added a portable `--os`/`os` lease selector with Ubuntu 26.04 as the preferred Linux image where provider catalogs support it, while preserving explicit provider image overrides.
- Added Azure `capacity.regions` fallback with region-scoped managed network names and Azure capacity hints, matching the AWS capacity-routing model.
- Added a repo-local Crabbox hydrate workflow and documented Azure as the preferred Windows/WSL2 provider when Azure quota or credits are available.
- Added `crabbox run --lease-output <file>` for reusable delegated-run session JSON, starting with Blacksmith Testbox. Thanks @RomneyDa.

### Fixed

- Fixed failed-run summaries so application output mentioning provider auth no longer looks like a provider/auth blocker, shell `&&` command chains explain short-circuit behavior, observed phases identify the likely failed phase, and opt-in automatic JUnit discovery can add structured test failures.
- Fixed Azure Spot VM provisioning to send `billingProfile.maxPrice: -1` explicitly in both direct and brokered mode, keeping Crabbox leases on Spot pricing without price-threshold evictions.
- Fixed coordinator-backed lease creation to wait long enough for slow cloud bootstraps such as Azure Windows/WSL2 before timing out locally.
- Fixed Azure failed-candidate cleanup retries to emit Worker-side progress logs while Azure waits out NIC and public IP dependency locks.
- Fixed brokered Azure region ordering so an explicit request or `CRABBOX_AZURE_LOCATION` is attempted before the coordinator default.
- Fixed native Windows `--fresh-pr` runs so PR checkout, local patch application, and post-bootstrap SSH port changes work over PowerShell.
- Fixed native Windows Actions env handoff so `crabbox run` can consume bash-style hydrate env files and reuse hydrated Node/pnpm paths.
- Fixed AWS coordinator EC2 polling to tolerate transient `InvalidInstanceID.NotFound` after instance creation and to report parsed AWS XML errors.
- Fixed AWS coordinator provisioning retries so wrapped opaque `RunInstances` errors are retried instead of failing immediately.
- Fixed Daytona provider sandbox inventory to use Daytona's cursor-based listing API.
- Removed OpenClaw-specific hosted broker defaults and documentation from the generic Crabbox broker login flow.

## 0.20.0 - 2026-05-26

### Added

- Added default artifact manifests for `crabbox artifacts publish`, plus `crabbox artifacts list` and `crabbox artifacts pull` for URL-backed proof handoff with size and SHA256 verification.
- Added `crabbox providers` to print the registered provider capability matrix, including targets, backend kind, coordinator mode, aliases, and feature flags.
- Added failed-run follow-through output with a compact digest that shows the failed phase, likely area, retryability, next commands, and a short redacted tail.
- Added `crabbox doctor --from-run <run-id>` to load provider, target, class, type, lease, and phase context from recorded run history before diagnostics.
- Added `crabbox logs --tail`, `crabbox events --type`, `crabbox events --phase`, and `crabbox results --failed-only` for faster recorded-run triage.

### Fixed

- Fixed Blacksmith Testbox runs so repo-level env allowlists for SSH-backed providers no longer block delegated Testbox warmup.
- Fixed AWS Linux desktop bootstrap so generated theme helpers include the latest WebVNC desktop styling on fresh leases.
- Fixed AWS Linux desktop bootstrap so existing desktop services are restarted after profile changes instead of leaving stale XFCE/X11 services running under a Wayland profile.
- Changed the experimental Wayland desktop bootstrap to use labwc, giving WebVNC sessions normal draggable, decorated windows instead of Sway tiling defaults.
- Fixed the W&B Sandboxes provider default endpoint to follow the current upstream `api.cwsandbox.com` API host.
- Fixed Linux WebVNC desktop panel styling so status and taskbar items avoid harsh high-contrast borders in dark mode.
- Fixed Linux WebVNC terminal windows so the XFCE Terminal menu bar follows the dark desktop theme.

## 0.19.0 - 2026-05-25

### Added

- Added `provider: wandb` for W&B/CoreWeave Sandbox delegated runs through the native gRPC API. Thanks @zozo123.
- Added AWS doctor capacity readiness checks that surface Spot and On-Demand vCPU quota pressure before warmup. Thanks @jwmoss.
- Added an experimental Linux `--desktop-env wayland` profile using labwc, WayVNC, Wayland browser launch env, and `grim` screenshots while keeping XFCE as the default desktop.

### Fixed

- Fixed coordinator-backed AWS SSH ingress so active lease source CIDRs are preserved through provider-owned access reconciliation instead of core AWS special cases. Thanks @obviyus.
- Fixed coordinator-backed one-shot runs to replace a lease once when SSH drops after sync but before the command starts, stopping the stale lease and retrying sync on the replacement.
- Fixed Linux desktop theme setup so WebVNC sessions install and prefer native Arc-Dark/other dark XFCE themes instead of custom-painting panel and window chrome.
- Fixed Linux WebVNC desktop sessions so they follow the portal light/dark toggle and system theme changes after the remote desktop has already connected.
- Fixed run failure summaries and timing JSON to classify likely blocked stages, redact known HTML auth challenge bodies from failure excerpts, and reject unsupported Blacksmith environment forwarding before warmup.
- Fixed desktop browser launches so Linux WebVNC browser sessions inherit the dark desktop theme, advertise dark color-scheme preference to web apps, and repair older managed browser wrappers before launch.

## 0.18.0 - 2026-05-23

### Added

- Added `provider: upstash-box` for delegated Upstash Box sandbox runs through the Box REST API, including archive sync, `run`, `warmup`, `list`, `status`, `stop`, config/env overrides, and provider docs.

### Fixed

- Fixed portal and documentation theme toggles so dark mode shows only the sun icon and light mode shows only the moon icon.
- Fixed remote Parallels hosts so `prlctl` is found on standard Mac install paths, and made snapshot fork dry-runs reject non-forkable power-on snapshots consistently.

### Changed

- Changed Linux desktop/WebVNC leases to seed and apply XFCE, GTK, GSettings, and terminal dark theme settings, and changed the portal theme toggle to preserve a system-synced mode.

## 0.17.1 - 2026-05-22

### Added

- Added `crabbox run --emit-proof` support for Blacksmith Testbox delegated runs, including bounded local stdout/stderr, timing, and metadata artifacts for successful proof runs.
- Added local-container Docker socket pass-through with host-visible work roots so `provider: docker` leases can run Docker-based test suites through the host daemon.

### Fixed

- Fixed local-container Docker socket pass-through on Docker Desktop, OrbStack, Colima, and similar local VM runtimes by mounting the daemon-visible socket path instead of the client context socket path.
- Fixed local-container Docker socket sync on local VM runtimes that reject rsync mtime updates on host-mounted work roots.
- Fixed local-container Docker socket bootstrap to prefer Docker's current Debian/Ubuntu CLI package before falling back to distro `docker.io`.
- Fixed `crabbox cleanup --provider docker` support for stale local-container leases.
- Fixed `provider: docker` stop/release cleanup so host-visible per-lease work directories created for Docker socket pass-through are removed with the lease.
- Fixed local Actions hydration for repo-local composite actions, cache no-ops, simple input conditions, safe `hashFiles`, secret-expression rejection, and Node 24.x setup on minimal Debian images.
- Fixed Parallels linked-clone provisioning to require an explicit source snapshot so `prlctl` cannot create a template-side linked-clone snapshot implicitly.

## 0.17.0 - 2026-05-21

### Added

- Added `provider: parallels` for local and remote Mac Parallels Desktop fleets, including template and snapshot-backed cloning, direct checkpoints, desktop/VNC forwarding, and Linux, macOS, and Windows guests.
- Added `provider: runpod` for RunPod public TCP SSH leases through the RunPod REST API, including Crabbox sync/run, `crabbox ssh`, `crabbox doctor`, and provider docs. Thanks @zozo123.
- Added a thin macOS developer-tools image mint wrapper that keeps paid host allocation explicit while wiring the reusable prep script, promotion, checkpoint proof, and lifecycle evidence defaults.
- Added AWS Linux and Windows developer-image prep scripts plus a guarded mint wrapper for baking Docker, Node 24, pnpm, GitHub CLI, and common developer tooling into fast-booting Crabbox AMIs.
- Added explicit AWS Fast Snapshot Restore promotion support for hot developer-image AMIs via `crabbox image promote --fast-snapshot-restore --fsr-az <az>` and the AWS developer-image mint wrapper.
- Added `crabbox image fsr-status` and the coordinator Fast Snapshot Restore status route for checking live AWS snapshot/AZ state after promotion.
- Added a light/dark mode toggle to the Crabbox documentation site that defaults to the system color scheme, persists the choice in local storage, and applies before first paint to avoid a flash.
- Added `provider: local-container` with `docker`, `container`, and `local-docker` aliases for local Linux container leases and optional desktop/browser/WebVNC smoke boxes through Docker-compatible runtimes such as Docker Desktop, OrbStack, and Colima.

### Changed

- Changed the portal lease table filter bar from a long single-choice pill list to grouped state, provider, OS, kind, and admin ownership selectors.
- Changed the macOS developer-tools mint wrapper to default to a full Xcode macOS 15 / Swift 6.2 toolchain on newer EC2 Mac host families, while keeping CLT-only image bakes explicit.

### Fixed

- Fixed direct GCP leases so new VMs set GCP `maxRunDuration` with `DELETE` for the TTL hard cap, install a guest-side idle expiry guard for expired ready/active leases when possible, and `crabbox cleanup --provider gcp` removes stale local GCP claim files after provider inventory no longer contains the lease.
- Fixed Windows developer-image bootstrap readiness so setup completion is written before restarting SSH and native Windows bakes wait for a stable SSH window before continuing.
- Fixed the Windows developer-image mint wrapper so the final PowerShell prep chunk decodes and runs inline instead of relying on a separate post-upload command.
- Fixed Windows developer-image prep so Docker Engine installation is deferred until after the required Containers feature reboot.
- Fixed Windows developer-image bakes so the Docker Containers feature can interrupt SSH without aborting the image mint, as long as the reboot marker is present.
- Fixed Windows developer-image warmup proof so the mint wrapper keeps the source lease alive with an SSH command instead of waiting on stale coordinator readiness.
- Fixed Windows developer-image prep so fresh Chocolatey and Node shims are visible in the active PowerShell session, and first-pass Docker feature installs exit cleanly before final tool verification.
- Fixed Windows developer-image Docker Engine installation to use static Docker binaries instead of the stale DockerMsftProvider package feed.
- Fixed Windows developer-image AMI prep to reset EC2Launch state before capture so candidate instances run per-lease user data and accept the new Crabbox SSH key.
- Fixed Windows developer-image prep to leave Crabbox-managed OpenSSH in place instead of installing Chocolatey's OpenSSH package over the active lease transport.
- Fixed Windows developer-image minting to retry idempotent prep-script chunk uploads, run long prep through a detached scheduled task, and require a stable post-reboot SSH window before the second prep pass.
- Fixed AWS developer-image bakes behind configured security groups so coordinator heartbeats still refresh the configured Crabbox SSH ports, and aligned the Worker Windows bootstrap ordering with the CLI path.

## 0.16.0 - 2026-05-18

### Added

- Added `provider: exe-dev` for exe.dev VM SSH leases through the exe.dev SSH API, including Crabbox sync/run, `crabbox ssh`, and provider docs.
- Added the Railway delegated provider for redeploying an existing Railway service, streaming build/runtime logs, and reporting deployment status through `crabbox run`, `status`, `stop`, and `list`. Thanks @zozo123.
- Added direct `crabbox doctor` readiness for all built-in providers without creating provider resources.
- Added direct `crabbox doctor --provider exe-dev` readiness through the exe.dev inventory API without creating VMs.
- Added Cloudflare runner readiness to `crabbox doctor --provider cloudflare` so runner URL, auth, and container bindings are checked without creating a sandbox. Thanks @altaywtf.
- Added `crabbox doctor --json`, provider error classification and hints, direct-check timeout/API/mutation labels, optional `--doctor-probe-ssh`, and `scripts/live-doctor-smoke.sh` for maintainer live coverage checks.
- Added `--slug` for `crabbox warmup`, fresh `crabbox run` leases, and `crabbox checkpoint fork`, plus `--label` for human-readable run history/timing metadata.
- Added a light/dark mode toggle to the crabbox portal header that defaults to the system color scheme, persists the choice in local storage, and applies before first paint to avoid a flash.
- Added a reusable macOS developer-tool prep script for image bakes that verifies Command Line Tools, installs Homebrew plus common CLI tooling, activates Node 24/pnpm, and exposes stable SSH-visible tool shims.
- Added an account-guarded EC2 Mac Dedicated Host quota request helper for turning macOS lifecycle smoke quota evidence into a dry-run or explicit AWS Service Quotas request.
- Added a no-spend macOS coordinator remediation audit helper that bundles provider identity, IAM policy, host quota, host allocation dry-run, guarded IAM apply dry-run, and guarded quota request dry-run evidence into `summary.json`.

### Changed

- Changed Actions hydration to run repo workflow setup locally over SSH by default, auto-hydrate `crabbox run` when `actions.workflow` is configured, and keep GitHub self-hosted runner registration behind `--github-runner` fallback.
- Changed AWS macOS AMI selection so newer `mac-m*` EC2 Mac leases use macOS 15 images while `mac2*` and legacy `mac1.metal` continue using launchable macOS 14 images.
- Hardened macOS image lifecycle smoke so source, candidate, and promoted images must expose Command Line Tools-compatible Apple developer tools, Swift, Homebrew, and common Node/pnpm developer tooling before promotion, with stricter macOS 15 and Swift tools 6.2 defaults for `mac-m*` host families.
- Clarified WebVNC docs to include coordinator-backed AWS macOS desktop leases in the supported portal bridge surface.

### Fixed

- Fixed AWS macOS lease bootstrap so EC2 Mac instances explicitly install the Crabbox SSH key, enable Remote Login on configured ports, and treat Screen Sharing as available for WebVNC even when a dedicated host lease predates the `desktop=true` label.
- Fixed AWS WebVNC reconnects so coordinator lease heartbeats refresh SSH ingress from the caller source before local bridge startup retries.
- Fixed the portal so configured AWS macOS Dedicated Hosts appear as lease-like dedicated rows with host detail pages, attached-lease access actions, and local start/WebVNC commands for host-pinned desktop leases.
- Fixed WebVNC daemon restarts so the background bridge keeps its lease claim after a repo checkout changes.
- Fixed macOS WebVNC bridge churn by using a smaller bridge pool for macOS Screen Sharing instead of opening the default multi-slot VNC pool.
- Fixed macOS WebVNC portal performance by using latency-biased noVNC compression and quality defaults for Screen Sharing sessions.
- Fixed WebVNC portal credential failures so bare or stale macOS links stop with a clear status instead of opening a blank retry loop.
- Fixed WebVNC local bridge startup so resolved SSH fallback ports are reused for the foreground VNC tunnel instead of falling back during probes and then tunneling the stale configured port.
- Fixed Railway `crabbox run` redeploys to use Railway's deployment redeploy mutation so live Docker-image services return the new deployment ID reliably.
- Fixed pinned AWS macOS host/image launches so region fallback cannot silently route a candidate image proof onto a different region or host.
- Fixed direct AWS AMI checkpoint create, inspect, delete, and fork paths so source instances are validated before host preparation and recorded account/direct-backend metadata is honored even after coordinator configuration changes.
- Fixed direct AWS macOS AMI checkpoint forks so resolved and recorded EC2 Mac Dedicated Host pins are reused after coordinator routing is disabled.
- Fixed AWS macOS native checkpoint selection so brokered and direct macOS checkpoints use AMI-backed snapshots by default instead of raw EBS snapshot forks that EC2 Mac cannot reliably relaunch.
- Fixed macOS image lifecycle smoke checkpoint forks so EC2 Mac host recycle waits require stable availability and retry once after transient host recycle failures.
- Fixed macOS image lifecycle smoke checkpoint forks so forked macOS leases request desktop/WebVNC metadata before collecting WebVNC evidence.
- Fixed macOS image lifecycle smoke summaries so paid EC2 Mac Dedicated Host allocation failures preserve stderr, blocker text, and remediation guidance instead of writing an empty blocker.
- Fixed EC2 Mac Dedicated Host state parsing so live AWS `DescribeHosts` responses are recognized as reusable by macOS lifecycle smoke instead of falling through to a new host allocation path.
- Fixed existing AWS macOS lease commands so `crabbox run --id ... --target macos` defaults the irrelevant capacity market to On-Demand instead of failing Spot validation before reaching the lease.
- Fixed recursive run artifact globs so `**` works on older Bash without crossing unintended path segments.
- Fixed `crabbox doctor` local tool checks so providers that do not use local SSH/rsync do not fail on those tools.

## 0.15.0 - 2026-05-17

### Added

- Added `crabbox capsule` for local GitHub Actions failure replay manifests, including capture, inspect, replay, promotion, and documentation for how capsules compose with actions hydration and checkpoints. Thanks @zozo123.
- Added AWS macOS support to native `crabbox checkpoint` snapshot/image creation and forks, including host-pin metadata and On-Demand fork defaults.
- Added direct AWS AMI checkpoint creation so non-brokered AWS Linux/macOS leases can use `crabbox checkpoint create --mode native` or `--strategy image` without a coordinator.
- Added `--take-control` for WebVNC portal handoffs so opened browser viewers can automatically become the keyboard and mouse controller after connecting.
- Added `scripts/macos-image-lifecycle-smoke.sh` for guarded AWS EC2 Mac host allocation, source macOS lease boot, WebVNC bridge proof, AMI creation, candidate-image smoke, promotion, promoted-image smoke, cleanup, and durable `summary.json` evidence.
- Added a no-spend macOS host region preflight helper for checking reusable EC2 Mac Dedicated Hosts, dry-run allocation readiness, and Dedicated Mac host quota across configured AWS regions before approving paid allocation.
- Added an account-guarded macOS image lifecycle IAM apply helper for trusted operators remediating coordinator AWS permissions from smoke artifacts, including automatic local AWS profile matching.
- Added parsed IAM policy target details to `crabbox admin providers identity --provider aws --json` so operators know which role or user needs the macOS image lifecycle policy.
- Added provider-scoped admin entrypoints: `crabbox admin providers identity`, `crabbox admin providers policy`, and `crabbox admin hosts` for host lifecycle operations. Existing `admin aws-*` and `admin mac-hosts` commands remain compatibility aliases.
- Added provider-neutral `CRABBOX_HOST_ID` / `hostId` config for host-pinned leases while keeping `CRABBOX_AWS_MAC_HOST_ID` / `aws.macHostId` as AWS compatibility aliases.
- Added provider-neutral coordinator admin routes for host lifecycle and provider identity operations, while keeping the legacy AWS routes as compatibility fallbacks.
- Added compatibility aliases `crabbox admin mac-hosts`, `crabbox admin aws-identity`, `crabbox admin aws-policy`, and `crabbox admin aws-policy --mac-hosts` for existing AWS macOS operator workflows.
- Added a broker-side AWS orphan sweep that periodically scans configured AWS capacity regions from the Durable Object alarm and can terminate confirmed Crabbox-tagged EC2 orphans.
- Added an AWS orphan-audit script for trusted operators to find Crabbox-tagged EC2 instances left behind in old provider accounts after credential or account rotation.
- Added macOS image lifecycle evidence files for host discovery, quota, dry-run, allocation, image creation, image promotion, warmup, host wait, WebVNC daemon startup, WebVNC status, and artifact directories for blocked, partial, and completed runs.
- Added regression coverage for the guarded macOS image lifecycle smoke and configurable WebVNC post-start grace period.

### Changed

- Hardened the macOS image lifecycle smoke so native checkpoint snapshot creation, checkpoint forks, WebVNC proof, and checkpoint cleanup run before candidate-image promotion.
- Hardened the macOS image lifecycle smoke so EC2 Mac Dedicated Host scrubbing, WebVNC daemon cleanup, active portal bridge checks, and Mac host family fallback are covered before image promotion.
- Changed AWS promoted image records to be scoped by target, architecture, server type, and region so macOS AMIs do not become the default image for Linux or Windows leases.
- Changed native checkpoint records to preserve the source provider server type so macOS snapshot forks reuse the matching EC2 Mac host family unless `--type` is explicitly overridden.
- Changed AWS macOS instance fallback candidates to include current Apple silicon Mac host families before the legacy `mac1.metal` fallback.
- Changed EC2 Mac Dedicated Host quota checks to use direct Service Quotas lookups for known Mac host families before falling back to broader quota listing.
- Changed the macOS host preflight and image lifecycle smoke to use the provider-neutral admin host/provider commands and `CRABBOX_HOST_ID` when pinning leases to an allocated host.
- Changed the macOS image lifecycle smoke artifact to include the coordinator provider identity used for IAM remediation.
- Changed macOS image lifecycle smoke blocker commands to use portable evidence filenames with the guarded IAM apply helper for coordinator permission remediation.
- Changed macOS image lifecycle smoke summaries to record artifact-relative evidence paths so published bundles do not expose local checkout paths.
- Changed macOS image lifecycle blocked summaries to include a `blocker.reason` alias for automation that expects a short blocker reason.
- Changed standalone macOS host region preflight blockers to use the guarded IAM apply helper instead of manual account-match shell snippets.
- Updated Go provider SDKs and Worker runtime/toolchain dependencies.
- Documented the AWS account-match and IAM remediation flow for attaching the combined macOS image lifecycle policy to the coordinator role or user.
- Clarified the EC2 Mac host IAM policy, including create-time tag permissions, Dedicated Mac host quota checks, and the split between baseline AWS provider permissions and paid macOS image bake, WebVNC, promotion, and cleanup permissions.
- Clarified AWS security guardrail docs so IAM Access Analyzer external-access analyzers are created in every configured capacity region, while S3 Block Public Access and IAM password policy remain account-level controls.

### Fixed

- Fixed code-scanning findings in container command execution, Worker sanitizers, docs link/build helpers, and JSON error responses.
- Fixed live smoke scripts so provider-specific missing workflow, snapshot, CLI, Python client, or Semaphore config prerequisites fail before allocating resources, and added Sprites coverage to the live provider smoke.
- Fixed live coordinator auth smoke so GitHub-authenticated coordinator identities are accepted and Cloudflare Access credential gaps print an actionable prerequisite error.
- Fixed raw SSH-provider JS package command failures so Crabbox probes obvious `pnpm`, `npm`, `node`, `corepack`, `yarn`, and `bun` entrypoints before syncing and fails with hydration/setup guidance instead of an empty `exit 127` tail.
- Fixed `crabbox webvnc --open` so opened portal links make the lease visible to authenticated org users instead of showing a misleading 404 when CLI auth and browser auth differ.
- Fixed WebVNC portal click forwarding so controller clicks reach the remote desktop while preserving focus and browser context-menu suppression.
- Fixed WebVNC `--take-control` handoff links so the portal keeps retrying the automatic control claim until the opened viewer is registered as an observer.
- Fixed remote macOS screenshots so `crabbox screenshot` captures the Screen Sharing/VNC framebuffer instead of relying on `screencapture` from non-interactive SSH sessions.
- Fixed remote macOS screenshots against no-auth VNC servers by reading the RFB 3.8 security result before framebuffer negotiation.
- Fixed brokered AWS macOS launches so stale host ids, missing Mac hosts, regional AMI gaps, and unavailable default Mac capacity can fall back to usable host, region, image, or alternate Mac host family candidates.
- Fixed brokered AWS macOS launches so newer `mac-m*` Mac host fallback candidates resolve macOS 15 AMIs instead of reusing the earlier Apple silicon macOS 14 AMI query.
- Fixed coordinator-backed macOS lease reuse so follow-up `run`, sync, and image smoke commands use the brokered `/Users/ec2-user/crabbox` work root instead of Linux's `/work/crabbox`.
- Fixed coordinator-backed macOS checkpoint metadata so an auto-discovered provider host id is preserved for snapshot forks.
- Fixed AWS image deletion so scoped promoted macOS images cannot be deleted until another image is promoted.
- Fixed brokered Azure leases so the CLI only sends `azureOSDisk` when the user explicitly configures it, preserving the coordinator default while keeping new Azure leases checkpointable by default. Thanks @jwmoss.
- Fixed managed Windows bootstraps so native Windows leases skip desktop/VNC setup unless `--desktop` is requested, while WSL2 leases keep their Windows core and Linux setup paths separate. Thanks @jwmoss.
- Fixed macOS image lifecycle cleanup and release paths so script-allocated hosts and local WebVNC daemons are stopped after source-only, candidate-only, blocked, partial, and completed runs.
- Fixed macOS image lifecycle cleanup so script-allocated EC2 Mac Dedicated Hosts are released from failure traps when host release is requested.
- Fixed EC2 Mac Dedicated Host allocation and release handling so paid host IDs returned by AWS are not retried in another availability zone after post-allocation describe failures, and failed `ReleaseHosts` results are surfaced instead of reported as released.
- Fixed macOS image lifecycle region-preflight blockers so they preserve guarded IAM helper remediation commands from the region preflight evidence instead of falling back to manual account-match snippets.
- Fixed macOS image lifecycle and host-region preflight blockers so remediation commands use neutral `crabbox` commands and the guarded IAM apply helper instead of embedding local binary paths, checkout paths, or manual account-match snippets.
- Fixed macOS image lifecycle blocked summaries so quota preflight failures, EC2 Mac host dry-run IAM failures, rerun commands, and short `blocker.reason` aliases are preserved in evidence.
- Fixed macOS image lifecycle evidence and artifact summaries so paths are only populated after the matching files or directories are captured.
- Fixed EC2 Mac host dry-run JSON output so AWS authorization failures do not expose raw provider error details in operator logs.
- Fixed EC2 Mac host quota checks so unsupported regional Mac quota resources return an empty quota result instead of a 502 preflight error.
- Fixed missing coordinator Mac host admin endpoints so they report a blocked preflight instead of an empty preflight failure.
- Fixed external macOS AMI promotion so x86 Mac images are keyed by their described architecture instead of defaulting to Apple silicon metadata.
- Fixed provider-neutral admin command errors so older coordinators report the neutral route and the legacy compatibility route that both returned 404.
- Fixed provider-neutral host pin requests and lease records so the public JSON field is `hostId`, while `hostID` remains accepted for compatibility.

## 0.14.0 - 2026-05-15

### Added

- Added `crabbox admin lease-audit` so operators can compare expired brokered AWS lease records against live cloud instance state and fail automation when a record still maps to a live instance.
- Added `crabbox checkpoint` native disk-snapshot checkpoints for brokered AWS, Azure, and GCP Linux leases, optional provider image checkpoints via `--strategy image`, local workspace archives for generic POSIX SSH leases, inspect/list/delete flows, archive restore, and checkpoint forks into fresh leases.
- Added checkpoint audit and cleanup management with `crabbox checkpoint list --verify`, `inspect --verify`, and `prune --older-than`.
- Added `provider: cloudflare` delegated runs for Cloudflare Containers through a Worker runner, including archive sync, warm containers, local claim cleanup, and deployment docs. Thanks @altaywtf.
- Added Cloudflare runner deploy-smoke tooling, CI coverage for the container runner Go module, and redacted `crabbox config show` output for Cloudflare runner auth.
- Added `crabbox list --refresh` so local Cloudflare claims can be checked against live runner state on demand.
- Added brokered provider snapshot/image deletion for AWS EBS snapshots and AMIs, Azure managed disk snapshots and managed images, and GCP disk snapshots and machine images.
- Added Modal and Tensorlake to the top-level provider docs and delegated sandbox configuration examples. Thanks @stainlu.
- Added provider feature flags for workspace checkpoint, fork, restore, and native snapshot capabilities. Thanks @stainlu.

### Changed

- Improved checkpoint documentation with clearer native vs archive distinction, workflow mechanics, security warnings, and command reference examples.

### Fixed

- Fixed delegated Blacksmith Testbox warmup/run flows so successful allocations refresh the coordinator runner portal instead of waiting for a later manual list.
- Fixed Code bridge upstream URL handling so browser-controlled paths cannot select a non-loopback upstream target, and clamped `CRABBOX_AWS_ROOT_GB` parsing to valid `int32` values.
- Fixed `crabbox admin lease-audit --fail-on-live` so recently terminated AWS instances returned by `DescribeInstances` do not fail cleanup automation as live resources.
- Fixed checkpoint archive restores so large archives stream over SSH without buffering the full tarball in memory and unpack through a per-restore remote temp file. Thanks @stainlu.
- Fixed Daytona toolbox archive sync so failed remote extracts still remove the uploaded `/tmp/crabbox-*.tgz` archive. Thanks @stainlu.
- Fixed Islo exec-upload fallback cleanup so failed archive decodes or extracts still remove temporary upload files. Thanks @stainlu.
- Fixed Cloudflare runner URL validation so configured runner URLs cannot include query or fragment components that corrupt API request paths. Thanks @stainlu.
- Fixed Cloudflare stop so missing runner containers prune their stale local claims instead of leaving users to run cleanup manually. Thanks @stainlu.
- Fixed the Crabbox plugin provider schema so current providers and aliases such as `modal`, `tensorlake`, and `cf` can be selected. Thanks @stainlu.
- Fixed coordinator TTL cleanup so provider deletion failures keep leases active with retry metadata instead of silently expiring while cloud instances continue running.
- Fixed direct AWS security-group maintenance so stale Crabbox-owned SSH ingress rules are pruned before adding the current source CIDRs.
- Fixed E2B sync cleanup so remote upload archives are removed even when extraction fails. Thanks @stainlu.
- Fixed Hetzner Cloud server-list parsing so `private_net` arrays from the API no longer break list, doctor, warmup, or reused-run flows. Thanks @muqsitnawaz.
- Fixed installed tagged builds so `crabbox --version` and proof metadata report the Go module build version instead of the development fallback. Thanks @stainlu.
- Fixed Modal sync cleanup so remote upload archives are removed even when extraction fails. Thanks @stainlu.
- Fixed native provider checkpoint creation so AWS, Azure, and GCP snapshot/image checkpoints flush source filesystem writes before calling the provider API.
- Fixed `crabbox actions hydrate --id tbx_...` so Blacksmith Testbox IDs skip owned-cloud runner registration instead of failing on GitHub self-hosted-runner permissions.
- Fixed Tensorlake timing JSON so delegated runs include the lease slug and reused sandboxes preserve the stored claim slug. Thanks @stainlu.
- Fixed Tensorlake workdir validation so broad sandbox paths are rejected before sync or command execution. Thanks @stainlu.

## 0.13.0 - 2026-05-13

### Added

- Added `provider: modal` delegated runs for Modal Sandboxes through the local Modal Python client, including archive sync, env allowlist forwarding, docs, and no-live-credential tests.
- Added `crabbox run --full-resync` / `--fresh-sync` to reset stale remote workdirs before syncing, plus `--env-helper` for reusable profile-backed env wrappers on POSIX SSH leases.
- Added native Windows support for `crabbox run --script` / `--script-stdin` and a real native Windows `--preflight` probe.
- Added configurable `crabbox run --preflight` tool probes via `--preflight-tools`, `CRABBOX_PREFLIGHT_TOOLS`, and `run.preflightTools`.

### Changed

- Improved sync and SSH watchdog output so long quiet syncs and dead SSH waits include concrete retry/replace hints.
- Clarified hosted broker access for non-allowlisted users and documented the minimum self-hosted broker setup. Thanks @alan-mathison-enigma.

### Fixed

- Fixed AWS broker security-group maintenance so stale Crabbox-owned SSH ingress rules are pruned before adding the current source CIDRs. Thanks @obviyus.
- Fixed Proxmox VM bootstrap to wait for the guest IP and bootstrap over SSH after clone/start, avoiding fragile guest-agent exec behavior. Thanks @mine-13-zoom.
- Fixed AWS Windows WSL2 exact `--type` requests so instance families without nested virtualization fail before leasing with a targeted repair hint.
- Fixed coordinator-backed AWS acquisition so readiness failures delete the just-created instance before retrying, while CLI retries still require an explicit cleanup signal.
- Fixed coordinator-backed acquisition so repeated confirmed stale AWS instance cleanups get a larger retry budget instead of failing after the second stale instance.
- Fixed `crabbox code` on leases that fall back from SSH port 2222 to 22, and improved foreground tunnel startup errors to include SSH failure details.
- Fixed `crabbox run --preflight --preflight-tools none` so it prints only the workspace summary without running remote probes.
- Fixed native Windows `crabbox run --preflight` so user and cwd diagnostics are always printed alongside configurable tool probes.
- Fixed native Windows `--script` and `--env-from-profile` uploads so non-ASCII PowerShell source and profile values stay UTF-8 under Windows PowerShell.
- Fixed native Windows `--env-from-profile` uploads so allowed profile values are written relative to the synced workdir and failures include the remote PowerShell error.

## 0.12.0 - 2026-05-12

### Added

- Added Azure native Windows desktop/VNC and Windows WSL2 lease support, matching the AWS Windows capability boundary. Thanks @jwmoss.
- Added `provider: proxmox` for direct Proxmox VE Linux QEMU VM leases, including template clone, cloud-init SSH key injection, guest-agent bootstrap, docs, and cleanup support.
- Added `provider: tensorlake` delegated runs for Tensorlake Firecracker sandboxes through the `tensorlake` CLI, including archive sync, env allowlist forwarding, docs, and live-provider coverage. Thanks @zozo123.
- Added `crabbox run --preflight`, `--capture-stderr`, automatic failure bundles, env-forwarding summaries, and `CRABBOX_PHASE:<name>` timing markers for easier live/provider run debugging.
- Added `crabbox run --keep-on-failure` so failed one-shot runs can leave the exact lease available for SSH inspection until idle/TTL expiry.
- Added `crabbox run --script <file>` and `--script-stdin` so larger remote commands can be uploaded and executed as files instead of quoted shell strings.
- Added `crabbox run --env-from-profile <file>` and repeatable `--allow-env <name>` for redacted, first-class live-secret forwarding from local profile files.
- Added `crabbox run --fresh-pr <owner/repo#number>` for fresh remote GitHub PR checkouts, with optional `--apply-local-patch`.
- Added `crabbox azure login` so direct Azure users can persist the active `az login` subscription, tenant, and location without manually exporting service-principal environment variables. Thanks @galiniliev.
- Added `azure.network` / `CRABBOX_AZURE_NETWORK` so Azure direct leases can SSH through private VNet addresses when using VPN/private-network access. Thanks @galiniliev.
- Added `scripts/proxmox-build-template.sh` to build a Crabbox-ready Ubuntu 24.04 Proxmox template from a public cloud image. Thanks @VACInc.

### Changed

- Changed sync guardrails to count the dirty delta when local changes are present while still printing the full candidate size, making dirty-worktree iteration less noisy.
- Expanded default sync excludes for common generated churn such as `.ignored`, `.vite`, `playwright-report`, `test-results`, and local `.crabbox` log/capture directories, and added top-directory hints for large sync candidates.
- Changed automatic failure-bundle stdout/stderr capture to cap implicit temp logs while still allowing explicit `--capture-stdout` / `--capture-stderr` files for full local streams.
- Documented `--fresh-pr ... --apply-local-patch` as the preferred fast path for PR iteration from noisy local checkouts.
- Documented Azure CLI login setup, private-network SSH selection, and regional constraints for reused Azure VNet/subnet/NSG resources. Thanks @galiniliev.
- Clarified that Blacksmith delegated runs cannot forward CLI-side `--env-from-profile` values and should use workflow-side secrets.
- Documented Islo's `islo ssh --setup` host-alias flow for ad-hoc SSH access to Islo sandboxes. Thanks @zozo123.

### Fixed

- Fixed shared-token coordinator auth so caller-supplied `X-Crabbox-Owner` and `X-Crabbox-Org` headers cannot select the authenticated owner/org. Thanks @Hinotoi-agent.
- Fixed Code, WebVNC, and Egress bridge ticket creation so `use`-shared lease users cannot mint lease-side bridge-agent tickets without manage access. Thanks @Hinotoi-agent.
- Fixed repo-local `env.allow: ["*"]` so it no longer forwards every local environment variable to remote commands. Thanks @Hinotoi-agent.
- Fixed Windows SSH sync by disabling unsupported OpenSSH ControlMaster multiplexing and preferring WSL rsync/path conversion when available. Thanks @galiniliev.
- Fixed Tensorlake slug resolution so stale claims from other providers cannot shadow an active Tensorlake sandbox slug.
- Fixed Sprites and Namespace Devbox work-root validation so broad roots are rejected before create/prepare flows. Thanks @stainlu.
- Fixed Sprites list pagination so missing or repeated continuation tokens fail instead of spinning or accepting malformed pages. Thanks @stainlu.
- Fixed Namespace Devbox prepare error reporting so prepare failures are not hidden behind earlier SSH config fallback errors. Thanks @stainlu.

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
- Added the Access-protected coordinator route `https://broker-access.example.com` for service-token proof and hardened automation.
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
- Moved the deployed coordinator route to the OpenClaw Cloudflare account at `https://broker.example.com` and scoped default broker org/auth settings to `openclaw`.
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

- Updated CLI defaults, docs, examples, and auth guidance to prefer `https://broker.example.com`.
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
