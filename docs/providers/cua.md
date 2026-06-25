# CUA Provider

Read when:

- choosing `provider: cua`;
- configuring CUA cloud Linux sandbox runs;
- troubleshooting CUA local claims, archive sync, cleanup, or live smoke;
- changing `internal/providers/cua`.

CUA is a delegated-run provider. Crabbox creates a CUA cloud Linux sandbox,
uploads the repository with Crabbox archive sync, asks CUA to run commands, and
records a local Crabbox claim for retained sandboxes. CUA owns command and file
transport. Crabbox owns provider selection, non-secret config, safe local
claims, sync guardrails, cleanup commands, and normalized output.

## Supported Commands

```sh
crabbox doctor --provider cua --json
crabbox warmup --provider cua
crabbox run --provider cua -- go test ./...
crabbox run --provider cua --id <lease-or-slug> -- go test ./...
crabbox list --provider cua
crabbox status --provider cua --id <lease-or-slug>
crabbox stop --provider cua <lease-or-slug>
crabbox cleanup --provider cua --dry-run
crabbox cleanup --provider cua
```

Fresh `run` creates a short-lived sandbox and deletes it by default after the
command finishes. `warmup`, `run --keep`, `run --keep-on-failure`, and reused
`run --id` keep the sandbox under a local Crabbox claim until `stop` or
`cleanup` removes it.

## Auth And API URL

Crabbox never stores CUA API keys in config and does not accept API keys on
argv. The bridge passes credentials to the CUA SDK through environment only:

```sh
export CRABBOX_CUA_API_KEY=...
# or
export CUA_API_KEY=...
```

`CRABBOX_CUA_API_KEY` wins over `CUA_API_KEY`. Command environment forwarding
strips CUA auth variables before running the user command inside the sandbox.

The API URL is trusted local input only. It can come from `--cua-api-url`,
`CRABBOX_CUA_API_URL`, or SDK-compatible `CUA_BASE_URL`. Repository YAML cannot
set the API URL. Overrides must be HTTPS, except loopback HTTP for local
development, and must not contain userinfo, query parameters, or fragments.

## Config

```yaml
provider: cua
target: linux
cua:
  image: ubuntu:24.04
  kind: container
  region: ""
  workdir: /workspace/crabbox
  vcpus: 0
  memoryMB: 0
  diskGB: 0
  startupTimeoutSecs: 0
  execTimeoutSecs: 600
  bridgeCommand: python3
  sdkPackage: cua
  sdkImport: cua
  sdkFallbackImport: cua_sandbox
  forgetMissing: false
```

Provider flags:

```text
--cua-api-url
--cua-image
--cua-kind
--cua-region
--cua-workdir
--cua-vcpus
--cua-memory-mb
--cua-disk-gb
--cua-startup-timeout-secs
--cua-exec-timeout-secs
--cua-bridge-command
--cua-sdk-package
--cua-sdk-import
--cua-sdk-fallback-import
--cua-forget-missing
```

Environment overrides:

```text
CRABBOX_CUA_API_URL / CUA_BASE_URL
CRABBOX_CUA_IMAGE
CRABBOX_CUA_KIND
CRABBOX_CUA_REGION
CRABBOX_CUA_WORKDIR
CRABBOX_CUA_VCPUS
CRABBOX_CUA_MEMORY_MB
CRABBOX_CUA_DISK_GB
CRABBOX_CUA_STARTUP_TIMEOUT_SECS
CRABBOX_CUA_EXEC_TIMEOUT_SECS
CRABBOX_CUA_BRIDGE_COMMAND
CRABBOX_CUA_SDK_PACKAGE
CRABBOX_CUA_SDK_IMPORT
CRABBOX_CUA_SDK_FALLBACK_IMPORT
CRABBOX_CUA_FORGET_MISSING
```

`--class` and `--type` are rejected for `provider=cua`; use CUA-specific image,
kind, and sizing flags instead.

## Lifecycle And Claims

Crabbox lease IDs use the `cuabx_` prefix. CUA sandbox names use the
`crabbox-cua-` prefix. Stop and cleanup are intentionally conservative:

- `stop` resolves a local Crabbox claim by lease ID or slug before deleting;
- `cleanup` scans local `cuabx_` claims only;
- both validate the active CUA API scope and expected sandbox name prefix;
- neither deletes arbitrary CUA account sandboxes from a loose name match.

If the remote sandbox is already gone, Crabbox leaves the claim in place by
default. Use `--cua-forget-missing` or `cua.forgetMissing: true` only when you
want stop or cleanup to remove stale local claims after a missing-sandbox proof.

## Sync And Command Execution

CUA uses Crabbox delegated archive sync. Normal `run` uploads tracked files plus
non-ignored untracked files according to the usual Crabbox sync rules. `sync.delete`
is honored by extracting into a staging directory and replacing the remote
workdir. `--sync-only`, `--no-sync`, and `--force-sync-large` are supported.

The default workdir is `/workspace/crabbox`. It must be an absolute path under
`/workspace/<dedicated-subdir>`; broad paths such as `/workspace` are rejected.

Use `--shell` for shell syntax:

```sh
crabbox run --provider cua --shell 'python -m pytest && go test ./...'
```

Use plain argv for one executable:

```sh
crabbox run --provider cua -- go test ./...
```

## Unsupported In V1

CUA is not a Crabbox SSH lease provider. These features are intentionally not
advertised in v1:

- SSH login, `connect`, `ssh`, `cp`, or port forwarding;
- desktop/browser/code surfaces, screenshots, VNC, mouse, keyboard, mobile, or
  agent-control helpers;
- broker/coordinator support;
- GitHub Actions hydration or Actions runner mode;
- cache volumes, URL bridge, run artifacts, run downloads, and captured stdout
  or stderr files;
- macOS, Windows, Android, local CUA runtime, snapshots, checkpoints, forks, or
  arbitrary account sandbox management.

Unsupported Crabbox flags fail before CUA resources are created.

## Live Smoke

The CUA live smoke is opt-in because it can create paid cloud resources:

```sh
CRABBOX_CUA_LIVE=1 CRABBOX_CUA_API_KEY=... scripts/live-cua-smoke.sh
```

Without `CRABBOX_CUA_LIVE=1`, the script prints:

```text
skipped reason=missing_CRABBOX_CUA_LIVE
```

With opt-in but no credentials, it prints `environment_blocked`. With
credentials, it builds a temporary local Crabbox binary, runs non-mutating
doctor, creates a short-lived CUA sandbox, proves warmup/run/reuse/status/list,
stops the sandbox, verifies cleanup, and prints exactly one final
classification:

- `live_cua_smoke_passed`
- `skipped`
- `environment_blocked`
- `quota_blocked`
- `validation_failed`
- `cleanup_failed`
- `diagnostic_only`

The script attempts cleanup on every path after a sandbox is created. If cleanup
cannot be proven, the final classification is `cleanup_failed` and the output
names the retained slug/lease context needed for manual cleanup.

## Troubleshooting

Run non-mutating readiness first:

```sh
crabbox doctor --provider cua --json
```

Common classes:

- `environment_blocked`: missing Python, unsupported Python version, missing
  CUA SDK import, missing credentials, network/TLS failure, or auth rejection.
- `quota_blocked`: CUA quota, rate limit, or capacity failure.
- `validation_failed`: bad config, bad workdir, unsupported target, or unknown
  bridge action.
- `diagnostic_only`: live smoke could not prove the full contract and did not
  have enough evidence to classify auth, quota, validation, or cleanup.

Keep provider tokens out of repo config, command arguments, timing JSON, logs,
claim files, and docs examples.
