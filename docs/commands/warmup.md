# warmup

`crabbox warmup` provisions or leases a remote box and waits until SSH and Crabbox bootstrap plumbing are ready.

```sh
crabbox warmup --class beast
crabbox warmup --provider aws --class beast --market on-demand
crabbox warmup --browser
crabbox warmup --tailscale
crabbox warmup --desktop --browser
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider azure --target windows
crabbox warmup --provider aws --target macos --desktop --market on-demand --type mac2.metal
crabbox warmup --actions-runner
crabbox warmup --provider blacksmith-testbox --blacksmith-workflow .github/workflows/ci-check-testbox.yml --blacksmith-job test
crabbox warmup --provider daytona --daytona-snapshot crabbox-ready
crabbox warmup --provider islo --islo-image docker.io/library/ubuntu:24.04
crabbox warmup --provider ssh --target macos --static-host mac-studio.local
crabbox warmup --provider ssh --target windows --windows-mode normal --static-host win-dev.local --static-work-root 'C:\crabbox' --browser
```

The command returns a stable `cbx_...` lease ID and a friendly slug. Reuse either for subsequent `run`, `status`, `ssh`, `inspect`, and `stop` commands; scripts should keep using the canonical ID.

With `--provider blacksmith-testbox`, the canonical ID is the Blacksmith `tbx_...` ID returned by `blacksmith testbox warmup`; Crabbox still assigns and stores a local slug for reuse.

With `--provider daytona`, the canonical ID is a Crabbox `cbx_...` lease backed
by a Daytona sandbox created from `daytona.snapshot`. `run` uses Daytona
SDK/toolbox APIs; `ssh` mints short-lived Daytona SSH access tokens and redacts
them from output.

With `--provider islo`, the canonical ID is
`isb_<crabbox-sandbox-name>`. Crabbox stores a local slug, but Islo owns sandbox
setup and command execution.

With `--provider ssh`, warmup claims an existing static SSH host instead of
creating cloud capacity. Use `--target macos`, `--target windows
--windows-mode normal`, or `--target windows --windows-mode wsl2` to select the
remote command/sync contract. Native Windows static hosts must already have
OpenSSH Server reachable, PowerShell, Git, `tar`, and a writable
`static.workRoot`. Restart `sshd` after installing Git so new SSH sessions see
the updated PATH.

With `--provider hetzner`, managed provisioning supports Linux only. Hetzner can
run Windows through ISO/snapshot installation flows, but Crabbox does not manage
that path today. Use `--provider aws --target windows` for managed Windows
desktop or WSL2, `--provider azure --target windows` for native Windows
SSH/sync/run, or `--provider ssh --target windows` for an existing Hetzner
Windows host.

With `--provider aws --target windows --windows-mode normal --desktop`, Crabbox
creates a real AWS Windows Server lease. EC2Launch user data installs OpenSSH
Server, Git for Windows, TightVNC Server, a per-lease local administrator named
`crabbox`, and a loopback VNC password retrievable through
`crabbox vnc --id <lease>`.

With `--provider aws --target windows --windows-mode wsl2`, Crabbox still
creates a Windows Server host, then enables WSL, VirtualMachinePlatform, and
HypervisorPlatform, reboots as needed, updates the WSL kernel from the web,
imports an Ubuntu rootfs, and prepares the Linux-side `crabbox-ready` toolchain.
The AWS launch enables nested virtualization and uses C8i, M8i, or R8i instance
families for this mode. Commands and sync then use the POSIX WSL contract.

With `--provider azure --target windows`, Crabbox creates a native Windows
Server lease, uses the Azure VM Agent Custom Script Extension to install
OpenSSH Server and Git for Windows, and configures the `crabbox` user for
SSH/sync/run. Azure Windows does not provision VNC/browser/WSL2.

With `--provider aws --target macos --desktop`, Crabbox launches an EC2 Mac
instance on an already allocated Dedicated Host. Set `CRABBOX_AWS_MAC_HOST_ID`
or `aws.macHostId`, use `--market on-demand`, and expect EC2 Mac host lifecycle
rules to dominate cleanup and cost. The default SSH user is `ec2-user`; the VNC
password printed by `crabbox vnc` is the per-lease macOS account password set by
bootstrap.

On success, `warmup` prints a concise total duration line. Add `--timing-json` to emit a final JSON timing record with provider, lease ID, slug, total duration, and exit code.

Flags:

```text
--provider hetzner|aws|azure|ssh|blacksmith-testbox|daytona|islo
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--profile <name>
--class <name>
--type <provider-type>
--market spot|on-demand
--ttl <duration>
--idle-timeout <duration>
--desktop
--browser
--code
--tailscale
--tailscale-tags <comma-separated tags>
--tailscale-hostname-template <template>
--tailscale-auth-key-env <env-var>
--tailscale-exit-node <name-or-100.x>
--tailscale-exit-node-allow-lan-access
--network auto|tailscale|public
--keep
--actions-runner
--reclaim
--timing-json
--blacksmith-org <org>
--blacksmith-workflow <file|name|id>
--blacksmith-job <job>
--blacksmith-ref <ref>
```

`--idle-timeout` releases the lease after no touch for that duration, default `30m`. `--ttl` remains the maximum wall-clock lifetime, default `90m`.
Warmup records a local claim tying the lease to the current repo; `--reclaim` overwrites an existing local claim for that lease.

`--browser` provisions a known browser binary and records it in
`/var/lib/crabbox/browser.env`. It can be used without `--desktop` for headless
browser automation. Managed Linux tries Google Chrome stable first, then a
Chromium package fallback.

`--desktop` provisions Xvfb, slim XFCE, and loopback-bound x11vnc for visible UI
automation and operator takeover. It does not imply a browser. Use
`--desktop --browser` when a headed browser should run in the visible display.

`--code` provisions `code-server` for Linux leases and enables
`crabbox code --id <lease>` to bridge the workspace through the authenticated
portal at `/portal/leases/<lease>/code/`.

`--tailscale` joins newly created managed Linux leases to the configured
tailnet. `--network` controls the SSH endpoint printed after readiness:
`auto` prefers the tailnet when reachable, `tailscale` requires it, and
`public` forces the provider/public host. Tailscale is a reachability layer, not
a provider; static hosts should put a MagicDNS name or 100.x address in
`static.host` instead. See [Tailscale](../features/tailscale.md).

For AWS, `--market` overrides `capacity.market` for this lease. Use
`--market on-demand` when Spot capacity is blocked or when a quota request was
approved only for the standard On-Demand quota. Explicit `--type` still means
exact type: Crabbox reports quota/capacity/policy failures instead of silently
changing capacity.

`--actions-runner` immediately registers the warm box as an ephemeral self-hosted GitHub Actions runner for the current repository. Most projects should prefer `crabbox actions hydrate --id <lease-id-or-slug>` after warmup because it also dispatches the workflow and waits for the ready marker.

`--actions-runner` is not supported with `blacksmith-testbox` because Blacksmith owns Testbox workflow hydration.

New leases use per-lease SSH keys under the user config directory:

```text
~/.config/crabbox/testboxes/<lease-id>/id_ed25519
```
