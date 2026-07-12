# Features

Feature docs explain what Crabbox can do and how the pieces fit together. They
cover capability-level contracts: what a feature is, when it applies, and how
the parts interact. Provider choice, adapter implementation, and per-provider
behavior belong in the [Provider Reference](../providers/README.md), not this
index. Command syntax and per-flag reference live in
[../commands/README.md](../commands/README.md).

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

## Coordinator and brokered fleet

- [Runtime adapter stack](runtime-adapter-stack.md): compose `adapter serve`,
  `adapter ingress`, and `adapter connect` behind a fleet UI.
- [Coordinator](coordinator.md): shared broker behavior across Cloudflare
  Durable Object and Node.js/PostgreSQL runtimes.
- [Portable coordinator](portable-coordinator.md): deploy and operate the Node/PostgreSQL runtime on a conventional container platform.
- [Private AWS workspaces](aws-private-workspaces.md): dedicated ECS Fargate coordinator, task-role credentials, SSM-only workspaces, and live canary.
- [Bring your own infrastructure](bring-your-own-infrastructure.md): connect a private control plane through generic providers and optional registered mode.
- [Slurm academic sandboxes](slurm-academic-sandboxes.md): offer Crabbox on campus Slurm clusters through a site-local external adapter before adding a built-in provider.
- [Browser portal](portal.md): authenticated lease/run UI, detail pages, bridge routes, and runner visibility.
- [Broker auth and routing](broker-auth-routing.md): GitHub login, shared bearer
  tokens, trusted proxy identity, optional Cloudflare Access, and public routes.
- [Auth and admin](auth-admin.md): login/logout/whoami and trusted operator controls.
- [Telemetry](telemetry.md): lightweight Linux load, memory, disk, uptime, and per-run resource samples.
- [History and logs](history-logs.md): coordinator run records, events, and retained remote output.
- [Cost and usage](cost-usage.md): guardrails, provider-backed pricing, and reporting.
- [Marketplace credits gateway](marketplace-credits.md): one customer credit balance and smart routing across brokered providers.
- [Lifecycle cleanup](lifecycle-cleanup.md): release, expiry, keep mode, and direct cleanup.

## Runners and reachability

- [Tailscale](tailscale.md): optional tailnet reachability for managed Linux leases and static hosts.
- [Pond](pond.md): group related leases and discover their Tailscale, URL bridge, or SSH-mesh reachability.
- [Mediated egress](egress.md): browser/app egress through an operator machine
  using the coordinator mediator.
- [Runner bootstrap](runner-bootstrap.md): cloud-init, installed tools, SSH port, and readiness.
- [Prebaked runner images](prebaked-images.md): provider-owned image storage and the image/cache/state boundary.
- [Image bake runbook](image-bake-runbook.md): exact AWS bake, candidate smoke, promotion, rollback, and cleanup flow.
- [SSH keys](ssh-keys.md): per-lease keys, provider key cleanup, and local storage.

## Sync, run, and recording

- [Sync](sync.md): Git file-list manifests, rsync, fingerprints, excludes, guardrails, and sanity checks.
- [Jobs](jobs.md): named repo-local warmup, hydrate, run, and cleanup workflows.
- [Station profiles](station-profiles.md): planned supervised workload records, agent profile, and model-access security gates.
- [Agent runtime bridge](agent-runtime-bridge.md): contract for future
  harness-in-the-box HTTP/SSE bridge support under Station.
- [Hermetic agent evidence](hermetic-agent-evidence.md): repo-local pattern for
  separate code/test writer contexts, QA arbitration, required artifacts, and
  Crabbox remote proof collection.
- [Deterministic perf evidence](deterministic-perf-evidence.md): contract for
  future reproducible metric budgets and perf gate evidence.
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
- [verify](../commands/verify.md) — check a signed run receipt
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
- [marketplace](../commands/marketplace.md) — credits gateway and smart routing quote preview
- [admin](../commands/admin.md) — admin operations
- [azure](../commands/azure.md) — Azure-specific commands
