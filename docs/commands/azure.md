# azure

`crabbox azure` groups Azure provider setup commands.

## azure login

`crabbox azure login` detects the active Azure subscription from the `az` CLI,
validates credentials through `DefaultAzureCredential`, and stores subscription,
tenant, and location in the user config. After this, direct-mode Azure commands
work without any `export AZURE_*` environment variables.

### Prerequisites

1. Install the [Azure CLI](https://aka.ms/installazurecli).
2. Run `az login` and select the subscription you want Crabbox to use.

### Usage

```sh
# Use the active az CLI subscription and default location (eastus):
crabbox azure login

# Pick a specific subscription:
crabbox azure login --subscription 00000000-0000-0000-0000-000000000000

# Pick a specific location:
crabbox azure login --location westus2

# JSON output:
crabbox azure login --json
```

After login succeeds, `crabbox warmup --provider azure` and
`crabbox run --provider azure` work immediately.

### Flags

```text
--subscription <id|name>    Azure subscription ID or name (default: active az CLI subscription)
--location <location>       Azure location for provisioning (default: eastus)
--json                      Print JSON output
```

### What it does

1. Runs `az account show` to detect the active subscription ID, tenant ID,
   and subscription name.
2. Validates that `DefaultAzureCredential` can acquire a token for Azure
   Resource Manager.
3. Writes `azure.subscriptionId`, `azure.tenantId`, and `azure.location`
   to the user config file (e.g. `~/.config/crabbox/config.yaml`).
4. Sets `provider: azure` if no default provider is configured.

### Auto-resolve

Even without running `crabbox azure login`, if the user config or environment
does not contain a subscription ID, Crabbox will attempt to detect it from
`az account show` at runtime. This makes `az login` + `crabbox warmup
--provider azure` work with zero configuration, though `crabbox azure login`
is recommended for a persistent setup.

Related docs:

- [Azure provider](../providers/azure.md)
- [Azure feature guide](../features/azure.md)
- [login (broker)](login.md)
- [config](config.md)
