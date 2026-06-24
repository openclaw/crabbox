# CUA Provider

Read when:

- choosing `provider: cua`;
- configuring the CUA provider contract, image, kind, workdir, sizing, or SDK
  bridge diagnostics;
- changing `internal/providers/cua`.

CUA is a delegated run provider foundation. Crabbox v1 treats CUA as a Linux
cloud sandbox where CUA owns command and file transport and Crabbox owns
provider selection, non-secret config, local claims, archive sync guardrails,
cleanup commands, and normalized output.

This page is intentionally limited to the PLAN-01 contract. The Python SDK
bridge, local claim reconciliation, lifecycle commands, archive sync behavior,
and live smoke instructions are completed by later implementation phases.

## Current Contract

The provider is registered as `cua` with no aliases. It advertises Linux,
delegated-run execution with archive sync and cleanup support, and it does not
advertise SSH, desktop/browser/code, Tailscale, URL bridge, snapshots,
checkpoints, forks, cache volumes, Actions hydration, MCP attachments, run
artifacts, or run downloads.

`crabbox doctor --provider cua --json` is non-mutating in this foundation phase.
It validates local config and reports the bridge/lifecycle work as deferred
without creating CUA resources.

## Auth And API URL

Crabbox does not persist CUA API keys in config and does not accept API keys on
argv. Later bridge code resolves credentials through CUA's environment or
credential store, starting with `CUA_API_KEY`.

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
