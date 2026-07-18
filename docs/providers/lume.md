# Lume Provider

Lume provides local ARM64 macOS SSH leases. Crabbox clones a stopped base VM,
runs over SSH, then stops and deletes the clone. It needs Apple silicon and the
`lume` CLI, but no coordinator, cloud account, or cloud credentials. Crabbox
serializes acquisition and rejects a third active macOS guest.

## Golden image

The configured base must be stopped. Its `lume` account (or `lume.user`) needs
Remote Login, workload tools, and the bundled first-boot hook:

```sh
scripts/install-macos-lume-image-hooks.sh
```

The hook blocks SSH until it installs the lease key, disables alternate
authentication, rotates host keys, and returns the ED25519 key and platform
identity through a private challenge-bound share. Crabbox pins that key before
network SSH. Keep credentials and personal keychains out of the base.

## Configuration

| Flag | Default | Description |
| --- | --- | --- |
| `--lume-cli` | `lume` | Lume CLI path |
| `--lume-base` | `crabbox-macos-golden` | Stopped base VM |
| `--lume-storage` | home storage | Persistent storage name |
| `--lume-user` | `lume` | Prepared SSH account |
| `--lume-work-root` | `/Users/lume/crabbox` | Guest work root |

Use trusted user config, `CRABBOX_LUME_*` variables, or flags. Repository config
cannot select the host, base, storage, or bootstrap user. Paths and `ephemeral`
are existing-lease-only.

## Lifecycle

Clone and start headless under an identity-fenced owner; authenticate first
boot and pin its key; use normal Crabbox SSH sync/execution; then run guarded
cleanup, stop, delete the exact claimed VM, and confirm absence.
