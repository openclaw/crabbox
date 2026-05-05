# record

`crabbox record` captures an MP4 from a desktop lease without opening a VNC
client.

```sh
crabbox warmup --desktop
crabbox record --id blue-lobster
crabbox record --id blue-lobster --duration 10s --output desktop.mp4
```

The command resolves and touches the lease like `crabbox screenshot`, verifies
that the lease has `desktop=true`, waits for the loopback desktop/VNC service,
then streams an MP4 over SSH. Linux records `DISPLAY=:99` with `ffmpeg`
`x11grab`. Windows creates a one-shot scheduled task inside the logged-in
`crabbox` console session and records with `ffmpeg` `gdigrab`.

New managed Linux desktop leases install `ffmpeg` as part of the desktop
profile. Existing desktop leases can record after `ffmpeg` is installed on the
guest. Windows recording requires `ffmpeg.exe` on the desktop lease.

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
--output <path>
--reclaim
```

Related docs:

- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [screenshot command](screenshot.md)
