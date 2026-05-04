# Source Map

Read when:

- checking whether docs match implementation;
- changing a feature that is documented in more than one place;
- preparing a release note from source instead of memory.

This page maps user-facing behavior back to implementation files. Keep docs descriptive; use these files as the source-backed check before changing behavior claims.

## CLI Surface

- Command router and top-level help: `internal/cli/app.go`
- Shared flag parsing and exit helpers: `internal/cli/flags.go`, `internal/cli/errors.go`
- Config defaults, YAML keys, env overrides, target selection, and class maps: `internal/cli/config.go`, `internal/cli/target.go`, `worker/src/config.ts`
- Network target resolution and Tailscale metadata: `internal/cli/network.go`
- `crabbox init` generated repo files: `internal/cli/init.go`
- Login/logout/whoami/config commands: `internal/cli/auth.go`, `internal/cli/config_cmd.go`
- Doctor checks: `internal/cli/doctor.go`
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

- Direct Hetzner provider: `internal/cli/hcloud.go`
- Direct AWS provider: `internal/cli/aws.go`
- Static SSH macOS/Windows provider: `internal/cli/static.go`
- Blacksmith Testbox CLI wrapper: `internal/cli/blacksmith.go`
- Worker Hetzner provider: `worker/src/hetzner.ts`
- Worker AWS EC2 provider: `worker/src/aws.ts`
- Worker AWS AMI create/read/promote routes: `worker/src/fleet.ts`, `worker/src/aws.ts`
- Provider feature docs: `docs/features/aws.md`, `docs/features/hetzner.md`, `docs/features/blacksmith-testbox.md`
- CLI cloud-init bootstrap: `internal/cli/bootstrap.go`
- Worker cloud-init bootstrap: `worker/src/bootstrap.ts`
- Tailscale feature contract: `docs/features/tailscale.md`
- Desktop/browser capability flags, env injection, and VNC checks: `internal/cli/capabilities.go`, `internal/cli/run.go`
- Desktop app launch into visible sessions: `internal/cli/desktop.go`
- VNC tunnel command: `internal/cli/vnc.go`
- WebVNC portal bridge: `internal/cli/webvnc.go`, `worker/src/portal.ts`, `worker/src/fleet.ts`
- Desktop screenshot command: `internal/cli/screenshot.go`
- Interactive desktop/VNC contract: `docs/features/interactive-desktop-vnc.md`, `docs/features/vnc-linux.md`, `docs/features/vnc-windows.md`, `docs/features/vnc-macos.md`

Bootstrap is intentionally tiny unless optional lease capabilities are requested:
OpenSSH, CA certificates, curl, Git, rsync, jq, `/work/crabbox`, cache
directories, and `crabbox-ready`. `--desktop` adds Xvfb/Openbox/x11vnc and
loopback VNC. `--browser` adds Chrome stable or a Chromium fallback. Project
runtimes such as Go, Node, pnpm, Docker, databases, and services are
repository-owned setup, usually through Actions hydration or repo scripts.

## Sync, Execution, Actions, Cache, And Results

- Remote command flow, sync/reuse/release, heartbeat lifecycle: `internal/cli/run.go`
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
- Lease config coercion: `worker/src/config.ts`
- Usage, pricing fallback, owner/org limits, cost guardrails: `worker/src/usage.ts`
- Worker package scripts and dependencies: `worker/package.json`
- Worker deployment config: `worker/wrangler.jsonc`

## OpenClaw Plugin

- Plugin metadata and config schema: `package.json`, `openclaw.plugin.json`
- Tool registration and CLI wrapper behavior: `index.js`
- Plugin tests: `index.test.js`

## Build, CI, Docs, And Release

- Go module and toolchain version: `go.mod`
- Go core coverage gate: `scripts/check-go-coverage.sh`
- CI gate: `.github/workflows/ci.yml`
- Release workflow and Homebrew tap fallback: `.github/workflows/release.yml`
- GoReleaser archives and Homebrew formula config: `.goreleaser.yaml`
- Docs command-surface check, link check, site builder, and Pages deployment: `scripts/check-command-docs.mjs`, `scripts/check-docs-links.mjs`, `scripts/build-docs-site.mjs`, `.github/workflows/pages.yml`
- Live provider smoke coverage: `scripts/live-smoke.sh`
- Live coordinator auth smoke coverage: `scripts/live-auth-smoke.sh`
