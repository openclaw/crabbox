# Daytona

Read when:

- choosing `provider: daytona`;
- configuring Daytona authentication, snapshots, or SSH access;
- understanding how the Daytona backend differs from a plain SSH-lease provider.

`provider: daytona` provisions [Daytona](https://www.daytona.io/) sandboxes from
a snapshot. It is registered as an SSH-lease provider, but the data plane is
hybrid: `warmup`, `run`, `list`, `status`, and `stop` drive Daytona's SDK and
toolbox APIs, while `ssh` mints a short-lived Daytona SSH token only when
interactive shell access is requested. Daytona is direct-from-CLI only — it
never runs through the coordinator — and supports Linux targets exclusively.

## Authentication

Crabbox accepts credentials from two sources, in precedence order:

1. Explicit Crabbox config or environment variables (highest priority).
2. The active Daytona CLI profile (used only when no explicit token is set).

Log in with the Daytona CLI to populate a profile:

```sh
daytona login
```

Crabbox reads the active profile's API key and active organization ID from the
Daytona CLI config when no explicit token is provided.

To set credentials directly, provide an API key:

```sh
export DAYTONA_API_KEY=...
```

or a JWT plus organization ID:

```sh
export DAYTONA_JWT_TOKEN=...
export DAYTONA_ORGANIZATION_ID=...
```

`DAYTONA_ORGANIZATION_ID` is required whenever JWT auth is used. If no API key,
JWT token, or authenticated CLI profile is found, lease operations fail with a
configuration error.

Each variable also has a `CRABBOX_`-prefixed form that takes precedence over the
bare Daytona name (useful when other tooling already owns the unprefixed
variable):

| Crabbox-prefixed                  | Daytona name               | Config key                |
| --------------------------------- | -------------------------- | ------------------------- |
| `CRABBOX_DAYTONA_API_KEY`         | `DAYTONA_API_KEY`          | `daytona.apiKey`          |
| `CRABBOX_DAYTONA_JWT_TOKEN`       | `DAYTONA_JWT_TOKEN`        | `daytona.jwtToken`        |
| `CRABBOX_DAYTONA_ORGANIZATION_ID` | `DAYTONA_ORGANIZATION_ID`  | `daytona.organizationId`  |
| `CRABBOX_DAYTONA_API_URL`         | `DAYTONA_API_URL`          | `daytona.apiUrl`          |

The API URL defaults to `https://app.daytona.io/api`.

## Config

The Daytona integration is snapshot-first: the snapshot owns CPU, memory, disk,
and installed tooling. Crabbox does not expose Daytona resource flags, so
`--class` and `--type` are rejected for `provider=daytona` — size the sandbox in
the snapshot instead.

```yaml
provider: daytona
target: linux
daytona:
  snapshot: my-app-ready
  target: "" # optional Daytona compute target
  user: daytona
  workRoot: /home/daytona/crabbox
  sshGatewayHost: ssh.app.daytona.io # fallback when the API omits an SSH command
  sshAccessMinutes: 30 # SSH access token TTL
```

| Config key             | Flag                          | Default                     |
| ---------------------- | ----------------------------- | --------------------------- |
| `daytona.snapshot`     | `--daytona-snapshot`          | _(required)_                |
| `daytona.target`       | `--daytona-target`            | _(empty)_                   |
| `daytona.user`         | `--daytona-user`              | `daytona`                   |
| `daytona.workRoot`     | `--daytona-work-root`         | `/home/daytona/crabbox`     |
| `daytona.sshGatewayHost` | `--daytona-ssh-gateway-host` | `ssh.app.daytona.io`       |
| `daytona.sshAccessMinutes` | `--daytona-ssh-access-minutes` | `30`                  |
| `daytona.apiUrl`       | `--daytona-api-url`           | `https://app.daytona.io/api` |

A snapshot is required; `warmup`/`run` fail without `--daytona-snapshot` or
`daytona.snapshot`.

## Examples

```sh
# Lease a sandbox from a snapshot and keep it warm.
crabbox warmup --provider daytona --daytona-snapshot my-app-ready

# Sync the local checkout into an existing lease and run a command.
crabbox run --provider daytona --id swift-crab -- pnpm test

# Open an interactive shell (mints a short-lived SSH token).
crabbox ssh --provider daytona --id swift-crab

# End the lease.
crabbox stop --provider daytona swift-crab
```

## Behavior

- **`warmup`** creates a Daytona sandbox from `daytona.snapshot`, waits for it to
  become ready, records Crabbox labels, then prints a normal Crabbox lease ID and
  slug.
- **`run --id`** resolves a Daytona sandbox, uploads a Crabbox sync-manifest
  archive through Daytona toolbox file APIs, extracts it in the sandbox, and
  executes the command through Daytona toolbox process APIs. The command transport
  is Daytona's SDK — not direct SSH.
- **`list`**, **`status`**, and **`stop`** find Crabbox-owned sandboxes via
  Daytona sandbox labels.
- **`ssh`** mints a fresh Daytona SSH access token (TTL `daytona.sshAccessMinutes`,
  default 30 minutes), parses the host and port from Daytona's returned SSH
  command (falling back to `daytona.sshGatewayHost` and port 22), and prints the
  token redacted as `<token>` unless `--show-secret` is passed.

Daytona is a hybrid backend: core rendering, lease labels, sync manifests, and
repo claim checks stay Crabbox-owned, while the `run` transport is the Daytona
SDK/toolbox. Actions runner hydration is not supported, because it requires a
long-lived, directly SSH-reachable runner host.

See [providers.md](../commands/providers.md) for the full provider matrix and
[capabilities.md](capabilities.md) for opt-in lease features.
