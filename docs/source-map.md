# Source Map

Read when:

- checking whether docs match implementation;
- changing a feature that is documented in more than one place;
- preparing a release note from source instead of memory.

This page maps user-facing behavior back to implementation files. Keep docs descriptive; use these files as the source-backed check before changing behavior claims.

## CLI Surface

- Kong command router and top-level help: `internal/cli/cli_kong.go`, `internal/cli/app.go`
- Shared flag parsing and exit helpers: `internal/cli/flags.go`, `internal/cli/errors.go`
- Config defaults, YAML keys, env overrides, target selection, and class maps: `internal/cli/config.go`, `internal/cli/target.go`, `worker/src/config.ts`
- Network target resolution and Tailscale metadata: `internal/cli/network.go`
- `crabbox init` generated repo files: `internal/cli/init.go`
- Login/logout/whoami/config commands: `internal/cli/auth.go`, `internal/cli/config_cmd.go`
- Doctor checks and broker provider-readiness output: `internal/cli/doctor.go`
- AWS image bake/promote commands: `internal/cli/image.go`, `internal/cli/coordinator.go`

## Leases, Slugs, Claims, And Expiry

- Canonical lease IDs and per-lease SSH key paths: `internal/cli/lease.go`
- Friendly slug generation, normalization, provider names, and direct collision handling: `internal/cli/slug.go`
- Repo claim files and reclaim checks: `internal/cli/claim.go`
- Direct-provider labels, safe label encoding, idle touch labels, TTL cap math: `internal/cli/provider_labels.go`
- Coordinator client request/response structs, slug lookup, heartbeats, usage, run history: `internal/cli/coordinator.go`
- Worker env/request/record types: `worker/src/types.ts`
- Worker lease records, public routes, slug allocation, heartbeat expiry math, alarms: `worker/src/fleet.ts`
- Worker Tailscale OAuth auth-key minting: `worker/src/tailscale.ts`
- Worker slug generation and provider labels: `worker/src/slug.ts`, `worker/src/provider-labels.ts`

## Providers And Runner Bootstrap

- Direct Hetzner provider: `internal/providers/hetzner`, with API client helpers in `internal/cli/hcloud.go`
- Direct AWS provider: `internal/providers/aws`, with API client helpers in `internal/cli/aws.go`
- Direct Azure provider: `internal/providers/azure`, with API client helpers in `internal/cli/azure.go`
- Direct Google Cloud provider: `internal/providers/gcp`, with API client helpers in `internal/cli/gcp.go`
- Direct Proxmox provider: `internal/providers/proxmox`, with API client helpers in `internal/cli/proxmox.go`
- Static SSH macOS/Windows provider: `internal/providers/ssh`, with target mapping helpers in `internal/cli/static.go`
- Blacksmith Testbox backend and argument/parsing helpers: `internal/providers/blacksmith`
- Namespace Devbox SSH lease backend and CLI wrapper: `internal/providers/namespace`
- Sprites SSH lease backend and API/CLI wrapper: `internal/providers/sprites`
- Daytona provider backend and SDK/toolbox wrapper: `internal/providers/daytona`
- Islo delegated backend and SDK wrapper: `internal/providers/islo`
- E2B delegated backend and REST/envd wrapper: `internal/providers/e2b`
- Tensorlake delegated backend and `tensorlake` CLI wrapper: `internal/providers/tensorlake`
- Provider backend interfaces, registry, and request/result types:
  `internal/cli/provider_backend.go`
- Built-in provider registration packages:
  `internal/providers/hetzner`, `internal/providers/aws`,
  `internal/providers/azure`, `internal/providers/proxmox`,
  `internal/providers/gcp`, `internal/providers/ssh`, `internal/providers/blacksmith`,
  `internal/providers/namespace`, `internal/providers/daytona`, `internal/providers/islo`,
  `internal/providers/semaphore`, `internal/providers/sprites`, `internal/providers/e2b`,
  `internal/providers/tensorlake`, `internal/providers/all`
- Built-in provider backend implementations:
  `internal/providers/aws`, `internal/providers/azure`, `internal/providers/gcp`,
  `internal/providers/hetzner`, `internal/providers/proxmox`,
  `internal/providers/ssh`, `internal/providers/blacksmith`,
  `internal/providers/namespace`, `internal/providers/daytona`, `internal/providers/islo`,
  `internal/providers/semaphore`, `internal/providers/sprites`, `internal/providers/e2b`,
  `internal/providers/tensorlake`, plus shared helpers in `internal/providers/shared`
- Worker Hetzner provider: `worker/src/hetzner.ts`
- Worker AWS EC2 provider: `worker/src/aws.ts`
- Worker AWS AMI create/read/promote routes: `worker/src/fleet.ts`, `worker/src/aws.ts`
- Provider feature docs: `docs/features/aws.md`, `docs/features/azure.md`, `docs/features/hetzner.md`, `docs/features/blacksmith-testbox.md`, `docs/features/namespace-devbox.md`, `docs/features/namespace-devbox-setup.md`, `docs/features/semaphore.md`, `docs/features/sprites.md`, `docs/features/daytona.md`, `docs/features/islo.md`, `docs/features/e2b.md`
- Provider reference docs: `docs/providers/README.md`, `docs/providers/aws.md`, `docs/providers/azure.md`, `docs/providers/gcp.md`, `docs/providers/hetzner.md`, `docs/providers/proxmox.md`, `docs/providers/ssh.md`, `docs/providers/blacksmith-testbox.md`, `docs/providers/namespace-devbox.md`, `docs/providers/daytona.md`, `docs/providers/islo.md`, `docs/providers/semaphore.md`, `docs/providers/sprites.md`, `docs/providers/e2b.md`, `docs/providers/tensorlake.md`
- Provider/backend authoring guide: `docs/provider-backends.md`
- CLI cloud-init bootstrap: `internal/cli/bootstrap.go`
- Worker cloud-init bootstrap: `worker/src/bootstrap.ts`
- Tailscale feature contract: `docs/features/tailscale.md`
- Desktop/browser/code capability flags, env injection, and VNC checks: `internal/cli/capabilities.go`, `internal/cli/run.go`
- Desktop app launch into visible sessions: `internal/cli/desktop.go`
- VNC tunnel command: `internal/cli/vnc.go`
- WebVNC portal bridge: `internal/cli/webvnc.go`, `worker/src/portal.ts`, `worker/src/fleet.ts`
- Web code portal bridge: `internal/cli/code.go`, `worker/src/portal.ts`, `worker/src/fleet.ts`
- Mediated egress bridge: `internal/cli/egress.go`, `internal/cli/coordinator.go`, `internal/cli/desktop.go`, `worker/src/index.ts`, `worker/src/fleet.ts`, `docs/features/egress.md`
- Desktop screenshot command: `internal/cli/screenshot.go`
- Interactive desktop/VNC contract: `docs/features/interactive-desktop-vnc.md`, `docs/features/vnc-linux.md`, `docs/features/vnc-windows.md`, `docs/features/vnc-macos.md`

Bootstrap is intentionally tiny unless optional lease capabilities are requested:
OpenSSH, CA certificates, curl, Git, rsync, jq, `/work/crabbox`, cache
directories, and `crabbox-ready`. `--desktop` adds Xvfb/slim XFCE/x11vnc and
loopback VNC. `--browser` adds Chrome stable or a Chromium fallback. `--code`
adds code-server for authenticated portal editor access. Project
runtimes such as Go, Node, pnpm, Docker, databases, and services are
repository-owned setup, usually through Actions hydration or repo scripts.

## Sync, Execution, Actions, Cache, And Results

- Remote command flow, sync/reuse/release, heartbeat lifecycle: `internal/cli/run.go`
- Named repo-local job orchestration: `internal/cli/job.go`
- Native Windows target archive sync and PowerShell command wrapping: `internal/cli/sync_windows_target.go`, `internal/cli/ssh.go`
- Git manifest, rsync plan, fingerprints, guardrails: `internal/cli/repo.go`
- Sync plan command: `internal/cli/sync_plan.go`
- SSH command output and direct SSH touch behavior: `internal/cli/ssh.go`, `internal/cli/ssh_cmd.go`
- Per-lease SSH known_hosts and ControlMaster config: `internal/cli/ssh.go`
- GitHub Actions hydrate/register/dispatch bridge: `internal/cli/actions.go`
- Cache stats/purge/warm commands: `internal/cli/cache.go`
- Run history/event/attach/log commands and retained run logs: `internal/cli/history.go`, `internal/cli/run_recorder.go`, `internal/cli/run_output_events.go`, `internal/cli/runlog.go`
- JUnit result parsing and remote markers: `internal/cli/results.go`, `internal/cli/results_parse.go`, `internal/cli/results_remote.go`

## Worker API, Cost, And Operations

- Worker auth and top-level routing: `worker/src/index.ts`, `worker/src/http.ts`
- Fleet Durable Object routes and lease/run storage: `worker/src/fleet.ts`
- Browser portal lease detail, bridge status, and run log/event pages: `worker/src/portal.ts`, `worker/src/fleet.ts`
- Lease config coercion: `worker/src/config.ts`
- Usage, pricing fallback, owner/org limits, cost guardrails: `worker/src/usage.ts`
- Worker package scripts and dependencies: `worker/package.json`
- Worker deployment config: `worker/wrangler.jsonc`

## OpenClaw Plugin

- Plugin metadata and config schema: `package.json`, `openclaw.plugin.json`
- Tool registration and CLI wrapper behavior: `index.js`
- Plugin tests: `index.test.js`
- Plugin feature doc: `docs/features/openclaw-plugin.md`

## Cross-cutting Feature Docs

- Configuration precedence and YAML schema: `docs/features/configuration.md` (config code in `internal/cli/config.go`, `internal/cli/config_cmd.go`)
- Jobs: `docs/features/jobs.md` (orchestration code in `internal/cli/job.go`; config in `internal/cli/config.go`)
- Identifiers (lease IDs, slugs, claims, run IDs): `docs/features/identifiers.md` (code in `internal/cli/lease.go`, `internal/cli/slug.go`, `internal/cli/claim.go`)
- Doctor checks: `docs/features/doctor.md` (code in `internal/cli/doctor.go`;
  coordinator readiness API in `worker/src/fleet.ts`)
- Network and reachability: `docs/features/network.md` (code in `internal/cli/network.go`)
- Lease capabilities: `docs/features/capabilities.md` (code in `internal/cli/capabilities.go`)
- Environment forwarding: `docs/features/env-forwarding.md` (forwarding logic in `internal/cli/run.go`)
- Mediated egress: `docs/features/egress.md` (CLI/Worker bridge for browser/app egress through an operator machine)
- Capacity and fallback: `docs/features/capacity-fallback.md` (code in `internal/cli/aws.go`, `worker/src/aws.ts`, class maps in `internal/cli/config.go`)
- Telemetry: `docs/features/telemetry.md` (code in `internal/cli/telemetry.go`)
- Browser portal: `docs/features/portal.md` (code in `worker/src/portal.ts`)
- Provider authoring guide: `docs/features/provider-authoring.md` (cross-references `internal/cli/provider_backend.go` and `internal/providers/*`)
- Concepts/glossary: `docs/concepts.md`
- Getting started walkthrough: `docs/getting-started.md`

## Build, CI, Docs, And Release

- Go module and toolchain version: `go.mod`
- Go core coverage gate: `scripts/check-go-coverage.sh`
- CI gate: `.github/workflows/ci.yml`
- Release workflow and Homebrew tap fallback: `.github/workflows/release.yml`
- GoReleaser archives and Homebrew formula config: `.goreleaser.yaml`
- Docs command-surface check, link check, site builder, and Pages deployment: `scripts/check-command-docs.mjs`, `scripts/check-docs-links.mjs`, `scripts/build-docs-site.mjs`, `.github/workflows/pages.yml`
- Live provider smoke coverage: `scripts/live-smoke.sh`
- Live coordinator auth smoke coverage: `scripts/live-auth-smoke.sh`
