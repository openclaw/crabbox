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

Current Lume versions expose each shared directory beneath its basename. Crabbox
uses the fixed guest path `/Volumes/My Shared Files/crabbox-bootstrap` inside a
fresh private host directory for each acquisition. Updated image hooks also
accept the mount-root layout used by older single-share runtimes; they do not
search other shared directories. Reinstall the matching image hooks in existing
golden images before using a named-share Lume runtime. Updating only the host CLI
does not update the guest hook or its launchd watch paths.

Status reads retain an already authenticated guest endpoint so `status --wait`
can probe readiness, without creating connection material or updating the claim.
Inactive or incomplete guests remain metadata-only; a ready guest with missing
or invalid host-key material is not silently trusted. Cleanup treats quiet
`lsof` exit code 1 as a partial match, while warnings, unexpected records, and
other processes' open files still prevent VM deletion.

Defaults: `lume`; base `crabbox-macos-golden`; storage; user `lume`; root
`/Users/lume/crabbox`.

Trusted config, `CRABBOX_LUME_*`, flags. Repo cannot set host, base, storage, or
bootstrap user. Lume's unlisted `ephemeral` storage is unsupported.

Crabbox pins a durable marker in each storage root. Missing or changed storage
identity retains lease claims and keys for fail-closed recovery.
Terminal release and automatic cleanup honor cancellation while waiting for a
lease's claim lock, releasing the capacity lock so other operations can proceed.
Cancellation before admission preserves the VM, claim and SSH key; confirmed
successful cleanup still finishes durable claim retirement. This does not change
the independent acquisition-rollback and recovery-publication policy.

Clone/start; SSH; run; clean; destroy; confirm absent.

## Acquisition recovery

Rollback uses the last successfully published exact lease claim. A failed
metadata refresh does not replace that authority with an empty or uncertain
result. If the stored claim has changed, Crabbox retains the VM and SSH key and
reports the recovery failure rather than continuing without the claim fence.
The existing rollback timeout and recovery-publication lifetime are unchanged.

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

## Heartbeat persistence

Heartbeat and successful foreground touches persist last-use time and idle policy
in the exact existing claim. `heartbeat --idle-timeout` replaces the stored
window; omission preserves it, and the original TTL cap remains in force. Fresh
status reads restore that policy without renewing or rewriting the lease.

Renewal starts only after acquisition has committed its authenticated endpoint.
Startup and recovery claims are retained without a heartbeat write, so concurrent
readiness polling cannot invalidate the acquisition owner's claim revision.
Both idle-ready and running-workload states remain renewable and usable over SSH.

Renewal requires the recorded storage and immutable VM identities. Older claims
without those bindings are retained, not adopted or silently renewed. Warm a new
ownership-bound lease and preserve or migrate existing work before choosing to
retire an older VM. Foreground touch failures retain the existing warning behavior.
