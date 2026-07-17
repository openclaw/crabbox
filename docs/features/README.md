# Features

Crabbox features are reusable capability contracts for configuration, fleet
control, runner access, synchronization, execution, and evidence. Use this page
to understand what Crabbox can do after a provider has supplied the execution
target.

Provider choice, direct/coordinator routing, API-key and credential expectations,
sandbox/provider behavior, adapter-specific caveats, and the generated provider
matrix live in the [Provider Reference](../providers/README.md). Use
`crabbox providers` and `crabbox providers recommend` to inspect the provider
capabilities compiled into your current binary. Exact command syntax and flags
live in the [Command Reference](../commands/README.md).

## Foundations

- [Configuration](configuration.md): precedence, YAML schema, profiles, classes, and env vars.
- [Identifiers](identifiers.md): lease IDs, slugs, run IDs, claims, and how lookup resolves.
- [Doctor checks](doctor.md): what `crabbox doctor` validates and how to extend it.
- [Network and reachability](network.md): `--network auto|tailscale|public`, port fallback, and the public/tailnet planes.
- [Lease capabilities](capabilities.md): `--desktop`, `--browser`, and `--code` selection rules.
- [Environment forwarding](env-forwarding.md): name-based env allowlist for the remote command.

## Fleet control and coordination

- [Runtime adapter stack](runtime-adapter-stack.md): compose `adapter serve`,
  `adapter ingress`, and `adapter connect` behind a fleet UI.
- [Coordinator](coordinator.md): shared broker behavior across Cloudflare
  Durable Object and Node.js/PostgreSQL runtimes.
- [Portable coordinator](portable-coordinator.md): deploy and operate the Node/PostgreSQL runtime on a conventional container platform.
- [Bring your own infrastructure](bring-your-own-infrastructure.md): connect a private control plane through generic providers and optional registered mode.
- [Browser portal](portal.md): authenticated lease/run UI, detail pages, bridge routes, and runner visibility.
- [Broker auth and routing](broker-auth-routing.md): GitHub login, shared bearer
  tokens, trusted proxy identity, optional Cloudflare Access, and public routes.
- [Auth and admin](auth-admin.md): login/logout/whoami and trusted operator controls.
- [Telemetry](telemetry.md): lightweight Linux load, memory, disk, uptime, and per-run resource samples.
- [History and logs](history-logs.md): coordinator run records, events, and retained remote output.
- [Cost and usage](cost-usage.md): guardrails, provider-backed pricing, and reporting.
- [Marketplace credits gateway](marketplace-credits.md): one customer credit balance and smart routing across brokered capacity.
- [Lifecycle cleanup](lifecycle-cleanup.md): release, expiry, keep mode, and direct cleanup.

## Runners and reachability

- [Tailscale](tailscale.md): optional tailnet reachability for managed Linux leases and static hosts.
- [Pond](pond.md): group related leases and discover their Tailscale, URL bridge, or SSH-mesh reachability.
- [Mediated egress](egress.md): browser/app egress through an operator machine
  using the coordinator mediator.
- [Runner bootstrap](runner-bootstrap.md): cloud-init, installed tools, SSH port, and readiness.
- [Prebaked runner images](prebaked-images.md): image storage and the image/cache/state boundary.
- [Image bake runbook](image-bake-runbook.md): exact bake, candidate smoke, promotion, rollback, and cleanup flow.
- [SSH keys](ssh-keys.md): per-lease keys, cleanup, and local storage.

## Sync, execution, and evidence

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
- [Editor handoff](../commands/open.md): prepare a synced lease for an external editor and keep its activity alive.
- [Interactive desktop and VNC](interactive-desktop-vnc.md): VNC hub, support matrix, tunnel model, and QA boundaries.
- [Artifacts](artifacts.md): screenshots, video, trimmed GIFs, logs, metadata, templates, and PR publishing.
- [Linux VNC](vnc-linux.md): Linux desktop setup and troubleshooting.
- [Windows VNC](vnc-windows.md): Windows desktop setup and troubleshooting.
- [macOS VNC](vnc-macos.md): macOS desktop setup and troubleshooting.
- [Test results](test-results.md): JUnit summaries attached to recorded runs.
- [Cache controls](cache.md): inspect, purge, and warm remote package/build caches.
- [Cache volumes](cache-volumes.md): provider-backed persistent cache mounts for rebuildable speed state.

## Integrations

- [Repository onboarding](repository-onboarding.md): `crabbox init`, repo config, workflow stub, and agent skill.
