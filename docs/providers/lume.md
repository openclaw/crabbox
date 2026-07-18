# Lume Provider

Local macOS via `lume`.

Base: Lume user, Remote Login, and `scripts/install-macos-lume-image-hooks.sh`
for lease key, locked auth, pinned host key.

Packaged installs: set `tag="v$(crabbox --version)"`; download the installer,
`macos-lume-firstboot.sh`, `macos-lume-firstboot-launchdaemon.plist`, and
`macos-cua-driver-launchagent.plist` from
`https://raw.githubusercontent.com/openclaw/crabbox/$tag/scripts/` into one
directory. Copy it into the running base VM, run the installer there, then stop
the base. Use the exact installed tag, never `main` or `latest`.

Defaults: `lume`, base `crabbox-macos-golden`, Lume storage, user `lume`, work
root `/Users/lume/crabbox`.

Use trusted config, `CRABBOX_LUME_*`, or flags. Repo config cannot set host,
base, storage, or bootstrap user; paths/`ephemeral`: existing leases only.

Clone/start; pin SSH; run; clean; destroy; confirm absent.
