# Azure Dynamic Sessions Provider

Read when:

- choosing `provider: azure-dynamic-sessions` (or `provider: azure` with the
  `dynamic-sessions` backend);
- running Linux commands inside Azure Container Apps dynamic sessions instead of
  a full SSH VM;
- changing `internal/providers/azuredynamicsessions` or the runner image.

This is a **delegated-run** provider: there is no SSH box. Azure owns the
Hyper-V-isolated session pool and its lifecycle; Crabbox owns the runner image,
local claims, archive sync, command streaming, and timing output. It is direct
from the CLI only and never goes through the broker.

The same backend is selectable two ways:

- `provider: azure-dynamic-sessions`, or
- `provider: azure` with `azure.backend: dynamic-sessions` (alias `--azure-backend dynamic-sessions`).

Microsoft docs:

- <https://learn.microsoft.com/en-us/azure/container-apps/session-pool>
- <https://learn.microsoft.com/en-us/azure/container-apps/sessions-custom-container>

## When To Use

Use Dynamic Sessions for fast, isolated, Linux-only command runs where you do
not need SSH, a desktop, a browser, code-server, or Actions hydration, and where
sizing and egress are governed by the Azure session pool rather than per-lease
flags. For a full SSH VM (including macOS/Windows targets and desktop/browser
capabilities) use [`provider: azure`](azure.md) instead.

## Requirements

- An Azure Container Apps custom-container dynamic session pool.
- A pool image that exposes the Crabbox container runner on port `8787`; build it
  from `worker/azure-dynamic-sessions.Dockerfile`.
- The caller holds the `Azure ContainerApps Session Executor` role on the pool.
- Either `CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN` holds a bearer token for the
  `https://dynamicsessions.io` audience, or `az` is logged in locally so Crabbox
  can mint one.

### Authentication

Crabbox resolves a bearer token in this order:

1. `CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN`, if set.
2. Otherwise `az account get-access-token --resource https://dynamicsessions.io`.
   If `azure.tenant`/`azure.subscription` (or their env/flags) are set, they are
   passed as `--tenant`/`--subscription`.

The endpoint must be `https://` on an `*.azurecontainerapps.io` host (a
loopback `http://localhost` endpoint is allowed for local testing). Endpoints
with userinfo, query strings, or fragments are rejected.

## Building The Runner Image

The bundled image is intentionally small: the Crabbox container runner plus
`bash`, `ca-certificates`, `curl`, `git`, `jq`, `ripgrep`, and `tar`. Extend the
Dockerfile or supply your own image when a pool needs Node, Go, Python,
browsers, or other test runtimes.

For private Azure Container Registry images, prefer a managed identity with
`AcrPull` on the registry and pass that identity through `--registry-identity`,
rather than putting registry passwords on the command line:

```sh
az acr login --name <registry>

docker buildx build \
  --platform linux/amd64 \
  --push \
  --tag <registry>.azurecr.io/crabbox-runner:<tag> \
  --file worker/azure-dynamic-sessions.Dockerfile \
  worker

identity_id="$(az identity show \
  --name <pull-identity> \
  --resource-group example-sandboxes-rg \
  --query id \
  --output tsv)"

identity_principal_id="$(az identity show \
  --name <pull-identity> \
  --resource-group example-sandboxes-rg \
  --query principalId \
  --output tsv)"

registry_id="$(az acr show \
  --name <registry> \
  --query id \
  --output tsv)"

az role assignment create \
  --assignee "$identity_principal_id" \
  --role AcrPull \
  --scope "$registry_id"

az containerapp sessionpool create \
  --name example-pool \
  --resource-group example-sandboxes-rg \
  --environment example-env \
  --registry-server <registry>.azurecr.io \
  --registry-identity "$identity_id" \
  --container-type CustomContainer \
  --image <registry>.azurecr.io/crabbox-runner:<tag> \
  --target-port 8787 \
  --cpu 0.25 \
  --memory 0.5Gi \
  --cooldown-period 300 \
  --max-sessions 20 \
  --ready-sessions 1 \
  --network-status EgressEnabled \
  --location eastus
```

Fetch the pool management endpoint Crabbox needs:

```sh
az containerapp sessionpool show \
  --name example-pool \
  --resource-group example-sandboxes-rg \
  --query "properties.poolManagementEndpoint" \
  --output tsv
```

## Configuration

Point Crabbox at the custom-container pool management endpoint:

```yaml
provider: azure-dynamic-sessions
target: linux
azureDynamicSessions:
  endpoint: https://<pool>.<environment-id>.eastus.azurecontainerapps.io
  workdir: /workspace/crabbox
```

Equivalent Azure-family form:

```yaml
provider: azure
target: linux
azure:
  backend: dynamic-sessions
azureDynamicSessions:
  endpoint: https://<pool>.<environment-id>.eastus.azurecontainerapps.io
  workdir: /workspace/crabbox
```

### Settings

| Config key (`azureDynamicSessions.*`) | Flag | Env override | Default |
| --- | --- | --- | --- |
| `endpoint` | `--azure-dynamic-sessions-endpoint` | `CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT` | (required) |
| `apiVersion` | `--azure-dynamic-sessions-api-version` | `CRABBOX_AZURE_DYNAMIC_SESSIONS_API_VERSION` | `2025-02-02-preview` |
| `workdir` | `--azure-dynamic-sessions-workdir` | `CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR` | `/workspace/crabbox` |
| `timeoutSecs` | `--azure-dynamic-sessions-timeout-secs` | `CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS` | `1800` (falls back to `--ttl` when unset) |

The token uses `CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN` (see
[Authentication](#authentication)).

Legacy `azureDynamicSessions.pool` and
`CRABBOX_AZURE_DYNAMIC_SESSIONS_POOL` values are rejected. The
`endpoint` setting is already the pool-specific management endpoint.

`workdir` must be an absolute path and may not be a broad system directory such
as `/`, `/tmp`, `/usr`, or bare `/workspace`; pick a dedicated subdirectory.

## Behavior

- **`warmup`** allocates a random `azds-…` session identifier and verifies the
  runner by calling `/health` through the pool management endpoint. `--keep`
  (default) retains the session for reuse; without it the session is stopped via
  `/.management/stopSession`.
- **`run`** uploads the dirty checkout as a gzip tar archive to the runner over
  `/v1/files`, extracts it under `workdir`, then streams the command over
  `/v1/exec` (NDJSON event stream). `--no-sync` skips the upload, `--sync-only`
  stops after sync. Acquired-but-not-kept sessions are stopped after the run.
  `--lease-output` records the session lease, whether it was reused or retained,
  and the matching `crabbox stop` cleanup command.
  API redirects are followed only when they remain on the configured endpoint's
  origin, preventing credentials and request bodies from crossing trust boundaries.
- **`status`** / **`list`** read `/.management/getSession` and
  `/.management/listSessions` and join them with local Crabbox claims. `status`
  can `--wait` for readiness.
- **`stop`** stops the session and removes the local claim; a missing session
  just clears the stale claim.

Sessions are tracked by **local claims** scoped to the endpoint. `run`,
`status`, and `stop` accept kept Crabbox lease IDs or slugs only — raw Dynamic
Sessions identifiers are rejected unless they are already claimed.

## Limitations

- Targets `linux` only.
- Supported commands: `warmup`, `run`, `status`, `list`, `stop`, `doctor`.
- No SSH, VNC, desktop, browser, code-server, Actions hydration, downloads, or
  run artifacts — the provider has no SSH target. SSH-run artifact flags such as
  `--artifact-glob` and `--require-artifact` are rejected.
- `--class` and `--type` are rejected; choose CPU/memory and egress in the Azure
  session pool configuration.
- `--actions-runner` is rejected.
- Sync is archive upload/extract, not rsync, so rsync-specific options
  (for example `--checksum`) are rejected.
