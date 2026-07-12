# Superserve Provider

Read this when:
- choosing `provider: superserve`;
- configuring the Superserve API endpoint, template, snapshot, workdir, network allow or deny lists, or cleanup behavior;
- changing `internal/providers/superserve`.

Superserve provides hosted Linux sandboxes through the Superserve API. It is a
**delegated-run** provider: Crabbox calls the Superserve control plane for
sandbox lifecycle and metadata ownership, activates the sandbox for data-plane
access, uploads a portable archive, and runs the command through Superserve's
exec API. There is no direct Crabbox SSH target and no local rsync.

Crabbox owns local config, repo claims, slug allocation, archive sync
guardrails, metadata ownership, run timing summaries, and normalized
`list`/`status` output. Superserve owns sandbox state, file upload, command
transport, and sandbox deletion.

## When To Use

Use Superserve when commands should run in a hosted Linux sandbox and you do
not need a full Crabbox SSH lease. It fits isolated test runs, quick remote
proofs, and agent-style command execution where archive sync plus delegated
exec is enough.

Use an SSH-lease provider such as AWS, Hetzner, Azure, Google Cloud, Static SSH,
or Local Container when you need `crabbox ssh`, VNC, code-server, Actions
hydration, direct rsync behavior, or desktop/browser/code capability flags.

Superserve is Linux-only. Desktop, browser, code, VNC, SSH, Tailscale, and
Actions runner hydration are not available.

## Setup

Create a Superserve API key in the Superserve console, then export it through
the environment. Crabbox never accepts the key as a command-line flag and does
not persist it in `crabbox.yaml`, `.crabbox.yaml`, or trusted user config.

## Auth

```sh
export CRABBOX_SUPERSERVE_API_KEY=ss_live_...
# or
export SUPERSERVE_API_KEY=ss_live_...
```

`CRABBOX_SUPERSERVE_API_KEY` takes precedence when both variables are present.
The key is sent only in the `X-API-Key` header to the configured Superserve
control plane. Data-plane requests use the short-lived sandbox access token
from Superserve in the `X-Access-Token` header. Neither token is stored in
Crabbox claims or config.

Rotate the key if it was ever pasted into a chat, shell history, issue, PR,
log, or persistent artifact.

## Commands

```sh
crabbox doctor --provider superserve
crabbox warmup --provider superserve --superserve-template superserve/base
crabbox run --provider superserve -- pnpm test
crabbox run --provider superserve --id quiet-river --no-sync -- echo reused
crabbox run --provider superserve --id quiet-river --sync-only
crabbox status --provider superserve --id quiet-river --wait
crabbox list --provider superserve --json
crabbox stop --provider superserve quiet-river
crabbox cleanup --provider superserve --dry-run
```

`warmup` always keeps the sandbox until an explicit `stop`. A `run` without
`--id` creates a sandbox and deletes it after the command unless `--keep` or
`--keep-on-failure` asks Crabbox to retain it.

## Config

```yaml
provider: superserve
target: linux
superserve:
  template: superserve/base
  workdir: /workspace/crabbox
  timeoutSecs: 5400              # 0 derives the sandbox lifetime from Crabbox TTL
  execTimeoutSecs: 600
  networkAllowOut:
    - api.example.com
  networkDenyOut:
    - 169.254.169.254/32
```

Trusted user config may also set `superserve.baseUrl`. Repository config cannot
set the base URL, because redirecting API-key traffic from a checked-in file is
not trusted.

Provider flags, each overriding the matching config key:

```text
--superserve-base-url
--superserve-template
--superserve-snapshot
--superserve-workdir
--superserve-timeout-secs
--superserve-exec-timeout-secs
--superserve-network-allow-out
--superserve-network-deny-out
--superserve-forget-missing
```

Environment overrides:

```text
CRABBOX_SUPERSERVE_API_KEY
SUPERSERVE_API_KEY
CRABBOX_SUPERSERVE_BASE_URL
SUPERSERVE_BASE_URL
CRABBOX_SUPERSERVE_TEMPLATE
CRABBOX_SUPERSERVE_SNAPSHOT
CRABBOX_SUPERSERVE_WORKDIR
CRABBOX_SUPERSERVE_TIMEOUT_SECS
CRABBOX_SUPERSERVE_EXEC_TIMEOUT_SECS
CRABBOX_SUPERSERVE_NETWORK_ALLOW_OUT
CRABBOX_SUPERSERVE_NETWORK_DENY_OUT
CRABBOX_SUPERSERVE_FORGET_MISSING
```

Defaults: API URL `https://api.superserve.ai`, template `superserve/base`,
workdir `/workspace/crabbox`, command timeout `600` seconds, and a sandbox
lifetime derived from Crabbox's TTL. Superserve caps sandbox lifetimes at
`604800` seconds (7 days).

The base URL must be absolute, must not include userinfo, query parameters, or
a fragment, and must use HTTPS except for loopback development endpoints.
Like the official SDK, custom control-plane URLs use Superserve's production
sandbox data plane.

## Lifecycle

1. `warmup` or `run` without `--id` creates one Superserve sandbox from the
   configured template or snapshot.
2. Crabbox writes Superserve metadata that identifies the sandbox as
   Crabbox-owned, records the API endpoint scope, claim, slug, and repo path,
   then stores a local claim with an `ssbx_...` lease ID.
3. `run` activates the sandbox to obtain a short-lived data-plane token.
4. Unless `--no-sync` is set, Crabbox builds a portable archive and uploads it
   through Superserve's file API.
5. When `sync.delete: true` is configured, Crabbox extracts into a staging
   directory and atomically replaces the configured workdir.
6. The user command executes through Superserve's exec API. Streamed output is
   used when available; otherwise Crabbox falls back to buffered exec output.
7. One-shot sandboxes are deleted after successful `run` unless `--keep` is set.
   Retained sandboxes can be reused with `--id` and later removed with `stop`.

`run --lease-output` records the Superserve lease, reuse/retention state, and
matching `crabbox stop --provider superserve --id ...` cleanup command for
orchestrators that need to inspect or clean up retained sandboxes later.

## Capabilities

- SSH: no.
- Crabbox sync: yes, delegated archive upload and remote extraction.
- Provider sync: no separate provider-native sync step.
- Desktop / browser / code / VNC: no.
- Actions hydration: no.
- Coordinator broker: no, Superserve always runs direct from the CLI.
- Pause/resume: not advertised in v1.

## Live Smoke

Run the guarded hosted lifecycle smoke only when you intend to create a real
short-lived Superserve sandbox:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=superserve \
CRABBOX_SUPERSERVE_API_KEY=ss_live_... \
scripts/live-smoke.sh
```

The top-level smoke dispatches to the provider-specific script:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=superserve \
CRABBOX_SUPERSERVE_API_KEY=ss_live_... \
scripts/live-superserve-smoke.sh
```

The smoke builds `bin/crabbox` unless `CRABBOX_BIN` points at an existing
binary, creates one uniquely named Crabbox-owned sandbox, verifies initial
archive sync and environment forwarding, checks `doctor`, `status`, and
`list`, reuses the same sandbox for a second sync that adds, updates, and
deletes files, proves nonzero exit-code propagation, then stops the sandbox and
confirms that no matching Crabbox-owned inventory remains.

The script exits with `classification=environment_blocked` when live mode,
provider selection, or an API key is missing. Authentication, DNS, TLS, and
connectivity failures are also classified as environment blocked. Quota,
capacity, admission, and rate-limit failures are classified separately. A
successful live proof prints `classification=live_superserve_smoke_passed`.

## Limitations

- Output may be buffered when Superserve's streaming exec endpoint is not
  available.
- Very large repositories may hit upload or request-size limits; normal
  Crabbox large-sync guardrails still apply.
- `--class` and `--type` are rejected for Superserve; choose
  `--superserve-template` or `--superserve-snapshot` instead.
- `--checksum` and SSH/rsync-specific sync options are not supported.
- Delegated run options that require an SSH target or local proof surface are
  rejected, including script injection, fresh PR hydration, full rsync, env
  helpers, output capture artifacts, downloads, artifact globs, emitted proof
  bundles, and `--stop-after`.
- IDs must be a Crabbox slug, an `ssbx_...` lease ID, or a raw Superserve
  sandbox ID that has matching Crabbox ownership metadata.

## Related Docs

- [Provider backends](../provider-backends.md)
- [Provider Reference](README.md)
