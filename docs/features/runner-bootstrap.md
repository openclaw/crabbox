# Runner Bootstrap

Read when:

- changing cloud-init;
- debugging machines that never become SSH-ready;
- changing the minimal runner contract or readiness checks.

Brokered cloud runners are Ubuntu machines prepared by cloud-init. They do not need coordinator credentials.

Bootstrap creates:

- the `crabbox` user;
- SSH key-only access;
- SSH on the primary port, default `2222`, and configured fallback ports, default `22`;
- `/work/crabbox`;
- shared package caches.

Bootstrap installs:

- curl and CA certificates;
- Git;
- rsync;
- jq;
- OpenSSH server.

Bootstrap intentionally does not install project language runtimes such as Go, Node, pnpm, Docker, databases, or service dependencies. Those belong in GitHub Actions hydration, devcontainers, Nix, mise/asdf, or repository setup scripts. A brokered machine should not pass readiness until `crabbox-ready` succeeds over SSH.

Interactive desktop tooling is an optional lease profile, not part of the
minimal bootstrap. See [Interactive desktop and VNC](interactive-desktop-vnc.md)
for the planned boundary: Crabbox owns the desktop/VNC machine capability, while
scenario systems own browser automation and proof artifacts.

Tailscale is optional too. `--tailscale` on a managed Linux lease installs the
Tailscale package, joins the configured tailnet, writes non-secret metadata
under `/var/lib/crabbox`, and extends `crabbox-ready` with a bounded 100.x
address check. The bootstrap does not persist the auth key after `tailscale up`.
Brokered leases receive a one-off key from the coordinator; direct-provider
leases read it from `CRABBOX_TAILSCALE_AUTH_KEY`. See [Tailscale](tailscale.md).

Static SSH targets are not bootstrapped by Crabbox. They are assumed to be
operator-managed:

- macOS and Windows WSL2 targets need SSH, `bash`, `git`, `rsync`, and `tar`;
- native Windows targets need OpenSSH, PowerShell, `git`, and `tar`;
- `static.workRoot` must point at a writable directory for that target mode.

For native Windows, install Git before the Crabbox check or restart OpenSSH
Server afterward so new non-interactive SSH sessions inherit Git and `tar` on
PATH.

The CLI prefers the configured SSH port and can fall back through `ssh.fallbackPorts` during early bootstrap or operator-network egress restrictions. Set `ssh.fallbackPorts: []` or `CRABBOX_SSH_FALLBACK_PORTS=none` when the fallback should be disabled. Long term, snapshots or provider images can replace slow cloud-init once the bootstrap contract is stable.

Related docs:

- [Providers](providers.md)
- [Tailscale](tailscale.md)
- [SSH keys](ssh-keys.md)
- [run command](../commands/run.md)
- [doctor command](../commands/doctor.md)
