# Railway Provider

Read when:

- choosing `provider: railway`;
- pointing Crabbox at an existing Railway service;
- changing `internal/providers/railway`.

[Railway](https://railway.com) is a deployment platform whose primitives are
projects, environments, services, and deployments. Railway's public API is
GraphQL at `https://backboard.railway.com/graphql/v2` and is authenticated with
`Authorization: Bearer $RAILWAY_API_TOKEN`. Account-scoped tokens from
`/account/tokens` operate across every project the account can see.

## Mapping And Semantic Differences

Railway has no synchronous `POST /exec`-style endpoint. There is no shell, no
SSH mutation, no `service.run(cmd)` GraphQL field. Crabbox therefore maps
`crabbox run` onto the closest available lifecycle:

1. The caller must point at a pre-existing Railway service with
   `--id <serviceId>`, plus `--railway-project <projectId>` and
   `--railway-environment <environmentId>` (or the matching env vars / YAML).
2. The provider issues the GraphQL mutation
   `environmentTriggersDeploy(input: { projectId, environmentId, serviceId })`
   to kick a redeploy of that service.
3. The provider polls `deployment(id: $deploymentId)` for the deployment id
   returned by the mutation, while fetching newly available
   `buildLogs(deploymentId, limit)` and `deploymentLogs(deploymentId, limit)`
   messages to stdout.
4. The deployment `status` is mapped to a process exit code: `SUCCESS` and
   `SLEEPING` become `0`; terminal failure/removal states (`FAILED`, `CRASHED`,
   `REMOVED`, `SKIPPED`) become `1`. Non-terminal states are polled until they
   finish or the 30 minute timeout expires.

This means **the user's command argument is informational only**. Railway runs
whatever start command the service is configured with (in `railway.toml`, the
dashboard, or the `serviceInstanceUpdate` GraphQL mutation). The crabbox
command argument is echoed to stderr and shows up in run telemetry, but it is
not what the Railway container actually executes. If you need
"run-this-exact-command-and-get-output", use the `exe-dev`, `e2b`, or `modal`
provider instead — they fit that contract.

`Stop` calls `deploymentStop(id: $deploymentId)` against the latest deployment
for the service. `Status` returns the latest deployment status and readiness.
`List` flattens `projects { edges { node { services { edges { node { ... } } } } } }`
into one row per service. `Warmup` is rejected so Crabbox never silently
provisions billable Railway resources — create the service yourself in the
Railway dashboard or via `serviceCreate`.

## When To Use

Use Railway when the workload is already a Railway service and the redeploy
loop is what you want from a Crabbox run (for example, "rebuild and stream
new build/runtime log messages after a config change"). Pick a different
provider for ad-hoc command execution.

## Commands

```sh
crabbox run --provider railway --no-sync \
  --id $RAILWAY_SERVICE_ID \
  --railway-project $RAILWAY_PROJECT_ID \
  --railway-environment $RAILWAY_ENVIRONMENT_ID \
  -- pnpm test
```

```sh
crabbox status --provider railway --id $RAILWAY_SERVICE_ID
crabbox stop   --provider railway $RAILWAY_SERVICE_ID
crabbox list   --provider railway
```

`warmup` is rejected; service creation must happen out-of-band.

## Auth

```sh
export RAILWAY_API_TOKEN=...   # required, account or project token from /account/tokens
```

`CRABBOX_RAILWAY_API_TOKEN` is also accepted and wins over `RAILWAY_API_TOKEN`,
matching the precedence used by other delegated providers
(`CRABBOX_E2B_API_KEY` over `E2B_API_KEY`, `CRABBOX_EXE_API_KEY` over
`EXE_API_KEY`). The token is read from the environment only; the provider does
not register a CLI flag for it. Do not pass the token on the command line.

The canonical Railway request shape is:

```sh
curl -X POST https://backboard.railway.com/graphql/v2 \
  -H "Authorization: Bearer $RAILWAY_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query":"query { projects { edges { node { id name } } } }"}'
```

Crabbox sends the same `Authorization: Bearer $RAILWAY_API_TOKEN` header and a
JSON `{query, variables}` body to the same endpoint.

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

## Capabilities

- SSH: no.
- Crabbox sync: no. `--no-sync` is required.
- Provider sync: no.
- Desktop/browser/code: no.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- Railway has no synchronous exec primitive; the user command is informational.
  The service's configured start command is what runs.
- `--keep`, `--reclaim`, `--class`, `--type` are rejected — Railway owns
  service lifecycle.
- `warmup` is rejected to avoid silently creating billable Railway resources.
- The provider requires `--id`, `--railway-project`, and `--railway-environment`
  for `run`, `status`, and `stop`. `list` only needs the API token.
- Non-2xx HTTP responses and GraphQL `errors[]` envelopes are surfaced as
  `railwayAPIError`, mirroring the error shape used by other delegated
  providers.

Related docs:

- [Provider backends](../provider-backends.md)
