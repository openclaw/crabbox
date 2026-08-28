# Daytona

Read when:

- choosing `provider: daytona`;
- configuring Daytona authentication, snapshots, or SSH access;
- understanding how the Daytona backend differs from a plain SSH-lease provider.

`provider: daytona` provisions [Daytona](https://www.daytona.io/) sandboxes and
supports Linux targets exclusively. Direct mode is hybrid: `warmup`, `run`,
`list`, `status`, and `stop` drive Daytona's SDK and toolbox APIs, while `ssh`
mints a short-lived SSH token. Brokered mode keeps the API key in the
coordinator, returns an expiring SSH identity to the authorized client, and
uses Crabbox's normal SSH/rsync run path.

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

| Crabbox-prefixed                  | Daytona name              | Config key               |
| --------------------------------- | ------------------------- | ------------------------ |
| `CRABBOX_DAYTONA_API_KEY`         | `DAYTONA_API_KEY`         | `daytona.apiKey`         |
| `CRABBOX_DAYTONA_JWT_TOKEN`       | `DAYTONA_JWT_TOKEN`       | `daytona.jwtToken`       |
| `CRABBOX_DAYTONA_ORGANIZATION_ID` | `DAYTONA_ORGANIZATION_ID` | `daytona.organizationId` |
| `CRABBOX_DAYTONA_API_URL`         | `DAYTONA_API_URL`         | `daytona.apiUrl`         |

The API URL defaults to `https://app.daytona.io/api`.

For brokered mode, configure the coordinator instead of client auth:

```text
DAYTONA_CRABBOX_KEY               # required secret
CRABBOX_DAYTONA_SNAPSHOT          # optional shared snapshot
CRABBOX_DAYTONA_TARGET            # optional compute target
CRABBOX_DAYTONA_SSH_ACCESS_MINUTES # minimum token TTL; default 120
```

The coordinator accepts no Daytona API credential from lease requests.
Clients authenticate only to Crabbox; no Daytona CLI profile or Daytona API
environment variable is required on the client.

Use `crabbox doctor --provider daytona` to verify the broker fallback without
creating a sandbox. The readiness endpoint performs a read-only inventory
request and reports the client auth boundary, coordinator control plane,
SSH/rsync data plane, snapshot source, and current inventory count.

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

| Config key                 | Flag                           | Default                      |
| -------------------------- | ------------------------------ | ---------------------------- |
| `daytona.snapshot`         | `--daytona-snapshot`           | _(required)_                 |
| `daytona.target`           | `--daytona-target`             | _(empty)_                    |
| `daytona.user`             | `--daytona-user`               | `daytona`                    |
| `daytona.workRoot`         | `--daytona-work-root`          | `/home/daytona/crabbox`      |
| `daytona.sshGatewayHost`   | `--daytona-ssh-gateway-host`   | `ssh.app.daytona.io`         |
| `daytona.sshAccessMinutes` | `--daytona-ssh-access-minutes` | `30`                         |
| `daytona.apiUrl`           | `--daytona-api-url`            | `https://app.daytona.io/api` |

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
  Both direct creation paths use private previews and Daytona's native hard TTL.
  Allocation is recorded before readiness polling; failed startup triggers
  ownership-checked cleanup and preserves the recovery claim if cleanup fails.
- **`run --id`** resolves a Daytona sandbox, uploads a Crabbox sync-manifest
  archive through Daytona toolbox file APIs, extracts it in the sandbox, and
  executes the command through Daytona toolbox process APIs. The command transport
  is Daytona's SDK — not direct SSH.
  Resync preserves installed dependencies and remote-only files, deleting only
  source paths removed from the sync manifest. Periodic activity refreshes keep
  quiet commands alive until their command deadline or the hard sandbox TTL.
- **`list`** and **`status`** discover sandboxes only when Daytona labels bind
  them to the Daytona provider and a canonical Crabbox lease. Direct IDs with
  missing or mismatched ownership labels are rejected.
- **`run --id`**, **`ssh`**, and **`stop`** additionally require a local claim
  that binds the exact Daytona sandbox ID to that lease. A legacy labelled
  sandbox with an unbound claim must be adopted explicitly with `--reclaim`
  from its owning repository before it can be reused or deleted.
- **`ssh`** mints a fresh Daytona SSH access token (TTL `daytona.sshAccessMinutes`,
  default 30 minutes), parses the host and port from Daytona's returned SSH
  command (falling back to `daytona.sshGatewayHost` and port 22), and prints the
  token redacted as `<token>` unless `--show-secret` is passed.

Daytona is a hybrid backend: core rendering, lease labels, sync manifests, and
repo claim checks stay Crabbox-owned, while the `run` transport is the Daytona
SDK/toolbox. Actions runner hydration is not supported, because it requires a
long-lived, directly SSH-reachable runner host.

In brokered mode the Worker creates and deletes the sandbox, verifies exact
lease labels before destructive cleanup, refreshes the SSH token before expiry,
redacts that token from the portal, and treats an already absent owned sandbox
as successful cleanup. Workspaces and ready pools are disabled because they
persist an SSH endpoint beyond the rotating credential.

## Snapshot bootstrap administration

For snapshots of an existing direct lease, use `crabbox checkpoint create
--provider daytona --id <lease> --mode native --no-reboot=false`, then
`checkpoint fork`, `inspect --verify`, and `delete`. See
[Native snapshots and forks](../providers/daytona.md#native-snapshots-and-forks)
for the stop/restart contract and cleanup. The administration endpoint below is
the separate coordinator workflow for bootstrapping shared base snapshots.

The coordinator exposes `POST /v1/admin/providers/daytona/snapshot-bootstrap`
for creating a reusable Daytona snapshot without giving clients Daytona
credentials. The request requires coordinator admin authentication and an
explicit `confirm: true` because it creates paid provider resources.

```json
{
	"name": "crabbox-ready",
	"cpu": 2,
	"memoryGiB": 4,
	"diskGiB": 10,
	"baseImage": "registry.example/crabbox@sha256:<64 lowercase hex characters>",
	"confirm": true
}
```

CPU is limited to 1-4, memory to 1-8 GiB, and disk to 3-10 GiB. `baseImage`
must use an immutable SHA-256 digest. The coordinator rejects an existing
snapshot name, verifies the resources Daytona actually applied, waits for the
new snapshot to become active for up to 20 minutes, and waits for the temporary
builder to be destroyed or absent before reporting successful cleanup. It also
configures Daytona to stop an idle builder after 30 minutes and delete it after
it remains stopped for another 60 minutes if the Worker cleanup request is
lost.

After this route is deployed, mint a snapshot through the protected
default-branch workflow. The `image-publisher` environment supplies coordinator
admin auth, so the operator needs no Daytona credential:

```sh
gh workflow run daytona-snapshot-bootstrap.yml \
  --ref main \
  -f name=crabbox-ready \
  -f cpu=2 \
  -f memoryGiB=4 \
  -f diskGiB=10 \
  -f "baseImage=registry.example/crabbox@sha256:<digest>" \
  -f confirm=create
```

The workflow requires the protected default-branch definition and environment
approval, serializes snapshot creation, verifies the exact applied resources
and `cleanup=deleted`, and uploads only sanitized proof without the builder ID
or coordinator response.

See [providers.md](../commands/providers.md) for the full provider matrix and
[capabilities.md](capabilities.md) for opt-in lease features.
