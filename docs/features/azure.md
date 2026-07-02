# Azure

Read this when:

- choosing Azure as the Crabbox provider;
- debugging Azure VM capacity, quotas, images, or SSH readiness;
- changing the Azure provisioning code in the CLI.

Azure is a brokered provider for Linux, native Windows, and Windows WSL2 leases.
It creates VMs in a shared resource group, tags them with Crabbox lease
metadata, and bootstraps the standard SSH/sync contract through cloud-init on
Linux or a Custom Script Extension on Windows. Native Windows can also run the
shared desktop/VNC bootstrap once SSH is reachable; WSL2 runs the shared Windows
WSL2 bootstrap and then uses the POSIX sync/run contract through WSL.

Azure works in **direct mode** with local Azure credentials and in **brokered
mode** through Worker-owned service principal secrets. Prefer Azure for Windows
and Windows WSL2 when your subscription has credits or available quota. It
follows the same ordered region fallback as AWS for brokered leases, while
keeping Windows classes smaller than the large Linux-focused classes.

This page covers the `azure` VM backend. Azure Container Apps dynamic sessions
are a separate, delegated-run backend selected with `--azure-backend
dynamic-sessions`; see the [providers overview](providers.md).

## Targets

| Target | Brokered | Notes |
| --- | --- | --- |
| Linux | Yes | Cloud-init bootstrap, SSH, rsync, optional desktop/browser/code. |
| Windows native | Yes | Native Windows SSH/sync/run and optional desktop/VNC. No browser/code. |
| Windows WSL2 | Yes | Nested-virtualization VM sizes; POSIX sync/run/actions hydration through WSL. |
| macOS | No | Azure offers no managed macOS; use AWS EC2 Mac or a static SSH host. |

```sh
crabbox warmup --provider azure --class beast
crabbox warmup --provider azure --arch arm64 --class fast
crabbox warmup --provider azure --class beast --azure-os-disk ephemeral
crabbox warmup --provider azure --class beast --azure-os-disk ephemeral-preview
crabbox run --provider azure --class standard -- pnpm test
crabbox warmup --provider azure --target windows --class standard
crabbox warmup --provider azure --target windows --desktop --class standard
crabbox warmup --provider azure --target windows --windows-mode wsl2 --class standard
crabbox warmup --provider azure --desktop --browser
crabbox vnc --id blue-lobster --open
```

## Classes And Capacity

Each class maps to an ordered list of VM sizes. Crabbox falls back through the
list when Azure rejects a SKU for capacity or quota. An explicit `--type` is
exact and fails clearly when the SKU cannot be created.

Linux classes:

```text
standard  Standard_D32ads_v6, Standard_D32ds_v6, Standard_F32s_v2, then D/F 16-vCPU fallbacks
fast      Standard_D64ads_v6, Standard_D64ds_v6, Standard_F64s_v2, then D/F 48- and 32-vCPU fallbacks
large     Standard_D96ads_v6, Standard_D96ds_v6, then D/F 64- and 48-vCPU fallbacks
beast     Standard_D192ds_v6, Standard_D128ds_v6, then D/F 96- and 64-vCPU fallbacks
```

ARM64 classes use Azure Cobalt Dpsv6/Dpdsv6 candidates. Linux ARM64 also uses
ARM64 Ubuntu Marketplace images. Windows ARM64 is native Windows only because
Azure Cobalt ARM64 sizes do not support the nested virtualization WSL2 needs;
it requires `azure.image` / `CRABBOX_AZURE_IMAGE` on the request, or
`CRABBOX_AZURE_WINDOWS_ARM64_IMAGE` on the coordinator, to name an ARM64 Windows
Marketplace or custom image because the built-in Windows default is x64:

```text
standard  Standard_D32pds_v6, Standard_D32ps_v6, then 16-vCPU fallbacks
fast      Standard_D64pds_v6, Standard_D64ps_v6, then 48/32-vCPU fallbacks
large     Standard_D96pds_v6, Standard_D96ps_v6, then 64/48-vCPU fallbacks
beast     Standard_D96pds_v6, Standard_D96ps_v6, then 64-vCPU fallbacks
```

Native Windows and WSL2 amd64 use a smaller scale, and the default candidates
support the nested virtualization WSL2 needs:

```text
standard  Standard_D2ads_v6, Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, then Standard_D2as_v6
fast      Standard_D4ads_v6, Standard_D4ds_v6, Standard_D4ads_v5, Standard_D4ds_v5, then Standard_D4as_v6
large     Standard_D8ads_v6, Standard_D8ds_v6, Standard_D8ads_v5, Standard_D8ds_v5, then Standard_D8as_v6
beast     Standard_D16ads_v6, Standard_D16ds_v6, Standard_D16ads_v5, Standard_D16ds_v5, then Standard_D8ads_v6
```

**Spot:** with `--market spot`, Azure Spot VMs use eviction policy `Delete` and
`billingProfile.maxPrice: -1`, so price alone never evicts a lease while Azure
still charges no more than the on-demand price. When `capacity.fallback` starts
with `on-demand`, a spot rejection retries the same SKU on-demand.

**Region fallback:** set `capacity.regions` (flag `--regions`, env
`CRABBOX_CAPACITY_REGIONS`) for direct mode, or `CRABBOX_AZURE_REGIONS` on the
Worker for brokered mode, to try additional regions after a capacity rejection.
Single-region leases keep the shared network names unchanged; for multi-region
fallback Crabbox appends the region to the managed vnet and NSG names so one
resource group can hold independent regional networks.

Azure pricing is not hardcoded. Use `CRABBOX_COST_RATES_JSON` on the coordinator for
exact Azure cost guardrails.

## OS Disk Mode

Azure leases use managed `StandardSSD_LRS` OS disks by default (config
`azure.osDisk: managed`), so native checkpoint/fork works without picking a SKU
by hand. Use `azure.osDisk: ephemeral` or `--azure-os-disk ephemeral` only for
stateless leases that should use a local OS disk; provisioning fails if the
selected SKU has no ephemeral OS support, and native Azure checkpoint/fork is
unavailable.

Managed Azure Windows leases using `windows.mode=normal` can also create direct
OS-disk checkpoints for fast prepared-desktop reuse. Snapshot creation requires
`crabbox checkpoint create --strategy disk-snapshot --no-reboot=false`; Crabbox
restarts the source VM after the snapshot and rehydrates every fork with fresh
SSH, Windows, and loopback-only VNC credentials. A per-fork deny-all network
security group keeps the copied VM unreachable until credential rotation
finishes, then Crabbox attaches the normal shared SSH allowlist.

`azure.osDisk: ephemeral-preview` opts into Azure's public-preview
full-caching mode for ephemeral OS disks. Crabbox sends Compute API
`2025-04-01` with `diffDiskSettings.enableFullCaching: true`; for known
Crabbox Azure fallback lists it skips 2-core, 4-core, and no-local-disk SKUs
that the preview cannot support. `azure.osDisk: auto` is accepted for
compatibility and resolves to managed.

## Quick Start With `az login`

The simplest setup uses the Azure CLI — no environment variables required:

```sh
az login
crabbox azure login
crabbox warmup --provider azure
```

`crabbox azure login` detects the active subscription from the `az` CLI,
validates credentials through `DefaultAzureCredential`, and stores the
subscription, tenant, and location in user config. If you skip it, a direct
Azure command falls back to `az account show` at runtime to detect the
subscription and tenant; a location is still required. See the
[azure command docs](../commands/azure.md) for flags and details.

## Direct Auth And Env

At provision time Crabbox authenticates with `DefaultAzureCredential`, or with a
client-secret credential when `azure.tenantId`, `azure.clientId`, and the
`AZURE_CLIENT_SECRET` environment variable are all present.

Service principal variables consumed by `DefaultAzureCredential`:

```text
AZURE_TENANT_ID
AZURE_CLIENT_ID
AZURE_CLIENT_SECRET
AZURE_SUBSCRIPTION_ID
```

Crabbox-specific overrides (take precedence over the `AZURE_*` forms):

```text
CRABBOX_AZURE_SUBSCRIPTION_ID
CRABBOX_AZURE_TENANT_ID
CRABBOX_AZURE_CLIENT_ID
CRABBOX_AZURE_LOCATION
CRABBOX_AZURE_RESOURCE_GROUP
CRABBOX_AZURE_IMAGE
CRABBOX_AZURE_WINDOWS_ARM64_IMAGE
CRABBOX_AZURE_OS_DISK
CRABBOX_AZURE_VNET
CRABBOX_AZURE_SUBNET
CRABBOX_AZURE_NSG
CRABBOX_AZURE_SSH_CIDRS
CRABBOX_AZURE_NETWORK
CRABBOX_AZURE_BACKEND
```

The service principal needs the
[Contributor](https://learn.microsoft.com/azure/role-based-access-control/built-in-roles#contributor)
role on the target resource group (or on the subscription, if you want Crabbox
to create the resource group on first use).

Brokered Azure uses `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`,
and `AZURE_SUBSCRIPTION_ID` on the coordinator. Operators own the shared infra
through `CRABBOX_AZURE_*`. Lease requests may override only `azureLocation`,
`azureImage`, and `azureOSDisk`. Use `CRABBOX_AZURE_REGIONS` for brokered Azure
region fallback (`CRABBOX_CAPACITY_REGIONS` stays AWS-specific).

## Shared Infra

The first acquire in an empty subscription creates:

- a resource group (default `crabbox-leases`);
- a virtual network (`crabbox-vnet`, `10.42.0.0/16`) and subnet
  (`crabbox-subnet`, `10.42.0.0/24`);
- a network security group (`crabbox-nsg`) with SSH rules derived from
  `azure.sshCIDRs`, the configured SSH port, and fallback ports.

These resources are created with `createOrUpdate` and reused across leases;
per-lease provisioning creates only the public IP, NIC, VM, and OS disk. The
vnet, subnet, and NSG are regional. When reusing an existing shared resource
group, set `azure.location` / `CRABBOX_AZURE_LOCATION` to the same region as the
existing vnet and NSG, or pick distinct `azure.vnet`, `azure.subnet`, and
`azure.nsg` names for a new region.

The default location is `eastus`. The default Linux image is
`Canonical:ubuntu-26_04-lts:server:latest`; native Windows defaults to
`MicrosoftWindowsServer:windowsserver2022:2022-datacenter-smalldisk-g2:latest`.
With `architecture: arm64` or `--arch arm64`, Linux defaults switch to
`Canonical:ubuntu-26_04-lts:server-arm64:latest` or the matching
`ubuntu-24_04-lts:server-arm64` image when `--os ubuntu:24.04` is set. Windows
ARM64 uses ARM64 VM sizes with the selected Windows Marketplace or custom image.
Set `azure.image` / `CRABBOX_AZURE_IMAGE` as a `Publisher:Offer:SKU:Version`
reference to override. In brokered mode, use
`CRABBOX_AZURE_WINDOWS_ARM64_IMAGE` on the coordinator to provide the default ARM64
Windows image without changing the global Azure image fallback for Linux or
AMD64 Windows leases.

## VPN / Private Network

When you reach the Azure virtual network over a VPN, set `azure.network:
private` in config or `CRABBOX_AZURE_NETWORK=private` in the environment. Crabbox
then uses the VM NIC's private IP (e.g. `10.42.0.4`) instead of the public IP for
SSH connectivity.

```yaml
azure:
  network: private
```

```sh
export CRABBOX_AZURE_NETWORK=private
crabbox warmup --provider azure
```

When `network` is `private` but the NIC has no private IP yet, Crabbox falls
back to the public IP. The default is `public`.

## Desktop

Azure Linux desktop leases use the standard VNC path: resize-capable TigerVNC,
a lightweight XFCE session, VNC bound to `127.0.0.1:5900`, and an SSH local
tunnel created by `crabbox vnc`. Azure native Windows desktop leases use the shared
managed Windows bootstrap to install TightVNC, create the local `crabbox`
administrator, enable auto-logon, and expose VNC only through an SSH tunnel.
The OpenSSH, Git for Windows, and TightVNC downloads are pinned and SHA-256
verified before extraction or execution.
Azure WSL2 leases enable WSL, VirtualMachinePlatform, and HypervisorPlatform,
update the WSL kernel, verify and import a versioned Ubuntu rootfs, and prepare the Linux-side
`crabbox-ready` toolchain. Azure Windows does not provision browser/code
targets.

## Cleanup

Direct cleanup is best-effort through Crabbox lease tags. `crabbox cleanup
--provider azure` enumerates VMs in the configured resource group, skips kept or
unexpired leases, and cascade-deletes expired ones. The shared resource group,
vnet, subnet, and NSG are preserved.

## Related docs

- [Providers](providers.md)
- [Capacity fallback](capacity-fallback.md)
- [Linux VNC](vnc-linux.md)
- [Windows VNC](vnc-windows.md)
- [azure command](../commands/azure.md)
- [cleanup command](../commands/cleanup.md)
