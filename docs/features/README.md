# Features

Feature docs explain what Crabbox can do and how the pieces fit together. Command syntax lives in [../commands/README.md](../commands/README.md).

Read when:

- you want a capability overview;
- you are deciding where a behavior belongs;
- you need the feature-level contract before changing code.

Core features:

- [Coordinator](coordinator.md): brokered leases through Cloudflare Workers and Durable Objects.
- [Broker auth and routing](broker-auth-routing.md): GitHub login, shared bearer tokens, optional Cloudflare Access, and Worker routes.
- [Providers](providers.md): provider overview, target matrix, classes, and fallback.
- [AWS](aws.md): EC2 Linux, Windows, WSL2, EC2 Mac, capacity, AMIs, and security groups.
- [Hetzner](hetzner.md): Linux-only managed Hetzner behavior, classes, and cleanup.
- [Blacksmith Testbox](blacksmith-testbox.md): delegated Testbox backend behavior.
- [Tailscale](tailscale.md): optional tailnet reachability for managed Linux leases and static hosts.
- [Runner bootstrap](runner-bootstrap.md): cloud-init, installed tools, SSH port, and readiness.
- [Sync](sync.md): Git file-list manifests, rsync, fingerprints, excludes, guardrails, and sanity checks.
- [Actions hydration](actions-hydration.md): let GitHub Actions prepare a runner, then sync local work into that workspace.
- [Interactive desktop and VNC](interactive-desktop-vnc.md): VNC hub, support matrix, tunnel model, and QA boundaries.
- [Linux VNC](vnc-linux.md), [Windows VNC](vnc-windows.md), [macOS VNC](vnc-macos.md): OS-specific desktop setup and troubleshooting.
- [SSH keys](ssh-keys.md): per-lease keys, provider key cleanup, and local storage.
- [Cost and usage](cost-usage.md): guardrails, provider-backed pricing, and reporting.
- [History and logs](history-logs.md): coordinator run records, events, and retained remote output.
- [Test results](test-results.md): JUnit summaries attached to recorded runs.
- [Cache controls](cache.md): inspect, purge, and warm remote package/build caches.
- [Auth and admin](auth-admin.md): login/logout/whoami and trusted operator controls.
- [Lifecycle cleanup](lifecycle-cleanup.md): release, expiry, keep mode, and direct cleanup.
- [Repository onboarding](repository-onboarding.md): `crabbox init`, repo config, workflow stub, and agent skill.
- [Source map](../source-map.md): implementation files behind documented behavior.

Command docs:

- [doctor](../commands/doctor.md)
- [init](../commands/init.md)
- [warmup](../commands/warmup.md)
- [run](../commands/run.md)
- [history](../commands/history.md)
- [logs](../commands/logs.md)
- [results](../commands/results.md)
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
