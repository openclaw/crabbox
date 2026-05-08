# desktop

`crabbox desktop launch` starts an app inside a desktop lease without taking
over VNC manually.

```sh
crabbox warmup --desktop --browser
crabbox desktop launch --id blue-lobster --browser --url https://example.com
crabbox desktop launch --id blue-lobster --browser --url https://example.com --webvnc --open
crabbox desktop launch --id blue-lobster --browser --url https://discord.com/login --egress discord --webvnc --open
crabbox desktop launch --id blue-lobster -- xterm
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
Use them instead of hand-written `xdotool` snippets. `desktop type` uses raw
`xdotool type` only for simple alphanumeric text; text with emails, passwords,
symbols such as `@` or `+`, URLs, whitespace, or long payloads goes through the
remote clipboard and paste path because keyboard layouts can otherwise corrupt
special characters.

`desktop paste` accepts `--text` or stdin. `desktop key` accepts either
`--id <lease> <keys>` or the positional lease form `<lease> <keys>`; the key
sequence is parsed after lease flags so common forms such as
`crabbox desktop key blue-lobster ctrl+l` and
`crabbox desktop key -id blue-lobster ctrl+l` send `ctrl+l`, not the lease id.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|azure|ssh|semaphore|daytona
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
