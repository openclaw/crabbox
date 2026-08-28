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
