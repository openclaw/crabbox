# Features

Feature docs explain what Crabbox can do and how the pieces fit together. Command syntax lives in [../commands/README.md](../commands/README.md).

Read when:

- you want a capability overview;
- you are deciding where a behavior belongs;
- you need the feature-level contract before changing code.

## Foundations

- [Configuration](configuration.md): precedence, YAML schema, profiles, classes, env vars.
- [Identifiers](identifiers.md): lease IDs, slugs, run IDs, claims, and how lookup resolves.
- [Doctor checks](doctor.md): what `crabbox doctor` validates and how to extend it.
- [Network and reachability](network.md): `--network auto|tailscale|public`, port fallback, public/tailnet planes.
- [Lease capabilities](capabilities.md): `--desktop`, `--browser`, and `--code` selection rules.
- [Environment forwarding](env-forwarding.md): name-based env allowlist for the remote command.

## Brokered fleet

- [Coordinator](coordinator.md): brokered leases through Cloudflare Workers and Durable Objects.
- [Browser portal](portal.md): authenticated lease/run UI, detail pages, bridge routes, and runner visibility.
- [Broker auth and routing](broker-auth-routing.md): GitHub login, shared bearer tokens, optional Cloudflare Access, and Worker routes.
- [Auth and admin](auth-admin.md): login/logout/whoami and trusted operator controls.
- [Telemetry](telemetry.md): lightweight Linux load, memory, disk, uptime, and run resource samples.
- [History and logs](history-logs.md): coordinator run records, events, and retained remote output.
- [Cost and usage](cost-usage.md): guardrails, provider-backed pricing, and reporting.
- [Lifecycle cleanup](lifecycle-cleanup.md): release, expiry, keep mode, and direct cleanup.

## Providers

- [Providers](providers.md): provider overview, target matrix, classes, and fallback.
- [Capacity and fallback](capacity-fallback.md): class chains, market spot/on-demand, region/AZ routing.
- [Provider backends](../provider-backends.md): contract reference for backend interfaces and registration.
- [Authoring a provider](provider-authoring.md): step-by-step guide to writing a new provider.
- [AWS](aws.md): EC2 Linux, Windows, WSL2, EC2 Mac, capacity, AMIs, and security groups.
- [Azure](azure.md): Azure Linux/native Windows, shared infra, capacity, and cleanup.
- [Google Cloud](../providers/gcp.md): GCP Compute Engine Linux SSH leases.
- [Hetzner](hetzner.md): Linux-only managed Hetzner behavior, classes, and cleanup.
- [Proxmox](../providers/proxmox.md): direct Proxmox VE Linux QEMU VM clones.
- [Blacksmith Testbox](blacksmith-testbox.md): delegated Testbox backend behavior.
- [Namespace Devbox](namespace-devbox.md): Namespace Devbox SSH leases with Crabbox sync/run.
- [Namespace Devbox setup](namespace-devbox-setup.md): CLI install, auth token profile, and live checks.
- [Semaphore](semaphore.md): Semaphore CI job leases with Crabbox SSH sync/run.
- [Sprites](sprites.md): Sprites microVM SSH leases through `sprite proxy`.
- [Daytona](daytona.md): Daytona SDK/toolbox sandbox leases with optional short-lived SSH access.
- [Islo](islo.md): delegated Islo sandbox runs using the Islo Go SDK.
- [E2B](e2b.md): delegated E2B sandbox runs using E2B sandbox APIs.

## Runners and reachability

- [Tailscale](tailscale.md): optional tailnet reachability for managed Linux leases and static hosts.
- [Mediated egress](egress.md): browser/app egress through an operator machine using the Cloudflare Worker mediator.
- [Runner bootstrap](runner-bootstrap.md): cloud-init, installed tools, SSH port, and readiness.
- [Prebaked runner images](prebaked-images.md): provider-owned image storage and the image/cache/state boundary.
- [Image bake runbook](image-bake-runbook.md): exact AWS bake, candidate smoke, promotion, rollback, and cleanup flow.
- [SSH keys](ssh-keys.md): per-lease keys, provider key cleanup, and local storage.

## Sync, run, and recording

- [Sync](sync.md): Git file-list manifests, rsync, fingerprints, excludes, guardrails, and sanity checks.
- [Jobs](jobs.md): named repo-local warmup, hydrate, run, and cleanup workflows.
- [Actions hydration](actions-hydration.md): let GitHub Actions prepare a runner, then sync local work into that workspace.
- [Interactive desktop and VNC](interactive-desktop-vnc.md): VNC hub, support matrix, tunnel model, and QA boundaries.
- [Artifacts](artifacts.md): screenshots, video, trimmed GIFs, logs, metadata, templates, and PR publishing.
- [Linux VNC](vnc-linux.md), [Windows VNC](vnc-windows.md), [macOS VNC](vnc-macos.md): OS-specific desktop setup and troubleshooting.
- [Test results](test-results.md): JUnit summaries attached to recorded runs.
- [Cache controls](cache.md): inspect, purge, and warm remote package/build caches.

## Integrations

- [OpenClaw plugin](openclaw-plugin.md): agent tools that wrap the CLI.
- [Repository onboarding](repository-onboarding.md): `crabbox init`, repo config, workflow stub, and agent skill.
- [Source map](../source-map.md): implementation files behind documented behavior.

## Command docs

- [doctor](../commands/doctor.md)
- [init](../commands/init.md)
- [warmup](../commands/warmup.md)
- [run](../commands/run.md)
- [history](../commands/history.md)
- [logs](../commands/logs.md)
- [results](../commands/results.md)
- [artifacts](../commands/artifacts.md)
- [cache](../commands/cache.md)
- [status](../commands/status.md)
- [list](../commands/list.md)
- [usage](../commands/usage.md)
- [ssh](../commands/ssh.md)
- [vnc](../commands/vnc.md)
- [inspect](../commands/inspect.md)
- [stop](../commands/stop.md)
- [actions](../commands/actions.md)
- [cleanup](../commands/cleanup.md)
- [config](../commands/config.md)
- [login](../commands/login.md)
- [logout](../commands/logout.md)
- [whoami](../commands/whoami.md)
- [admin](../commands/admin.md)
