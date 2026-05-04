# webvnc

`crabbox webvnc` bridges a desktop lease into the authenticated coordinator
portal.

```sh
crabbox warmup --desktop
crabbox webvnc --id blue-lobster
crabbox webvnc --id blue-lobster --network tailscale
crabbox webvnc --id blue-lobster --open
```

The command resolves the lease like `crabbox vnc`, verifies that the lease has
`desktop=true`, starts the normal SSH tunnel to the runner's loopback VNC
service, mints a short-lived bridge ticket over the authenticated coordinator
API, and opens a websocket bridge to the coordinator with that ticket. The
browser connects to `/portal/leases/<lease>/vnc` after GitHub portal auth, and
the Durable Object pairs that browser websocket with the local bridge process.

This keeps the security boundary the same as `crabbox vnc`:

- VNC stays bound to runner loopback.
- The cloud provider does not open public VNC ingress.
- The coordinator authenticates the browser through portal auth and the bridge
  through a one-use short-lived ticket.
- The noVNC client is served from the coordinator origin, not a third-party CDN.
- The local `crabbox webvnc` process must keep running while the browser uses
  the desktop.

`--network tailscale` changes only the SSH endpoint used for the local tunnel.
The runner VNC service stays bound to loopback.

`--open` opens the portal page after the bridge starts. If the VNC password is
available, the command also places it in the URL fragment for the local browser
tab. URL fragments are not sent to the coordinator. If the portal login flow
redirects first, the page may still prompt for the VNC password; use the
password printed by the command.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws
--target linux|macos|windows
--windows-mode normal|wsl2
--network auto|tailscale|public
--local-port <port>
--open
--reclaim
```

Limitations:

- Coordinator-backed Hetzner and AWS desktop leases are supported.
- Static SSH hosts are intentionally not supported yet because the portal cannot
  prove that host-managed VNC credentials and prompts are safe to expose.
- Blacksmith Testbox still owns its own machine connectivity.
