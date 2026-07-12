# vnc

`crabbox vnc` prints (or opens) the connection details for a desktop-capable
lease. For a managed desktop lease it gives you a loopback SSH tunnel, a local
VNC endpoint, and the generated per-lease password. For a static SSH host it
describes an existing host-managed VNC service instead of pretending the host
is a Crabbox-created box.

Use it when you want to view or manually drive the visible desktop inside a
lease:

```sh
crabbox warmup --desktop
crabbox vnc --id blue-lobster
crabbox vnc --id blue-lobster --network tailscale
crabbox vnc --id blue-lobster --open
```

The lease must have been warmed with `--desktop`; `crabbox vnc` does not add a
desktop to an existing lease. See [warmup](warmup.md) and
[Interactive desktop and VNC](../features/interactive-desktop-vnc.md).

## Synopsis

```text
crabbox vnc --id <lease-id-or-slug> [flags]
```

You can also pass the lease id or slug as the first positional argument instead
of `--id`.

## Flags

```text
--id <lease-id-or-slug>      Target lease (or pass it as the first argument).
--provider <name>            Provider for the lease/target. Defaults to config.
--target linux|macos|windows OS of the target box.
--windows-mode normal|wsl2   Windows session mode.
--static-host <host>         Static SSH host (provider=ssh).
--static-user <user>         Static SSH user.
--static-port <port>         Static SSH port.
--static-work-root <path>    Static work root.
--network auto|tailscale|public  Network path used to reach the box.
--local-port <port>          Local tunnel port. Auto-picks 5901-5999 if unset.
--open                       Start the tunnel (if needed) and open a VNC client.
--native-handoff             Emit one JSON handoff and own its foreground tunnel.
--host-managed               Allow --open against a static host-managed VNC.
--reclaim                    Claim this lease for the current repo.
```

`--provider` accepts any SSH-capable provider (the help text lists the current
set). The static `--static-*` flags only apply to `--provider ssh`.

## Examples

Managed Windows and EC2 Mac desktop leases use the same command:

```sh
crabbox warmup --provider aws --target windows --desktop
crabbox warmup --provider azure --target windows --desktop
crabbox vnc --id crimson-crab

crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id silver-squid
```

Static hosts are explicit and host-managed:

```sh
crabbox vnc --provider ssh --target macos --static-host mac-studio.example.com
crabbox vnc --provider ssh --target windows --static-host win-dev.example.com
```

## Output

A managed Linux lease prints:

```text
lease: cbx_... slug=blue-lobster provider=aws target=linux
managed: true
display: :99
ssh tunnel:
  ssh -i ... -p 2222 -N -o GatewayPorts=no -L 127.0.0.1:5901:127.0.0.1:5900 alice@203.0.113.10
vnc:
  127.0.0.1:5901
password: ...
Keep the tunnel process running while connected.
```

Run the printed `ssh -N -L ...` tunnel in another terminal, then connect your
VNC client to the printed `127.0.0.1:<port>` endpoint. The tunnel forwards your
chosen local port (`--local-port`, or an auto-picked port in `5901-5999`) to
`127.0.0.1:5900` on the remote box. Crabbox binds the local forward explicitly
to IPv4 `127.0.0.1`. Linux and macOS hosts verify that the launched SSH process
owns that exact listener before returning or opening a VNC client. Windows uses
the system TCP owner table to require the exact tracked, non-forking `ssh.exe`
PID; a reachable listener owned by another process is rejected. Platforms
without an exact ownership implementation fail closed. `adapter serve`
remains Linux/macOS-only.

Use `--open` to let Crabbox start the tunnel and open the local VNC URL for you:

```sh
crabbox vnc --id blue-lobster --open
```

`--open` starts the SSH tunnel in the background, waits until the local port is
reachable, prints the tunnel pid, then opens `vnc://127.0.0.1:<port>` with your
OS's URL handler. Dedicated VNC and WebVNC tunnels explicitly set
`ControlMaster=no`, `ControlPath=none`, `ControlPersist=no`, and
`ForkAfterAuthentication=no`, so they cannot silently reuse, background, or
leave behind an SSH connection whose listener is no longer owned by the
tracked process. Opening URLs is supported on macOS, Linux, and Windows.

`--native-handoff` is the machine-readable native-client contract. After its
loopback tunnel is ready it writes exactly one JSON line to stdout, with schema
`crabbox/vnc-handoff/v1`, loopback host and port, and the in-memory VNC
credentials. It remains in the foreground until the client closes it. The flag
is incompatible with `--open`; clients should consume stdout as a private pipe
and terminate the Crabbox process when the viewer disconnects.

For the same desktop inside the authenticated broker portal instead of a native
VNC client, use [`crabbox webvnc --id <lease> --open`](webvnc.md). WebVNC still
relies on a loopback SSH tunnel and never exposes the runner's VNC port.

### Network mode

When `--network tailscale` is selected, only the SSH endpoint changes. The
managed VNC service stays loopback-bound on the runner and is still reached
through the SSH tunnel.

### Static direct VNC

For a static host where loopback VNC is not reachable but the host answers on
`host:5900` directly, `crabbox vnc` prints a direct endpoint instead of a
tunnel:

```text
target: static-host slug=- provider=ssh os=macos host=mac-studio.example.com
managed: false
note: this is an existing host VNC service, not a Crabbox-created box
direct vnc:
  mac-studio.example.com:5900
  vnc://mac-studio.example.com:5900
vnc:
  mac-studio.example.com:5900
credentials: host-managed
Connect directly to the printed VNC endpoint.
```

Treat direct static VNC as operator-managed and keep it on a trusted network
such as Tailscale or a private LAN.

## Credentials

Managed desktop leases use generated per-lease credentials. The password is
stored only on the instance and is read over SSH when `crabbox vnc` runs;
Crabbox does not keep it in provider tags, labels, or run history.

Password locations:

| Target | Password file |
| --- | --- |
| Linux | `/var/lib/crabbox/vnc.password` |
| Windows | `C:\ProgramData\crabbox\vnc.password` |
| macOS | `/var/db/crabbox/vnc.password` |

Managed Windows leases also print the generated Windows console login:

```text
password: Cb1!...
windows username: crabbox
windows password: Cb1!...
```

That login belongs to the Crabbox-created Windows instance, not your local
machine. Windows desktop bootstrap creates a local `crabbox` administrator,
configures auto-logon for that user, installs TightVNC, and keeps VNC reachable
only through the SSH tunnel.

Managed AWS macOS leases print the macOS account login:

```text
password: ...
macos username: ec2-user
macos password: ...
```

That password is generated per lease and set on the macOS account during
bootstrap.

Static macOS and Windows hosts are different: their VNC or Screen Sharing
credentials are host-managed because those targets are existing machines.
Crabbox does not synthesize or print them. For static hosts the output prints
`credentials: host-managed` plus a hint to use the host's own account or VNC
password.

## Managed vs static

Managed means Crabbox created the box and owns the desktop setup:

- cloud instance lifecycle;
- SSH key and connection metadata;
- desktop or VNC service setup;
- generated per-lease password;
- `desktop=true` lease capability;
- tunnel-only access.

Static means Crabbox is pointing at an existing SSH host:

- the host already exists;
- the operator owns VNC setup and credentials;
- the host may be your LAN, Tailscale, or another durable machine;
- opening VNC can show that host's OS login prompt.

`--open` refuses host-managed static VNC unless you also pass `--host-managed`.
That guard prevents a local Mac or durable Windows host prompt from being
mistaken for a Crabbox-created cloud box:

```sh
crabbox vnc --provider ssh --target macos --static-host mac-studio.example.com --host-managed --open
```

Only use `--host-managed` when you intentionally want to open the existing
host's VNC or Screen Sharing prompt.

## Provider support

| Provider / target | Managed VNC | Notes |
| --- | --- | --- |
| Hetzner Linux | Yes | Requires `--desktop`; installs slim XFCE, resize-capable TigerVNC, and capture tools. |
| AWS Linux | Yes | Requires `--desktop`; same Linux desktop profile. |
| Azure Linux | Yes | Requires `--desktop`; same Linux desktop profile. |
| AWS Windows | Yes | Requires `--target windows --desktop`; installs Git for Windows and TightVNC after EC2Launch enables OpenSSH. Spot or On-Demand follows the AWS capacity config. |
| Azure Windows | Yes | Requires `--target windows --desktop`; installs Git for Windows and TightVNC after the Custom Script Extension enables OpenSSH. Capacity follows the Azure class/SKU config. |
| AWS macOS | Yes | Requires `--target macos --desktop --market on-demand` plus an available EC2 Mac Dedicated Host. Brokered mode can discover a host; direct mode requires `CRABBOX_HOST_ID` or `hostId`; `CRABBOX_AWS_MAC_HOST_ID` and `aws.macHostId` remain compatibility aliases. |
| Static Linux | Host-managed | Requires an existing loopback VNC service on the host. |
| Static macOS | Host-managed | Uses existing Screen Sharing or VNC. |
| Static Windows | Host-managed | Uses an existing VNC server. |
| Blacksmith Testbox | No | `crabbox vnc` exits with an error; Blacksmith owns machine connectivity. |

AWS EC2 Mac has a cost and lifecycle constraint: Mac instances run on allocated
EC2 Mac Dedicated Hosts, are On-Demand only, and the Dedicated Host has a
24-hour minimum allocation period. Crabbox launches onto an available host or a
host id you provide; it does not allocate a host implicitly. Trusted operators
can use `crabbox admin hosts offerings|quota|list|allocate|release --provider aws --target macos`.

## Security model

Crabbox VNC is tunnel-first:

- managed VNC binds to `127.0.0.1:5900` on the remote box;
- the cloud security group does not open public VNC ingress;
- your local machine connects through SSH port forwarding;
- the normal lease TTL and idle-timeout lifecycle still apply;
- generated passwords are read only on demand over SSH.

For static hosts, direct `host:5900` VNC is offered only when that endpoint is
already reachable. Keep direct static VNC on a trusted network.

## Screenshots

Use [`crabbox screenshot`](screenshot.md) when you need a PNG but do not need to
open a VNC client:

```sh
crabbox screenshot --id blue-lobster --output desktop.png
```

Screenshots share the same managed desktop boundary as VNC. Static macOS and
Windows hosts are rejected so Crabbox does not accidentally capture your local
or home-host desktop.

Windows screenshots run a one-shot scheduled task inside the logged-in
`crabbox` console session, because non-interactive SSH sessions cannot reliably
capture the visible Windows desktop.

## Troubleshooting

**`lease ... was not created with desktop=true`** — Warm a new lease with
`--desktop`. Existing non-desktop leases do not gain a desktop after creation.

**`target does not expose VNC on 127.0.0.1:5900`** (managed) or
**`target does not expose VNC through SSH loopback 127.0.0.1:5900 or direct host:5900`**
(static) — The SSH connection works, but no VNC service is listening on remote
loopback (and, for static hosts, the host is not reachable on `:5900` directly).
On managed boxes, inspect bootstrap logs or warm a fresh lease. On static hosts,
start or configure the host's VNC service.

**VNC opens an OS credential prompt** — Check `managed:` in the output. If it
says `managed: false`, you opened a static host; those credentials belong to
that host. For a Crabbox-created Windows or macOS box, use the generated
username/password printed by `crabbox vnc`.

**Tunnel command uses port `22` instead of `2222`** — Expected on AWS Windows.
EC2Launch enables the first OpenSSH foothold on port `22`, and Crabbox records
the working SSH port after probing fallbacks.

**Windows screenshot is black or fails from raw SSH** — Use `crabbox screenshot`
rather than an ad hoc PowerShell `CopyFromScreen` over SSH; the command captures
from the logged-in console session using a scheduled task.

**macOS launch fails with a missing host id** — Use `--market on-demand` and
make sure an available EC2 Mac Dedicated Host is allocated in the selected AWS
region. Set `CRABBOX_HOST_ID` or `hostId` only when you want to pin a specific
host or when running the direct AWS provider. Brokered host pinning requires
admin authentication unless the host has a retained instance from the same
owner and organization's released lease; other users rely on automatic
available-host discovery. `CRABBOX_AWS_MAC_HOST_ID` and `aws.macHostId` remain compatibility
aliases.

## Related docs

- [screenshot](screenshot.md)
- [webvnc](webvnc.md)
- [warmup](warmup.md)
- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [Linux VNC](../features/vnc-linux.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
- [Tailscale](../features/tailscale.md)
- [Providers](../providers/README.md)
