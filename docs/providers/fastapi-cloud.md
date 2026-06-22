# FastAPI Cloud Provider

Read when:

- choosing `provider: fastapi-cloud`;
- pointing Crabbox at an existing FastAPI Cloud app;
- changing `internal/providers/fastapicloud`.

[FastAPI Cloud](https://fastapicloud.com) is a deployment platform for FastAPI
apps. Its CLI packages and uploads an app, then FastAPI Cloud builds, deploys,
and verifies that service. Crabbox models FastAPI Cloud as a `service-control`
provider: it can inspect apps and their latest deployment status through the
FastAPI Cloud API, but it cannot create an SSH lease, sync a workspace, or run
an arbitrary command.

## When To Use

Use FastAPI Cloud when the workload is already a FastAPI Cloud app and you want
Crabbox to report its deployment readiness from the same provider matrix as
other backends.

Do not use this provider for generic sandbox execution. FastAPI Cloud deploys a
configured application; it does not expose a synchronous `run(cmd)` or SSH
primitive that Crabbox can use for ad-hoc tests. For command execution, choose a
delegated-run provider such as `e2b`, `modal`, or `docker-sandbox`, or an
SSH-lease provider such as `aws`, `hetzner`, or `ssh`.

## Run Contract

`crabbox run --provider fastapi-cloud ... -- <command>` fails before calling the
FastAPI Cloud API. Accepting the command would imply Crabbox executed it inside
the app, which FastAPI Cloud does not expose.

Deploy with FastAPI Cloud's own deployment path instead:

```sh
fastapi deploy --app-id "$FASTAPI_CLOUD_APP_ID"
```

or through a FastAPI Cloud deploy token in CI. Then use Crabbox for inspection:

```sh
crabbox status --provider fastapi-cloud --id "$FASTAPI_CLOUD_APP_ID"
crabbox list --provider fastapi-cloud --fastapi-cloud-team-id "$FASTAPI_CLOUD_TEAM_ID"
```

`warmup` is rejected because app creation and deployment belong to FastAPI
Cloud. `stop` is rejected because this provider does not expose an app stop or
delete operation.

## Auth

```sh
export FASTAPI_CLOUD_TOKEN=...      # deploy token or compatible bearer token
export FASTAPI_CLOUD_APP_ID=...     # optional default app for status/list
export FASTAPI_CLOUD_TEAM_ID=...    # optional team for list/doctor
```

`CRABBOX_FASTAPI_CLOUD_TOKEN` is also accepted and wins over
`FASTAPI_CLOUD_TOKEN`. The token is read from the environment only; the provider
does not register a CLI flag for it, so it is never passed on the command line.

The provider sends REST requests to `https://api.fastapicloud.com/api/v1` with
`Authorization: Bearer <token>` and `Accept: application/json`.
If a token is scoped to one app, use `FASTAPI_CLOUD_APP_ID`; team-wide `list`
may require a token that can read that team.

## Config

```yaml
provider: fastapi-cloud
target: linux
fastapiCloud:
  apiUrl: https://api.fastapicloud.com/api/v1
  appId: <app-id>
  teamId: <team-id>
```

Provider flags:

```text
--fastapi-cloud-url
--fastapi-cloud-app-id
--fastapi-cloud-team-id
```

Environment overrides:

```text
CRABBOX_FASTAPI_CLOUD_TOKEN    (or FASTAPI_CLOUD_TOKEN)
CRABBOX_FASTAPI_CLOUD_API_URL  (or FASTAPI_CLOUD_API_URL)
CRABBOX_FASTAPI_CLOUD_APP_ID   (or FASTAPI_CLOUD_APP_ID)
CRABBOX_FASTAPI_CLOUD_TEAM_ID  (or FASTAPI_CLOUD_TEAM_ID)
```

The API URL must use `https` unless it targets localhost for tests. The loopback
exception (`localhost`, `127.0.0.1`, `::1`) is matched on the parsed hostname, so
spoofed authorities such as `http://localhost@example.com` are correctly rejected.
Note that an `http://localhost...` URL transmits the bearer token in cleartext to
whatever process listens on that port, so the exception is intended only for local
testing against a trusted listener.

## Commands

```sh
crabbox status --provider fastapi-cloud --id "$FASTAPI_CLOUD_APP_ID"
```

`status` fetches the app and its latest deployment. FastAPI Cloud `success` and
`verifying_skipped` deployments are reported as ready; known build, extraction,
deployment, and verification failures are reported as failed; in-progress
states are surfaced as the upstream status string.

```sh
crabbox list --provider fastapi-cloud --fastapi-cloud-team-id "$FASTAPI_CLOUD_TEAM_ID"
```

`list` returns apps for a configured team ID. If no team ID is configured but an
app ID is configured, `list` returns that single app.

```sh
crabbox doctor --provider fastapi-cloud
```

`doctor` verifies the configured app or team can be read with the current token.

## Capabilities

- Target: Linux only.
- SSH: no.
- Crabbox sync: no. `--no-sync` is required for `run`, which then rejects the
  command.
- Provider sync: FastAPI Cloud deploy flow only, outside this provider.
- Generic `run`: no.
- Warmup: no.
- Stop/delete: no.
- Desktop/browser/code: no.
- Coordinator: no (direct from CLI only).

## Gotchas

- FastAPI Cloud is a deployment platform, not a remote shell or Crabbox
  sandbox.
- `--keep`, `--reclaim`, `--class`, and `--type` are rejected because FastAPI
  Cloud owns app lifecycle and sizing.
- Sync options are rejected: `--no-sync` is required, and `--sync-only`,
  `--checksum`, `--force-sync-large`, and `--full-resync` all error out.
- `--shell` is rejected (no interactive session) and `--env-summary` is rejected
  (this provider cannot forward per-run environment variables).
- App listing needs a team ID; status can use either `--id` or a configured app
  ID.
- API compatibility is based on the app and deployment endpoints used by the
  public FastAPI Cloud CLI. If the upstream API changes, this provider should be
  updated with matching tests.

Related docs:

- [Provider backends](../provider-backends.md)
