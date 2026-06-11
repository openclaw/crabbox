# Microsoft Execution Containers Provider

Use `provider: mxc` to run a command from a local Windows checkout inside
Microsoft Execution Containers (MXC). The initial integration targets the
one-shot `processcontainer` backend from MXC schema `0.6.0-alpha`.

MXC is currently public preview software. Microsoft explicitly warns that its
current generated policies can be overly permissive, so this provider must not
be treated as a production security boundary yet.

## Prerequisites

- Windows build 26100 or newer.
- Build or install `wxc-exec.exe` from <https://github.com/microsoft/mxc>.
- Keep the repository on the local Windows filesystem.

`crabbox doctor --provider mxc --target windows` launches a harmless sandbox,
not just MXC's read-only probe. This matters because MXC 0.6.1 reports the
BaseContainer tier as present on stock Azure Windows 11 24H2 and 25H2 images,
but execution still fails with disabled feature keys. A Windows Server vNext
build 29595 host also lacked the BaseContainer and BFS paths and selected MXC's
AppContainer+DACL fallback instead. Treat image compatibility as an upstream
preview constraint and require a green doctor before use.

The DACL fallback is disabled by default because it changes host ACLs. To use it
on an isolated disposable host, first run the companion release tool elevated:

```powershell
wxc-host-prep.exe prepare-system-drive
crabbox doctor --provider mxc --target windows --mxc-allow-dacl-mutation
```

Then pass `--mxc-allow-dacl-mutation` to each run. This opt-in allows MXC to add
the access-control entries required by its AppContainer fallback to the
workspace and explicitly allowed tool paths. Do not use it on a shared host.

MXC's experimental `windows_sandbox` backend also requires Windows Sandbox,
host Python, nested virtualization, and the companion release binaries.

## Usage

```powershell
crabbox run --provider mxc --target windows -- cmd.exe /d /c cd
```

Outbound network access is blocked by default:

```powershell
crabbox run --provider mxc --mxc-network allow --mxc-allow-windows-ui --shell -- npm test
```

Use `--shell` for Windows `.cmd` and `.bat` shims such as `npm`, `pnpm`, and
`yarn`; direct mode fails closed instead of passing unescaped arguments through
`cmd.exe`. Shell mode uses Windows PowerShell and therefore requires the explicit
`--mxc-allow-windows-ui` capability opt-in.

## Configuration

```yaml
provider: mxc
target: windows
mxc:
  cliPath: wxc-exec.exe
  version: 0.6.0-alpha
  containment: processcontainer
  network: block
  readOnlyPaths: []
  readWritePaths: []
  allowedHosts: []
  blockedHosts: []
  allowDaclMutation: false
  allowWindowsUI: false
  experimental: false
```

The checkout root is automatically added to `filesystem.readwritePaths`.
Windows system and Program Files roots are added read-only. Crabbox does not
blanket-allow every host `PATH` directory: user-installed toolchains and child
executables outside those roots must be opted in with `--mxc-readonly-paths` or
`mxc.readOnlyPaths`. Forwarded Crabbox environment variables become MXC process
environment entries. The MXC JSON and private temporary workspace share a
per-run directory whose inherited Windows ACLs are removed and replaced with
current-user-only access, so secret values are not exposed in command-line
arguments or a permissive shared temp directory.
MXC's host-DACL mutation fallback is disabled by default; hosts that cannot
provide a non-mutating containment tier fail closed unless the operator opts in
explicitly on a disposable host.

Programs that require Win32k system calls, including Windows PowerShell, also
need `--mxc-allow-windows-ui`. Keep it off for console-only tools:

```powershell
crabbox run --provider mxc --target windows --mxc-allow-dacl-mutation --mxc-allow-windows-ui -- powershell.exe -NoProfile -Command 'Get-Location'
```

Provider flags:

```text
--mxc-cli <path>
--mxc-version <schema>
--mxc-containment <backend>
--mxc-network block|allow
--mxc-readonly-paths <csv>
--mxc-readwrite-paths <csv>
--mxc-allowed-hosts <csv>
--mxc-blocked-hosts <csv>
--mxc-allow-dacl-mutation
--mxc-allow-windows-ui
--mxc-experimental
```

Only `processcontainer` is enabled by default. Other MXC backends require
`--mxc-experimental`. The provider is one-shot: `warmup`, persistent lease IDs,
`status`, and `stop` are intentionally unsupported until MXC state-aware
lifecycles stabilize beyond the current `isolation_session` preview.
