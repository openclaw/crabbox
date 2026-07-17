# Lume Provider

Lume is a local SSH-lease provider. Crabbox clones a stopped macOS VM with
Lume, starts the clone headless with a private bootstrap share, installs a
unique Crabbox SSH public key through that local share, syncs the checkout over
SSH, runs commands through the normal Crabbox SSH executor, and stops and
deletes the clone on release.

The provider is local only. It does not use the coordinator or cloud
credentials.

**Target:** macOS on ARM64.

**Host:** an Apple silicon Mac with the `lume` CLI installed. Apple's macOS
virtualization license permits at most two running macOS guests on one host.
Crabbox serializes acquisitions through a host-local cross-process lock and
rejects a third running or starting guest before cloning.

## Golden-image contract

The configured base must be a stopped Lume VM. Its guest account must:

- be named `lume` by default, or match `lume.user`;
- have Remote Login enabled;
- have the tools needed by the intended workload;
- include the bundled first-boot hook that rotates SSH host keys, installs the
  per-lease key, and disables SSH password authentication for the lease user.

Crabbox never uses Lume's unattended-install password. It passes a random
challenge and lease public key through a private `0700` VirtioFS share. The
golden image denies all SSH login until the hook installs that key, disables
alternate authentication sources, and verifies the effective sshd policy.

Install the bundled image hooks before stopping a reusable base:

```sh
scripts/install-macos-lume-image-hooks.sh
```

The hook rotates clone host keys and returns the new ED25519 key plus platform
identity through the challenge-bound share only after sshd serves that key.
Crabbox pins it before the first network SSH connection.

Keep reusable layers credential-free. Preinstalled tools are fine; accounts,
tokens, private keys, signing identities, and personal keychains are not. Add
credentials only to a disposable clone after boot.

## Configuration

CLI flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--lume-cli` | `lume` | Path to the Lume CLI |
| `--lume-base` | `crabbox-macos-golden` | Stopped VM cloned for every lease |
| `--lume-storage` | home storage | Registered persistent Lume storage name. Direct paths and `ephemeral` remain supported for resolving and removing existing leases, but not new leases. |
| `--lume-user` | `lume` | Prepared guest SSH account |
| `--lume-work-root` | `/Users/lume/crabbox` | Guest work root below the user's home |

YAML in the trusted user config printed by `crabbox config path`:

```yaml
provider: lume
lume:
  cliPath: /opt/homebrew/bin/lume
  base: crabbox-macos-golden
  storage: fast
  user: lume
  workRoot: /Users/lume/crabbox
```

`cliPath`, `base`, and `storage` select host executables or local VM data, so
Crabbox ignores those fields in repository-local `crabbox.yaml` and
`.crabbox.yaml`. Set them in trusted user config, environment variables, or
explicit CLI flags.

Environment variables: `CRABBOX_LUME_CLI`, `CRABBOX_LUME_BASE`,
`CRABBOX_LUME_STORAGE`, `CRABBOX_LUME_USER`, and
`CRABBOX_LUME_WORK_ROOT`.

## Lifecycle

1. Clone the stopped base and start `lume run <lease-vm> --no-display` under a
   recorded, identity-fenced owner process.
2. Wait for a running guest and IP, then authenticate the first-boot challenge
   and pin the returned host key before the first SSH connection.
3. Use normal Crabbox SSH verification, sync, and command execution.
4. Run guarded remote cleanup, stop the VM and owner process, delete the exact
   claimed VM, and confirm inventory absence.

Run the guarded local smoke after preparing a stopped base:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=lume \
CRABBOX_LUME_BASE=crabbox-macos-golden \
scripts/live-smoke.sh
```

## Not yet supported

- Desktop, web VNC, and screenshots; Lume's generated VNC endpoint needs a
  loopback/tunnel security design before Crabbox can advertise it.
- Crabbox-managed Tailscale enrollment; `--tailscale` is rejected.
- Windows or Linux guests; this provider exposes only Lume's macOS path.
