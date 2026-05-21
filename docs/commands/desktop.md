# desktop

`crabbox desktop launch` starts an app inside a desktop lease without taking
over VNC manually.

```sh
crabbox warmup --desktop --browser
crabbox desktop launch --id blue-lobster --browser --url https://example.com
crabbox desktop launch --id blue-lobster --browser --url https://example.com --webvnc --open
crabbox desktop launch --id blue-lobster --browser --url https://example.com --webvnc --open --take-control
crabbox desktop launch --id blue-lobster --browser --url https://discord.com/login --egress discord --webvnc --open
crabbox desktop launch --id blue-lobster -- xterm
crabbox desktop terminal --id blue-lobster --sixel -- ./scripts/visual-smoke.sh
crabbox desktop terminal --id blue-lobster --record terminal.mp4 --record-duration 6s -- ./scripts/visual-smoke.sh
crabbox desktop record --id blue-lobster --duration 6s --fps 8 --output desktop.mp4
crabbox desktop proof --id blue-lobster --output artifacts/blue-lobster-proof -- ./scripts/visual-smoke.sh
crabbox desktop proof --id blue-lobster --publish-pr 123 -- ./scripts/visual-smoke.sh
crabbox desktop doctor --id blue-lobster
crabbox desktop click --id blue-lobster --x 640 --y 420
crabbox desktop paste --id blue-lobster --text "peter@example.com"
printf 'peter@example.com' | crabbox desktop paste --id blue-lobster
crabbox desktop type --id blue-lobster --text "hello"
crabbox desktop key --id blue-lobster ctrl+l
crabbox desktop key blue-lobster ctrl+l
```

The command resolves and touches the lease, verifies `desktop=true`, waits for
the loopback VNC service, then starts the process detached from the SSH session.
With `--browser`, Crabbox probes the target browser the same way `run --browser`
does and launches `BROWSER` when no explicit command is provided.
With `--webvnc`, the command keeps running after launch and bridges the desktop
into the authenticated WebVNC portal. Add `--open` to open that portal locally.
Add `--take-control` when the opened WebVNC viewer should immediately become
the keyboard and mouse controller instead of joining as an observer.
Browser launches default to a windowed human desktop with the remote panel and
title bar visible; use `--fullscreen` only for capture/video workflows.

`--egress <profile>` passes the active lease-local egress proxy to the launched
browser as `--proxy-server=http://127.0.0.1:3128`, so the browser exits to the
internet through the operator machine running `crabbox egress start`. Start
the egress bridge first; the flag currently requires `--browser`. Override the
proxy address with `--egress-proxy host:port` if you started egress on a
non-default port. See [egress](egress.md) for the full bridge model.

On Windows, SSH sessions cannot directly own the visible console desktop, so
Crabbox writes a one-shot PowerShell launcher under `C:\ProgramData\crabbox` and
runs it as an interactive scheduled task for the logged-in `crabbox` user. The
launcher minimizes existing windows, starts the app, and tries to foreground
the new process. On Linux and macOS, the command is detached with `setsid` or
`nohup`.

`crabbox desktop terminal` starts a visible terminal with predictable sizing.
On native Windows it uses Git-for-Windows `mintty`, which is already present
after bootstrap and can render Sixel inline images when `--sixel` is set. The
launcher starts `mintty.exe` directly through the interactive PowerShell task so
paths under `Program Files` and shell arguments keep normal Windows quoting.
That keeps visual terminal smokes from needing hand-written batch launchers.
On macOS targets it launches Ghostty through `open -na Ghostty.app`, preserving
normal shell quoting while giving visual smokes a real terminal window.
Add `--screenshot <path>` or `--record <path>` to capture proof after launch;
`--wait-visible` controls the settle delay before capture. `--record` also
writes a sampled `*.contact.png` contact sheet by default. A contact sheet is a
single PNG grid made from frames sampled across the MP4, so reviewers can verify
animation/progress without opening the video. Disable it with
`--no-contact-sheet` or tune it with `--contact-sheet-frames`,
`--contact-sheet-cols`, and `--contact-sheet-width`.

`crabbox desktop record` records the active desktop to MP4. Linux uses remote
`ffmpeg`/X11 capture. Native Windows captures a frame sequence inside the
interactive console session and encodes the MP4 locally with `ffmpeg`. The
command writes a sidecar contact sheet unless `--no-contact-sheet` is passed.
MP4 recording currently supports Linux and native Windows targets; macOS
desktop commands support launch, screenshot, VNC, and input flows, but not
recording.

`crabbox desktop proof` is the one-shot visual QA command for terminal smokes.
It launches the terminal command, waits for the UI to settle, and writes a
bundle:

- `metadata.json`
- `screenshot.png`
- `diagnostics.txt`
- `screen.mp4`
- `screen.contact.png`

Use `--publish-pr <n>` to publish that bundle directly through the same
artifact backend as `crabbox artifacts publish`. The default storage is
`auto`, so `CRABBOX_ARTIFACTS_STORAGE`/bucket/base-url env defaults still apply
and a logged-in coordinator uploads through broker-owned artifact storage when
no explicit storage is configured. Otherwise pass `--publish-storage`,
`--publish-bucket`, `--publish-base-url`, or use `crabbox artifacts publish`
for advanced storage flags. `desktop terminal --record` supports the same
`--publish-pr` flow when the record path lives inside an artifact directory, for example
`--record artifacts/proof/screen.mp4 --screenshot artifacts/proof/screenshot.png`.

Recorder diagnostics are written by `desktop proof` and can also be requested
from `desktop terminal --diagnostics <path>`. They include local
`ffmpeg`/`ffprobe`, VNC loopback status, and target-specific recorder probes:
Task Scheduler, TightVNC service, and interactive user state on Windows;
`DISPLAY`, remote `ffmpeg`, `xdpyinfo`, screen size, and VNC listener state on
Linux.

`crabbox desktop doctor` checks the selected lease without syncing the repo.
For Linux desktop leases it reports VM/session health separately from portal
health: `DISPLAY`, Xvfb/window manager/panel, VNC listener, `xdotool`,
clipboard tool, browser binary, `ffmpeg`, screen size, screenshot capture, and
WebVNC bridge/viewer state. Failures include a one-line repair suggestion so
you can tell session bugs from WebVNC/browser-portal bugs.

Desktop launch and input failures now surface the failing layer directly in the
CLI output. For example, a missing visible browser reports `problem: browser not
launched`, a dead input path reports `problem: input stack dead`, and a broken
portal path reports `problem: VNC bridge disconnected` or `problem: WebVNC
daemon not running`. The same output includes exact `rescue:` commands such as
`crabbox desktop doctor --id <lease>` or `crabbox webvnc reset --id <lease>
--open`.

Input helpers also operate on the selected lease over SSH without repo sync.
Use them instead of hand-written input snippets. `desktop click` supports
managed Linux, macOS, and native Windows targets. `desktop type` uses raw
`xdotool type` only for simple alphanumeric text on Linux; text with emails,
passwords, symbols such as `@` or `+`, URLs, whitespace, or long payloads goes
through the remote clipboard and paste path because keyboard layouts can
otherwise corrupt special characters.

`desktop paste` accepts `--text` or stdin. `desktop key` accepts either
`--id <lease> <keys>` or the positional lease form `<lease> <keys>`; the key
sequence is parsed after lease flags so common forms such as
`crabbox desktop key blue-lobster ctrl+l` and
`crabbox desktop key -id blue-lobster ctrl+l` send `ctrl+l`, not the lease id.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|azure|parallels|ssh|semaphore|daytona
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--browser
--url <url>
--webvnc
--open
--fullscreen
--egress <profile>
--egress-proxy <host:port>
--reclaim
```

Input helper flags:

```text
desktop terminal --id <lease-id-or-slug> [--font-size <n>] [--cols <n>] [--rows <n>] [--sixel]
desktop terminal --id <lease-id-or-slug> [--screenshot <path>] [--record <path>] [--record-duration <duration>] [--record-fps <n>] [--wait-visible <duration>] [--diagnostics <path>] [--publish-pr <n>] -- <command...>
desktop record --id <lease-id-or-slug> [--output <path>] [--duration <duration>] [--fps <n>] [--no-contact-sheet]
desktop proof --id <lease-id-or-slug> [--output <dir>] [--record-duration <duration>] [--record-fps <n>] [--publish-pr <n>] -- <command...>
desktop terminal|proof [--contact-sheet=false|--no-contact-sheet] [--contact-sheet-output <path>] [--contact-sheet-frames <n>] [--contact-sheet-cols <n>] [--contact-sheet-width <px>]
desktop terminal|proof --publish-pr <n> [--publish-storage auto|broker|local|s3|cloudflare|r2] [--publish-bucket <name>] [--publish-prefix <path>] [--publish-base-url <url>] [--publish-repo owner/name] [--publish-summary <text>|--publish-summary-file <path>] [--publish-dry-run] [--publish-no-comment]
desktop doctor --id <lease-id-or-slug>
desktop click --id <lease-id-or-slug> --x <n> --y <n>
desktop paste --id <lease-id-or-slug> --text <text>
desktop paste --id <lease-id-or-slug> < input.txt
desktop type --id <lease-id-or-slug> --text <text>
desktop key --id <lease-id-or-slug> <keys>
desktop key <lease-id-or-slug> <keys>
desktop key --id <lease-id-or-slug> --keys <keys>
```

Related docs:

- [egress](egress.md)
- [vnc](vnc.md)
- [webvnc](webvnc.md)
- [Lease capabilities](../features/capabilities.md)
- [Mediated egress](../features/egress.md)
