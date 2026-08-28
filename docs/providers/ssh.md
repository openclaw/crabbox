# Static SSH Provider

Read when:

- choosing `provider: ssh`, `provider: static`, or `provider: static-ssh`;
- reusing an existing Linux, macOS, or Windows host instead of provisioning one;
- changing `internal/providers/ssh` or static-host sync behavior.

Static SSH is the provider for machines Crabbox does **not** create. The backend
resolves a configured SSH target and hands it to core, which owns sync, command
execution, results, tunnels, and status rendering. Crabbox does not provision,
stop, or delete the machine, or account for its cost. The host's lifecycle is
yours; commands can still perform connection cleanup when releasing a lease.

The provider id is `ssh`, with aliases `static` and `static-ssh`. It is
direct-only and is never brokered through the coordinator.

## When To Use

Use Static SSH when:

- the machine already exists and should not be provisioned by Crabbox;
- you want to target a local Mac, LAN host, lab VM, or persistent Windows box;
- cloud provider cleanup and cost guardrails do not apply.

Use AWS, Azure, Google Cloud, or Hetzner when you want Crabbox to create and
delete the machine for you.

## Quick Start

```sh
crabbox run --provider ssh --static-host buildbox.local -- pnpm test
crabbox ssh --provider ssh --id buildbox.local
crabbox run --provider static-ssh --target windows --static-host win-dev.local \
  -- pwsh -NoProfile -Command '$PSVersionTable'
```

`warmup` for Static SSH does not provision a machine. It validates the
configured target and returns it as a lease-like object so the rest of the
warm-box workflow (`run`, `ssh`, `status`, tunnels) behaves the same as for
provisioned providers.

`stop` (also spelled `release`) removes the local claim after attempting the
shared connection cleanup described below. `run` does the same when its release
policy fires: normally after a fresh one-shot run, but not a kept run or a run
reusing an ID by default. Remote cleanup is best-effort: failures warn but do
not block local unclaiming. The static backend itself only removes the local
claim and cached target; it never stops or deletes the machine. There is no
static machine `cleanup` action.

### Connection cleanup

Commands attempt to write an Actions stop marker for the lease ID at
`$HOME/.crabbox/actions/<leaseID>.stop` on POSIX targets, or under
`C:\ProgramData\crabbox\actions` on native Windows. This signals the
[Actions hydration](../features/actions-hydration.md) workflow to end its
keep-alive phase. The remote directory and marker can be created even when no
hydration state exists or reading it fails.

Cleanup also attempts to stop local mediated-egress daemon state. On Linux
(or an unspecified target OS), remote egress cleanup attempts to stop processes
matching the common Crabbox egress-client command pattern, even if this lease
did not set up egress. That match is not restricted to a lease or session and
can affect other matching clients accessible to the SSH account. Other target
OSes skip this remote egress step.

Remote Tailscale logout is attempted only when stored lease metadata marks
Tailscale as enabled. Ordinary static `--tailscale` provisioning is unsupported;
using an existing tailnet address or MagicDNS name does not set that metadata
or trigger logout by itself. The metadata gate is not a live node-ownership
check.

## Targets

Static SSH supports all four targets:

- `linux`
- `macos`
- `windows` with `windows.mode: normal` (PowerShell over OpenSSH, archive sync)
- `windows` with `windows.mode: wsl2` (POSIX contract inside WSL)

`target` and (for Windows) `windows.mode` must match the real host — Crabbox
cannot infer whether a Windows host runs native PowerShell or WSL2 commands.
On Linux, macOS, and WSL2 targets, Crabbox's workspace-owner protocol invokes
`/bin/sh` explicitly and does not require the SSH account to use a POSIX login
shell; zsh, Bash, and Fish login shells are supported.

### Architecture assertions and observations

All three provider names accept `amd64` and `arm64` for all four targets.
`--arch`, `CRABBOX_ARCH`, or a nonempty YAML `architecture` is an explicit
assertion, including `amd64`. The omitted `amd64` configuration default is
not an assertion. For example:

```sh
crabbox run --provider ssh --target macos --arch arm64 \
  --static-host mac.example.com -- xcodebuild test
```

Acquisition and prepared reuse (including `run --id` and cached leases) check
known repository ownership before SSH readiness or architecture probes. An
explicitly approved host override does not bypass the owner of an existing lease
ID. Prepared reuse of claimed targets, including cached leases, also verifies the
exact stored claim snapshot and static target identity before opening SSH.
Architecture is measured after readiness and before updating the claim or allowing
sync, hydration, or workload execution.
The probe uses the resolved SSH user, working port, and existing trust/credential
transport. It never chooses emulation or forces `arch -arm64`. Explicit assertions
require fresh, supported matching evidence; missing, malformed, contradictory, or
translated evidence fails closed.

Linux uses `uname -m` for the SSH execution environment. WSL2 runs that probe
**inside WSL**, through the existing Windows-to-WSL wrapper, which stages a
temporary script. macOS combines `uname` with hardware and Rosetta `sysctl`
queries. Native Windows combines `IsWow64Process2` native-machine evidence with
the current PowerShell process's `RuntimeInformation.ProcessArchitecture`.
An unknown WOW64 process-machine value alone is not proof of native execution.
Unavailable APIs or process queries remain unknown; no environment variable or
older `.NET OSArchitecture` value substitutes for native-host evidence.

Without an assertion, supported measured architecture is published even when it
differs from the configured default. Unknown measurements produce a bounded
warning and permit unconstrained use. SSH authentication, identity, transport,
timeout, and cancellation errors still fail. Translated launchers expose host,
process, and translation fields; they cannot satisfy an explicit native assertion.
These observations describe the probe/SSH environment, not bare-metal provenance
on POSIX or the architecture of every later executable. An ARM shell does not
prove that Node or another workload binary is native.

Lease metadata contains normalized `architecture` (or `unknown`),
`architecture_source`, `architecture_scope`, `architecture_version`, and
`architecture_observed_at` (Unix milliseconds), with host/process/translation
fields where available.
`ServerType.Architecture` is populated only for supported measured architecture.
Opaque route bindings prevent evidence from being attached to a different
endpoint, user, or target after an override. No raw probe output is persisted.
Offline `List`/non-prepared `Resolve` returns only **historical** timestamped
evidence, or unknown for legacy/unmatched claims; it does not contact the host.
Execution always refreshes this evidence. Touch preserves it without making it
fresh again.

Prepared resolution returns fresh evidence with the original exact claim snapshot;
it does not change the persisted claim or the adapter's cache. `run` publishes
the evidence through its repository-aware claim transaction before work. A
different repository must use `--reclaim` before preparation can open SSH;
reclaim permits probing but adopts the claim only after successful architecture
validation. Rejected preparation leaves the claim, timestamps, and cache unchanged.
The snapshot is checked again after probing and during guarded publication so
a concurrent replacement or removal cannot be overwritten. Acquisition still
publishes its evidence directly through the guarded repository claim transaction.
Pond/admin preparation with no repository context does not infer an owner or
adopt a claim: a caller that persists the returned endpoint must use its existing
guarded publication step. Until then, offline lookup and Touch retain the
previously published evidence.

`config show` remains offline: its architecture is a configured/effective value,
not observed architecture or proof of supported runtime behavior. The JSON
`architectureExplicit` boolean (text: `architecture_explicit`) distinguishes an
explicit assertion from the default. Observations never rewrite that configuration.

### Upgrading existing static-host configuration

Older static SSH versions accepted configured values such as `architecture: amd64`
or `CRABBOX_ARCH=amd64` without checking the host. These values are now strict
assertions, including `amd64` inherited from user configuration or the environment.
Together with `--arch`, they must match fresh evidence on acquisition and prepared
reuse. An unchanged configuration can therefore fail after upgrading if the SSH
environment has a different architecture, runs under translation, or cannot provide
the required evidence. There is no compatibility fallback for explicit values.

For automatic discovery, remove `architecture` from every applicable user and
repository config, including any config selected by a profile or wrapper through
`CRABBOX_CONFIG`. Unset `CRABBOX_ARCH` and remove exports that set it again in shell
profiles or CI. Also stop passing `--arch` in commands or wrappers. Check the
effective config from the same directory and environment used for execution:

```sh
unset CRABBOX_ARCH
crabbox config path
crabbox config show --json
```

Verify `architectureExplicit` is `false` (text output: `architecture_explicit=false`).
The displayed `architecture` may still be the offline default `amd64`; fresh SSH
evidence determines the discovered architecture. A blank YAML value does not clear
an inherited assertion, and an empty or unset `CRABBOX_ARCH` does not clear YAML.
Normally user config loads first, then `crabbox.yaml`, then `.crabbox.yaml`, then
nonempty environment overrides. `CRABBOX_CONFIG` selects a single file instead of
the normal user/repository files. Remove the value from its contributing sources;
see [config show](../commands/config.md#config-show) for the merged view.

If a strict constraint is intended, keep an explicit supported value (`amd64` or
`arm64`) matching the actual SSH environment, and ensure it can provide matching,
non-translated evidence. Changing the assertion does not select an emulator or
change the host. The probe describes the SSH environment only: it does not prove
that every workload binary, such as Node, runs natively. Discovery also leaves SSH
trust, endpoint identity, and repository claim/reclaim checks unchanged.

## Configuration

The static target lives under the `static:` block. SSH credentials fall back to
the shared `ssh:` block when the matching `static:` field is empty.

### Linux

```yaml
provider: ssh
target: linux
static:
  host: buildbox.local
  user: crabbox
  port: "22"
  workRoot: /work/crabbox
```

### macOS

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

When no generic or `static.workRoot` override is configured, a macOS target
uses `/Users/<resolved-user>/crabbox`, where `<resolved-user>` is the final SSH
user after applying `ssh.user` and `static.user` precedence.

### Windows (native)

```yaml
provider: ssh
target: windows
windows:
  mode: normal
static:
  host: win-dev.local
  user: builder
  port: "22"
  workRoot: C:\crabbox
```

### Windows (WSL2)

```yaml
provider: ssh
target: windows
windows:
  mode: wsl2
static:
  host: win-dev.local
  user: builder
  port: "22"
  workRoot: /home/builder/crabbox
```

### Config fields

| `static:` key | Purpose |
| --- | --- |
| `host` | SSH host or IP (required). |
| `user` | SSH user. Falls back to `ssh.user`, then `$USER`. |
| `port` | SSH port. Falls back to `ssh.port`; the base default is `2222` with a `22` fallback. |
| `workRoot` | Remote checkout/work directory. |
| `id` | Optional stable lease id (default derived from `host`). |
| `name` | Optional friendly slug (default derived from `host`). |

The SSH private key comes from the shared `ssh.key` field (or `CRABBOX_SSH_KEY`).
There is no per-host key field; the static provider connects with your existing
key, not a key Crabbox generates.

A repository-defined `static.host` cannot silently inherit a key or ambient SSH
authentication from user config, the environment, an SSH agent, or local SSH
config. Define `static.host` and a relative, symlink-resolved `ssh.key` file
contained by the repository in the same repository config, or approve the
destination explicitly with `--static-host` or `CRABBOX_STATIC_HOST`. Absolute,
missing, and repository-escaping key paths require explicit host approval.

### Flags

```text
--static-host
--static-user
--static-port
--static-work-root
```

### Environment

```text
CRABBOX_STATIC_HOST
CRABBOX_STATIC_USER
CRABBOX_STATIC_PORT
CRABBOX_STATIC_WORK_ROOT
CRABBOX_STATIC_ID
CRABBOX_STATIC_NAME
CRABBOX_SSH_USER
CRABBOX_SSH_KEY
CRABBOX_SSH_PORT
```

## Host Requirements

POSIX hosts (Linux, macOS, WSL2) need:

- SSH access for the configured user;
- `git`, `rsync`, `tar`, and `sh`;
- a writable `static.workRoot`;
- desktop/browser/code tooling only if those capabilities are requested.

Windows native hosts need:

- the OpenSSH server;
- PowerShell;
- `tar` for archive sync;
- VNC/browser tooling only if desktop flows are requested.

WSL2 hosts additionally need WSL installed and reachable through `wsl.exe`, with
Linux tooling inside the default distribution and `static.workRoot` set to a WSL
path.

## Capabilities

| Capability | Support |
| --- | --- |
| SSH | yes |
| Crabbox sync | yes |
| `cp` | yes on POSIX and WSL2 targets (rsync over resolved SSH) |
| `tunnel` | yes (local and remote loopback only) |
| Desktop / browser / code | host-dependent (requires the tooling installed on the host) |
| Actions hydration | Linux hosts only |
| Tailscale | use the host's existing tailnet address or MagicDNS name |
| Coordinator (brokered) | never — direct-only |

## Gotchas

- General disk, workspace, process, and leftover-state housekeeping on static
  hosts remains yours to manage; release-time connection cleanup is limited to
  the operations described above.
- Static hosts drift. Run `crabbox doctor --provider ssh` and a small
  `crabbox run` before long jobs.
- The provider connects with your configured SSH key; it does not mint a
  per-lease key the way provisioned providers do.

## Related

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Sync](../features/sync.md)
- [SSH keys](../features/ssh-keys.md)
- [SSH lease transport](../features/ssh-transport.md)
