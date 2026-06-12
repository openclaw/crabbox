# desktop

`crabbox desktop` drives a visible desktop session on a lease that was warmed
with the `--desktop` capability: launch apps and terminals, send pointer and
keyboard input, capture screenshots and video, collect proof bundles, and check
session readiness. Each subcommand resolves the lease, touches it to keep it
alive, verifies the desktop capability, and acts over SSH without syncing the
repository.

```sh
crabbox warmup --desktop --browser
crabbox desktop launch --id swift-crab --browser --url https://example.com
crabbox desktop launch --id swift-crab --browser --url https://example.com --webvnc --open
crabbox desktop launch --id swift-crab --browser --url https://example.com --webvnc --open --take-control
crabbox desktop launch --id swift-crab --browser --url https://my-app.example.com/login --egress my-app --webvnc --open
crabbox desktop launch --id swift-crab -- xterm
crabbox desktop terminal --id swift-crab --sixel -- ./scripts/visual-smoke.sh
crabbox desktop terminal --id swift-crab --record terminal.mp4 --record-duration 6s -- ./scripts/visual-smoke.sh
crabbox desktop record --id swift-crab --duration 6s --fps 8 --output desktop.mp4
crabbox desktop proof --id swift-crab --output artifacts/swift-crab-proof -- ./scripts/visual-smoke.sh
crabbox desktop proof --id swift-crab --publish-pr 123 -- ./scripts/visual-smoke.sh
crabbox desktop doctor --id swift-crab
crabbox desktop click --id swift-crab --x 640 --y 420
crabbox desktop paste --id swift-crab --text "alice@example.com"
printf 'alice@example.com' | crabbox desktop paste --id swift-crab
crabbox desktop type --id swift-crab --text "hello"
crabbox desktop key --id swift-crab ctrl+l
crabbox desktop key swift-crab ctrl+l
```

Most subcommands accept `--id <lease-id-or-slug>`; the input helpers and
`terminal`/`proof` also accept the lease id as the first positional argument
when `--id` is omitted. Lease selection flags (`--provider`, `--target`,
`--windows-mode`, `--network`, and the `--static-*` flags for the `ssh`
provider) work the same as on `run` and `warmup`. The desktop helpers reject
delegated providers such as Blacksmith, which own their own machine
connectivity.

## desktop launch

`crabbox desktop launch` starts an app inside the desktop session without
attaching a VNC viewer first. It waits for the loopback VNC service, then starts
the process detached from the SSH session.

With `--browser`, Crabbox probes the target browser the same way
`run --browser` does and launches the resolved `BROWSER` when no explicit
command is given; pass `--url` to open a page. Browser launches default to a
windowed desktop with the title bar visible, so the WebVNC viewer sees a normal
human desktop; pass `--fullscreen` only for capture/video workflows.

With `--webvnc`, the command keeps running after launch and bridges the desktop
into WebVNC (the same bridge as `crabbox webvnc`). Coordinator-backed leases use
the authenticated web portal; the local container provider uses local noVNC over
SSH. Add `--open` to open the viewer locally and `--take-control` to make the
opened viewer the keyboard/mouse controller instead of an observer. `--open` and
`--take-control` both require `--webvnc`.

`--egress <profile>` passes the active lease-local egress proxy to the launched
browser as `--proxy-server=http://127.0.0.1:3128`, so the browser exits to the
internet through the operator machine running `crabbox egress start`. Start the
egress bridge first; the flag currently requires `--browser`. Override the proxy
address with `--egress-proxy host:port` if egress is listening elsewhere. See
[egress](egress.md) for the full bridge model.

On Linux and macOS the process is detached with `setsid` or `nohup`. On native
Windows, an SSH session cannot directly own the visible console desktop, so
Crabbox writes a one-shot PowerShell launcher under `C:\ProgramData\crabbox` and
runs it as an interactive scheduled task for the logged-in `crabbox` user. The
launcher starts the app in a normal window and brings it to the foreground.

```text
desktop launch --id <lease-id-or-slug>
  [--browser] [--url <url>] [--fullscreen]
  [--webvnc [--open] [--take-control]]
  [--egress <profile>] [--egress-proxy <host:port>]
  [--reclaim]
  -- <command...>
```

## desktop terminal

`crabbox desktop terminal` starts a visible terminal with predictable sizing
(`--font-size`, `--cols`, `--rows`). On Linux it uses `xterm` (or `foot`/
`gnome-terminal` on Wayland/GNOME desktop envs). On native Windows it launches
Git-for-Windows `mintty`, which is present after bootstrap and can render Sixel
inline images when `--sixel` is set. On macOS it launches Ghostty via
`open -na Ghostty.app`.

Add `--screenshot <path>` or `--record <path>` to capture proof after launch;
`--wait-visible <duration>` controls the settle delay before capture (defaults
to 2s when a capture is requested). `--record` writes an MP4 using
`--record-duration` (default 5s) and `--record-fps` (default 8), and also writes
a sampled `*.contact.png` contact sheet by default. A contact sheet is a single
PNG grid of frames sampled across the MP4, so reviewers can verify
animation/progress without opening the video. Disable it with `--no-contact-sheet`
or tune it with `--contact-sheet-frames`, `--contact-sheet-cols`, and
`--contact-sheet-width`. Add `--diagnostics <path>` to write recorder
diagnostics alongside the capture.

`--record` requires a target that supports video capture: Linux with
`ffmpeg`/`x11grab`, or a native Windows desktop. Wayland desktop envs are not
yet supported for video.

When `--record` writes into an artifact directory, the same `--publish-*` flags
as [`artifacts publish`](artifacts.md) publish that bundle (for example
`--record artifacts/proof/screen.mp4 --screenshot artifacts/proof/screenshot.png
--publish-pr 123`).

```text
desktop terminal --id <lease-id-or-slug>
  [--font-size <n>] [--cols <n>] [--rows <n>] [--sixel]
  [--screenshot <path>] [--record <path>] [--record-duration <duration>] [--record-fps <n>]
  [--wait-visible <duration>] [--diagnostics <path>]
  [--no-contact-sheet | --contact-sheet=false]
  [--contact-sheet-output <path>] [--contact-sheet-frames <n>] [--contact-sheet-cols <n>] [--contact-sheet-width <px>]
  [--publish-pr <n> ...] [--reclaim]
  -- <command...>
```

## desktop record

`crabbox desktop record` records the active desktop to MP4. It is an alias of
[`artifacts video`](artifacts.md): Linux uses remote `ffmpeg`/X11 capture, and
native Windows captures a frame sequence inside the interactive console session
and encodes the MP4 locally with `ffmpeg`. macOS is not supported for recording.
A sidecar contact sheet is written unless `--no-contact-sheet` is passed.

```text
desktop record --id <lease-id-or-slug>
  [--output <path>] [--duration <duration>] [--fps <n>]
  [--no-contact-sheet | --contact-sheet=false]
  [--contact-sheet-output <path>] [--contact-sheet-frames <n>] [--contact-sheet-cols <n>] [--contact-sheet-width <px>]
```

Defaults: `--duration 10s`, `--fps 15`, `--contact-sheet-frames 5`,
`--contact-sheet-cols 5`, `--contact-sheet-width 320`. The output defaults to
`crabbox-<slug>-screen.mp4`.

## desktop proof

`crabbox desktop proof` is the one-shot visual QA command for terminal smokes.
It launches the terminal command, waits for the UI to settle (`--wait-visible`,
default 2s), and writes a bundle into `--output` (default
`artifacts/<slug>-proof`):

- `metadata.json`
- `screenshot.png`
- `diagnostics.txt`
- `screen.mp4`
- `screen.contact.png`

Recording uses `--record-duration` (default 5s) and `--record-fps` (default 8),
and requires a Linux or native Windows target as with `desktop terminal`.

Use `--publish-pr <n>` to publish the bundle through the same artifact backend
as [`artifacts publish`](artifacts.md). The default storage is `auto`, so
`CRABBOX_ARTIFACTS_STORAGE`/bucket/base-url env defaults still apply and a
logged-in coordinator uploads through broker-owned artifact storage when no
explicit storage is configured. For finer control use `--publish-storage`,
`--publish-bucket`, `--publish-base-url`, or run `artifacts publish` directly.

```text
desktop proof --id <lease-id-or-slug>
  [--output <dir>] [--font-size <n>] [--cols <n>] [--rows <n>] [--sixel]
  [--wait-visible <duration>] [--record-duration <duration>] [--record-fps <n>]
  [--no-contact-sheet | --contact-sheet=false]
  [--contact-sheet-output <path>] [--contact-sheet-frames <n>] [--contact-sheet-cols <n>] [--contact-sheet-width <px>]
  [--publish-pr <n>] [--publish-dir <dir>]
  [--publish-storage auto|broker|local|s3|cloudflare|r2]
  [--publish-bucket <name>] [--publish-prefix <path>] [--publish-base-url <url>] [--publish-repo owner/name]
  [--publish-template openclaw|mantis]
  [--publish-summary <text> | --publish-summary-file <path>]
  [--publish-dry-run] [--publish-no-comment]
  [--reclaim]
  -- <command...>
```

Recorder diagnostics (written by `desktop proof`, or on demand with
`desktop terminal --diagnostics <path>`) include local `ffmpeg`/`ffprobe`, VNC
loopback status, and target-specific recorder probes: Task Scheduler, TightVNC
service, and interactive user state on Windows; `DISPLAY`, remote `ffmpeg`,
`xdpyinfo`, screen size, and VNC listener state on Linux.

## desktop doctor

`crabbox desktop doctor` checks the selected lease without syncing the repo. For
Linux desktop leases it reports VM/session health separately from portal health:
`DISPLAY`, Xvfb/window manager/panel (or Wayland session for Wayland envs), VNC
listener, `xdotool`/`wtype`, clipboard tool, browser binary, `ffmpeg`, screen
size, and screenshot capture. On coordinator-backed leases it also reports the
WebVNC bridge/viewer state. Failures carry a one-line repair suggestion so you
can tell a session bug apart from a WebVNC/browser-portal bug. Non-Linux targets
get a reduced report.

```text
desktop doctor --id <lease-id-or-slug>
```

## Input helpers

The input helpers operate on the selected lease over SSH without repo sync. Use
them instead of hand-written input snippets.

- `desktop click` moves the pointer and left-clicks at `--x`/`--y`. Supported on
  Linux, macOS, and native Windows (`windowsMode=normal`).
- `desktop type` enters text. Simple alphanumeric text up to 64 characters goes
  through `xdotool type` (or `wtype` on Wayland); text containing emails,
  passwords, symbols such as `@` or `+`, URLs, whitespace, or longer payloads is
  routed through the clipboard/paste path so keyboard layouts cannot corrupt
  special characters. Linux only.
- `desktop paste` pastes text from `--text` or stdin via the remote clipboard.
  Linux only.
- `desktop key` sends an `xdotool` key sequence (or a single modifier+key
  combination on Wayland). Linux only.

`desktop key` accepts either `--id <lease> <keys>` or the positional lease form
`<lease> <keys>`; the key sequence is parsed after lease flags, so
`crabbox desktop key swift-crab ctrl+l` and
`crabbox desktop key --id swift-crab ctrl+l` both send `ctrl+l`, not the lease
id. You can also pass the sequence explicitly with `--keys`.

```text
desktop click --id <lease-id-or-slug> --x <n> --y <n>
desktop type  --id <lease-id-or-slug> --text <text>
desktop paste --id <lease-id-or-slug> --text <text>
desktop paste --id <lease-id-or-slug> < input.txt
desktop key   --id <lease-id-or-slug> <keys>
desktop key   <lease-id-or-slug> <keys>
desktop key   --id <lease-id-or-slug> --keys <keys>
```

## Failure surfacing

Launch and input failures surface the failing layer directly in the CLI output.
A missing visible browser reports `problem: browser not launched`, a dead input
path reports `problem: input stack dead`, and a broken portal path reports
`problem: VNC bridge disconnected` or `problem: WebVNC daemon not running`. Each
message includes exact `rescue:` commands such as
`crabbox desktop doctor --id <lease>` or
`crabbox webvnc reset --id <lease> --open`.

## Related docs

- [egress](egress.md)
- [vnc](vnc.md)
- [webvnc](webvnc.md)
- [screenshot](screenshot.md)
- [artifacts](artifacts.md)
- [Lease capabilities](../features/capabilities.md)
- [Mediated egress](../features/egress.md)
