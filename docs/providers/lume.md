# Lume Provider

Base: Lume user + Remote Login; installer locks auth/pins host key.

Provider control uses the configured Lume CLI in the local process context.
That describes the control-plane interface, not guest SSH/bootstrap credentials
or authenticated readiness. Static metadata never checks the CLI or guest login.

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

Crabbox pins a durable marker in each storage root. Missing or changed storage
identity retains lease claims and keys for fail-closed recovery.

Clone/start; SSH; run; clean; destroy; confirm absent.

## Configuration bindings

The five non-secret settings use the shared typed configuration bindings:

| YAML key under `lume` | Environment variable | Flag |
| --- | --- | --- |
| `cliPath` | `CRABBOX_LUME_CLI` | `--lume-cli` |
| `base` | `CRABBOX_LUME_BASE` | `--lume-base` |
| `storage` | `CRABBOX_LUME_STORAGE` | `--lume-storage` |
| `user` | `CRABBOX_LUME_USER` | `--lume-user` |
| `workRoot` | `CRABBOX_LUME_WORK_ROOT` | `--lume-work-root` |

Only `workRoot` is accepted from repository configuration; the other four keys
require trusted user configuration, environment variables, or flags. Empty YAML
or environment strings preserve prior values, while explicitly empty flags assign
before the existing selected-provider defaults and validation run.

Runtime defaults can derive `/Users/<user>/crabbox` after a guest-user change or
inherit a custom generic work root. That user-dependent decision, native storage
resolution, and validation remain provider-owned; the shared bindings do not read
Lume settings or create a VM.
