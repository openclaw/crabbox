# Interactive Desktop And VNC

Read when:

- adding or using browser/UI QA that needs a visible Linux desktop;
- deciding whether Mantis, OpenClaw, or Crabbox owns VNC setup;
- debugging an interactive QA lease that needs operator takeover.

Interactive desktop support belongs in Crabbox. Crabbox owns machine lifecycle,
network reachability, SSH keys, lease expiry, and provider-specific setup.
Scenario systems such as Mantis should ask for the needed machine capability
and then drive browser automation, screenshots, artifacts, and PR comments from
inside that lease.

The intended contract is:

- `crabbox warmup --desktop` leases or reuses a machine with the normal Crabbox
  SSH contract plus a desktop profile;
- `crabbox warmup --browser` leases or reuses a Linux machine with a known
  browser binary for headless automation;
- `crabbox warmup --desktop --browser` combines a visible session with a browser
  for headed automation;
- `crabbox vnc --id <lease>` prints a tunnel command and connection metadata for
  operator takeover, including `managed: true` for Crabbox-created desktops and
  `managed: false` for static host services;
- `crabbox run --id <lease> --desktop -- <command...>` runs UI automation in
  the desktop session;
- `crabbox run --id <lease> --browser -- <command...>` injects browser env
  without requiring a desktop;
- `crabbox desktop launch --id <lease> --browser --url <url>` opens a browser
  or app in the visible desktop and detaches it from SSH;
- desktop services bind to loopback on the runner and are reachable through SSH
  tunnels only;
- `--network tailscale` can move the SSH tunnel endpoint onto the tailnet, but
  managed VNC still binds to `127.0.0.1:5900` on the runner;
- screenshots, traces, videos, and browser profiles remain regular command
  artifacts owned by the caller or repository workflow.

Login and browser profile state are caller-owned. `--browser` only guarantees a
browser binary and env such as `BROWSER` and `CHROME_BIN`; it does not create,
sync, unlock, or migrate a logged-in profile. On managed Linux, a manual login
through VNC persists only for that lease and disappears with the machine unless
the caller stores a profile artifact intentionally. On static macOS or Windows,
the target may already have a logged-in OS browser profile, but Crabbox does not
copy Keychain, DPAPI, cookies, or Chrome sync state across hosts or operating
systems.

For repeatable logged-in tests, the scenario layer should create a named
profile or import app-specific auth state, for example a Playwright storage
state file, from the repository's normal secret flow. Avoid syncing full browser
profile directories between operating systems; browser credentials are often
machine- and user-encrypted.

Crabbox should provision the reusable machine capability:

- Xvfb or a lightweight compositor/display manager;
- a small window manager suitable for browser automation;
- Chrome stable or a Chromium fallback when `--browser` is requested;
- x11vnc or an equivalent VNC server bound to `127.0.0.1`;
- a per-lease VNC password retrieved over SSH by `crabbox vnc`.

Crabbox should not own product-specific scenario logic:

- provider tokens and app credentials;
- Discord, Slack, WhatsApp, email, or OpenClaw workflow setup;
- screenshots that prove a bug before and after a fix;
- PR comments or issue triage.

Those belong to Mantis or the repository workflow. Crabbox's job is to make the
machine debuggable and reproducible.

Security rules:

- never expose VNC directly to the public internet;
- do not expose managed VNC directly on the Tailscale 100.x interface;
- prefer SSH local forwarding such as `localhost:5901 -> 127.0.0.1:5900`;
- generate per-lease VNC passwords for managed desktop leases;
- redact passwords from logs and run records;
- stop desktop services when the lease stops;
- keep the normal TTL and idle-timeout lifecycle in force.

Provider notes:

- Hetzner and AWS brokered Linux leases use cloud-init to install Xvfb, XFCE,
  x11vnc, and optional Chrome/Chromium.
- AWS brokered Windows desktop leases use EC2Launch v2 `enableOpenSsh` for the
  first AWS key-backed foothold. The Crabbox CLI then installs Git for Windows
  and TightVNC, creates a local `crabbox` administrator, stores the per-lease
  password under `C:\ProgramData\crabbox`, enables Windows auto-logon for that
  user, and verifies loopback VNC after the reboot. VNC is reached through the
  SSH tunnel; the security group only needs SSH.
- AWS brokered macOS desktop leases require an allocated EC2 Mac Dedicated Host
  and On-Demand capacity. Bootstrap enables Screen Sharing for `ec2-user` and
  stores the generated password on the instance for `crabbox vnc`.
- Static SSH Linux hosts can participate when the operator accepts responsibility
  for packages and display services.
- Static macOS hosts are existing Macs, not Crabbox-created boxes. They can
  participate when Screen Sharing or another
  VNC-compatible service is already available on `127.0.0.1:5900` over SSH or
  directly on `host:5900`. Credentials are host-managed because Apple Remote
  Desktop authentication still belongs to the target host.
- Static Windows hosts are existing Windows machines, not Crabbox-created boxes.
  They can participate only when the operator already provides a VNC-compatible
  service on `127.0.0.1:5900` for SSH tunneling or, for trusted static networks,
  directly on `host:5900`. Opening Windows requires `--host-managed` because the
  password prompt belongs to the target OS, not Crabbox.
- Blacksmith Testbox can run headless browser automation today, but VNC takeover
  needs a Blacksmith-supported SSH tunnel or connection-info API before Crabbox
  can offer the same `vnc` command there.
- EC2 Mac host allocation, host scrubbing, and the AWS 24-hour host lifecycle
  remain operator concerns; Crabbox only launches onto a host id it is given.

For Mantis, the first consumer should be a Discord QA lane:

1. lease a desktop-capable Linux runner;
2. hydrate OpenClaw and the Discord bot credentials;
3. create a named browser profile;
4. reproduce the baseline and capture screenshots;
5. apply or check out the candidate fix;
6. rerun the same scenario and capture candidate screenshots;
7. attach artifacts and a compact visual summary to the PR.

Related docs:

- [Runner bootstrap](runner-bootstrap.md)
- [Providers](providers.md)
- [Tailscale](tailscale.md)
- [SSH keys](ssh-keys.md)
- [Actions hydration](actions-hydration.md)
