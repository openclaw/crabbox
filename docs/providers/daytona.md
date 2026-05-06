# Daytona Provider

Read when:

- choosing `provider: daytona`;
- configuring Daytona API auth, snapshots, or SSH access;
- changing `internal/providers/daytona`.

Daytona is a hybrid provider. `run` and `warmup` use Daytona SDK/toolbox APIs
for sandbox lifecycle, archive upload, extraction, and process execution.
Explicit `ssh` access mints a short-lived Daytona SSH token and then uses the
normal Crabbox SSH client.

## When To Use

Use Daytona when the sandbox image should come from a Daytona snapshot and
command execution should stay inside Daytona's toolbox APIs. Use AWS, Hetzner,
or Static SSH when you need a normal long-lived SSH lease for Actions hydration
or VNC/code workflows.

## Commands

```sh
crabbox warmup --provider daytona --daytona-snapshot crabbox-ready
crabbox run --provider daytona --daytona-snapshot crabbox-ready -- pnpm test
crabbox run --provider daytona --id blue-lobster -- pnpm test:changed
crabbox ssh --provider daytona --id blue-lobster
crabbox stop --provider daytona blue-lobster
```

## Auth

Use the Daytona CLI login:

```sh
daytona login --api-key ...
```

Crabbox reads the active Daytona CLI profile when no Daytona auth environment
variables are set.

You can also use explicit environment auth with an API key:

```sh
export DAYTONA_API_KEY=...
```

or JWT auth:

```sh
export DAYTONA_JWT_TOKEN=...
export DAYTONA_ORGANIZATION_ID=...
```

`DAYTONA_ORGANIZATION_ID` is required with JWT auth.
Explicit environment or Crabbox config values override the Daytona CLI profile.

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

## Lifecycle

1. Create or resolve a Daytona sandbox from `daytona.snapshot`.
2. Store Crabbox labels and local repo claims.
3. For `run`, build the Crabbox sync manifest, create a gzipped tar archive,
   stream the archive to Daytona toolbox upload, extract it, and execute through
   Daytona process APIs.
4. For `ssh`, request short-lived SSH access, parse Daytona's `sshCommand`, and
   redact the token in normal output.
5. Delete the sandbox on release unless the lease is kept.

## Capabilities

- SSH: yes, explicit short-lived token access.
- Crabbox sync: yes, archive sync through Daytona toolbox.
- Desktop/browser/code: no current Crabbox VNC/code surface.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- `daytona.snapshot` is required when creating a sandbox.
- Snapshot contents own CPU, memory, disk, and installed tooling in this mode.
- Daytona `run` is delegated to toolbox APIs; it is not the same as core-over-SSH
  execution.
- `--actions-runner` is rejected because it needs a normal SSH lease host.

Related docs:

- [Feature: Daytona](../features/daytona.md)
- [Provider backends](../provider-backends.md)
