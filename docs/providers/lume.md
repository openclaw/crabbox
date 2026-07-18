# Lume Provider

Base: Lume user + Remote Login; installer locks auth/pins host key.

Packaged: set `tag="v$(crabbox --version)"`; fetch
`install-macos-lume-image-hooks.sh`, `macos-lume-firstboot.sh`,
`macos-lume-firstboot-launchdaemon.plist`, and
`macos-cua-driver-launchagent.plist` from
`https://raw.githubusercontent.com/openclaw/crabbox/$tag/scripts/`. Copy/run in
base; stop it. No `main`/`latest`.

Defaults: `lume`; base `crabbox-macos-golden`; storage; user `lume`; root
`/Users/lume/crabbox`.

Trusted config, `CRABBOX_LUME_*`, flags. Repo cannot set host, base, storage, or
bootstrap user. Lume's unlisted `ephemeral` storage is unsupported.

Clone/start; SSH; run; clean; destroy; confirm absent.
