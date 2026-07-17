# Lume Provider

Read this when you:

- choose `provider: lume` (aliases `local-lume`, `lume-macos`);
- want isolated local macOS leases on an Apple silicon Mac;
- maintain a stopped Lume golden VM for repeatable development sessions;
- change `internal/providers/lume`.

Lume is a local SSH-lease provider. Crabbox clones a stopped macOS VM with
Lume, starts the clone headless, injects a unique Crabbox SSH public key through
Lume's guest transport, syncs the checkout over SSH, runs commands through the
normal Crabbox SSH executor, and stops and deletes the clone on release.

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
- accept the default Lume bootstrap SSH password so Crabbox can add the
  per-lease public key;
- have the tools needed by the intended workload;
- regenerate SSH host keys on the clone's first boot.

Lume's unattended installer currently creates `lume`/`lume`. Crabbox uses that
known bootstrap password through a short-lived host-local askpass helper only
during identity readiness and key injection; it does not store a guest password
in repository config or place it on process argv. Hardened images that change
the bootstrap password are not supported yet.

Install the bundled image hooks before stopping a reusable base. The SSH
identity hook is mandatory; when Cua Driver is already installed, the same
installer also loads its optional LaunchAgent:

```sh
scripts/install-macos-lume-image-hooks.sh
```

The first-boot hook records the base VM's platform identity, then regenerates
OpenSSH host keys whenever a clone boots with a new identity. It publishes the
readable identity marker only after sshd serves the newly generated ED25519
key. Crabbox waits for that marker, then pins the served key while injecting
the lease's public key:

```sh
sudo install -d -o root -g wheel -m 0755 /usr/local/libexec
sudo install -o root -g wheel -m 0755 \
  scripts/macos-lume-firstboot.sh \
  /usr/local/libexec/crabbox-lume-firstboot
sudo install -o root -g wheel -m 0644 \
  scripts/macos-lume-firstboot-launchdaemon.plist \
  /Library/LaunchDaemons/dev.crabbox.lume-firstboot.plist
sudo launchctl bootstrap system \
  /Library/LaunchDaemons/dev.crabbox.lume-firstboot.plist
```

Keep reusable layers credential-free. It is safe to preinstall signed tools
such as Xcode, Homebrew, Tailscale, or Cua Driver, but leave GitHub and Tailscale
logged out and do not bake API tokens, SSH private keys, signing identities, or
personal keychains into the base. Add credentials only to a disposable clone
after it boots.

A useful local layering model is:

```text
macOS + Xcode/CLT + Homebrew
  -> language/tooling layer
    -> Crabbox/Cua Driver control layer
      -> one disposable clone per Crabbox lease
```

Lume uses APFS copy-on-write cloning, so stopped layers and per-lease clones are
fast and initially share unchanged disk blocks.

## Configuration

CLI flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--lume-cli` | `lume` | Path to the Lume CLI |
| `--lume-base` | `crabbox-macos-golden` | Stopped VM cloned for every lease |
| `--lume-storage` | home storage | Optional Lume storage name or path |
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
4. A short-lived password bootstrap polls the clone identity marker without a
   host-key assumption, then pins the rotated key in a per-lease `known_hosts`
   file while adding the unique per-lease public key. Normal key-authenticated
   Crabbox SSH reuses that pinned identity immediately afterward.
5. Normal Crabbox SSH host verification, sync, and execution take over.
6. Release runs guarded remote cleanup while SSH is still available, asks Lume
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

## Cua Driver and credentials

Cua Driver can be installed in a control-layer image and granted macOS
Accessibility, Screen & System Audio Recording, and Automation consent before
that layer is stopped. A controller can then invoke its CLI or MCP transport
through the lease's authenticated SSH connection. Do not expose an unauthenticated
driver service on the guest network.

`scripts/macos-cua-driver-launchagent.plist` is the corresponding per-user
LaunchAgent template. Install it in `~/Library/LaunchAgents/` for the prepared
GUI account and bootstrap it in that user's `gui/<uid>` launchd domain after
the signed `/Applications/CuaDriver.app` bundle is installed.

Session credentials should be injected after clone boot through the existing
Crabbox environment/SSH workflow or another short-lived secret channel. For
Tailscale, keep the base logged out and authenticate each clone with a separate
ephemeral or tagged auth key; revoke or log it out during guarded remote
cleanup.

## Not yet supported

- Crabbox `--desktop`, `webvnc`, and screenshot features. Lume creates a VNC
  endpoint even with `--no-display`, but its listener and generated credential
  need an explicit loopback/tunnel security design before Crabbox advertises it.
- Crabbox-managed Tailscale enrollment. The provider currently rejects
  `--tailscale`; manual post-boot enrollment is separate from the provider.
- Custom Lume guest bootstrap passwords.
- Windows or Linux guests. This provider intentionally exposes Lume's macOS
  path only; use Crabbox's Windows and Linux providers for those targets.
