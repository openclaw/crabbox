# record

`crabbox record` captures an MP4 from a desktop lease without opening a VNC
client.

```sh
crabbox warmup --desktop
crabbox record --id blue-lobster
crabbox record --id blue-lobster --duration 10s --output desktop.mp4
crabbox record --id blue-lobster --duration 2m --output task.mp4 --while -- ./drive-ui.sh
```

The command resolves and touches the lease like `crabbox screenshot`, verifies
that the lease has `desktop=true`, waits for the loopback desktop/VNC service,
then streams an MP4 over SSH. Linux records `DISPLAY=:99` with `ffmpeg`
`x11grab`. Windows, including `--windows-mode wsl2` leases, records the visible
Windows console by creating a one-shot scheduled task inside the logged-in
`crabbox` session and using `ffmpeg` `gdigrab`.

New managed Linux desktop leases install `ffmpeg` as part of the desktop
profile. Existing desktop leases can record after `ffmpeg` is installed on the
guest. Windows recording requires `ffmpeg.exe` on the desktop lease.

Use `--while -- <local-command...>` when another tool should drive the remote
desktop while Crabbox records it. Crabbox waits until the remote recorder is
armed, runs the local command, then stops the recorder and writes the MP4. The
local driver gets `CRABBOX_RECORD_LEASE_ID` and `CRABBOX_RECORD_PROVIDER` in its
environment so it can call commands such as `crabbox desktop launch`,
`crabbox screenshot`, or a scenario runner against the same lease. `--duration`
is a hard cap for the local driver command; if the driver is still running at
that limit, Crabbox stops the driver, stops the recorder, and returns an error.
The recording itself can include a small setup/teardown margin around the driver
so it does not miss the tail of the interaction. `--while` currently supports
POSIX desktop targets.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|ssh
--target linux|macos|windows
--windows-mode normal|wsl2
--network auto|tailscale|public
--duration <duration>
--fps <frames-per-second>
--size <width>x<height>|auto
--while -- <local-command...>
--output <path>
--reclaim
```

Related docs:

- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [screenshot command](screenshot.md)
