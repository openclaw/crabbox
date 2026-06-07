# Source Map

Read this when:

- checking whether the docs still match the implementation;
- changing a feature that is documented in more than one place;
- writing a release note from source instead of memory.

This page maps user-facing behavior to the files that implement it. The docs are
descriptive; treat these files as the source-backed check before changing any
behavior claim. When a doc and the code disagree, the code wins.

Crabbox has three implementation surfaces:

- **CLI** — Go, under `cmd/crabbox` (entrypoint) and `internal/cli` (commands,
  flags, lease/sync/run logic).
- **Provider adapters** — Go, under `internal/providers/<name>`. Each adapter
  registers a provider and implements either an SSH-lease backend or a
  delegated-run backend. `internal/providers/all/all.go` imports every adapter
  for its registration side effects; `internal/providers/shared` holds common
  helpers.
- **Worker broker** — TypeScript, under `worker/src`. A Cloudflare Worker plus a
  single Fleet Durable Object that brokers leases, runs, usage, and live
  bridges. Only `aws`, `azure`, `gcp`, and `hetzner` can be brokered; everything
  else runs direct from the CLI.

## CLI Surface

- Kong command tree and top-level help: `internal/cli/cli_kong.go`, `internal/cli/app.go`
- Per-command `flag` parsing, shared lease-create flags, and exit helpers:
  `internal/cli/flags.go`, `internal/cli/lease_flags.go`, `internal/cli/errors.go`, `internal/cli/fmt.go`
- Config defaults, YAML keys, env overrides, and per-provider config sections:
  `internal/cli/config.go`, `worker/src/config.ts`
- Target selection (linux/macos/windows) and class maps: `internal/cli/target.go`, `internal/cli/config.go`
- Network target resolution and Tailscale metadata: `internal/cli/network.go`
- Named profiles: `internal/cli/profiles.go`
- `crabbox init` generated repo files (workflow, skill, config): `internal/cli/init.go`, `internal/cli/init_detect.go`
- `login`/`logout`/`whoami` and `config path|show|set-broker`: `internal/cli/auth.go`, `internal/cli/config_cmd.go`
- `azure login` (subscription detection via `az` CLI): `internal/cli/azure_login.go`, `internal/cli/azure_cli.go`
- Doctor checks, broker/provider readiness output, and pond doctor: `internal/cli/doctor.go`, `internal/cli/doctor_pond.go`
- `providers`, `usage`, and admin commands: `internal/cli/providers.go`, `internal/cli/usage.go`, `internal/cli/admin.go`
- Provider image bake/promote/fsr-status/delete: `internal/cli/image.go`, `internal/cli/os_image.go`, `internal/cli/coordinator.go`

## Leases, Slugs, Claims, And Expiry

- Canonical lease IDs (`cbx_<12 hex>`) and per-lease SSH key paths: `internal/cli/lease.go`
- Friendly slug generation, normalization, and collision handling: `internal/cli/slug.go`
- Repo-local claim files and `--reclaim` checks: `internal/cli/claim.go`
- Direct-provider labels, safe label encoding, idle-touch labels, TTL cap math: `internal/cli/provider_labels.go`
- Lease status/inspect/list/share/unshare/stop/cleanup commands: `internal/cli/status.go`, `internal/cli/inspect.go`, `internal/cli/pool.go`, `internal/cli/share.go`, `internal/cli/rescue.go`
- Coordinator client request/response structs, slug lookup, heartbeats, usage, run history: `internal/cli/coordinator.go`, `internal/cli/provider_coordinator.go`
- Lease backend selection (brokered vs direct SSH vs delegated): `internal/cli/provider_backend.go`
- Worker env/request/record types: `worker/src/types.ts`
- Worker lease records, public routes, slug allocation, heartbeat/expiry math, cleanup alarms: `worker/src/fleet.ts`
- Worker lease config coercion and defaults (ttl, idle, ssh port, class): `worker/src/config.ts`
- Worker Tailscale OAuth auth-key minting: `worker/src/tailscale.ts`
- Worker slug generation and provider labels: `worker/src/slug.ts`, `worker/src/provider-labels.ts`

## Providers And Runner Bootstrap

Provider adapters live under `internal/providers/<name>` and each expose
`Name()`, `Aliases()`, and `Spec()` in their `provider.go`. The `Spec.Kind`
field distinguishes an SSH-lease backend (Crabbox provisions and connects to an
SSH-reachable box) from a delegated-run backend (the provider owns sync and run;
there is no SSH lease). `internal/providers/all/all.go` imports them all.

SSH-lease providers:

- AWS (EC2): `internal/providers/aws`, with CLI helpers in `internal/cli/aws.go`, `internal/cli/aws_ssh_cidr.go`, `internal/cli/aws_windows_bootstrap.go`
- Azure VM: `internal/providers/azure`, with CLI helpers in `internal/cli/azure.go`
- Google Cloud (Compute Engine): `internal/providers/gcp`, with CLI helpers in `internal/cli/gcp.go`
- Hetzner Cloud: `internal/providers/hetzner`, with CLI helpers in `internal/cli/hcloud.go`
- Parallels (macOS VM host): `internal/providers/parallels`, with CLI helpers in `internal/cli/parallels.go`
- Proxmox VE: `internal/providers/proxmox`, with CLI helpers in `internal/cli/proxmox.go`
- Static/BYO SSH host: `internal/providers/ssh`, with target mapping in `internal/cli/static.go`
- Local Docker container: `internal/providers/localcontainer`
- Canonical Multipass local Ubuntu VM: `internal/providers/multipass`
- Daytona, exe.dev, Namespace devbox, RunPod, Semaphore, Sprites, Railway:
  `internal/providers/daytona`, `internal/providers/exedev`, `internal/providers/namespace`,
  `internal/providers/runpod`, `internal/providers/semaphore`, `internal/providers/sprites`,
  `internal/providers/railway`

Delegated-run providers (no SSH lease):

- Cloudflare Containers: `internal/providers/cloudflare`, with the Worker runtime in `worker/src/cloudflare-container-runner.ts`
- E2B, Islo, Modal, Tensorlake, Upstash, Blacksmith, W&B:
  `internal/providers/e2b`, `internal/providers/islo`, `internal/providers/modal`,
  `internal/providers/tensorlake`, `internal/providers/upstashbox`,
  `internal/providers/blacksmith`, `internal/providers/wandb`
- Azure Container Apps dynamic sessions (shares the `azure` family, but
  delegated-run): `internal/providers/azuredynamicsessions`, runner image `worker/azure-dynamic-sessions.Dockerfile`

Shared and registration:

- Provider backend interfaces, kinds, coordinator modes, and feature flags: `internal/cli/provider_backend.go`
- Shared backend helpers: `internal/providers/shared`
- Built-in provider registration (imports every adapter): `internal/providers/all/all.go`

Worker-side provider operations (brokered providers only):

- Hetzner: `worker/src/hetzner.ts`
- AWS EC2 (provision, capacity fallback, Mac hosts, orphan sweep): `worker/src/aws.ts`
- Azure / GCP provision and image routes: `worker/src/azure.ts`, `worker/src/gcp.ts`
- Image create/read/delete/promote routing: `worker/src/fleet.ts`, `worker/src/os-image.ts`

Bootstrap:

- CLI cloud-init bootstrap: `internal/cli/bootstrap.go`
- Worker cloud-init bootstrap: `worker/src/bootstrap.ts`

Bootstrap stays intentionally small unless optional lease capabilities are
requested: OpenSSH, CA certificates, curl, Git, rsync, jq, the work root
(`/work/crabbox` on Linux), cache directories, and the `crabbox-ready` readiness
marker. `--desktop` adds Xvfb, a slim XFCE session, and loopback x11vnc;
`--browser` adds Chrome stable with a Chromium fallback; `--code` adds
code-server for the authenticated portal editor. Project runtimes (Go, Node,
pnpm, Docker, databases) are repository-owned setup, usually driven through
Actions hydration or repo scripts.

Provider docs:

- Per-provider feature notes: `docs/features/aws.md`, `docs/features/azure.md`, `docs/features/hetzner.md`, `docs/features/blacksmith-testbox.md`, `docs/features/namespace-devbox.md`, `docs/features/namespace-devbox-setup.md`, `docs/features/semaphore.md`, `docs/features/sprites.md`, `docs/features/daytona.md`, `docs/features/islo.md`, `docs/features/e2b.md`
- Per-provider reference: `docs/providers/README.md` plus one file per provider under `docs/providers/`
- Provider/backend authoring guide: `docs/provider-backends.md`, `docs/features/provider-authoring.md`
- Tailscale contract: `docs/features/tailscale.md`

## Capabilities, Desktop, And Access Bridges

- Desktop/browser/code capability flags, env injection, and VNC checks: `internal/cli/capabilities.go`, `internal/cli/run.go`
- Desktop app launch, terminal, record, proof, input (`click`/`type`/`paste`/`key`): `internal/cli/desktop.go`, `internal/cli/desktop_input.go`, `internal/cli/desktop_proof.go`
- VNC tunnel command: `internal/cli/vnc.go`
- Screenshot capture (incl. direct RFB grab): `internal/cli/screenshot.go`, `internal/cli/rfb_screenshot.go`
- WebVNC portal bridge: `internal/cli/webvnc.go`, `worker/src/portal.ts`, `worker/src/fleet.ts`
- Web code-server portal bridge: `internal/cli/code.go`, `worker/src/portal.ts`, `worker/src/fleet.ts`
- Mediated egress bridge: `internal/cli/egress.go`, `internal/cli/coordinator.go`, `worker/src/index.ts`, `worker/src/fleet.ts`
- Interactive desktop/VNC contract: `docs/features/interactive-desktop-vnc.md`, `docs/features/vnc-linux.md`, `docs/features/vnc-windows.md`, `docs/features/vnc-macos.md`
- Mediated egress doc: `docs/features/egress.md`

## Sync, Execution, Actions, And Results

- Remote command flow, sync/reuse/release, heartbeat lifecycle: `internal/cli/run.go`
- Run subcomponents (fresh PR, env profiles, downloads, capture, scripts, artifacts, failure handling, phases, observability): `internal/cli/run_fresh_pr.go`, `internal/cli/run_env_profile.go`, `internal/cli/run_download.go`, `internal/cli/run_capture.go`, `internal/cli/run_script.go`, `internal/cli/run_artifacts.go`, `internal/cli/run_failure.go`, `internal/cli/run_phase.go`, `internal/cli/run_observability.go`
- Git manifest, rsync plan, fingerprints, and guardrails: `internal/cli/repo.go`
- Sync-plan preview command: `internal/cli/sync_plan.go`
- Native Windows target archive sync and PowerShell command wrapping: `internal/cli/sync_windows_target.go`, `internal/cli/ssh.go`
- SSH command output, direct SSH touch, per-lease known_hosts and ControlMaster config: `internal/cli/ssh.go`, `internal/cli/ssh_cmd.go`
- Named repo-local job orchestration: `internal/cli/job.go`
- GitHub Actions hydrate/register/dispatch bridge: `internal/cli/actions.go`
- Actions-first failure capsules: `internal/cli/capsule.go`
- VM/workspace checkpoints (create/list/inspect/restore/fork/delete/prune): `internal/cli/checkpoint.go`, `internal/cli/checkpoint_native.go`, `internal/cli/checkpoint_store.go`
- Cache list/stats/purge/warm commands: `internal/cli/cache.go`
- Run history/logs/events/attach and retained run logs: `internal/cli/history.go`, `internal/cli/run_recorder.go`, `internal/cli/run_output_events.go`, `internal/cli/runlog.go`, `internal/cli/control_ws.go`
- JUnit result parsing, remote markers, and the `results` command: `internal/cli/results.go`, `internal/cli/results_parse.go`, `internal/cli/results_remote.go`
- Per-run telemetry sampling: `internal/cli/telemetry.go`
- Run timing breakdowns: `internal/cli/timing.go`

## Artifacts, Media, And Pond

- Artifact bundle collect/video/gif/template/publish/list/pull: `internal/cli/artifacts.go`, `internal/cli/artifacts_manifest.go`, `internal/cli/artifacts_publish.go`
- Media preview (trimmed GIF/MP4): `internal/cli/media.go`
- Worker artifact upload endpoint and storage: `worker/src/artifacts.ts`, `worker/src/fleet.ts`
- Pond peer discovery, SSH-mesh forwards, ACL tags, bulk release: `internal/cli/pond.go`, `internal/cli/pond_acl.go`, `internal/cli/pond_bridge.go`, `internal/cli/pond_bridge_doctor.go`, `internal/cli/pond_mesh.go`

## Worker API, Cost, And Operations

- Worker auth and top-level routing: `worker/src/index.ts`, `worker/src/auth.ts`
- HTTP response/request helpers: `worker/src/http.ts`
- GitHub OAuth login flow and user-token issuance: `worker/src/oauth.ts`
- Fleet Durable Object routes and lease/run storage: `worker/src/fleet.ts`
- Browser portal lease detail, bridge status, and run log/event pages: `worker/src/portal.ts`, `worker/src/fleet.ts`
- Usage aggregation, pricing fallback, owner/org limits, and cost guardrails: `worker/src/usage.ts`
- Worker package scripts and dependencies: `worker/package.json`
- Worker deployment config: `worker/wrangler.jsonc`, `worker/wrangler.cloudflare.jsonc`

## Cross-cutting Feature Docs

- Configuration precedence and YAML schema: `docs/features/configuration.md` (code: `internal/cli/config.go`, `internal/cli/config_cmd.go`)
- Jobs: `docs/features/jobs.md` (code: `internal/cli/job.go`)
- Identifiers (lease IDs, slugs, claims, run IDs): `docs/features/identifiers.md` (code: `internal/cli/lease.go`, `internal/cli/slug.go`, `internal/cli/claim.go`)
- Doctor checks: `docs/features/doctor.md` (code: `internal/cli/doctor.go`; readiness API in `worker/src/fleet.ts`)
- Network and reachability: `docs/features/network.md` (code: `internal/cli/network.go`)
- Lease capabilities: `docs/features/capabilities.md` (code: `internal/cli/capabilities.go`)
- Environment forwarding: `docs/features/env-forwarding.md` (logic in `internal/cli/run.go`, `internal/cli/run_env_profile.go`)
- Mediated egress: `docs/features/egress.md`
- Capacity and fallback: `docs/features/capacity-fallback.md` (code: `internal/cli/aws.go`, `worker/src/aws.ts`; class maps in `internal/cli/config.go`)
- Telemetry: `docs/features/telemetry.md` (code: `internal/cli/telemetry.go`)
- Browser portal: `docs/features/portal.md` (code: `worker/src/portal.ts`)
- Capsules: `docs/features/capsules.md` (code: `internal/cli/capsule.go`)
- Checkpoints: `docs/features/checkpoints.md` (code: `internal/cli/checkpoint.go`)
- Pond: `docs/features/pond.md` (code: `internal/cli/pond*.go`)
- Artifacts: `docs/features/artifacts.md` (code: `internal/cli/artifacts*.go`)
- Provider authoring guide: `docs/features/provider-authoring.md` (cross-references `internal/cli/provider_backend.go` and `internal/providers/*`)
- Concepts/glossary: `docs/concepts.md`
- Getting started walkthrough: `docs/getting-started.md`

## Build, CI, Docs, And Release

- Go module and toolchain version: `go.mod`
- Go core coverage gate: `scripts/check-go-coverage.sh`
- CI gate: `.github/workflows/ci.yml`
- Release workflow and Homebrew tap fallback: `.github/workflows/release.yml`
- GoReleaser archives and Homebrew formula config: `.goreleaser.yaml`
- Docs command-surface check, link check, site builder, and Pages deploy: `scripts/check-command-docs.mjs`, `scripts/check-docs-links.mjs`, `scripts/build-docs-site.mjs`, `.github/workflows/pages.yml`
- Live provider smoke coverage: `scripts/live-smoke.sh`
- Live coordinator auth smoke coverage: `scripts/live-auth-smoke.sh`
