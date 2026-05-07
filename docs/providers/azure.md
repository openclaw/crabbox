# Azure Provider

Read when:

- choosing `provider: azure`;
- debugging Azure VM capacity, quotas, images, or SSH readiness;
- changing `internal/providers/azure` or the direct Azure provisioning code.

Azure is a managed provider for Linux and native Windows SSH leases. Azure
provisions the VM, public IP, NIC, and OS disk, then Crabbox owns SSH
readiness, sync, command execution, results, and cleanup.

## When To Use

Use Azure when the team's cloud capacity lives in an Azure subscription, or
when Microsoft tooling, Entra ID, or Azure-specific networking constraints
make AWS or Hetzner inappropriate. Use Hetzner for cheaper Linux-only
capacity and AWS for Windows desktop, Windows WSL2, or macOS targets.

Azure supports direct mode and brokered Linux/native Windows leases. Direct
mode uses local Azure credentials. Brokered mode uses the operator-owned
Azure service principal configured on the Worker.

## Commands

```sh
crabbox warmup --provider azure --class beast
crabbox run --provider azure --class standard -- pnpm test
crabbox warmup --provider azure --target windows --class standard
crabbox warmup --provider azure --desktop --browser
crabbox ssh --provider azure --id blue-lobster
crabbox stop --provider azure blue-lobster
crabbox cleanup --provider azure
```

`--type` is exact (e.g. `--type Standard_D32ads_v6`). Use `--class` when SKU
fallback is desired.

## Config

```yaml
provider: azure
target: linux
class: beast
azure:
  subscriptionId: 00000000-0000-0000-0000-000000000000
  tenantId: 00000000-0000-0000-0000-000000000000
  clientId: 00000000-0000-0000-0000-000000000000
  location: eastus
  resourceGroup: crabbox-leases
  image: Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest
  vnet: crabbox-vnet
  subnet: crabbox-subnet
  nsg: crabbox-nsg
  sshCIDRs: []
```

`subscriptionId`, `tenantId`, and `clientId` may be set in config or sourced
from environment variables. The client secret is never read from config; it
must come from the environment.

Important direct-mode environment:

```text
AZURE_SUBSCRIPTION_ID
AZURE_TENANT_ID
AZURE_CLIENT_ID
AZURE_CLIENT_SECRET
CRABBOX_AZURE_SUBSCRIPTION_ID
CRABBOX_AZURE_TENANT_ID
CRABBOX_AZURE_CLIENT_ID
CRABBOX_AZURE_LOCATION
CRABBOX_AZURE_RESOURCE_GROUP
CRABBOX_AZURE_IMAGE
CRABBOX_AZURE_VNET
CRABBOX_AZURE_SUBNET
CRABBOX_AZURE_NSG
CRABBOX_AZURE_SSH_CIDRS
```

`AZURE_*` are the standard service principal env vars consumed by
`DefaultAzureCredential`. Crabbox does not read or print the client secret.

Brokered mode uses the same Azure service-principal secrets on the Worker:
`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, and
`AZURE_SUBSCRIPTION_ID`. Operators own the resource group, vnet, subnet,
NSG, and SSH CIDR defaults through `CRABBOX_AZURE_*` env vars. A lease
request may override only `azureLocation` and `azureImage`.

## Auth

If `azure.tenantId` and `azure.clientId` (or `CRABBOX_AZURE_TENANT_ID` /
`CRABBOX_AZURE_CLIENT_ID`) are configured and `AZURE_CLIENT_SECRET` is set
in the environment, Crabbox builds a `ClientSecretCredential` from those
explicit values. Otherwise it falls back to
[`azidentity.NewDefaultAzureCredential`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#DefaultAzureCredential),
which scans environment, workload identity, managed identity, and CLI
credentials in order. The simplest setup is a service principal with the
[Contributor](https://learn.microsoft.com/azure/role-based-access-control/built-in-roles#contributor)
role scoped to the resource group, configured via:

```sh
export AZURE_TENANT_ID=...
export AZURE_CLIENT_ID=...
export AZURE_CLIENT_SECRET=...
export AZURE_SUBSCRIPTION_ID=...
```

See [Authenticate Go apps to Azure services with service principals](https://learn.microsoft.com/azure/developer/go/sdk/authentication/local-development-service-principal).

## Lifecycle

1. Resolve credentials per the rules above.
2. Ensure the shared resource group, virtual network, subnet, and network
   security group exist. Crabbox first issues `Get` calls against each
   resource. If a resource exists without the `managed_by=crabbox` tag,
   Crabbox refuses to mutate it and returns an adopt-or-rename error. If a
   resource exists with the tag, it is left alone (Crabbox does not
   overwrite tags, address spaces, subnets, or rules on subsequent
   acquires). If a resource is missing, it is created with Crabbox tags
   and the configured layout. Inbound SSH rules are derived from
   `azure.sshCIDRs`, the configured SSH port, and any fallback ports.
3. Mint a per-lease SSH key.
4. Pick the configured class SKU candidates and try each in order.
5. For each lease: create a public IP, NIC, and VM with cloud-init in
   `osProfile.customData` and the SSH key in
   `osProfile.linuxConfiguration.ssh.publicKeys` for Linux. Native Windows
   uses a Windows Server small-disk Gen2 image, Windows `osProfile` fields
   (`adminPassword`, `computerName`, and `windowsConfiguration`), and a
   Custom Script Extension that runs the Crabbox bootstrap saved in
   `C:\AzureData\CustomData.bin`.
6. Query Azure Resource SKUs for the VM size. If Azure reports ephemeral OS
   disk support, use a local ephemeral OS disk. Otherwise use a managed
   `StandardSSD_LRS` OS disk.
7. Tag the VM, NIC, and public IP with Crabbox lease metadata.
8. Wait for the public IP to allocate, then for SSH and `crabbox-ready`.
9. Let core sync and run over SSH.
10. On release/cleanup, cascade-delete VM → NIC → public IP → OS disk. The
   shared infra remains.

## Classes

Default Linux SKUs:

```text
standard  Standard_D32ads_v6, Standard_D32ds_v6, Standard_F32s_v2, Standard_D32ads_v5, Standard_D32ds_v5, then D/F 16-vCPU fallbacks
fast      Standard_D64ads_v6, Standard_D64ds_v6, Standard_F64s_v2, Standard_D64ads_v5, Standard_D64ds_v5, then D/F 48-vCPU and 32-vCPU fallbacks
large     Standard_D96ads_v6, Standard_D96ds_v6, Standard_D96ads_v5, Standard_D96ds_v5, then D/F 64-vCPU and 48-vCPU fallbacks
beast     Standard_D192ds_v6, Standard_D128ds_v6, then D/F 96-vCPU and 64-vCPU fallbacks
```

Default native Windows SKUs:

```text
standard  Standard_D2ads_v6, Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, then Standard_D2as_v6
fast      Standard_D4ads_v6, Standard_D4ds_v6, Standard_D4ads_v5, Standard_D4ds_v5, then Standard_D4as_v6
large     Standard_D8ads_v6, Standard_D8ds_v6, Standard_D8ads_v5, Standard_D8ds_v5, then Standard_D8as_v6
beast     Standard_D16ads_v6, Standard_D16ds_v6, Standard_D16ads_v5, Standard_D16ds_v5, then Standard_D8ads_v6
```

Class-based provisioning falls back across the candidate list when Azure
rejects a SKU for capacity or quota
(`SkuNotAvailable`, `QuotaExceeded`, `AllocationFailed`,
`OverconstrainedAllocationRequest`). Spot leases fall back to on-demand when
`capacity.fallback` starts with `on-demand`. Explicit `--type` is exact.
The default Linux candidates mirror the AWS Linux class table's vCPU scale.
The default Windows candidates mirror the AWS native Windows class table's
vCPU scale. Azure native Windows support covers SSH, sync, and run; Windows
WSL2 and macOS remain AWS or static-SSH targets.

## Capabilities

- SSH: yes.
- Crabbox sync: yes.
- Native Windows: yes for SSH, sync, and run.
- Desktop / browser / code: Linux only on Azure.
- Tailscale: Linux managed leases.
- Actions hydration: yes, Linux SSH leases.
- Coordinator: yes, brokered Linux/native Windows leases.

## Gotchas

- Azure VM names are constrained to 1-64 characters and cannot contain
  underscores. The `leaseProviderName` helper substitutes underscores
  for dashes; if you customize naming, keep that constraint in mind.
- Windows computer names are limited to 15 characters. Crabbox keeps the VM
  resource name stable and derives a shorter Windows `computerName`.
- The first acquire in an empty subscription pays the cost of creating the
  shared resource group, vnet, and NSG. Subsequent acquires only create
  per-lease resources.
- If you already have a resource group / vnet / NSG with the configured
  names, Crabbox will refuse to mutate them unless they carry
  `managed_by=crabbox` as a tag. Either tag them to adopt, choose
  different names in `azure.*` config, or let Crabbox create dedicated
  resources.
- `crabbox stop --provider azure <name>` will only act on VMs that carry
  `crabbox=true` (and either no `provider` tag or `provider=azure`). A
  manually-named VM in the resource group will not be deleted by Crabbox.
- The default SSH NSG rule allows `0.0.0.0/0` when `azure.sshCIDRs` is
  empty. Set explicit CIDRs for any production-adjacent setup.
- Azure costs are not hardcoded in Crabbox. Set `CRABBOX_COST_RATES_JSON`
  when you need exact Azure cost guardrails.
- Azure native Windows uses Custom Script Extension because Windows custom
  data is saved to disk but not executed by Azure provisioning. Do not add
  rebooting bootstrap work to that extension path.
- Azure does not provide managed Windows WSL2 or macOS through this provider.
  Use AWS or `provider: ssh` for those targets.
- Direct-mode cleanup is best effort. Use `crabbox cleanup --provider azure`
  to sweep expired direct leases.

Related docs:

- [Feature: Azure](../features/azure.md)
- [Linux VNC](../features/vnc-linux.md)
- [Provider backends](../provider-backends.md)
