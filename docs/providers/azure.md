# Azure Provider

Read this when you are:

- choosing `provider: azure`;
- debugging Azure VM capacity, quotas, images, or SSH readiness;
- changing `internal/providers/azure` or the direct Azure provisioning code.

Azure is a managed provider for Linux, native Windows, and Windows WSL2 leases.
Azure provisions the VM, public IP, NIC, and OS disk; Crabbox then owns SSH
readiness, optional desktop/VNC or WSL2 bootstrap, sync, command execution, test
results, and cleanup.

## When to use Azure

Reach for Azure when your cloud capacity lives in an Azure subscription, or when
Microsoft tooling, Entra ID, or Azure-specific networking constraints make
[AWS](aws.md) or [Hetzner](hetzner.md) a poor fit. Use Hetzner for cheaper
Linux-only capacity and AWS for macOS targets — Azure does not provision macOS.

For Windows and Windows WSL2, Azure is a good default when the subscription has
healthy credits or reserved capacity. Brokered Azure leases use the same ordered
capacity-region fallback model as AWS.

Azure supports both execution modes:

- **Direct mode** uses local Azure credentials and talks to Azure directly from
  the CLI.
- **Brokered mode** routes lease lifecycle through the Worker (the broker), which
  holds an operator-owned Azure service principal. The CLI still does SSH, sync,
  and command execution directly to the runner host.

## Commands

```sh
crabbox warmup --provider azure --class beast
crabbox warmup --provider azure --arch arm64 --class fast
crabbox warmup --provider azure --class beast --azure-os-disk ephemeral
crabbox warmup --provider azure --class beast --azure-os-disk ephemeral-preview
crabbox run --provider azure --class standard -- pnpm test
crabbox run --provider azure --azure-backend dynamic-sessions -- pnpm test
crabbox warmup --provider azure --target windows --class standard
crabbox warmup --provider azure --target windows --desktop --class standard
crabbox warmup --provider azure --target windows --windows-mode wsl2 --class standard
crabbox warmup --provider azure --desktop --browser
crabbox ssh --provider azure --id blue-lobster
crabbox stop --provider azure blue-lobster
crabbox cleanup --provider azure
```

`--type` is exact (for example `--type Standard_D32ads_v6`). Use `--class` when
you want SKU fallback. Azure leases use managed OS disks by default, so native
checkpoint/fork works without extra flags. Pass `--azure-os-disk ephemeral` only
for stateless leases that do not need native checkpoint/fork. Pass
`--azure-os-disk ephemeral-preview` to opt into Azure's public-preview
full-caching mode for ephemeral OS disks.

## Backend selection

`azure.backend` (CLI `--azure-backend`, env `CRABBOX_AZURE_BACKEND`) selects the
Azure-family backend:

- `vm` (default) — Azure Virtual Machines with SSH leases. This is what this page
  documents.
- `dynamic-sessions` — routes to the
  [`azure-dynamic-sessions`](azure-dynamic-sessions.md) provider for delegated
  Linux runs inside an Azure Container Apps Dynamic Sessions pool.

## Config

```yaml
provider: azure
target: linux
architecture: amd64
class: beast
azure:
  backend: vm
  subscriptionId: 00000000-0000-0000-0000-000000000000
  tenantId: 00000000-0000-0000-0000-000000000000
  clientId: 00000000-0000-0000-0000-000000000000
  location: eastus
  resourceGroup: crabbox-leases
  image: Canonical:ubuntu-26_04-lts:server:latest
  osDisk: managed
  vnet: crabbox-vnet
  subnet: crabbox-subnet
  nsg: crabbox-nsg
  sshCIDRs: []
  network: public
```

`subscriptionId`, `tenantId`, and `clientId` may be set in config or sourced from
environment variables. The client secret is never read from config — it must come
from the environment.

Set `architecture: arm64` or pass `--arch arm64` for Linux ARM leases. Crabbox
then switches class fallback to Azure Cobalt Dpsv6/Dpdsv6 sizes and uses the
matching Ubuntu ARM64 Marketplace image unless `azure.image` is explicitly set.
ARM64 is not supported for native Windows, WSL2, or macOS targets.

`azure.network` selects which IP the CLI uses for SSH: `public` (default) uses the
VM public IP, `private` uses the NIC private IP from the vnet. Use `private` when
connecting through a VPN to the Azure virtual network.

`azure.osDisk` accepts `managed`, `ephemeral`, `ephemeral-preview`, or `auto`:

- `managed` (default) provisions a managed `StandardSSD_LRS` OS disk so Azure
  native disk-snapshot checkpoints work.
- `ephemeral` opts into a local OS disk. It requires a SKU with ephemeral OS disk
  support, fails during provisioning when the selected SKU cannot support it, and
  disables native Azure checkpoint/fork.
- `ephemeral-preview` enables Azure ephemeral OS disk full caching with Compute
  API `2025-04-01`. It is public preview, has the same checkpoint/fork limits as
  `ephemeral`, and skips known unsupported 2-core, 4-core, and no-local-disk
  Azure SKUs from Crabbox fallback lists.
- `auto` is accepted for compatibility and resolves to `managed`.

### Environment variables

Direct-mode config can be supplied entirely via environment:

```text
AZURE_SUBSCRIPTION_ID            # or CRABBOX_AZURE_SUBSCRIPTION_ID
AZURE_TENANT_ID                  # or CRABBOX_AZURE_TENANT_ID
AZURE_CLIENT_ID                  # or CRABBOX_AZURE_CLIENT_ID
AZURE_CLIENT_SECRET              # service-principal secret (never read from config)
CRABBOX_AZURE_BACKEND            # vm | dynamic-sessions
CRABBOX_AZURE_LOCATION
CRABBOX_AZURE_RESOURCE_GROUP
CRABBOX_AZURE_IMAGE
CRABBOX_AZURE_OS_DISK            # managed | ephemeral | ephemeral-preview | auto
CRABBOX_AZURE_VNET
CRABBOX_AZURE_SUBNET
CRABBOX_AZURE_NSG
CRABBOX_AZURE_SSH_CIDRS          # comma-separated
CRABBOX_AZURE_NETWORK            # public | private
```

`AZURE_*` are the standard service-principal variables consumed by
`DefaultAzureCredential`. Crabbox does not read or print the client secret.

## Auth

The simplest setup uses the Azure CLI — no environment variables needed:

```sh
az login
crabbox azure login
crabbox warmup --provider azure
```

`crabbox azure login` detects the active subscription, validates credentials, and
stores the subscription ID, tenant ID, and location (default `eastus`) in user
config. After that, `DefaultAzureCredential` picks up the `az login` session
automatically.

For service-principal setups (CI, automation, shared environments), use
environment variables. If `azure.tenantId` and `azure.clientId` (or
`CRABBOX_AZURE_TENANT_ID` / `CRABBOX_AZURE_CLIENT_ID`) are configured and
`AZURE_CLIENT_SECRET` is set in the environment, Crabbox builds a
`ClientSecretCredential` from those explicit values. Otherwise it falls back to
[`azidentity.NewDefaultAzureCredential`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#DefaultAzureCredential),
which scans environment, workload identity, managed identity, and CLI credentials
in order. The simplest service-principal grant is the
[Contributor](https://learn.microsoft.com/azure/role-based-access-control/built-in-roles#contributor)
role scoped to the resource group:

```sh
export AZURE_TENANT_ID=...
export AZURE_CLIENT_ID=...
export AZURE_CLIENT_SECRET=...
export AZURE_SUBSCRIPTION_ID=...
```

See [Authenticate Go apps to Azure services with service principals](https://learn.microsoft.com/azure/developer/go/sdk/authentication/local-development-service-principal).

## Brokered mode

Brokered leases reuse the same Azure service-principal secrets on the Worker:
`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, and
`AZURE_SUBSCRIPTION_ID`. Operators own the resource group, vnet, subnet, NSG, OS
disk mode, and SSH CIDR defaults through the `CRABBOX_AZURE_*` env vars on the
Worker. A lease request may override only `azureLocation`, `azureImage`, and
`azureOSDisk`.

Set `CRABBOX_AZURE_REGIONS` on the Worker for Azure-specific capacity fallback;
`CRABBOX_CAPACITY_REGIONS` remains the AWS region fallback list.

Run `crabbox doctor --provider azure --target windows` before leasing through the
broker. The coordinator readiness check reports missing Worker secret names
without exposing values, and lease creation fails with `provider_not_configured`
until the required service-principal secrets are present.

## Lifecycle

1. Resolve credentials per the rules above.
2. Ensure the shared resource group, virtual network, subnet, and network
   security group exist. Crabbox first issues `Get` calls against each resource:
   - If a resource exists without the `managed_by=crabbox` tag, Crabbox refuses
     to mutate it and returns an adopt-or-rename error.
   - If a resource exists with the tag, it is left alone (Crabbox does not
     overwrite tags, address spaces, subnets, or rules on later acquires).
   - If a resource is missing, it is created with Crabbox tags and the configured
     layout. Inbound SSH rules are derived from `azure.sshCIDRs`, the configured
     SSH port, and any fallback ports.
3. Mint a per-lease SSH key.
4. Pick the configured class SKU candidates and try each in order.
5. Create a public IP, NIC, and VM. Linux passes cloud-init in
   `osProfile.customData` and the SSH key in
   `osProfile.linuxConfiguration.ssh.publicKeys`. Native Windows uses a Windows
   Server small-disk Gen2 image, Windows `osProfile` fields (`adminPassword`,
   `computerName`, `windowsConfiguration`), and a non-rebooting Custom Script
   Extension that runs the initial SSH bootstrap saved in
   `C:\AzureData\CustomData.bin`.
6. Choose the OS disk mode (see Config above).
7. Tag the VM, NIC, and public IP with Crabbox lease metadata.
8. Wait for the public IP to allocate, then for SSH readiness.
9. For `--desktop` on native Windows, run the shared Windows desktop bootstrap
   over SSH: install TightVNC, configure the generated `crabbox` Windows login,
   enable auto-logon, reboot once, and wait for SSH/VNC readiness.
10. For `--windows-mode wsl2`, run the shared Windows WSL2 bootstrap over SSH:
    enable WSL/VirtualMachinePlatform/HypervisorPlatform, reboot as needed, import
    Ubuntu, and wait for the Linux-side `crabbox-ready` check.
11. Let core sync and run over SSH.
12. On release/cleanup, cascade-delete VM -> NIC -> public IP -> OS disk. The
    shared infra remains.

## Classes

Default Linux SKU candidates (first that provisions wins):

```text
standard  Standard_D32ads_v6, Standard_D32ds_v6, Standard_F32s_v2, Standard_D32ads_v5, Standard_D32ds_v5, then D/F 16-vCPU fallbacks
fast      Standard_D64ads_v6, Standard_D64ds_v6, Standard_F64s_v2, Standard_D64ads_v5, Standard_D64ds_v5, then D/F 48-vCPU and 32-vCPU fallbacks
large     Standard_D96ads_v6, Standard_D96ds_v6, Standard_D96ads_v5, Standard_D96ds_v5, then D/F 64-vCPU and 48-vCPU fallbacks
beast     Standard_D192ds_v6, Standard_D128ds_v6, then D/F 96-vCPU and 64-vCPU fallbacks
```

Default native Windows and WSL2 SKU candidates:

```text
standard  Standard_D2ads_v6, Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, then Standard_D2as_v6
fast      Standard_D4ads_v6, Standard_D4ds_v6, Standard_D4ads_v5, Standard_D4ds_v5, then Standard_D4as_v6
large     Standard_D8ads_v6, Standard_D8ds_v6, Standard_D8ads_v5, Standard_D8ds_v5, then Standard_D8as_v6
beast     Standard_D16ads_v6, Standard_D16ds_v6, Standard_D16ads_v5, Standard_D16ds_v5, then Standard_D8ads_v6
```

Class-based provisioning falls back across the candidate list when Azure rejects a
SKU for capacity or quota (`SkuNotAvailable`, `QuotaExceeded`, `AllocationFailed`,
`OverconstrainedAllocationRequest`). When `capacity.regions` (or broker-side
`CRABBOX_AZURE_REGIONS`) is set, Crabbox also tries those Azure regions in order
and uses region-scoped shared network names for the fallback path. Spot leases
fall back to on-demand when `capacity.fallback` starts with `on-demand`. Azure
Spot VMs use eviction policy `Delete` and `billingProfile.maxPrice: -1`, so price
alone does not evict a lease while Azure still charges no more than the on-demand
price. Explicit `--type` is exact for size selection but still allows configured
region fallback.

## Capabilities

- SSH: yes.
- Crabbox sync: yes.
- Native Windows: SSH, sync, run, and desktop/VNC.
- Windows WSL2: SSH, sync, run, and Actions hydration (through the POSIX WSL
  contract).
- Desktop: Linux and native Windows.
- Browser / code: Linux only.
- Tailscale: Linux managed leases.
- Cleanup: yes.
- Coordinator (brokered): Linux, native Windows, and WSL2 leases.

Azure does not provision macOS through this provider. Use [AWS](aws.md) or
[`provider: ssh`](ssh.md) for macOS targets.

## Gotchas

- Azure VM names are constrained to 1-64 characters and cannot contain
  underscores. The `leaseProviderName` helper substitutes dashes for underscores;
  keep that constraint in mind if you customize naming.
- Windows computer names are limited to 15 characters. Crabbox keeps the VM
  resource name stable and derives a shorter Windows `computerName`.
- The first acquire in an empty subscription pays the cost of creating the shared
  resource group, vnet, and NSG. Later acquires only create per-lease resources.
- Shared Azure network resources are regional. Leases use the configured
  `azure.vnet`/`azure.nsg` names when they match the target region. If a
  Crabbox-managed VNet or NSG with that base name already exists in another
  region, Crabbox automatically uses region-scoped names such as
  `crabbox-vnet-westeurope`. Multi-region fallback also uses region-scoped
  shared network names, so one managed resource group can hold fallback networks
  safely.
- If you already have a resource group / vnet / NSG with the configured names,
  Crabbox refuses to mutate them unless they carry the `managed_by=crabbox` tag.
  Tag them to adopt, choose different names in `azure.*` config, or let Crabbox
  create dedicated resources.
- `crabbox stop --provider azure <name>` only acts on VMs that carry `crabbox=true`
  (and either no `provider` tag or `provider=azure`). A manually named VM in the
  resource group will not be deleted by Crabbox.
- The default SSH NSG rule allows `0.0.0.0/0` when `azure.sshCIDRs` is empty. Set
  explicit CIDRs for any production-adjacent setup.
- Azure costs are not hardcoded in Crabbox. Set `CRABBOX_COST_RATES_JSON` when you
  need exact Azure cost guardrails.
- Azure native Windows uses a Custom Script Extension because Windows custom data
  is written to disk but not executed by Azure provisioning. Keep that extension
  path non-rebooting; Windows desktop/VNC setup runs later over SSH.
- Direct-mode cleanup is best effort. Use `crabbox cleanup --provider azure` to
  sweep expired direct leases.

## Related docs

- [Azure Dynamic Sessions provider](azure-dynamic-sessions.md)
- [Linux VNC](../features/vnc-linux.md)
- [Provider backends](../provider-backends.md)
