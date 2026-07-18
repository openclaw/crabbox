# Lume Provider

Lume provides macOS SSH leases on Apple silicon with the `lume` CLI and no cloud
account.

## Golden image

The stopped base needs the configured Lume user with Remote Login and this
first-boot hook:

```sh
scripts/install-macos-lume-image-hooks.sh
```

The hook installs the lease key, disables alternate authentication, rotates host
keys, and authenticates identity before SSH. Keep credentials out of the base.

## Configuration

| Flag | Default | Selects |
| --- | --- | --- |
| `--lume-cli` | `lume` | CLI executable |
| `--lume-base` | `crabbox-macos-golden` | Stopped base VM |
| `--lume-storage` | home | Persistent storage |
| `--lume-user` | `lume` | Prepared SSH account |
| `--lume-work-root` | `/Users/lume/crabbox` | Guest work root |

Use trusted config, `CRABBOX_LUME_*`, or flags. Repository config cannot
select host, base, storage, or bootstrap user; paths and `ephemeral` are
existing-lease-only.

## Lifecycle

Clone/start headless; pin SSH; run; guarded-clean; stop; delete the claimed VM;
confirm absence.
