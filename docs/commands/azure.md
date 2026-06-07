# azure

`crabbox azure` groups Azure provider setup commands. It currently has a single
subcommand, `azure login`, which captures your Azure subscription details into
the user config so direct-mode Azure leases work without exporting `AZURE_*`
environment variables on every invocation.

## azure login

`crabbox azure login` detects the active Azure subscription from the local `az`
CLI, validates that credentials can acquire a management-plane token, and stores
the subscription, tenant, and location in the user config.

### Prerequisites

1. Install the [Azure CLI](https://aka.ms/installazurecli) (`az` must be on your
   `PATH`).
2. Run `az login` and select the subscription you want Crabbox to use.

### Usage

```sh
# Use the active az CLI subscription and the default location (eastus):
crabbox azure login

# Pick a specific subscription by ID or name:
crabbox azure login --subscription 00000000-0000-0000-0000-000000000000

# Pick a specific location:
crabbox azure login --location westus2

# Emit JSON for scripting:
crabbox azure login --json
```

After login succeeds, `crabbox warmup --provider azure` and
`crabbox run --provider azure -- <command>` work immediately.

### Flags

```text
--subscription <id|name>    Azure subscription ID or name (default: active az CLI subscription)
--location <location>       Azure location for provisioning (default: eastus)
--json                      Print JSON output
```

### What it does

1. Runs `az account show` (with `--subscription` when you pass `--subscription`)
   to read the subscription ID, tenant ID, and subscription name.
2. Validates that `DefaultAzureCredential` can acquire a token for the Azure
   Resource Manager scope (`https://management.azure.com/.default`). If this
   fails, run `az login` and retry.
3. Writes `azure.subscriptionId`, `azure.tenantId`, and `azure.location` to the
   writable user config file (for example `~/.config/crabbox/config.yaml`).
4. Sets `provider: azure` only when no default provider is already configured.

JSON output (`--json`) includes `subscription`, `tenant`, `name`, `location`,
and `configPath`.

### Auto-resolve without azure login

Running `crabbox azure login` is the recommended way to persist configuration,
but it is not strictly required. When a direct Azure command starts and no
subscription is set in config or the environment, Crabbox falls back to
`az account show` at runtime to detect the subscription (and tenant). This lets
`az login` plus `crabbox warmup --provider azure` work with zero extra setup. A
location is still required: set `azure.location`, pass `--location`, or export
`CRABBOX_AZURE_LOCATION`.

### Credentials and environment

`azure login` only stores subscription, tenant, and location — it never stores
secrets. At provision time Crabbox authenticates with `DefaultAzureCredential`,
or with a client-secret credential when `azure.tenantId`, `azure.clientId`, and
the `AZURE_CLIENT_SECRET` environment variable are all present.

Equivalent values can also come from the environment instead of config:
`CRABBOX_AZURE_SUBSCRIPTION_ID` (or `AZURE_SUBSCRIPTION_ID`),
`CRABBOX_AZURE_TENANT_ID` (or `AZURE_TENANT_ID`),
`CRABBOX_AZURE_CLIENT_ID` (or `AZURE_CLIENT_ID`), and `CRABBOX_AZURE_LOCATION`.

## Related docs

- [Azure provider](../providers/azure.md)
- [Azure feature guide](../features/azure.md)
- [login (broker)](login.md)
- [config](config.md)
