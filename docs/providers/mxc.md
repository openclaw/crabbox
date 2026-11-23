# Microsoft Execution Containers Provider

Use `provider: mxc` to run a command from a local Windows checkout inside
Microsoft Execution Containers (MXC). The native provider emits the stable
`1.0.0` one-shot contract supported by Microsoft MXC `v1.1.0`.

MXC remains preview software. A valid schema does not establish host support
or prove isolation; do not treat this provider as a production security boundary.

## Prerequisites

- A supported Windows 11 host with native PSEC (BaseContainer) support.
  Build 26100 is only Crabbox's coarse minimum check, not a compatibility proof.
  Microsoft lists 24H2 build 26100.9278 and 25H2 build 26200.9278 as initial
  ProcessContainer releases; required features depend on the request and updates.
- Build or install `wxc-exec.exe` from Microsoft MXC `v1.1.0`.
- Keep the repository on the local Windows filesystem.

`crabbox doctor --provider mxc --target windows` launches a harmless sandbox,
not just a read-only probe. Require a green doctor on the actual host before use.
MXC rejects requests that native PSEC cannot enforce before container creation.
There is no AppContainer or host-DACL mutation fallback.

## Usage

```powershell
crabbox run --provider mxc --target windows -- cmd.exe /d /c cd
```

Egress, private-network ingress and bidirectional host loopback are denied by
default. `--mxc-network allow` changes only the egress default; it does not
open private-network ingress or host loopback:

```powershell
crabbox run --provider mxc --mxc-network allow --mxc-allow-windows-ui --shell -- npm test
```

Use `--shell` for Windows `.cmd` and `.bat` shims such as `npm`, `pnpm`, and
`yarn`; direct mode fails closed instead of passing unescaped arguments through
`cmd.exe`. Shell mode uses Windows PowerShell and requires the explicit
`--mxc-allow-windows-ui` capability opt-in. Keep it off for console-only tools.

## Configuration

```yaml
provider: mxc
target: windows
mxc:
  cliPath: wxc-exec.exe
  version: 1.0.0
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
`mxc.readOnlyPaths`. The resolved direct executable is also added read-only.

Forwarded Crabbox environment variables and a bounded set of Windows runtime
variables become explicit MXC `process.env` entries, with
`inheritDefaultEnv: false`; the host environment is not inherited wholesale.
MXC requires `SYSTEMROOT` and `LOCALAPPDATA` for ProcessContainer and rejects
missing required entries. Native PSEC may replace `LOCALAPPDATA` with the
container-profile path. The MXC JSON and private temporary workspace share a
per-run directory whose inherited Windows ACLs are replaced with current-user
access. `TEMP` and `TMP` point at that private workspace.

The lease TTL is emitted in milliseconds. Stable MXC accepts 0 (no timeout)
through 4294967295ms; positive values below 1ms and out-of-range effective TTLs
are rejected rather than disabling or overflowing the timeout.

### Networking and migration from retired schemas

Only schema `1.0.0` is supported by this adapter. Explicit legacy versions
(including `0.6.0-alpha`), other published versions and development contracts
are rejected, never silently rewritten. Microsoft MXC `v1.1.0` no longer accepts
`0.6.0-alpha`, so retaining a second untested legacy serializer is not supported.
Review existing settings before selecting `--mxc-version 1.0.0`.

The CLI/config names `allowedHosts` and `blockedHosts` remain for compatibility,
but their entries must be numeric IPv4/IPv6 addresses or canonical network
CIDRs. Single addresses become `/32` or `/128` destinations. Rules apply to
all outbound protocols and ports; explicit deny rules take precedence in MXC.
DNS names, wildcards, URLs, ports, scoped IPv6, IPv4-mapped IPv6 and CIDRs with
host bits are rejected. Crabbox never resolves names into a transient IP
allowlist or drops unsupported rules. Existing CLI/environment CSV binding
semantics are retained: whitespace and blank CSV elements are removed before
policy validation, and delimiter-only CSV resets the whole list (including an
inherited deny list). Use that reset only intentionally; it removes existing
rules. A raw YAML list with a blank element is rejected instead. For example:

```powershell
crabbox run --provider mxc --mxc-allowed-hosts 203.0.113.0/24 --mxc-blocked-hosts 203.0.113.7 -- cmd.exe /d /c echo ready
```

These destinations are illustrative, not reachable service endpoints. No DNS
access is implicitly granted. The emitted network shape is:

```json
{
  "egress": {
    "default": "deny",
    "allow": [{"to": [{"cidr": "203.0.113.0/24"}]}],
    "deny": [{"to": [{"cidr": "203.0.113.7/32"}]}]
  },
  "ingress": {"default": "deny", "hostLoopback": "deny"}
}
```

ProcessContainer cannot grant private-network outbound access without also
granting private-network inbound access. Crabbox deliberately leaves both
ingress controls denied; private-network or host-loopback access is not
available through these egress flags. There is no proxy or hostname fallback.

`allowDaclMutation: true` / `--mxc-allow-dacl-mutation` is rejected with
migration guidance: the stable schema accepts that field only as an ignored
compatibility setting, not as permission to enable a fallback. Remove the
option or set it to false and use a native-PSEC-capable host.

Only `processcontainer` and the `process` intent (which resolves to
ProcessContainer on Windows) are supported. Other containments are rejected
even with `--mxc-experimental`, since their policy/environment semantics
cannot preserve this native adapter's contract. The experimental flag remains
a pass-through executor opt-in, not a policy bypass.

Provider flags:

```text
--mxc-cli <path>
--mxc-version 1.0.0
--mxc-containment processcontainer|process
--mxc-network block|allow
--mxc-readonly-paths <csv>
--mxc-readwrite-paths <csv>
--mxc-allowed-hosts <numeric-IP-or-CIDR-csv>
--mxc-blocked-hosts <numeric-IP-or-CIDR-csv>
--mxc-allow-dacl-mutation=false
--mxc-allow-windows-ui
--mxc-experimental
```

The provider is one-shot: `warmup`, persistent lease IDs, `status`, `stop`,
`run-session` and `--lease-output` are unsupported. The separate SSH-host
Windows runtime has its own pinned executor contract; this migration does
not change it.

## Upstream contract references

- [Supported versions](https://github.com/microsoft/mxc/blob/v1.1.0/schemas/schema-version.json)
- [Immutable stable schema](https://github.com/microsoft/mxc/blob/v1.1.0/schemas/stable/mxc-config.schema.1.0.0.json)
- [Schema semantics](https://github.com/microsoft/mxc/blob/v1.1.0/docs/schema.md)
- [ProcessContainer networking](https://github.com/microsoft/mxc/blob/v1.1.0/docs/backends/process-container/networking.md)
- [Windows support](https://github.com/microsoft/mxc/blob/v1.1.0/docs/backends/process-container/os-version-support.md)
