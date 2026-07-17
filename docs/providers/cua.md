# CUA Provider

Read when:

- evaluating the experimental `provider: cua` integration;
- checking CUA SDK, authentication, or account inventory;
- inspecting an existing CUA sandbox;
- changing `internal/providers/cua`.

CUA support is experimental and read-only. Crabbox can run diagnostics, list
existing account sandboxes, and inspect an existing sandbox. It cannot create,
modify, stop, or delete a CUA sandbox.

The upstream CUA create API does not currently accept an idempotency key or a
client-assigned unique identity returned by create, list, and get. If a create
succeeds remotely but the client times out before receiving the generated
sandbox ID, Crabbox cannot identify or delete the billed resource safely.
`warmup` and every `run` mode therefore fail closed before the Python bridge or CUA
SDK can issue a create request. Deletion is also disabled: the upstream API
cannot atomically bind a delete to an immutable sandbox identity, so a
verify-then-delete sequence could delete a replacement with the same name.
Track both requirements in
https://github.com/openclaw/crabbox/issues/381.

## Setup And Doctor

Install the CUA SDK in a Python 3.12 or 3.13 environment. Point
`cua.bridgeCommand` or `--cua-bridge-command` at that environment's Python
executable when it is not the default `python3`:

```sh
python3.13 -m venv ~/.venvs/crabbox-cua
~/.venvs/crabbox-cua/bin/pip install cua
crabbox doctor --provider cua --cua-bridge-command ~/.venvs/crabbox-cua/bin/python --json
```

The smaller `cua-sandbox` fallback supports Python 3.11 through 3.13. The
doctor result always reports `experimental=true` and `provisioning=false`, plus
a warning linked to the tracking issue. With credentials configured, doctor
verifies them through a read-only inventory request. It does not create,
modify, stop, or delete a sandbox.

CUA credentials are passed to the SDK through environment only. Set either
`CRABBOX_CUA_API_KEY` or `CUA_API_KEY`; the Crabbox-specific name wins when both
exist. Crabbox does not store either value in config or accept it on argv. The
SDK bridge uses an isolated home directory, so it does not consume credentials
from `~/.cua/credentials`. It also sets `CUA_TELEMETRY_ENABLED=false` for every
diagnostic subprocess. Standard HTTP proxy, no-proxy, and SSL certificate-store
environment settings are forwarded to the isolated subprocess; unrelated
ambient variables remain excluded.

The API URL is trusted local input only. It can come from `--cua-api-url`,
`CRABBOX_CUA_API_URL`, or SDK-compatible `CUA_BASE_URL`. Repository YAML cannot
set it. Overrides must use HTTPS, except loopback HTTP for local development,
and cannot contain userinfo, query parameters, or fragments. A terminal `/v1`
is removed because the SDK adds its own API version path.

## Available Commands

These commands are read-only:

```sh
crabbox doctor --provider cua --json
crabbox list --provider cua
crabbox status --provider cua --id <existing-cua-sandbox-id>
crabbox status --provider cua --id <claimed-lease-or-slug>
```

`list` returns existing CUA account inventory and labels each entry
`claimed=true|false`. `status` accepts a raw CUA sandbox ID for read-only
inspection. Neither command creates a local claim or adopts the resource.

Every `run`, including `run --id <claimed-lease-or-slug>`, and every `warmup`
fails with the upstream idempotency guard. `stop` and `cleanup` fail with the
immutable-identity guard. No flag or config value enables remote mutation.

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
```

Image, kind, region, and sizing settings are retained for configuration
compatibility and future use after the upstream safety gate is satisfied. They
do not enable provisioning.

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
```

Environment overrides use the matching `CRABBOX_CUA_*` names. `CUA_BASE_URL`
is also accepted for the API URL. `--class` and `--type` are rejected for
`provider=cua`.

## Existing Claims

Crabbox lease IDs use the `cuabx_` prefix. Exact claims bound to the normalized
API endpoint and a non-secret fingerprint of the current environment
credential can label matching inventory as `claimed=true`. Changing
credentials fails closed, even on the same endpoint. Legacy URL-only claims
are not adopted. Crabbox never synthesizes a claim from inventory or
timestamps, and claims do not enable mutation in this mode.

## Diagnostic Smoke

The CUA smoke is non-mutating and requires no live opt-in:

```sh
scripts/live-cua-smoke.sh
```

It builds a temporary binary, proves that `warmup` is rejected before the SDK
bridge, and runs doctor. Missing SDK or credentials are reported as
`environment_blocked`; a ready diagnostic environment reports
`diagnostic_only`. The smoke never creates a billable resource.

Keep provider tokens out of repository config, command arguments, timing JSON,
logs, claim files, and documentation examples.
