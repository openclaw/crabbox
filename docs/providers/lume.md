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

Lume's unattended installer currently creates `lume`/`lume`, but Crabbox never
uses that password. Before starting a clone, Crabbox creates a fresh `0700`
host directory containing a random challenge and the lease public key, then
passes it only to that `lume run` process with `--shared-dir`. The first-boot
hook installs that key as the sole `authorized_keys` entry and requires public
key authentication for the lease user before Crabbox makes any network SSH
connection.

Installing the image hook also places sshd in a deny-all bootstrap state on the
golden image (`AuthorizedKeysFile none`, with password methods disabled). This
intentionally prevents SSH access to the stopped-image template after setup.
Each clone remains locked while its VirtioFS share mounts; only a valid
challenge switches sshd to the configured lease user and key.
Later reboots with the same VM identity preserve that lease SSH configuration.
The hook disables alternate key-command, principal, host-based, GSSAPI, and
trusted-CA sources and verifies sshd's effective per-user policy before it
publishes readiness.

Install the bundled image hooks before stopping a reusable base. The SSH
identity hook is mandatory; when Cua Driver is already installed, the same
installer also loads its optional LaunchAgent:

```sh
scripts/install-macos-lume-image-hooks.sh
```

The hook rotates clone host keys and returns the new ED25519 key plus platform
identity through the challenge-bound share only after sshd serves that key.
Crabbox pins it before the first network SSH connection.

Keep reusable layers credential-free. It is safe to preinstall signed tools
such as Xcode, Homebrew, Tailscale, or Cua Driver, but leave GitHub and Tailscale
logged out and do not bake API tokens, SSH private keys, signing identities, or
personal keychains into the base. Add credentials only to a disposable clone
after it boots.

Lume uses APFS copy-on-write cloning, so stopped layers and per-lease clones are
fast and initially share unchanged disk blocks.

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

1. `lume clone <base> <lease-vm>` creates an APFS copy-on-write clone.
2. Crabbox starts `lume run <lease-vm> --no-display` as a detached VM-owner
   process and records a private lifecycle log in Crabbox's state directory.
3. Crabbox waits until Lume reports a running guest and IP. It does not trust
   Lume 0.3.16's `sshAvailable` bit because that release can report a false
   negative for an open SSH port.
4. The first-boot hook reads the random challenge, lease user, and public key
   from the private VirtioFS share. It installs only that key, disables SSH
   password and keyboard-interactive authentication for the user, and returns
   the rotated host key through the same share.
5. Crabbox verifies the challenge, writes the returned key to the per-lease
   `known_hosts`, and makes its first network SSH connection with the lease key
   and strict host verification.
6. Normal Crabbox SSH host verification, sync, and execution take over.
7. Release runs guarded remote cleanup while SSH is still available, asks Lume
   to stop the VM, verifies both `stopped` and termination of the recorded
   `lume run` owner process, signals the identity-fenced owner when Lume 0.3.16
   fails to stop it, then deletes the VM and confirms inventory absence.

Every lease gets a different VM, IP address, work directory, key, and macOS
machine identity. Multiple sessions are separate VMs, not multiple users in one
VM. Two sessions can run concurrently when host memory permits.

Run the guarded local smoke after preparing a stopped base:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=lume \
CRABBOX_LUME_BASE=crabbox-macos-golden \
scripts/live-smoke.sh
```

## Credentials

Inject session credentials only after clone boot. Keep reusable images logged
out of GitHub and Tailscale; use separate short-lived credentials per clone.

## Not yet supported

- Crabbox `--desktop`, `webvnc`, and screenshot features. Lume creates a VNC
  endpoint even with `--no-display`, but its listener and generated credential
  need an explicit loopback/tunnel security design before Crabbox advertises it.
- Crabbox-managed Tailscale enrollment. The provider currently rejects
  `--tailscale`; manual post-boot enrollment is separate from the provider.
- Windows or Linux guests. This provider intentionally exposes Lume's macOS
  path only; use Crabbox's Windows and Linux providers for those targets.
