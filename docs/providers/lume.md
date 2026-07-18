# Lume Provider

Local macOS via `lume`.

Base: Lume user, Remote Login, and `scripts/install-macos-lume-image-hooks.sh`
for lease key, locked auth, pinned host key.

Defaults: `lume`, base `crabbox-macos-golden`, Lume storage, user `lume`, work
root `/Users/lume/crabbox`.

Use trusted config, `CRABBOX_LUME_*`, or flags. Repo config cannot set host,
base, storage, or bootstrap user; paths/`ephemeral`: existing leases only.

Clone/start; pin SSH; run; clean; destroy; confirm absent.
