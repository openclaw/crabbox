# vnc

`crabbox vnc` prints a tunnel command and connection details for a
desktop-capable Crabbox lease or an explicitly configured static host.

```sh
crabbox warmup --desktop
crabbox vnc --id blue-lobster
crabbox vnc --id blue-lobster --open
crabbox vnc --provider ssh --target macos --static-host mac-studio.local
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
printed `localhost:<port>` endpoint. Managed desktop leases use a per-lease VNC
password stored on the runner. Linux stores it under `/var/lib/crabbox`, Windows
under `C:\ProgramData\crabbox`, and macOS under `/var/db/crabbox`; the password
is retrieved over SSH only when `vnc` is called. It is not stored in provider
labels or run history.

Managed AWS Windows leases also print the Windows console login next to the VNC
password:

```text
windows username: crabbox
windows password: ...
```

That is the generated user inside the Crabbox-created Windows instance. It is
not your local macOS password.

Managed AWS macOS leases print the EC2 macOS account login in the same style:

```text
macos username: ec2-user
macos password: ...
```

That password is generated per lease and set on the EC2 Mac account during
bootstrap.

Use `--open` to let Crabbox start the SSH tunnel, open the local VNC URL, and
print the tunnel process ID. Keep that tunnel process alive while connected.

Static hosts are existing machines, not Crabbox-created boxes. For static
hosts, Crabbox first tries the same SSH tunnel to
`127.0.0.1:5900` on the target. If a static host exposes VNC directly on
`host:5900`, Crabbox prints that endpoint instead. Direct static VNC is
operator-managed and should be limited to a trusted network such as Tailscale or
LAN. Opening a static macOS or Windows target means opening that existing
machine, not an external Crabbox instance.

Static host credentials are host-managed. On macOS, the built-in Screen Sharing
server uses the host's Screen Sharing or macOS account authentication. On
Windows, the prompt belongs to the installed VNC server. Crabbox does not print
or synthesize those passwords.

`--open` refuses host-managed static VNC by default so a host OS password prompt
is not mistaken for a Crabbox-created box. Pass `--host-managed` only when you
intentionally want to open that existing host's VNC login prompt.

Security boundary:

- VNC is never exposed directly to the public internet.
- Managed Linux binds x11vnc to `127.0.0.1:5900` on the runner.
- Managed Windows installs TightVNC and connects through the SSH tunnel.
- Managed macOS enables Screen Sharing and connects through the SSH tunnel.
- Crabbox does not add provider firewall or security-group ingress for VNC.
- Brokered leases use SSH tunnels only. Static hosts may also use direct
  operator-managed VNC when `host:5900` is already reachable.

Provider behavior:

- Brokered and direct Hetzner leases support Linux VNC only when created with
  `--desktop`.
- Brokered and direct AWS Linux leases support VNC when created with
  `--desktop`.
- Brokered and direct AWS native Windows leases support VNC when created with
  `--target windows --desktop`. EC2Launch opens the initial AWS key-backed
  OpenSSH foothold, then the Crabbox CLI installs Git for Windows, TightVNC,
  a local `crabbox` administrator, and Windows auto-logon for the lease.
- Brokered and direct AWS macOS leases support VNC when created with
  `--target macos --desktop --market on-demand` and an EC2 Mac Dedicated Host id
  from `CRABBOX_AWS_MAC_HOST_ID` or `aws.macHostId`.
- Static Linux can participate if the operator already configured Xvfb and
  loopback-bound x11vnc.
- Static macOS can participate when Screen Sharing or another VNC-compatible
  service is already available on `127.0.0.1:5900` over SSH or directly on
  `host:5900`. This reuses an existing Mac; it does not create a macOS Crabbox.
  Credentials are host-managed.
- Static native Windows can participate when a VNC server is already available
  on `127.0.0.1:5900` over SSH or directly on `host:5900`. Static Windows is
  still host-managed; managed Windows VNC is AWS-only.
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
--reclaim
```
