# Features

Feature docs explain what Crabbox can do and how the pieces fit together. They cover the
capability-level contract — what a feature is, when it applies, and how the parts interact.
Command syntax and per-flag reference live in [../commands/README.md](../commands/README.md).

Read when:

- you want a capability overview;
- you are deciding where a behavior belongs;
- you need the feature-level contract before changing code.

## Foundations

- [Configuration](configuration.md): precedence, YAML schema, profiles, classes, and env vars.
- [Identifiers](identifiers.md): lease IDs, slugs, run IDs, claims, and how lookup resolves.
- [Doctor checks](doctor.md): what `crabbox doctor` validates and how to extend it.
- [Network and reachability](network.md): `--network auto|tailscale|public`, port fallback, and the public/tailnet planes.
- [Lease capabilities](capabilities.md): `--desktop`, `--browser`, and `--code` selection rules.
- [Environment forwarding](env-forwarding.md): name-based env allowlist for the remote command.

## Brokered fleet

- [Coordinator](coordinator.md): brokered leases through the Cloudflare Worker and its Durable Object.
- [Browser portal](portal.md): authenticated lease/run UI, detail pages, bridge routes, and runner visibility.
- [Broker auth and routing](broker-auth-routing.md): GitHub login, shared bearer tokens, optional Cloudflare Access, and Worker routes.
- [Auth and admin](auth-admin.md): login/logout/whoami and trusted operator controls.
- [Telemetry](telemetry.md): lightweight Linux load, memory, disk, uptime, and per-run resource samples.
- [History and logs](history-logs.md): coordinator run records, events, and retained remote output.
- [Cost and usage](cost-usage.md): guardrails, provider-backed pricing, and reporting.
- [Lifecycle cleanup](lifecycle-cleanup.md): release, expiry, keep mode, and direct cleanup.

## Providers

- [Providers](providers.md): provider overview, target matrix, classes, and fallback.
- [Provider reference](../providers/README.md): per-adapter pages for every registered provider.
- [Capacity and fallback](capacity-fallback.md): class chains, market spot/on-demand, and region/AZ routing.
- [Provider backends](../provider-backends.md): contract reference for backend interfaces and registration.
- [Authoring a provider](provider-authoring.md): step-by-step guide to writing a new provider.

Provider deep-dives that live here in `features/`:

- [AWS](aws.md): EC2 Linux, Windows, WSL2, EC2 Mac, capacity, AMIs, and security groups.
- [Azure](azure.md): Azure Linux, Windows, WSL2, shared infra, capacity, and cleanup.
- [Hetzner](hetzner.md): Linux-only managed Hetzner behavior, classes, and cleanup.
- [Blacksmith Testbox](blacksmith-testbox.md): delegated Testbox runner behavior.
- [Namespace Devbox](namespace-devbox.md): Namespace Devbox SSH leases with Crabbox sync/run.
- [Namespace Devbox setup](namespace-devbox-setup.md): CLI install, auth token profile, and live checks.
- [Semaphore](semaphore.md): Semaphore CI job leases with Crabbox SSH sync/run.
- [Sprites](sprites.md): Sprites microVM SSH leases through `sprite proxy`.
- [Daytona](daytona.md): Daytona SDK/toolbox sandbox leases with optional short-lived SSH access.
- [Islo](islo.md): delegated Islo sandbox runs using the Islo Go SDK.
- [E2B](e2b.md): delegated E2B sandbox runs using the E2B sandbox APIs.

## Runners and reachability

- [Tailscale](tailscale.md): optional tailnet reachability for managed Linux leases and static hosts.
- [Pond](pond.md): group related leases and discover their Tailscale, URL bridge, or SSH-mesh reachability.
- [Mediated egress](egress.md): browser/app egress through an operator machine using the Cloudflare Worker mediator.
- [Runner bootstrap](runner-bootstrap.md): cloud-init, installed tools, SSH port, and readiness.
- [Prebaked runner images](prebaked-images.md): provider-owned image storage and the image/cache/state boundary.
- [Image bake runbook](image-bake-runbook.md): exact AWS bake, candidate smoke, promotion, rollback, and cleanup flow.
- [SSH keys](ssh-keys.md): per-lease keys, provider key cleanup, and local storage.

## Sync, run, and recording

- [Sync](sync.md): Git file-list manifests, rsync, fingerprints, excludes, guardrails, and sanity checks.
- [Jobs](jobs.md): named repo-local warmup, hydrate, run, and cleanup workflows.
- [Actions hydration](actions-hydration.md): let GitHub Actions prepare a runner, then sync local work into that workspace.
- [Capsules](capsules.md): local-first replay manifests for GitHub Actions failures.
- [Checkpoints](checkpoints.md): save, restore, and fork reusable remote workspaces.
- [Interactive desktop and VNC](interactive-desktop-vnc.md): VNC hub, support matrix, tunnel model, and QA boundaries.
- [Artifacts](artifacts.md): screenshots, video, trimmed GIFs, logs, metadata, templates, and PR publishing.
- [Linux VNC](vnc-linux.md), [Windows VNC](vnc-windows.md), [macOS VNC](vnc-macos.md): OS-specific desktop setup and troubleshooting.
- [Test results](test-results.md): JUnit summaries attached to recorded runs.
- [Cache controls](cache.md): inspect, purge, and warm remote package/build caches.
- [Cache volumes](cache-volumes.md): provider-backed persistent cache mounts for rebuildable speed state.

## Integrations

- [Repository onboarding](repository-onboarding.md): `crabbox init`, repo config, workflow stub, and agent skill.
- [Source map](../source-map.md): implementation files behind documented behavior.

## Command docs

### Setup and configuration
- [init](../commands/init.md) — initialize repo config
- [login](../commands/login.md) — authenticate with broker
- [logout](../commands/logout.md) — clear broker token
- [whoami](../commands/whoami.md) — show authenticated user
- [config](../commands/config.md) — show merged config
- [doctor](../commands/doctor.md) — validate prerequisites

### Lease lifecycle
- [warmup](../commands/warmup.md) — provision a warm box
- [run](../commands/run.md) — sync and run a command
- [job](../commands/job.md) — run a named repo job
- [status](../commands/status.md) — show lease status
- [list](../commands/list.md) — list active leases
- [stop](../commands/stop.md) — release a lease
- [cleanup](../commands/cleanup.md) — clean up stale leases

### Workspace management
- [sync-plan](../commands/sync-plan.md) — preview the sync manifest
- [actions](../commands/actions.md) — hydrate from repo workflow setup
- [capsule](../commands/capsule.md) — capture/replay Actions failures
- [checkpoint](../commands/checkpoint.md) — snapshot/restore/fork workspaces
- [cache](../commands/cache.md) — manage remote caches
- [image](../commands/image.md) — manage provider images

### Run observation
- [history](../commands/history.md) — list run history
- [logs](../commands/logs.md) — show run logs
- [events](../commands/events.md) — show run events
- [attach](../commands/attach.md) — attach to an active run
- [results](../commands/results.md) — show test results
- [artifacts](../commands/artifacts.md) — manage run artifacts
- [media](../commands/media.md) — capture screenshots/video

### Interactive access
- [ssh](../commands/ssh.md) — SSH to a lease
- [desktop](../commands/desktop.md) — desktop/input commands
- [vnc](../commands/vnc.md) — native VNC access
- [webvnc](../commands/webvnc.md) — browser-based VNC
- [code](../commands/code.md) — code-server access
- [screenshot](../commands/screenshot.md) — capture screenshots
- [egress](../commands/egress.md) — mediated egress proxy

### Pond and collaboration
- [pond](../commands/pond.md) — peer discovery and lifecycle across a lease group
- [share](../commands/share.md) — share lease access
- [unshare](../commands/unshare.md) — revoke shared access

### Operations
- [inspect](../commands/inspect.md) — detailed lease info
- [providers](../commands/providers.md) — show the provider capability matrix
- [usage](../commands/usage.md) — cost and usage reports
- [admin](../commands/admin.md) — admin operations
- [azure](../commands/azure.md) — Azure-specific commands
