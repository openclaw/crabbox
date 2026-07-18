# Lume Provider

Lume provides macOS SSH leases on Apple silicon with the `lume` CLI and no cloud
account. Crabbox rejects a third guest.

## Golden image

The stopped base needs the `lume` account (or configured `lume.user`) with
Remote Login, workload tools, and this first-boot hook:

```sh
scripts/install-macos-lume-image-hooks.sh
```

The hook installs the lease key, disables alternate authentication, rotates host
keys, and returns authenticated identity before network SSH. Keep credentials
and keychains out of the base.

## Configuration

| Flag | Default | Selects |
| --- | --- | --- |
| `--lume-cli` | `lume` | CLI executable |
| `--lume-base` | `crabbox-macos-golden` | Stopped base VM |
| `--lume-storage` | home | Persistent storage |
| `--lume-user` | `lume` | Prepared SSH account |
| `--lume-work-root` | `/Users/lume/crabbox` | Guest work root |

Use trusted user config, `CRABBOX_LUME_*`, or flags. Repository config cannot
select host, base, storage, or bootstrap user; paths and `ephemeral` are
existing-lease-only.

## Lifecycle

Clone and start headless; authenticate and pin SSH; run; then guarded-clean,
stop, delete the exact claimed VM, and confirm absence.
