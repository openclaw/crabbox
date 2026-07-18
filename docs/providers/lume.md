# Lume Provider

Base: Lume user, Remote Login, installer; lease key, locked auth, pinned host key.

Packaged: set `tag="v$(crabbox --version)"`; fetch
`install-macos-lume-image-hooks.sh`, `macos-lume-firstboot.sh`,
`macos-lume-firstboot-launchdaemon.plist`, and
`macos-cua-driver-launchagent.plist` from
`https://raw.githubusercontent.com/openclaw/crabbox/$tag/scripts/`. Copy/run in
base; stop it. No `main`/`latest`.

Defaults: CLI `lume`; base `crabbox-macos-golden`; Lume storage; user `lume`;
root `/Users/lume/crabbox`.

Trusted config, `CRABBOX_LUME_*`, or flags. Repo config cannot set host,
base, storage, or bootstrap user; paths/`ephemeral`: existing leases only.

Clone/start; pin SSH; run; clean; destroy; confirm absent.
