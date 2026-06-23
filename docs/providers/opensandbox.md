# OpenSandbox Provider

Read when:

- choosing `provider: opensandbox`;
- configuring OpenSandbox image, sizing, workdir, API URL, or proxy behavior;
- changing `internal/providers/opensandbox`.

OpenSandbox is a delegated run provider. Crabbox uses the OpenSandbox Go SDK
behind a provider-local client for sandbox lifecycle, file upload, and execd
command execution. OpenSandbox owns the container runtime and command transport;
Crabbox owns local config, repo claims, sync manifests and guardrails, slugs,
timing summaries, and normalized list/status rendering.

The API key travels only in request headers. Forwarded command environment and
archive content travel in request bodies, so Crabbox does not place secrets on
process command lines.

## When To Use

Use OpenSandbox when the run should execute in an OpenSandbox-managed Linux
container and an SSH lease is not required. Use AWS, Hetzner, Static SSH,
Daytona, or another SSH-lease provider when the workflow needs `crabbox ssh`,
VNC, code-server, Actions runner hydration, or provider-native SSH access.

## Prerequisites

An OpenSandbox API endpoint and API key. Prefer loading the key from a secret
manager or prompting into the environment so it never appears in shell history
or process arguments:

```sh
export CRABBOX_OPENSANDBOX_API_KEY="$(
  python3 -c 'import getpass; print(getpass.getpass("OpenSandbox API key: "))'
)"
```

`OPEN_SANDBOX_API_KEY` is accepted as an environment fallback.

Set the API URL explicitly from a trusted local environment or CLI flag. For a
local-development OpenSandbox server, use the SDK endpoint directly:

```sh
export CRABBOX_OPENSANDBOX_API_URL=http://localhost:8080
```

For hosted or shared deployments, set the HTTPS origin for that deployment.

## Commands

```sh
crabbox warmup --provider opensandbox
crabbox run --provider opensandbox -- go test ./...
crabbox run --provider opensandbox --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider opensandbox --id blue-lobster
crabbox stop --provider opensandbox blue-lobster
crabbox cleanup --provider opensandbox
```

## Auth

Crabbox resolves the API key from, in order,
`CRABBOX_OPENSANDBOX_API_KEY`, then `OPEN_SANDBOX_API_KEY`. It is sent through
the OpenSandbox SDK lifecycle and execd clients, never persisted in Crabbox
config and never placed on argv. If no key is resolvable, operations fail with
a clear error before any sandbox is created.

The API base URL can come from `--opensandbox-api-url`,
`CRABBOX_OPENSANDBOX_API_URL`, or `OPEN_SANDBOX_API_URL`. Repository YAML cannot
set the API URL. That prevents a checked-in config from redirecting an
automatically loaded API key. Overrides must be absolute HTTP(S) URLs; plain
HTTP is accepted only for `localhost` or loopback IPs during local development.
Userinfo, query parameters, and fragments are rejected.
Resolved execd endpoints follow the same transport rule: public endpoints must
use HTTPS, while plain HTTP is accepted only for loopback development servers.

Local lease claims are scoped to the normalized API URL and a random ownership
marker stored both locally and in OpenSandbox sandbox metadata. Before reusing
or deleting a retained sandbox, Crabbox verifies that both markers match. This
prevents a local claim from being applied to a sandbox at another endpoint or
account.

## Config

```yaml
provider: opensandbox
target: linux
openSandbox:
  image: ubuntu:24.04              # container image used for new sandboxes
  workdir: /workspace/crabbox      # sync target and exec cwd
  cpu: "1"                         # resource limit string; empty = service default
  memory: 2Gi                      # resource limit string; empty = service default
  timeoutSecs: 0                   # 0 = Crabbox TTL; positive values add a provider cap
  execTimeoutSecs: 600             # command/sync-helper timeout
  platformOS: linux                # set with platformArch; both empty = service default
  platformArch: amd64              # set with platformOS; both empty = service default
  secureAccess: false              # request secured endpoints
  useServerProxy: false            # route execd through the OpenSandbox server
```

Provider flags:

```text
--opensandbox-api-url
--opensandbox-image
--opensandbox-workdir
--opensandbox-cpu
--opensandbox-memory
--opensandbox-timeout-secs
--opensandbox-exec-timeout-secs
--opensandbox-platform-os
--opensandbox-platform-arch
--opensandbox-secure-access
--opensandbox-use-server-proxy
--opensandbox-forget-missing
```

Configuration flags have matching `CRABBOX_OPENSANDBOX_*` environment
overrides, for example `CRABBOX_OPENSANDBOX_IMAGE`,
`CRABBOX_OPENSANDBOX_WORKDIR`, and
`CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS`. The API URL also reads
`OPEN_SANDBOX_API_URL`. `--opensandbox-forget-missing` is deliberately
CLI-only so stale-claim removal always requires explicit intent.

### Environment Forwarding

`--allow-env` and `--env-from-profile` are supported. Forwarded values are sent
in the OpenSandbox execd request body (`envs`), never on the command line:

```sh
crabbox run --provider opensandbox --allow-env API_TOKEN -- printenv API_TOKEN
```

## Lifecycle

1. `warmup` or `run` without `--id` creates a sandbox with the configured image,
   resource limits, timeout, platform, secure-access setting, and Crabbox
   metadata (`crabbox=true`, `crabbox.name=...`, `crabbox.claim=...`).
2. The local lease is stored as `osbx_<sandbox-id>` with a friendly slug and a
   repo claim. The sandbox expiration is the earliest configured provider
   timeout or Crabbox TTL. Crabbox never renews that absolute deadline; idle
   timeout remains the sliding local inactivity policy.
3. By default `run` archive-syncs the working tree: a `git ls-files`-driven
   manifest is packed into a gzipped tar locally, uploaded through the
   OpenSandbox file API, and extracted into the configured workdir.
   `--no-sync` skips the archive step and only ensures the workdir exists.
   `--sync-only` syncs and stops without running a command.
4. The command runs through OpenSandbox execd with `cwd` set to the workdir and
   `envs` carrying forwarded environment values. The remote exit code is
   mirrored by Crabbox.
5. On release the sandbox is deleted unless `--keep` was set.
   `--keep-on-failure` retains a newly created sandbox after a sync, workspace
   setup, or command failure and prints rerun/stop guidance. Best-effort
   cleanup calls are bounded; failed cleanup reports the sandbox ID for manual
   provider-side cleanup.
6. `cleanup` removes retained sandboxes after their local sliding idle timeout.
   Cleanup rechecks ownership metadata and serializes with lease reuse.
   Missing-or-inaccessible claims are preserved unless `forgetMissing` is
   explicitly enabled.

## Capabilities

- SSH: not driven by Crabbox.
- Crabbox sync: yes, via gzipped archive upload and in-sandbox extraction.
- Env forwarding: yes, off-argv in the exec request body.
- Run session: yes. `--lease-output` records the OpenSandbox lease, whether it
  was reused, whether it was retained, and the matching `crabbox stop`
  cleanup command.
- Provider sync: no separate provider-side copy command is required.
- URL bridge: no Crabbox bridge integration in v1.
- Desktop / browser / code: no Crabbox VNC or code-server surface.
- Actions hydration: no.
- Coordinator: no. OpenSandbox runs directly through its API and never through
  the Crabbox broker.

## Gotchas

- `opensandbox` has no aliases in v1. `osb` is intentionally reserved so it
  cannot collide with the OpenSandbox CLI or a future maintainer choice.
- `--checksum` is rejected by core delegated-run guardrails because OpenSandbox
  does not expose Crabbox rsync checksum semantics. `--sync-only` and
  `--force-sync-large` are supported.
- `--no-sync` only ensures the workdir exists. It never applies `sync.delete`,
  so reusing a retained sandbox does not erase its workspace.
- With `sync.delete`, Crabbox uploads and extracts into a sibling staging
  directory, then replaces the workdir only after extraction succeeds.
- `crabbox list` starts from local OpenSandbox claims and fetches remote status
  for each retained sandbox. A missing sandbox remains visible as
  `missing-or-inaccessible` until explicitly forgotten.
- For an existing lease, a 404 is account-ambiguous and keeps the claim by
  default. Pass `--opensandbox-forget-missing` to `stop` only after confirming
  the sandbox is gone in the intended account.
- `warmup --actions-runner` is rejected because OpenSandbox is a delegated
  execution provider, not an SSH runner host.
- Large-sync guardrails still apply; pass `--force-sync-large` when a large
  archive sync is intentional.
- `openSandbox.workdir` must be an absolute path and cannot be a broad system
  directory such as `/`, `/tmp`, or `/workspace`.

## Live Smoke

The opt-in live smoke script checks prerequisites, builds the local CLI, creates
a short-lived sandbox, proves archive sync and off-argv environment forwarding,
reuses the retained sandbox with staged `sync.delete` replacement, checks
list/status, and stops it:

```sh
CRABBOX_OPENSANDBOX_API_KEY=... \
CRABBOX_OPENSANDBOX_API_URL=... \
scripts/live-opensandbox-smoke.sh
```

It prints exactly one classification:

- `live_opensandbox_smoke_passed`
- `environment_blocked`
- `quota_blocked`
- `diagnostic_only`

Related docs:

- [Provider backends](../provider-backends.md)
