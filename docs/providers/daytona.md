# Daytona Provider

Read this when you are:

- choosing `provider: daytona`;
- configuring Daytona API auth, snapshots, or SSH access;
- changing `internal/providers/daytona`.

Daytona is an SSH-lease provider with two data planes. Direct `run` and `warmup`
create the sandbox from a Daytona snapshot and drive sync and command execution
through the Daytona SDK/toolbox APIs. With a coordinator configured, the Worker
creates the sandbox and mints an expiring SSH access token; the CLI then uses
normal SSH and rsync without receiving the Daytona API key.

## When to use

Use Daytona when the box image should come from a Daytona snapshot and command
execution should stay inside Daytona's toolbox APIs. Reach for AWS, Hetzner, or
the static `ssh` provider instead when you need a normal long-lived SSH lease for
Actions hydration, desktop/VNC, or `code` workflows.

Use brokered Daytona when clients should share centrally managed Daytona
capacity without receiving the API key. Brokered Daytona supports normal
SSH/sync/run, but not workspaces, ready pools, Actions hydration,
desktop/browser/code, or Tailscale.

## Commands

```sh
crabbox warmup --provider daytona --daytona-snapshot crabbox-ready
crabbox run --provider daytona --daytona-snapshot crabbox-ready -- pnpm test
crabbox run --provider daytona --id swift-crab -- pnpm test:changed
crabbox ssh --provider daytona --id swift-crab
crabbox stop --provider daytona swift-crab
```

## Live Smoke

The shared live-smoke harness can validate Daytona without a coordinator:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=daytona CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

The smoke requires a snapshot through `CRABBOX_DAYTONA_SNAPSHOT`,
`DAYTONA_SNAPSHOT`, or `daytona.snapshot`. It exits before any Daytona `run`,
`list`, `warmup`, or `stop` command when the snapshot is missing, so
credentialless machines can verify the guard without mutating provider state.
With a snapshot configured, the harness runs one delegated Daytona command and
then lists normalized Daytona inventory.

For a coordinator deployment, store `DAYTONA_CRABBOX_KEY` as a Worker secret.
The optional `CRABBOX_DAYTONA_SNAPSHOT` Worker variable selects a shared
snapshot; when it is empty, the Daytona account default is used.

## Auth

Crabbox reads the active Daytona CLI profile when no Daytona auth values are set
in the environment or config:

```sh
daytona login --api-key ...
```

You can also supply explicit API-key auth:

```sh
export DAYTONA_API_KEY=...
```

or JWT auth:

```sh
export DAYTONA_JWT_TOKEN=...
export DAYTONA_ORGANIZATION_ID=...
```

`DAYTONA_ORGANIZATION_ID` is required with JWT auth. Explicit environment values
(or Crabbox config values) override the Daytona CLI profile.

Each auth variable also has a `CRABBOX_`-prefixed form that takes precedence over
the unprefixed one: `CRABBOX_DAYTONA_API_KEY`, `CRABBOX_DAYTONA_JWT_TOKEN`,
`CRABBOX_DAYTONA_ORGANIZATION_ID`, and `CRABBOX_DAYTONA_API_URL`.

## Config

```yaml
provider: daytona
target: linux
daytona:
  apiUrl: https://app.daytona.io/api
  snapshot: crabbox-ready
  target: ""
  user: daytona
  workRoot: /home/daytona/crabbox
  sshGatewayHost: ssh.app.daytona.io
  sshAccessMinutes: 30
```

The values above are the built-in defaults except for `snapshot` and `target`,
which are empty by default.

Provider flags:

```text
--daytona-api-url
--daytona-snapshot
--daytona-target
--daytona-user
--daytona-work-root
--daytona-ssh-gateway-host
--daytona-ssh-access-minutes
```

The non-auth settings can also be set through environment variables:
`CRABBOX_DAYTONA_SNAPSHOT`, `CRABBOX_DAYTONA_TARGET`, `CRABBOX_DAYTONA_USER`,
`CRABBOX_DAYTONA_WORK_ROOT`, `CRABBOX_DAYTONA_SSH_GATEWAY_HOST`, and
`CRABBOX_DAYTONA_SSH_ACCESS_MINUTES`.

## Lifecycle

1. Create or resolve a Daytona sandbox from `daytona.snapshot`.
2. Store Crabbox labels and a local repo claim for the lease.
3. For `run`, build the Crabbox sync manifest, stream a gzipped tar archive to
   the Daytona toolbox upload endpoint, extract it in the sandbox, and execute
   the command through the Daytona process APIs.
4. For `ssh`, request short-lived SSH access (TTL `daytona.sshAccessMinutes`),
   parse Daytona's `sshCommand`, and redact the token in normal output.
5. Delete the sandbox on release unless the lease is kept.

## Capabilities

- Provider kind: SSH-lease (Linux only).
- SSH: yes, via a short-lived Daytona SSH access token.
- Crabbox sync: yes, archive sync through the Daytona toolbox.
- Desktop / browser / code: no — Daytona has no Crabbox VNC or `code` surface.
- Actions hydration: no.
- Coordinator (broker): yes for Linux SSH/sync/run. The coordinator owns the
  API key and rotates the lease's SSH token.

## Gotchas

- Direct mode requires `daytona.snapshot` (or `--daytona-snapshot`). Brokered
  mode uses the coordinator's optional `CRABBOX_DAYTONA_SNAPSHOT`.
- `--class` and `--type` are rejected; size the sandbox through the snapshot.
- `--id <sandbox-id-or-slug>` is required to address an existing sandbox.
- Daytona `run` is delegated to the toolbox APIs; it is not core-over-SSH
  execution. Because of that, the following `run` options are rejected:
  `--sync-only`, `--checksum`, `--force-sync-large`, `--full-resync`,
  `--fresh-pr`, `--script` / `--script-stdin`, `--env-helper`,
  `--capture-stdout` / `--capture-stderr`, `--capture-on-fail`, `--download`,
  `--artifact-glob`, `--require-artifact`, `--emit-proof`, and `--stop-after`.
- `--actions-runner` is rejected because it needs a normal SSH lease host.
- `--keep-on-failure` keeps a newly created failed sandbox until Daytona
  auto-stop or an explicit `crabbox stop`.

## Related docs

- [Feature: Daytona](../features/daytona.md)
- [Provider backends](../provider-backends.md)
