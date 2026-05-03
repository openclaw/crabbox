# vnc

`crabbox vnc` prints a tunnel command and connection details for a
desktop-capable Crabbox lease or an explicitly configured static host.

```sh
crabbox warmup --desktop
crabbox vnc --id blue-lobster
crabbox vnc --id blue-lobster --open
crabbox vnc --provider ssh --target macos --static-host mac-studio.local
crabbox vnc --provider ssh --target macos --static-host mac-studio.local --managed-login
```

The command resolves the lease like `crabbox ssh`, claims and touches it like
manual use, verifies that VNC is bound to runner loopback, and prints:

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

Run the tunnel command in another terminal, then connect your VNC client to the
printed `localhost:<port>` endpoint. Managed Linux desktop leases use a
per-lease VNC password stored on the runner under `/var/lib/crabbox`; the
password is retrieved over SSH only when `vnc` is called. It is not stored in
provider labels or run history.

Use `--open` to let Crabbox start the SSH tunnel, open the local VNC URL, and
print the tunnel process ID. Keep that tunnel process alive while connected.

Static hosts are existing machines, not Crabbox-created boxes. For static
hosts, Crabbox first tries the same SSH tunnel to
`127.0.0.1:5900` on the target. If a static host exposes VNC directly on
`host:5900`, Crabbox prints that endpoint instead. Direct static VNC is
operator-managed and should be limited to a trusted network such as Tailscale or
LAN.

Static host credentials are host-managed. On macOS, the built-in Screen Sharing
server uses the host's Screen Sharing or macOS account authentication. On
Windows, the prompt belongs to the installed VNC server. Crabbox does not print
or synthesize those passwords.

For static macOS hosts reachable over SSH, `--managed-login` creates or reuses a
dedicated local account, enables Apple Remote Desktop access for that account,
skips first-run Setup Assistant panes, and prints the username/password that
Crabbox generated. The password is stored on the target under the SSH user's
Crabbox state directory and is reused on later calls. Override the account name
with `--managed-user <name>`.

Static Windows managed login is not automatic yet. Crabbox needs a management
channel such as SSH/WinRM plus a known VNC server setup path before it can
create a Windows account and set the VNC server password. Without that, Windows
VNC remains host-managed and requires `--host-managed`.

`--open` refuses host-managed static VNC by default so a host OS password prompt
is not mistaken for a Crabbox-created box. Pass `--host-managed` only when you
intentionally want to open that existing host's VNC login prompt.

Security boundary:

- VNC is never exposed directly to the public internet.
- Managed Linux binds x11vnc to `127.0.0.1:5900` on the runner.
- Crabbox does not add provider firewall or security-group ingress for VNC.
- Brokered leases use SSH tunnels only. Static hosts may also use direct
  operator-managed VNC when `host:5900` is already reachable.

Provider behavior:

- Brokered and direct AWS/Hetzner Linux leases support `vnc` only when created
  with `--desktop`.
- Static Linux can participate if the operator already configured Xvfb and
  loopback-bound x11vnc.
- Static macOS can participate when Screen Sharing or another VNC-compatible
  service is already available on `127.0.0.1:5900` over SSH or directly on
  `host:5900`. `--managed-login` can create a dedicated local macOS account for
  Crabbox-managed VNC authentication on that existing Mac.
- Static native Windows can participate when a VNC server is already available
  on `127.0.0.1:5900` over SSH or directly on `host:5900`. Crabbox does not
  create a Windows Crabbox, or install or configure the Windows VNC server in
  this release.
- Blacksmith Testbox does not support managed VNC in this release.

Flags:

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
--managed-login
--managed-user <user>
--reclaim
```
