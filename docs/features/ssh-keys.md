# SSH Keys

Read when:

- changing local key storage or key generation;
- debugging SSH authentication or host-key trust;
- changing how provider key pairs are imported or cleaned up.

Crabbox generates a fresh SSH client authentication key per lease by default.
This keeps a long-lived personal key out of every runner and gives the provider
layer a predictable, per-lease resource name it can import and later delete.

## Per-lease key generation

When a lease is created, the CLI runs `ssh-keygen` to produce a key it stores
locally. The key type is `ed25519` for most leases, and `rsa` (4096-bit) only
for AWS and Azure Windows targets, where the platform requires RSA. Generation
is idempotent: if a key already exists for the lease ID, it is reused as-is.

Local key storage lives under the Crabbox user config directory, outside the
repository:

```text
macOS:   ~/Library/Application Support/crabbox/testboxes/<lease>/id_ed25519
Linux:   ~/.config/crabbox/testboxes/<lease>/id_ed25519
```

The matching `<lease>/id_ed25519.pub` sits beside it. The key directory is
created with `0700` permissions.

## Provisioned host identity

For supported coordinator-backed Linux leases, the coordinator also generates
a separate Ed25519 server host-key pair and injects it before the machine's
first boot. It stores only the public half on the lease record; the private half
is sent only in the provider bootstrap payload. `crabbox inspect --json`
exposes the public identity as `sshHostKey` in exact `algorithm base64` form for
automation that pins the server identity before connecting.

This pre-boot path is available for Hetzner, GCP, and non-private AWS Linux
leases, and for Azure Linux leases not created from a snapshot. The field is
omitted for private AWS workspaces, Windows, macOS, Daytona, Azure snapshot,
registered, and direct-provider leases, where Crabbox cannot authoritatively
inject a host key before boot.

## Host-key trust and connection reuse

A per-lease `known_hosts` file lives next to the key
(`<lease>/known_hosts`). All SSH connections use:

- `StrictHostKeyChecking=accept-new` — trust a host's key on first contact, then
  pin it;
- `UserKnownHostsFile` pointed at the per-lease `known_hosts`;
- `IdentitiesOnly=yes` with `-i <key>` so only the lease key is offered;
- `ForwardAgent=no`, `ForwardX11=no`, and `ForwardX11Trusted=no` so broad local
  OpenSSH configuration cannot delegate ambient agent or X11 authority to a
  lease.

Because host keys are scoped to the lease's own file, a reused provider IP from
a previous lease never poisons the user's global `~/.ssh/known_hosts`, and two
leases sharing an address do not cross host-key state.

The coordinator host-key metadata does not change this interactive trust path:
`crabbox ssh` continues to use per-lease TOFU with `accept-new`.

On macOS and Linux, connection multiplexing is enabled
(`ControlMaster=auto`, `ControlPersist=10m`) with a `ControlPath` scoped by the
key path, so reused IPs do not share a control socket between leases. Windows
OpenSSH and secret-authenticated targets disable multiplexing
(`ControlMaster=no`).

## What the broker sees

In brokered mode the CLI sends only the public key to the coordinator; the
private key never leaves the local machine. The Worker imports or reuses that
public key in the target provider under a stable per-lease name derived from the
lease ID (`crabbox-<lease>`, with `_` rewritten to `-`):

- Hetzner uploads it as an SSH key, reusing an existing key with matching
  contents instead of creating a duplicate;
- AWS imports it as an EC2 key pair;
- Azure and GCP inject it through their respective instance metadata / key
  paths.

When the coordinator assigns a different final lease ID than the provisional one
the CLI started with, the CLI renames the local key directory to the final ID so
later `status`, `ssh`, `run --id`, and `stop` commands keep finding the key.

## Cleanup

Provider delete paths remove the per-lease cloud key or key pair when the
machine is deleted (for example AWS `DeleteKeyPair`, Hetzner SSH-key delete, and
the equivalent on other adapters). Several provider backends also remove the
local key directory when they release or clean up a lease (for example the
Parallels, local-container, Semaphore, Blacksmith, and Sprites adapters).

## Bringing your own key

Setting `CRABBOX_SSH_KEY` (or the `ssh.key` config value) points the CLI at an
existing private key instead of a generated per-lease one. `doctor` validates
that key — checking the private path and its `.pub` sibling — only when
`CRABBOX_SSH_KEY` is set; otherwise it reports the default per-lease mode as
healthy.

## Related docs

- [Security](../security.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Identifiers](identifiers.md)
- [ssh command](../commands/ssh.md)
- [doctor command](../commands/doctor.md)
