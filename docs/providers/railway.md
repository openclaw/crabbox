# Railway Provider

Read when:

- choosing `provider: railway`;
- pointing Crabbox at an existing Railway service;
- changing `internal/providers/railway`.

[Railway](https://railway.com) is a deployment platform whose primitives are
projects, environments, services, and deployments. Its public API is a GraphQL
endpoint at `https://backboard.railway.com/graphql/v2`, authenticated with an
account-scoped token (`Authorization: Bearer <token>`) created at
`/account/tokens`. Railway is a delegated-run provider: it owns service and
deployment lifecycle, and Crabbox drives it over GraphQL. There is no SSH lease,
no workspace sync, and no coordinator path.

## When To Use

Use Railway when the workload is already a Railway service and a redeploy loop
is what you want from a Crabbox run — for example, "rebuild and stream the new
build/runtime log messages after a config change." Railway has no synchronous
exec primitive (no shell, no `service.run(cmd)` field), so it cannot run an
arbitrary command and return its output. For ad-hoc command execution, pick a
provider that owns command transport, such as `e2b`, `modal`, or `exe-dev`, or
an SSH-lease provider such as `aws`, `hetzner`, or `ssh`.

## How `run` Maps Onto Railway

Railway has no `POST /exec` equivalent, so `crabbox run` maps onto the closest
deployment lifecycle:

1. Point at a pre-existing Railway service with `--id <serviceId>`, plus
   `--railway-project <projectId>` and `--railway-environment <environmentId>`
   (or the matching config keys / env vars).
2. The provider reads the service's latest deployment, then issues the
   `deploymentRedeploy` GraphQL mutation to redeploy the same build/image.
3. It polls the new deployment (`deployment(id:)`) with exponential backoff,
   streaming newly available `buildLogs` and `deploymentLogs` messages to
   stdout, until the deployment reaches a terminal state or the 30-minute
   timeout expires.
4. The terminal `status` maps to a process exit code: `SUCCESS` and `SLEEPING`
   become `0`; the failure/removal states (`FAILED`, `CRASHED`, `REMOVED`,
   `SKIPPED`) become `1`.

Because Railway runs whatever start command the service is configured with (via
`railway.toml`, the dashboard, or `serviceInstanceUpdate`), **the command you
pass to `crabbox run` is informational only.** It is echoed to stderr and shows
up in run telemetry, but the Railway container executes its own configured start
command, not your argument.

`status` returns the latest deployment status and readiness. `stop` calls
`deploymentStop` on the latest deployment for the service. `list` enumerates one
row per service across every project the token can see. `warmup` is rejected so
Crabbox never silently provisions billable Railway resources — create the
service yourself in the dashboard or via `serviceCreate` first.

## Commands

```sh
crabbox run --provider railway --no-sync \
  --id "$RAILWAY_SERVICE_ID" \
  --railway-project "$RAILWAY_PROJECT_ID" \
  --railway-environment "$RAILWAY_ENVIRONMENT_ID" \
  -- pnpm test
```

```sh
crabbox status --provider railway --id "$RAILWAY_SERVICE_ID" \
  --railway-project "$RAILWAY_PROJECT_ID" \
  --railway-environment "$RAILWAY_ENVIRONMENT_ID"
crabbox stop   --provider railway --id "$RAILWAY_SERVICE_ID" \
  --railway-project "$RAILWAY_PROJECT_ID" \
  --railway-environment "$RAILWAY_ENVIRONMENT_ID"
crabbox list   --provider railway
```

`warmup` is rejected; service creation must happen out-of-band. `run`, `status`,
and `stop` all require `--id`, `--railway-project`, and `--railway-environment`.
`list` needs only the API token.

## Auth

```sh
export RAILWAY_API_TOKEN=...   # required, account token from /account/tokens
```

`CRABBOX_RAILWAY_API_TOKEN` is also accepted and wins over `RAILWAY_API_TOKEN`,
matching the precedence used by other delegated providers. The token is read
from the environment or config only; the provider does not register a CLI flag
for it, so it is never passed on the command line. Crabbox sends the same
`Authorization: Bearer <token>` header and a JSON `{query, variables}` body that
a raw GraphQL request would:

```sh
curl -X POST https://backboard.railway.com/graphql/v2 \
  -H "Authorization: Bearer $RAILWAY_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query":"query { projects { edges { node { id name } } } }"}'
```

## Config

```yaml
provider: railway
target: linux
railway:
  apiUrl: https://backboard.railway.com/graphql/v2
  projectId: <project-uuid>
  environmentId: <environment-uuid>
```

Provider flags:

```text
--railway-url
--railway-project
--railway-environment
```

Environment overrides:

```text
CRABBOX_RAILWAY_API_TOKEN      (or RAILWAY_API_TOKEN)
CRABBOX_RAILWAY_API_URL        (or RAILWAY_API_URL)
CRABBOX_RAILWAY_PROJECT_ID     (or RAILWAY_PROJECT_ID)
CRABBOX_RAILWAY_ENVIRONMENT_ID (or RAILWAY_ENVIRONMENT_ID)
```

The `--railway-url` flag and `RAILWAY_API_URL` env var override `railway.apiUrl`.
A non-`https` URL is rejected unless it targets `localhost`.

## Capabilities

- Target: Linux only.
- SSH: no.
- Crabbox sync: no. `--no-sync` is required.
- Provider sync: no.
- URL bridge: yes (delegated url-bridge feature).
- Desktop/browser/code: no.
- Actions hydration: no.
- Coordinator: no (direct from CLI only).

## Gotchas

- Railway has no synchronous exec primitive. The command you pass is
  informational; the service's configured start command is what runs.
- `--keep`, `--reclaim`, `--class`, and `--type` are rejected — Railway owns
  service lifecycle and resource sizing.
- Sync options are rejected: `--no-sync` is required, and `--sync-only`,
  `--checksum`, `--force-sync-large`, and `--full-resync` all error out.
- `--shell` and per-run environment forwarding are rejected because Railway runs
  the service's own start command.
- `warmup` is rejected to avoid silently creating billable Railway resources.
- Non-2xx HTTP responses and GraphQL `errors[]` envelopes surface as a single
  Railway API error, mirroring the error shape used by other delegated
  providers.

Related docs:

- [Provider backends](../provider-backends.md)
