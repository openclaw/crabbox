# Blaxel Provider

Read when:

- choosing `provider: blaxel`;
- configuring Blaxel API credentials, workspace, image, region, or sandbox
  lifetime settings;
- changing `internal/providers/blaxel`.

Blaxel is a delegated run provider. Crabbox talks to the Blaxel REST API
directly for sandbox lifecycle, archive sync, process execution, status, and
cleanup. Blaxel owns the Linux sandbox and command transport; Crabbox owns local
config, repo claims, ownership labels, archive manifests and guardrails, friendly
slugs, timing summaries, and normalized list/status rendering.

The API key is sent only in the `Authorization` request header. Forwarded command
environment variables and uploaded files are sent through API request bodies, not
through process argv.

## When To Use

Use Blaxel when a short-lived Linux delegated runner is enough for a test or
automation command and you do not need Crabbox-managed SSH. Use AWS, Hetzner,
Static SSH, or another SSH-lease provider when you need SSH, VNC, code-server,
desktop/browser capability provisioning, Actions runner hydration, native
checkpoints, or a URL bridge.

Phase 1 is direct API execution only. The provider does not use the Crabbox
coordinator and does not expose Blaxel preview ports or hosted agents through
Crabbox.

## Prerequisites

Load a Blaxel API key from a secret manager or prompt it into the environment so
the value does not appear in shell history or process arguments:

```sh
export CRABBOX_BLAXEL_API_KEY="$(
  python3 -c 'import getpass; print(getpass.getpass("Blaxel API key: "))'
)"
```

`BL_API_KEY` is accepted as a vendor-style fallback. `CRABBOX_BLAXEL_WORKSPACE`
or `BL_WORKSPACE` can select a workspace when the account requires one.

Run a non-mutating readiness check before creating a sandbox:

```sh
crabbox doctor --provider blaxel --json
```

`doctor` validates the API URL, confirms an API key is loaded, reports whether a
workspace header will be sent, probes the Blaxel API, and lists inventory without
creating or deleting resources. Its messages include `mutation=false`.

## Commands

```sh
crabbox warmup --provider blaxel --slug test-live
crabbox run --provider blaxel -- go test ./...
crabbox run --provider blaxel --id test-live --shell 'python3 --version && pytest'
crabbox status --provider blaxel --id test-live --wait
crabbox list --provider blaxel --json
crabbox stop --provider blaxel test-live
crabbox cleanup --provider blaxel --dry-run
```

## Auth

Crabbox resolves the API key from `CRABBOX_BLAXEL_API_KEY`, then `BL_API_KEY`.
The key is never read from repository YAML, never persisted in Crabbox config,
and never placed on argv.

The API base URL defaults to `https://api.blaxel.ai`. A trusted local override
can come from `--blaxel-api-url` or `CRABBOX_BLAXEL_API_URL`. Repository YAML
cannot set the API URL: that prevents checked-in config from redirecting an
automatically loaded API key. Overrides must use HTTPS and cannot contain
userinfo, query parameters, or fragments; plain HTTP is accepted only for
`localhost` or loopback development endpoints.

When `CRABBOX_BLAXEL_WORKSPACE`, `BL_WORKSPACE`, `--blaxel-workspace`, or trusted
local config supplies a workspace, Crabbox sends it in the `X-Blaxel-Workspace`
header. Local lease claims are scoped to the normalized API URL and workspace,
so a retained sandbox cannot be reused or deleted from a different endpoint or
workspace by accident.

## Config

```yaml
provider: blaxel
target: linux
blaxel:
  region: ""                    # optional; empty uses Blaxel service policy
  image: ubuntu:24.04           # sandbox image
  memoryMB: 0                   # 0 uses the Blaxel default
  ttl: ""                       # sandbox lifetime; empty uses service default
  idleTTL: ""                   # sandbox idle lifetime; empty uses service default
  workdir: /workspace/crabbox   # absolute path; sync target and exec cwd
  execTimeoutSecs: 600          # command/sync-helper timeout
  forgetMissing: false          # explicit stale-claim cleanup for 404s
```

Trusted local config may also set `apiUrl` and `workspace`. Repository config may
set only non-secret runtime settings such as region, image, memory, lifetimes,
workdir, exec timeout, and `forgetMissing`.

Provider flags:

```text
--blaxel-api-url
--blaxel-workspace
--blaxel-region
--blaxel-image
--blaxel-memory-mb
--blaxel-ttl
--blaxel-idle-ttl
--blaxel-workdir
--blaxel-exec-timeout-secs
--blaxel-forget-missing
```

Environment overrides:

```text
CRABBOX_BLAXEL_API_KEY / BL_API_KEY
CRABBOX_BLAXEL_API_URL
CRABBOX_BLAXEL_WORKSPACE / BL_WORKSPACE
CRABBOX_BLAXEL_REGION / BL_REGION
CRABBOX_BLAXEL_IMAGE
CRABBOX_BLAXEL_MEMORY_MB
CRABBOX_BLAXEL_TTL
CRABBOX_BLAXEL_IDLE_TTL
CRABBOX_BLAXEL_WORKDIR
CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS
CRABBOX_BLAXEL_FORGET_MISSING
```

`--class` and `--type` are rejected for `provider=blaxel`; use
`--blaxel-memory-mb` and `--blaxel-image` instead.

### Environment Forwarding

`--allow-env` / `--env-from-profile` are supported. Values are sent in the
Blaxel process execution request body, not in the local Crabbox command line:

```sh
crabbox run --provider blaxel --allow-env API_TOKEN -- printenv API_TOKEN
```

## Lifecycle

1. `warmup` or `run` without `--id` creates a sandbox with a
   `crabbox-<repo-slug>-<random6>` name, the configured image/region/memory and
   lifetime settings, and initial Crabbox ownership labels.
2. Crabbox updates the sandbox labels with a local lease ID (`blx_<sandbox-id>`),
   a friendly slug, and a random ownership token. The same token is stored in
   the local lease claim.
3. Crabbox waits for the sandbox to reach a ready state.
4. By default, `run` archive-syncs the working tree: Crabbox builds a manifest,
   creates a gzipped tar archive locally, uploads it through the Blaxel file API,
   and extracts it into `blaxel.workdir`. `--no-sync` skips the archive and only
   ensures the workdir exists. `--sync-only` syncs and exits without running a
   command.
5. The command runs through the Blaxel process API with the configured workdir,
   forwarded env, and exec timeout. Crabbox mirrors the remote exit code.
6. New one-shot runs delete the sandbox unless `--keep` or `--keep-on-failure`
   retains it. `stop` deletes a retained sandbox only after the local claim and
   remote ownership labels match.

If create-time cleanup fails after Blaxel has created a sandbox, Crabbox records
a recovery claim. `cleanup --provider blaxel` can later find the matching
ownership label and delete only that sandbox. It does not delete by name prefix
alone.

## Capabilities

- SSH: no.
- Crabbox sync: yes, through archive upload and extraction (`archive-sync`).
- Env forwarding: yes, off-argv in the process request body.
- Cleanup: yes, local-claim and ownership-label based.
- URL bridge / preview ports: no Phase 1 Crabbox bridge.
- Desktop / browser / code: no Crabbox VNC, browser install, or code-server
  surface.
- Actions hydration: no.
- Checkpoints / forks / snapshots: no.
- Coordinator: no; Blaxel always runs direct from the CLI.

## Cleanup Safety

`crabbox list --provider blaxel` starts from local Blaxel claims and checks each
remote sandbox in the scoped API endpoint/workspace. Missing remote sandboxes are
shown as `missing-or-inaccessible` so stale claims stay visible.

`crabbox stop --provider blaxel <slug-or-lease>` deletes only a Crabbox-claimed
sandbox whose ownership labels still match the local claim. If the sandbox is
already missing, the claim is kept by default because a 404 can mean the wrong
workspace, endpoint, or account is configured. Pass `--blaxel-forget-missing`
only after confirming the sandbox is gone in the intended Blaxel workspace.

`crabbox cleanup --provider blaxel --dry-run` is safe to run before mutating
cleanup. Non-dry-run cleanup deletes expired owned claims and recovery matches
only when local claim scope and remote labels prove ownership.

## Live Smoke

The optional live proof script is gated and redaction-aware:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
scripts/live-blaxel-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blaxel scripts/live-blaxel-smoke.sh
```

Without the live gate, the script performs read-only preflight and exits with
`classification=skipped` when live mutation is not enabled. With the gate and
credentials, it creates one short-lived Crabbox-owned sandbox, proves archive
sync, command success, nonzero exit propagation, env forwarding, list/status,
stop, and cleanup dry-run. It writes proof logs under a private temp directory
or `CRABBOX_BLAXEL_LIVE_SMOKE_DIR`; summaries redact API keys, workspace names,
API endpoints, local paths, temp proof paths, IP addresses, and provider URLs.

Classifications:

- `live_blaxel_smoke_passed` — the gated live lifecycle passed and cleanup
  completed.
- `skipped` — live mutation was not explicitly enabled.
- `environment_blocked` — credentials, binary, tools, auth, workspace, DNS,
  TLS, or connectivity were unavailable.
- `quota_blocked` — Blaxel quota, capacity, or rate limits blocked the smoke.
- `validation_failed` — the provider returned success but the proof output did
  not satisfy the expected contract.

Raw proof logs are private diagnostics. Do not paste them into public issues,
pull requests, or chats.

## Gotchas

- `warmup --actions-runner` is rejected because Blaxel is delegated execution,
  not an SSH runner host.
- IDs accepted by `--id`, `status`, and `stop` are Crabbox slugs and
  `blx_<sandbox-id>` lease IDs with local claims. Unclaimed Blaxel sandboxes are
  rejected.
- `--no-sync` creates the workdir but does not apply `sync.delete`.
- With `sync.delete`, Crabbox extracts into a staging directory and replaces the
  configured workdir only after extraction succeeds.
- `--shell` wraps the command as `bash -lc '<joined args>'`. Plain commands that
  contain shell metacharacters or a leading `KEY=VALUE` assignment are
  auto-wrapped the same way.
- `blaxel.workdir` must be absolute. The default is `/workspace/crabbox`.
- `blaxel.execTimeoutSecs` defaults to `600`.
- Large-sync guardrails still apply; pass `--force-sync-large` only when a large
  archive sync is intentional.
- Blaxel API errors are redacted for configured API keys, but public logs should
  still be reviewed before sharing.

Related docs:

- [Provider backends](../provider-backends.md)
- [Provider model](README.md)
- [run](../commands/run.md)
- [cleanup](../commands/cleanup.md)
