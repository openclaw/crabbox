# Cloudflare Sandbox Provider

Read when:

- choosing `provider: cloudflare-sandbox`;
- configuring a Cloudflare Sandbox bridge URL, token, workdir, command timeout,
  or stale-claim cleanup behavior;
- changing `internal/providers/cloudflaresandbox`.

Cloudflare Sandbox is a delegated run provider. Crabbox talks to a configured
Cloudflare Sandbox bridge, asks that bridge to create a Cloudflare-managed Linux
sandbox, uploads a portable archive of the checkout, and runs commands through
the bridge command API. Cloudflare owns the sandbox runtime, file upload,
command transport, and sandbox deletion. Crabbox owns local config, repo claims,
slugs, ownership metadata, archive sync guardrails, command timing summaries,
and normalized `list` / `status` rendering.

The provider does not expose a Crabbox-managed SSH lease. Choose AWS, Hetzner,
Static SSH, Local Container, or another SSH-lease provider when you need
`crabbox ssh`, VNC, browser/code capability flags, Actions runner hydration,
raw rsync behavior, Tailscale, or provider-native SSH access.

Use the separate [Cloudflare provider](cloudflare.md) for the Crabbox
Cloudflare Containers runner. Use the separate
[Cloudflare Dynamic Workers provider](cloudflare-dynamic-workers.md) for Worker
module execution in the Cloudflare Workers runtime. `cloudflare-sandbox` is the
Cloudflare Sandbox bridge adapter for Linux command execution.

## Setup

Deploy or run a Cloudflare Sandbox bridge that implements the HTTP endpoints
Crabbox calls:

- `GET /health`
- `GET /v1/openapi.json`
- `POST /v1/sandboxes`
- `GET /v1/sandboxes`
- `GET /v1/sandboxes/{id}`
- `DELETE /v1/sandboxes/{id}`
- `POST /v1/sandboxes/{id}/exec`
- file upload support for delegated archive sync

Point Crabbox at that bridge through trusted config or environment. Keep bridge
tokens out of repo-local config and command-line arguments.

```sh
export CRABBOX_CLOUDFLARE_SANDBOX_URL=https://sandbox-bridge.example.workers.dev
export CRABBOX_CLOUDFLARE_SANDBOX_TOKEN=...
crabbox doctor --provider cloudflare-sandbox --json
```

`doctor` is read-only. It validates the bridge URL configuration, calls bridge
health, and requests the bridge OpenAPI document without creating, changing, or
deleting sandboxes. With no URL configured it fails clearly:

```text
cloudflare-sandbox requires cloudflareSandbox.url or CRABBOX_CLOUDFLARE_SANDBOX_URL
```

## Auth

Crabbox sends `cloudflareSandbox.token` or
`CRABBOX_CLOUDFLARE_SANDBOX_TOKEN` as the bridge bearer token when one is
configured. There is intentionally no `--cloudflare-sandbox-token` flag because
command-line arguments can be captured in shell history and process listings.

Provider authentication variables are stripped from forwarded command
environments. If a run uses `--allow-env` for Cloudflare provider credentials,
Crabbox warns and does not send those values into the sandbox command
environment. Stripped names include:

```text
CRABBOX_CLOUDFLARE_SANDBOX_TOKEN
CLOUDFLARE_API_TOKEN
CLOUDFLARE_ACCOUNT_ID
CF_API_TOKEN
CF_ACCOUNT_ID
```

## Commands

```sh
crabbox doctor --provider cloudflare-sandbox --json
crabbox warmup --provider cloudflare-sandbox --slug cfsbx-smoke
crabbox run --provider cloudflare-sandbox -- go test ./...
crabbox run --provider cloudflare-sandbox --id cfsbx-smoke --shell 'pnpm install && pnpm test'
crabbox run --provider cloudflare-sandbox --id cfsbx-smoke --sync-only
crabbox status --provider cloudflare-sandbox --id cfsbx-smoke --wait --json
crabbox list --provider cloudflare-sandbox --json
crabbox stop --provider cloudflare-sandbox cfsbx-smoke
crabbox cleanup --provider cloudflare-sandbox --dry-run
```

`warmup` keeps the sandbox until explicit `stop`, even if `--keep` is omitted.
A `run` without `--id` creates a sandbox and deletes it after the command unless
`--keep` or `--keep-on-failure` retains it.

## Config

```yaml
provider: cloudflare-sandbox
target: linux
cloudflareSandbox:
  url: https://sandbox-bridge.example.workers.dev
  workdir: /workspace/crabbox
  execTimeoutSecs: 600
  forgetMissing: false
```

Trusted user config may also use `bridgeUrl`; `url` is the shorter alias.
Repository-local config may set `workdir`, `execTimeoutSecs`, and
`forgetMissing`, but bridge connection values belong in trusted user config or
environment.

| Setting | Config key | Environment variable | Flag |
| --- | --- | --- | --- |
| Bridge URL | `url` / `bridgeUrl` | `CRABBOX_CLOUDFLARE_SANDBOX_URL` | `--cloudflare-sandbox-url` |
| Token | `token` | `CRABBOX_CLOUDFLARE_SANDBOX_TOKEN` | _(none, by design)_ |
| Workdir | `workdir` | `CRABBOX_CLOUDFLARE_SANDBOX_WORKDIR` | `--cloudflare-sandbox-workdir` |
| Command timeout | `execTimeoutSecs` | `CRABBOX_CLOUDFLARE_SANDBOX_EXEC_TIMEOUT_SECS` | `--cloudflare-sandbox-exec-timeout-secs` |
| Forget missing | `forgetMissing` | `CRABBOX_CLOUDFLARE_SANDBOX_FORGET_MISSING` | `--cloudflare-sandbox-forget-missing` |

Defaults: workdir `/workspace/crabbox`, command timeout `600` seconds, and
forget-missing disabled.

The bridge URL must be HTTPS unless it targets a loopback host for local fake
bridge tests. It must not contain userinfo, query parameters, or fragments.
`workdir` must be an absolute dedicated directory and cannot be broad system
paths such as `/`, `/tmp`, `/usr`, `/var`, `/home`, or `/workspace`.
`execTimeoutSecs` must be non-negative; `0` delegates timeout behavior to the
bridge.

`--class` and `--type` are rejected for this provider because Cloudflare
Sandbox sizing is not exposed through Crabbox's v1 bridge contract.

## Lifecycle

1. `warmup` or `run` without `--id` asks the bridge to create a sandbox with
   Crabbox ownership metadata and the configured workdir. The local lease is
   stored as `cfsbx_<sandbox-id>` with a friendly slug and a repo claim.
2. Unless `--no-sync` is set, Crabbox builds a portable gzipped archive from the
   working tree, uploads it through the bridge, and extracts it in the sandbox
   through delegated shell commands. With `sync.delete: true`, Crabbox stages
   extraction before replacing the configured workdir.
3. Commands run through the bridge exec API with `workingDir` set to the
   configured workdir and forwarded non-auth environment values sent in the
   request body. The client supports both server-sent-event exec output and a
   buffered JSON exec result.
4. `--sync-only` performs archive sync, prints the synced workdir, writes timing
   JSON when requested, and then follows the same one-shot cleanup rules as
   normal `run`.
5. One-shot sandboxes are deleted after successful `run` unless `--keep` is set.
   `--keep-on-failure` retains a newly created sandbox after sync, workspace, or
   command failures and prints reuse/stop guidance.
6. `status` and `list` use local claims plus bridge sandbox metadata. Crabbox
   verifies that the remote sandbox is still owned by the current Crabbox claim
   before rendering it.
7. `stop` deletes the Cloudflare Sandbox and removes the local claim. If the
   remote sandbox is already missing, Crabbox preserves the claim unless
   `--cloudflare-sandbox-forget-missing` or
   `cloudflareSandbox.forgetMissing: true` is set.
8. `cleanup` sweeps only local `cfsbx_...` claims in the active provider scope.
   It deletes idle-expired Crabbox-owned sandboxes, skips still-active claims,
   and treats missing-or-inaccessible sandboxes conservatively unless
   forget-missing is explicit.

## Capabilities

- SSH: no.
- Crabbox sync: yes, via portable archive upload and in-sandbox extraction.
- Env forwarding: yes, off-argv in the bridge request body; provider auth
  variables are stripped.
- Exec output: server-sent events when the bridge returns `text/event-stream`,
  or buffered JSON when the bridge returns a complete exec result.
- Desktop / browser / code / VNC: no.
- Actions hydration: no.
- Tailscale: no.
- URL sessions, storage mounts, checkpoints, ports, pause/resume: not exposed by
  Crabbox for this provider in v1.
- Coordinator broker: no, Cloudflare Sandbox runs direct from the CLI.

## Limitations

The current deterministic proof surface uses fake bridge tests. Do not treat
this provider page as proof of live Cloudflare account writes, quota behavior,
or bridge deployment compatibility. Run `doctor` against your bridge before
`run`, and use a disposable sandbox for any future live smoke.

Cloudflare Sandbox platform features such as URL bridges, persistent sessions,
storage mounts, and checkpoints are future candidates. They should be documented
here only after Crabbox implements and verifies those surfaces.
