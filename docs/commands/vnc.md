# vnc

`crabbox vnc` prints connection details for a desktop-capable Crabbox target.
For Crabbox-created desktop leases, it gives you an SSH tunnel, a local VNC
endpoint, and the generated per-lease password. For static SSH targets, it can
describe an existing host-managed VNC service, but it will not pretend that
host is a Crabbox-created box.

Use this command when you need to look at or manually drive the visible desktop
inside a lease:

```sh
crabbox warmup --desktop
crabbox vnc --id blue-lobster
crabbox vnc --id blue-lobster --open
```

Managed AWS Windows and EC2 Mac desktop leases use the same command:

```sh
crabbox warmup --provider aws --target windows --desktop --market on-demand
crabbox vnc --id crimson-crab

CRABBOX_AWS_MAC_HOST_ID=h-... \
  crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id silver-squid
```

Static hosts are explicit and host-managed:

```sh
crabbox vnc --provider ssh --target macos --static-host mac-studio.local
crabbox vnc --provider ssh --target windows --static-host win-dev.local
```

## Output

A managed Linux lease prints:

```text
lease: cbx_... slug=blue-lobster provider=aws target=linux
managed: true
display: :99
ssh tunnel:
  ssh -i ... -p 2222 -N -L 5901:127.0.0.1:5900 crabbox@203.0.113.10
vnc:
  localhost:5901
password: ...
Keep the tunnel process running while connected.
```

Run the printed `ssh -N -L ...` tunnel in another terminal, then connect your
VNC client to the printed `localhost:<port>` endpoint. The tunnel forwards your
local port to `127.0.0.1:5900` on the remote box.

Use `--open` when you want Crabbox to start the tunnel and open the local VNC
URL for you:

```sh
crabbox vnc --id blue-lobster --open
```

Use `crabbox webvnc --id <lease> --open` when you want the same desktop inside
the authenticated coordinator portal instead of a native VNC client. WebVNC
still uses a local SSH tunnel and does not expose the runner's VNC port.

Keep the tunnel process alive while you are connected.

## Credentials

Managed desktop leases use generated per-lease credentials. The password is
stored only on the instance and is retrieved over SSH when `crabbox vnc` runs.
Crabbox does not store it in provider tags, labels, or run history.

Password locations:

| Target | Password file |
| --- | --- |
| Linux | `/var/lib/crabbox/vnc.password` |
| Windows | `C:\ProgramData\crabbox\vnc.password` |
| macOS | `/var/db/crabbox/vnc.password` |

Managed AWS Windows leases also print the generated Windows console login:

```text
password: Cb1!...
windows username: crabbox
windows password: Cb1!...
```

That login belongs to the Crabbox-created Windows instance, not your local
machine. Windows desktop bootstrap creates a local `crabbox` administrator,
configures auto-logon for that user, installs TightVNC, and keeps VNC reachable
only through the SSH tunnel.

Managed AWS macOS leases print the EC2 macOS account login:

```text
password: ...
macos username: ec2-user
macos password: ...
```

That password is generated per lease and set on the EC2 Mac account during
bootstrap.

Static macOS and Windows hosts are different. Their VNC or Screen Sharing
credentials are host-managed, because those targets are existing machines.
Crabbox does not synthesize or print those passwords.

## Managed Vs Static

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
- the host may be your local LAN, Tailscale, or another durable machine;
- opening VNC can show that host's OS login prompt.

`--open` refuses host-managed static VNC unless you pass `--host-managed`.
That guard prevents a local Mac or durable Windows host prompt from being
mistaken for a Crabbox-created cloud box.

```sh
crabbox vnc --provider ssh --target macos --static-host mac-studio.local --host-managed --open
```

Only use `--host-managed` when you intentionally want to open the existing
host's VNC or Screen Sharing prompt.

## Provider Support

| Provider / target | Managed VNC | Notes |
| --- | --- | --- |
| Hetzner Linux | Yes | Requires `--desktop`; installs XFCE, Xvfb, and x11vnc. |
| AWS Linux | Yes | Requires `--desktop`; same Linux desktop profile. |
| AWS Windows | Yes | Requires `--target windows --desktop --market on-demand`; installs Git for Windows and TightVNC after EC2Launch enables OpenSSH. |
| AWS macOS | Yes | Requires `--target macos --desktop --market on-demand` plus `CRABBOX_AWS_MAC_HOST_ID` or `aws.macHostId`. |
| Static Linux | Host-managed | Requires an existing loopback VNC service on the host. |
| Static macOS | Host-managed | Uses existing Screen Sharing or VNC. |
| Static Windows | Host-managed | Uses an existing VNC server. |
| Blacksmith Testbox | No | Blacksmith owns machine connectivity today. |

AWS EC2 Mac has an important cost and lifecycle constraint: Mac instances run on
allocated EC2 Mac Dedicated Hosts, are On-Demand only, and the Dedicated Host
has a 24-hour minimum allocation period. Crabbox launches onto a host id you
provide; it does not allocate or scrub EC2 Mac hosts for you.

## Security Model

Crabbox VNC is tunnel-first:

- managed VNC binds to `127.0.0.1:5900` on the remote box;
- the cloud security group does not open public VNC ingress;
- the local machine connects through SSH port forwarding;
- the normal lease TTL and idle-timeout lifecycle still apply;
- generated passwords are retrieved only on demand over SSH.

For static hosts, direct `host:5900` VNC is allowed only when that endpoint is
already reachable. Treat direct static VNC as operator-managed and keep it on a
trusted network such as Tailscale or a private LAN.

## Screenshots

Use `crabbox screenshot` when you need a PNG but do not need to open a VNC
client:

```sh
crabbox screenshot --id blue-lobster --output desktop.png
```

Screenshots share the same managed desktop boundary as VNC. Static macOS and
Windows hosts are rejected so Crabbox does not accidentally capture your local
or home-host desktop.

Windows screenshots run a one-shot scheduled task inside the logged-in
`crabbox` console session. Non-interactive SSH sessions cannot reliably capture
the visible Windows desktop.

## Troubleshooting

`lease ... was not created with desktop=true`

Warm a new lease with `--desktop`. Existing non-desktop leases do not gain a
desktop after creation:

```sh
crabbox warmup --desktop
```

`target does not expose VNC on 127.0.0.1:5900`

The SSH connection works, but the desktop or VNC service is not listening on
remote loopback. On managed boxes, inspect bootstrap logs or warm a fresh lease.
On static hosts, start or configure the host's VNC service.

VNC opens an OS credential prompt

Check `managed:` in the output. If it says `managed: false`, you opened a
static host. Static host credentials belong to that host. For Crabbox-created
Windows or macOS, use the generated username/password printed by `crabbox vnc`.

Tunnel command uses port `22` instead of `2222`

That is expected on AWS Windows. EC2Launch enables the first OpenSSH foothold on
port `22`, and Crabbox records the working SSH port after probing fallbacks.

Windows screenshot is black or fails from raw SSH

Use `crabbox screenshot`, not an ad hoc PowerShell `CopyFromScreen` over SSH.
The command captures from the logged-in console session using a scheduled task.

macOS launch fails with missing host id

Set `CRABBOX_AWS_MAC_HOST_ID` or `aws.macHostId`, use `--market on-demand`, and
make sure the Dedicated Host is allocated in the selected AWS region.

## Flags

```text
--id <lease-id-or-slug>
--provider hetzner|aws|ssh
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--local-port <port>
--open
--host-managed
--reclaim
```

Related docs:

- [screenshot](screenshot.md)
- [warmup](warmup.md)
- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [Providers](../features/providers.md)
