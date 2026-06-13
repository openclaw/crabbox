# Windows Sandbox

Windows Sandbox is a local delegated-run provider for disposable native Windows
test runs on a Windows host. Crabbox generates a temporary `.wsb` file, maps a
host workspace into the sandbox, runs the requested command through the sandbox
logon command, streams mapped stdout/stderr files back to the terminal, then
shuts the sandbox down unless `--keep` or `--keep-on-failure` asks to leave it
open.

Provider id: `windows-sandbox`

Aliases: `wsb`, `windows-sandbox-provider`

Targets: native Windows only (`--target windows --windows-mode normal`)

Brokered: no. This provider runs entirely from the local CLI and does not use
the coordinator.

## Requirements

- A Windows host that supports and has enabled the Windows Sandbox optional
  feature.
- `WindowsSandbox.exe` available on `PATH`.
- No other Windows Sandbox session already running. Windows Sandbox supports one
  instance at a time.

The implementation follows Microsoft's `.wsb` configuration model:
[Windows Sandbox](https://learn.microsoft.com/en-us/windows/security/application-security/application-isolation/windows-sandbox/).

## Usage

```sh
crabbox run --provider windows-sandbox --target windows -- powershell -NoProfile -Command "Write-Host ok"
crabbox run --provider wsb --shell "go test ./..."
crabbox run --provider windows-sandbox --keep-on-failure -- npm test
crabbox doctor --provider windows-sandbox
```

When `--target` is omitted, `provider=windows-sandbox` defaults to native
Windows. `--windows-mode wsl2` is intentionally rejected because Windows
Sandbox exposes a disposable Windows desktop session, not a reusable WSL2 VM.

## Sync and lifecycle

Crabbox copies the local sync manifest into a temporary host workspace and maps
that directory into the sandbox at `C:\crabbox-work` by default. The sandbox also
receives a mapped control directory for the generated run script, stdout/stderr
logs, and exit-code sentinel.

`--force-sync-large` is supported for unusually large workspace copies.
`--sync-only` is rejected because each Windows Sandbox workspace is created for a
single run and has no stable lease id to reuse.
Tracked symlinks are rejected before launch because the Windows host copy step
must not require Developer Mode or administrator symlink privileges. Exclude the
path or replace it with a regular file before using this provider.

The temporary workspace is removed after successful runs. With `--keep`, or with
`--keep-on-failure` after a non-zero exit, Crabbox leaves both the sandbox window
and the host temp directory in place for inspection.

`warmup`, `list`, `status`, and `stop` are not persistent lifecycle operations
for this provider. Close the Windows Sandbox window to end a kept session.

## Configuration

```yaml
provider: windows-sandbox
windowsSandbox:
  workdir: C:\crabbox-work
  tempRoot: C:\crabbox-temp
  networking: enable
  vgpu: disable
  clipboard: disable
  protectedClient: default
  audioInput: disable
  videoInput: disable
  printerRedirection: disable
  memoryMB: 4096
```

Flags:

- `--windows-sandbox-workdir`
- `--windows-sandbox-temp-root`
- `--windows-sandbox-networking enable|disable|default`
- `--windows-sandbox-vgpu enable|disable|default`
- `--windows-sandbox-clipboard enable|disable|default`
- `--windows-sandbox-protected-client enable|disable|default`
- `--windows-sandbox-audio-input enable|disable|default`
- `--windows-sandbox-video-input enable|disable|default`
- `--windows-sandbox-printer-redirection enable|disable|default`
- `--windows-sandbox-memory-mb`

Environment variables use the same names with a `CRABBOX_WINDOWS_SANDBOX_`
prefix, for example `CRABBOX_WINDOWS_SANDBOX_NETWORKING=disable`.

Host paths, device redirection, networking, vGPU, protected-client mode, and
memory settings are accepted only from trusted user config, environment
variables, or explicit flags. Repository-local config may set only the sandbox
workdir; it cannot redirect host temporary files or loosen host sandbox policy.
