# Azure

Read when:

- choosing Azure as the Crabbox provider;
- debugging Azure VM capacity, quotas, images, or SSH readiness;
- changing Azure provisioning code in the CLI.

Azure is a managed provider for Linux and native Windows SSH leases. It
creates VMs in a shared resource group, tags them with Crabbox lease
metadata, and bootstraps the normal SSH/sync contract through cloud-init
on Linux or Custom Script Extension on Windows. It works in direct mode with
local Azure auth and in brokered mode through Worker-owned service principal
secrets.

## Targets

| Target | Managed | Notes |
| --- | --- | --- |
| Linux | Yes | Cloud-init bootstrap, SSH, rsync, optional desktop/browser/code. |
| Windows | Yes | Native Windows SSH/sync/run only. No Azure desktop/browser/WSL2. |
| macOS | No | Azure does not offer managed macOS; use AWS EC2 Mac or static SSH. |

Examples:

```sh
crabbox warmup --provider azure --class beast
crabbox run --provider azure --class standard -- pnpm test
crabbox warmup --provider azure --target windows --class standard
crabbox warmup --provider azure --desktop --browser
crabbox vnc --id blue-lobster --open
```

## Classes

```text
standard  Standard_D32ads_v6, Standard_D32ds_v6, Standard_F32s_v2, then D/F 16-vCPU fallbacks
fast      Standard_D64ads_v6, Standard_D64ds_v6, Standard_F64s_v2, then D/F 48-vCPU and 32-vCPU fallbacks
large     Standard_D96ads_v6, Standard_D96ds_v6, then D/F 64-vCPU and 48-vCPU fallbacks
beast     Standard_D192ds_v6, Standard_D128ds_v6, then D/F 96-vCPU and 64-vCPU fallbacks
```

Native Windows uses the smaller AWS Windows class scale:

```text
standard  Standard_D2ads_v6, Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, then Standard_D2as_v6
fast      Standard_D4ads_v6, Standard_D4ds_v6, Standard_D4ads_v5, Standard_D4ds_v5, then Standard_D4as_v6
large     Standard_D8ads_v6, Standard_D8ds_v6, Standard_D8ads_v5, Standard_D8ds_v5, then Standard_D8as_v6
beast     Standard_D16ads_v6, Standard_D16ds_v6, Standard_D16ads_v5, Standard_D16ds_v5, then Standard_D8ads_v6
```

Crabbox falls back through the candidate list when Azure rejects a SKU for
capacity or quota. Explicit `--type` is exact and fails clearly when the
SKU cannot be created. Spot leases fall back to on-demand when
`capacity.fallback` starts with `on-demand`.

Default Azure Linux class candidates mirror the vCPU scale of the AWS Linux
class table. Default Azure native Windows candidates mirror the AWS native
Windows class table. Crabbox asks Azure Resource SKUs whether the selected VM
supports ephemeral OS disks; ephemeral-capable sizes use local OS disks,
while exact `--type` requests for non-ephemeral sizes use managed
`StandardSSD_LRS` OS disks.

## Direct Auth And Env

Service principal env vars consumed by `DefaultAzureCredential`:

```text
AZURE_TENANT_ID
AZURE_CLIENT_ID
AZURE_CLIENT_SECRET
AZURE_SUBSCRIPTION_ID
```

Crabbox-specific overrides:

```text
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

The service principal needs the
[Contributor](https://learn.microsoft.com/azure/role-based-access-control/built-in-roles#contributor)
role on the target resource group (or subscription, if you want Crabbox to
create the resource group on first use).

Brokered Azure uses `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`,
`AZURE_CLIENT_SECRET`, and `AZURE_SUBSCRIPTION_ID` on the Worker. Operators
own the shared infra settings through `CRABBOX_AZURE_*`. Lease requests may
override only `azureLocation` and `azureImage`.

## Shared Infra

The first acquire in an empty subscription creates:

- a resource group (default `crabbox-leases`);
- a virtual network and subnet (`10.42.0.0/16` / `10.42.0.0/24`);
- a network security group with SSH rules derived from `azure.sshCIDRs`,
  the configured SSH port, and fallback ports.

These resources are created with `createOrUpdate` and reused across leases.
Per-lease provisioning creates only the public IP, NIC, VM, and OS disk.

Azure pricing is not hardcoded. Use `CRABBOX_COST_RATES_JSON` for exact
Azure cost guardrails.

## Desktop

Azure desktop leases use the standard Linux VNC path: Xvfb, a lightweight
desktop session, x11vnc bound to `127.0.0.1:5900`, and an SSH local tunnel
created by `crabbox vnc`. Azure native Windows currently supports SSH, sync,
and run only. Use AWS for managed Windows desktop or Windows WSL2.

## Cleanup

Direct cleanup is best-effort through Crabbox lease tags. `crabbox cleanup
--provider azure` enumerates VMs in the configured resource group, skips
kept or unexpired leases, and cascade-deletes expired ones. The shared
resource group, vnet, subnet, and NSG are preserved.

Related docs:

- [Providers](providers.md)
- [Linux VNC](vnc-linux.md)
- [cleanup command](../commands/cleanup.md)
