# screenshot

`crabbox screenshot` captures a PNG from a desktop lease without opening a VNC
client.

```sh
crabbox warmup --desktop
crabbox screenshot --id blue-lobster
crabbox screenshot --id blue-lobster --output desktop.png
```

The command resolves and touches the lease like `crabbox ssh`, verifies that the
lease has `desktop=true`, waits for the loopback desktop/VNC service, then
streams a PNG over SSH. Linux captures `DISPLAY=:99`. Windows creates a
one-shot scheduled task inside the logged-in `crabbox` console session, because
non-interactive SSH sessions cannot capture the visible desktop. macOS uses
`screencapture`.

For Windows, the screenshot reflects the active console session in the
Crabbox-created instance. Managed AWS Windows desktop leases enable auto-logon
for the generated `crabbox` user, store that password under
`C:\ProgramData\crabbox`, and use it only on the instance to run the scheduled
capture task.

If `--output` is omitted, Crabbox writes:

```text
crabbox-<slug-or-id>-screenshot.png
```

Static macOS and Windows targets are existing host machines, not Crabbox-created
desktops, so `screenshot` rejects those targets instead of capturing your local
or home-host desktop by accident. Managed AWS Windows and AWS macOS desktop
leases are Crabbox-created boxes and can be captured by lease id or slug.

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
--output <path>
--reclaim
```
