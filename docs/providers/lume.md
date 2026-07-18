# Lume Provider

Local macOS SSH leases. Needs the `lume` CLI.

## Golden image

The stopped base needs the Lume user, Remote Login, and
`scripts/install-macos-lume-image-hooks.sh`.

It installs the lease key, disables alternate auth, rotates host keys, and
authenticates before SSH. Keep credentials out of the base.

## Configuration

Defaults: CLI `lume`; base `crabbox-macos-golden`; storage Lume default; user
`lume`; guest work root `/Users/lume/crabbox`. Matching flags use `--lume-*`.

Use trusted config, `CRABBOX_LUME_*`, or flags. Repo config cannot select host,
base, storage, or bootstrap user; paths and `ephemeral` are existing-lease-only.

Clone/start headless; pin SSH; run; clean; stop; delete; confirm absence.
