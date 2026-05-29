# warmup

`crabbox warmup` leases a box and waits until it is ready: it provisions (or
claims) a remote machine, waits for SSH plus the Crabbox bootstrap plumbing to
come up, then keeps the lease so later commands can reuse it. Unlike
[`run`](run.md), warmup runs no command — it just hands you a ready box.

```sh
crabbox warmup --class beast
crabbox warmup --provider aws --class beast --market on-demand
crabbox warmup --provider aws --os ubuntu:26.04 --desktop --browser --desktop-env wayland
crabbox warmup --provider azure --class beast
crabbox warmup --provider azure --arch arm64 --class fast
crabbox warmup --browser
crabbox warmup --tailscale
crabbox warmup --slug update-flow-smoke
crabbox warmup --pond alpha --slug db
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider azure --target windows
crabbox warmup --provider aws --target macos --desktop --market on-demand --type mac2.metal
crabbox warmup --actions-runner
crabbox warmup --provider ssh --target macos --static-host mac-studio.example.com
crabbox warmup --provider ssh --target windows --static-host win-dev.example.com --static-work-root 'C:\crabbox' --browser
```

## Output

On success warmup prints two lines on stdout — the lease summary and the ready
SSH endpoint — followed by a total-duration line:

```text
leased cbx_0123456789ab slug=swift-crab provider=hetzner server=... type=... ip=... idle_timeout=30m expires=...
ready ssh=root@... :2222 network=public workroot=/work/crabbox
warmup complete total=42.1s
```

The canonical lease ID is `cbx_...`; the friendly `slug` is an auto-generated
`<adjective>-<noun>` handle (or a normalized `--slug` you requested). Reuse
either with later `run`, `status`, `ssh`, `inspect`, and `stop` commands.
Scripts should prefer the canonical ID. Add `--timing-json` to emit a final
JSON timing record (provider, lease ID, slug, total duration, exit code) on
stderr.

Warmup records a local claim binding the lease to the current repo checkout. Use
`--reclaim` to overwrite an existing claim for that lease.

## Lifetime: TTL and idle timeout

- `--ttl <duration>` is the maximum wall-clock lifetime. Default `90m`.
- `--idle-timeout <duration>` releases the lease after no touch for that long.
  Default `30m`.

## Naming and grouping

`--slug <slug>` requests a human-chosen slug for a new lease. Crabbox normalizes
it and may append a short suffix if an active lease already uses that slug.

`--pond <name>` tags a new lease into a named pond (stored as a reserved
provider label); `crabbox list --pond <name>` filters by it. When combined with
`--tailscale` on a Tailscale-capable provider, the CLI also advertises a
`tag:cbx-pond-<owner>-<name>` ACL tag, and cloud-init refreshes `/etc/hosts.cbx`
plus a managed `/etc/hosts` block every 30 seconds so Tailscale peers in the
same pond resolve each other as `<slug>.cbx`. See
[pond](../features/pond.md) for the one-time ACL snippet and `doctor --pond`
coverage. To let Crabbox install the Tailscale policy rows automatically, set
both `TS_API_KEY` and `CRABBOX_POND_ACL_BOOTSTRAP=1`; `TS_API_KEY` alone is
read-only verification for pond commands.

## Capabilities

Capabilities are opt-in features requested at warm time and validated against
the provider's feature set. See [capabilities](../features/capabilities.md).

- `--desktop` provisions a visible UI and loopback-bound VNC for automation and
  operator takeover. Linux defaults to Xvfb, a slim XFCE, and x11vnc. Use
  `--desktop-env wayland` for the experimental labwc/WayVNC profile on
  Ubuntu 26.04-compatible images, or `--desktop-env gnome` for a GNOME-apps
  profile with GNOME Panel taskbars over labwc/WayVNC (GNOME-profile app
  launches use Xwayland so the panel can list windows). `--desktop` does not
  imply a browser.
- `--browser` provisions a known browser binary and records it in
  `/var/lib/crabbox/browser.env`. It works without `--desktop` for headless
  automation; combine `--desktop --browser` to run a headed browser in the
  visible display. Managed Linux tries Google Chrome stable first, then a
  Chromium package fallback.
- `--code` provisions `code-server` on Linux leases and enables
  [`crabbox code --id <lease>`](code.md) to bridge the workspace through the
  authenticated portal at `/portal/leases/<lease>/code/`.

Reusing a lease later requires matching capability labels.

## OS image

`--os` selects a portable Linux OS image; `ubuntu:26.04` is the default.
Explicit provider image flags and config values still win for exact AMIs, URNs,
image families, or provider image names.

## Capacity and market (AWS)

For AWS, `--market spot|on-demand` overrides `capacity.market` for this lease.
Use `--market on-demand` when Spot capacity is blocked or a quota was approved
only for the standard On-Demand quota. An explicit `--type` always means the
exact type: Crabbox reports quota/capacity/policy failures instead of silently
switching capacity.

## Networking

`--tailscale` joins newly created managed Linux leases to the configured
tailnet. `--network auto|tailscale|public` controls the SSH endpoint printed
after readiness: `auto` prefers the tailnet when reachable, `tailscale`
requires it, and `public` forces the provider/public host. Tailscale is a
reachability layer, not a provider — static hosts should put a MagicDNS name or
`100.x` address in `static.host` instead. See
[Tailscale](../features/tailscale.md).

## Provider notes

### ssh (static / bring-your-own host)

With `--provider ssh`, warmup claims an existing host instead of creating cloud
capacity. Use `--target macos`, `--target windows --windows-mode normal`, or
`--target windows --windows-mode wsl2` to select the remote command/sync
contract. Native Windows static hosts must already have OpenSSH Server
reachable, PowerShell, Git, `tar`, and a writable `static.workRoot`. Restart
`sshd` after installing Git so new sessions see the updated PATH.

### hetzner

Managed Hetzner provisioning supports Linux only. For managed Windows, use
`--provider aws --target windows` or `--provider azure --target windows`; for an
existing Hetzner Windows host, use `--provider ssh --target windows`.

### aws — Windows

`--provider aws --target windows --windows-mode normal --desktop` creates a real
AWS Windows Server lease. EC2Launch user data installs OpenSSH Server, Git for
Windows, TightVNC Server, a per-lease local administrator named `crabbox`, and a
loopback VNC password retrievable through `crabbox vnc --id <lease>`.

`--provider aws --target windows --windows-mode wsl2` still creates a Windows
Server host, then enables WSL, VirtualMachinePlatform, and HypervisorPlatform,
reboots as needed, updates the WSL kernel, imports an Ubuntu rootfs, and
prepares the Linux-side `crabbox-ready` toolchain. The launch enables nested
virtualization and uses C8i, M8i, or R8i instance families. Commands and sync
then use the POSIX WSL contract.

### azure — Windows

`--provider azure --target windows` creates a native Windows Server lease, uses
the VM Agent Custom Script Extension to install OpenSSH Server and Git for
Windows, and configures the `crabbox` user for SSH/sync/run. Add `--desktop` to
run the shared Windows desktop bootstrap over SSH, install TightVNC, configure
auto-logon, and expose VNC through the normal SSH tunnel. With
`--windows-mode wsl2`, Crabbox enables WSL2 over SSH and uses the POSIX WSL
sync/run/actions contract. Azure Windows does not provision browser/code.

### azure — backend and OS disk

Azure leases use managed `StandardSSD_LRS` OS disks by default so native
disk-snapshot checkpoints can be created and forked. Use
`--azure-os-disk ephemeral` only for stateless leases that don't need native
Azure checkpoint/fork support; `--azure-os-disk auto` resolves to managed.

`--azure-backend dynamic-sessions` keeps `--provider azure` as the family
selector while routing to the `azure-dynamic-sessions` delegated backend.

### aws — macOS

`--provider aws --target macos --desktop` launches an EC2 Mac instance on an
already allocated Dedicated Host. Crabbox can discover an available host in the
selected region, or pin one with `CRABBOX_HOST_ID` / `hostId`
(`CRABBOX_AWS_MAC_HOST_ID` and `aws.macHostId` remain AWS compatibility
aliases). Use `--market on-demand`, and expect EC2 Mac host lifecycle rules to
dominate cleanup and cost. Warmup never allocates a Dedicated Host implicitly;
trusted operators manage host lifecycle with
`crabbox admin hosts offerings|quota|list|allocate|release --provider aws --target macos`.
The default SSH user is `ec2-user`; the VNC password from `crabbox vnc` is the
per-lease macOS account password set by bootstrap.

## GitHub Actions runner

`--actions-runner` immediately registers the warm box as an ephemeral
self-hosted GitHub Actions runner for the current repository. Most projects
should instead prefer [`crabbox actions hydrate --id <lease>`](actions.md) after
warmup, because it also dispatches the workflow and waits for the ready marker.

## Flags

```text
--provider <name>                  provider (see crabbox providers); default hetzner
--profile <name>                   configuration profile
--class <name>                     machine class; default beast
--arch amd64|arm64                 CPU architecture; arm64 is Linux-only on AWS/Azure
--os ubuntu:26.04|ubuntu:24.04     portable Linux OS image selector
--type <provider-type>             provider server/instance type
--market spot|on-demand            capacity market (AWS)
--slug <slug>                      request a friendly slug for a new lease
--pond <name>                      tag this lease into a pond
--expose <port>                    declare a TCP port reachable over the SSH-mesh plane; repeatable
--ttl <duration>                   maximum lease lifetime; default 90m
--idle-timeout <duration>          release after idle; default 30m
--desktop                          provision/require a visible desktop + VNC
--desktop-env xfce|wayland|gnome   Linux desktop environment
--browser                          provision/require a browser binary
--code                             provision/require code-server
--target linux|macos|windows       target OS
--windows-mode normal|wsl2         Windows mode
--static-host <host>               static SSH host (provider=ssh)
--static-user <user>               static SSH user
--static-port <port>               static SSH port
--static-work-root <path>          static target work root
--network auto|tailscale|public    network mode for the printed SSH endpoint
--tailscale                        join new managed Linux leases to the tailnet
--tailscale-tags <a,b,c>           Tailscale tags for new managed leases
--tailscale-hostname-template <t>  Tailscale hostname template
--tailscale-auth-key-env <env>     env var holding a direct-provider Tailscale auth key
--tailscale-exit-node <name|100.x> Tailscale exit node
--tailscale-exit-node-allow-lan-access
--keep                             keep the box after warmup; default true
--actions-runner                   register the box as an ephemeral GitHub Actions runner
--reclaim                          overwrite an existing local claim for this lease
--timing-json                      print a final JSON timing record on stderr
--azure-backend vm|dynamic-sessions
--azure-os-disk managed|ephemeral|auto
```

Provider-specific flags (for example `--azure-backend`, `--e2b-template`,
`--daytona-snapshot`) are contributed by the selected provider; run
`crabbox warmup --provider <name> --help` to see the set for a given provider,
and `crabbox providers` to list all providers and their capabilities.

## SSH keys

New leases use per-lease SSH keys under the user config directory (RSA for
AWS/Azure Windows, ed25519 otherwise):

```text
<user-config>/crabbox/testboxes/<lease-id>/id_ed25519
```

On macOS and Linux this is typically `~/Library/Application Support/crabbox/...`
or `~/.config/crabbox/...` respectively.
