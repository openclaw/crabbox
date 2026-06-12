# Railway Provider

Read when:

- choosing `provider: railway`;
- pointing Crabbox at an existing Railway service;
- changing `internal/providers/railway`.

[Railway](https://railway.com) is a deployment platform whose primitives are
projects, environments, services, and deployments. Its public API is a GraphQL
endpoint at `https://backboard.railway.com/graphql/v2`, authenticated with an
account-scoped token (`Authorization: Bearer <token>`) created at
`/account/tokens`. Railway is a `service-control` provider: it owns service and
deployment lifecycle, and Crabbox drives service inspection and stopping over
GraphQL. There is no SSH lease, no workspace sync, no arbitrary command
execution, and no coordinator path.

## When To Use

Use Railway when the workload is already a Railway service and you need Crabbox
to inspect or stop that service from the same provider matrix as other backends.
Railway has no synchronous exec primitive (no shell, no `service.run(cmd)`
field), so Crabbox rejects generic `run` requests instead of pretending the
provided command ran. For ad-hoc command execution, pick a provider that owns
command transport, such as `e2b`, `modal`, or `exe-dev`, or an SSH-lease provider
such as `aws`, `hetzner`, or `ssh`.

## Run Contract

`crabbox run --provider railway ... -- <command>` fails before calling the
Railway API. Railway runs whatever start command the service is configured with
(via `railway.toml`, the dashboard, or `serviceInstanceUpdate`), so accepting a
generic Crabbox command would create false-positive test results.

`status` returns the latest deployment status and readiness. `stop` calls
`deploymentStop` on the latest deployment for the service. `list` enumerates one
row per service across every project the token can see. `warmup` is rejected so
Crabbox never silently provisions billable Railway resources — create the
service yourself in the dashboard or via `serviceCreate` first.

## Commands

```sh
crabbox run --provider railway --no-sync --id "$RAILWAY_SERVICE_ID" -- false
# exits before any Railway API call; Railway cannot execute arbitrary commands
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

`warmup` is rejected; service creation must happen out-of-band. `run` is rejected
because there is no Railway exec API. `status` and `stop` require `--id`,
`--railway-project`, and `--railway-environment`. `list` needs only the API
token.

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
- Generic `run`: no. Railway has no arbitrary command execution API.
- URL bridge: yes (delegated url-bridge feature).
- Desktop/browser/code: no.
- Actions hydration: no.
- Coordinator: no (direct from CLI only).

## Gotchas

- Railway has no synchronous exec primitive. `run` rejects commands before
  touching the API.
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
