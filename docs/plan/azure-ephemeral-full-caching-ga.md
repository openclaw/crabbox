# Azure Ephemeral OS Disk Full Caching GA

Research and validation date: 2026-09-25. These notes record the Azure contract,
implementation changes, and live validation for this update.

## Verified Azure contract

Microsoft announces general availability on September 9, 2026. Full caching
supports all Azure public regions. Azure copies the base image after boot.
After that copy completes, local storage serves all OS disk I/O.
VM creation does not wait for that copy.
[Source: GA announcement](https://techcommunity.microsoft.com/blog/azurecompute/announcing-general-availability-of-ephemeral-os-disk-with-full-caching-for-vmvms/4554797).

The VM must have at least eight vCPUs. Azure lists these supported families:

| Family | Supported generations |
| --- | --- |
| N, L, M, H | All |
| D, DC, E, Eb, EC | v5, v6, v7 |
| F | v6, v7 |

The selected SKU must also support ephemeral OS disks and provide enough local
storage. The documented capacity requirement exceeds `2 × OS disk size + 1 GiB`.
Two-vCPU and four-vCPU support remains a future feature.
[Source: full caching prerequisites](https://learn.microsoft.com/en-us/azure/virtual-machines/ephemeral-os-disks#prerequisites-for-full-caching).

The deployment guide specifies Compute API `2025-04-01` or later. Its VM
example uses this OS disk object under `properties.storageProfile`:

```json
{
  "diffDiskSettings": {
    "option": "Local",
    "placement": "NvmeDisk",
    "enableFullCaching": true
  },
  "caching": "ReadOnly",
  "createOption": "FromImage",
  "managedDisk": {
    "storageAccountType": "StandardSSD_LRS"
  }
}
```

VMSS uses the same object under
`properties.virtualMachineProfile.storageProfile`. Azure CLI exposes
`--ephemeral-os-disk-enable-full-caching true`.
[Source: deployment guide](https://learn.microsoft.com/en-us/azure/virtual-machines/ephemeral-os-disks-deploy#vm-template-deployment-with-full-caching).

Placement is optional. Azure selects `CacheDisk` when present, otherwise
`ResourceDisk` or `NvmeDisk`. An explicit placement must match the SKU.
Crabbox can retain Azure's placement default.
[Source: DiffDiskSettings contract](https://learn.microsoft.com/en-us/javascript/api/@azure/arm-compute/diffdisksettings?view=azure-node-latest).

The GA deployment guide lists no feature-registration prerequisite for
VM/VMSS. This supports direct deployment without an added registration step.
AKS has a separate preview flag, `Microsoft.ContainerService/FullCachePreview`.
That AKS requirement does not govern Crabbox's Compute VM requests.
[Sources: VM deployment guide](https://learn.microsoft.com/en-us/azure/virtual-machines/ephemeral-os-disks-deploy),
[AKS preview prerequisites](https://learn.microsoft.com/en-us/azure/aks/full-cache-ephemeral-os-disk#prerequisites).

Full caching does not add persistence. Ephemeral disks still exclude disk
snapshots, VM image capture, and stop/deallocate support. Standard SSD and
Premium SSD base disks incur their applicable disk charges.
[Source: ephemeral disk limitations and SSD support](https://learn.microsoft.com/en-us/azure/virtual-machines/ephemeral-os-disks).

## Differences from preview

The preview announcement describes 29 supported regions and excludes two-vCPU
and four-vCPU sizes. It uses the same `enableFullCaching` request property and
`2025-04-01` API version. GA expands regional availability and publishes the
specific family list above. No replacement request property is documented.
[Sources: preview announcement](https://techcommunity.microsoft.com/blog/azurecompute/public-preview-ephemeral-os-disk-with-full-caching-for-vmvmss/4500191),
[GA announcement](https://techcommunity.microsoft.com/blog/azurecompute/announcing-general-availability-of-ephemeral-os-disk-with-full-caching-for-vmvms/4554797).

## API documentation discrepancy

The generated `2025-04-01` REST and ARM references omit `enableFullCaching`.
The official specification introduces it with `@added(Versions.v2025_11_01)`.
The Azure CLI feature request also states `2025-11-01` as its minimum API.
These sources disagree with the GA deployment guide's `2025-04-01` minimum.
[Sources: specification change](https://github.com/Azure/azure-rest-api-specs/pull/37861/files),
[Azure CLI feature request](https://github.com/Azure/azure-cli/issues/32351),
[2025-04-01 REST reference](https://learn.microsoft.com/en-us/rest/api/compute/virtual-machines/create-or-update?view=rest-compute-2025-04-01).

Use a raw ARM readback to verify `enableFullCaching: true` during live proof.
A successful boot alone does not prove full caching. The reviewed sources
expose no field that proves completion of the background cache copy.

Crabbox already pins Go `armcompute/v8` version `v8.3.0`. That version exposes
`DiffDiskSettings.EnableFullCaching` and serializes the field. Its native VM
create client uses Compute API `2026-04-01`. The Go implementation can use
the native SDK field and remove its old JSON injection workaround.
No dependency update is necessary. Context7 returns no matching documentation
for this field; the installed SDK source confirms the contract.
[Sources: pinned SDK model](https://github.com/Azure/azure-sdk-for-go/blob/sdk/resourcemanager/compute/armcompute/v8.3.0/sdk/resourcemanager/compute/armcompute/models.go#L1326),
[SDK request serialization](https://github.com/Azure/azure-sdk-for-go/blob/sdk/resourcemanager/compute/armcompute/v8.3.0/sdk/resourcemanager/compute/armcompute/models_serde.go#L3123),
[SDK VM create client](https://github.com/Azure/azure-sdk-for-go/blob/sdk/resourcemanager/compute/armcompute/v8.3.0/sdk/resourcemanager/compute/armcompute/virtualmachines_client.go#L410).

## Crabbox update

The original preview support in [the Azure full-caching PR](https://github.com/openclaw/crabbox/pull/186)
introduces `ephemeral-preview` and a raw ARM request because the SDK lacks the field.
This update replaces that path with the GA mode and the current SDK field.

`ephemeral` now selects GA full caching. The user selects one public mode.
`ephemeral-preview` is removed and returns an error that names `ephemeral` as
its replacement. `managed` remains the default; `auto` still selects managed.
The native Go SDK replaces the preview JSON and polling workaround.
The coordinator uses the same Compute API version, `2026-04-01`.

GA family/vCPU eligibility is separate from the conservative fallback heuristic.
Their intersection filters existing default candidate lists. After live
`EphemeralOSDiskSupported` validation succeeds, only the family/vCPU policy
applies. Azure validates image size and local capacity.

Direct fixed-ID creates use the shared SDK path with `If-None-Match: *`.
Ephemeral disks still cannot support native snapshot checkpoints.

Regression tests cover mode removal, native request fields, managed defaults,
Fsv2 rejection, small-size rejection, and live-supported sizes outside static
fallbacks.


## Live validation evidence

Validation uses the changed source on base commit `e4ca5858`, before PR creation.
A disposable Azure Linux VM runs Ubuntu 26.04 in `eastus` with on-demand capacity.
The documented `scripts/live-smoke.sh` completes provision, readiness, status,
inspect, SSH, cache inventory, file sync, command execution, and stop.

The following projection comes from the raw ARM `2026-04-01` readback.
It preserves disk settings and omits account, resource, lease, and VM identities.

```json
{
  "size": "Standard_D8ads_v6",
  "state": "Succeeded",
  "osDisk": {
    "osType": "Linux",
    "diskSizeGB": 30,
    "createOption": "FromImage",
    "caching": "ReadOnly",
    "diffDiskSettings": {
      "enableFullCaching": true,
      "option": "Local",
      "placement": "NvmeDisk"
    },
    "managedDisk": {
      "storageAccountType": "StandardSSD_LRS"
    }
  }
}
```

Selected terminal output from the live smoke:

```text
warmup complete total=1m33.848s
azure-full-caching-e2e
command complete in 2.27s total=8.699s
```

The command exits zero. A fixed-ID warmup replay also preserves one VM.
These comparison results derive from the saved before/after ARM responses and
resource-group absence check. They contain no original identity values.

```json
{
  "vmCountBeforeReplay": 1,
  "vmCountAfterReplay": 1,
  "sameResourceId": true,
  "sameImmutableVmId": true,
  "resourceGroupExistsAfterCleanup": false
}
```

Stop reports SSH exit-255 warnings for guest-side hydration and egress cleanup.
Azure deletion succeeds. The final resource-group readback confirms absence,
and the task's lease SSH keys and isolated credential cache are removed.

This proves Azure accepts full caching and the lease executes commands.
It does not measure completion of Azure's background cache copy or disk performance.
Live coverage is direct Linux x64. Coordinator provisioning uses HTTP fixtures;
Windows and ARM64 are not live-tested in this update.
Native Linux checkpoint attempts require a coordinator, so the direct live test
cannot prove ephemeral-specific snapshot rejection. Regression tests cover it.

## Local validation evidence

| Check | Result |
| --- | --- |
| Azure Go tests and built-CLI help contract with `-race` | Pass |
| `go vet ./...` and CLI build | Pass |
| `TestLocalContainerProviderE2E`, with the built CLI | Pass, 79.884 seconds |
| Full coordinator suite | 3,415 pass, 15 skip in two optional files |
| Worker and Node typechecks and builds | Pass |
| Worker formatting and lint | Pass, with pre-existing generated output excluded |
| Documentation checks and site build | Pass |
| Full Go race suite | 101 packages pass; CLI package times out after 20 minutes |
| Repository script suite on macOS | 1,450 pass, six skip, one failure |
| Failed script suite recheck in `node:24-bookworm` | All 44 tests pass |

The Docker E2E covers reusable SSH, stale claims, and four concurrent built-CLI
warmups. Commands execute, containers and keys disappear after stop, and released
fixed-ID tombstones remain. The first invocation omits `CRABBOX_BIN`; the complete
rerun uses the absolute path to the built CLI and passes.

The full Go run reports closed-localhost-SSH failures in Machine0 checkpoint,
brokered AWS, and static SSH tests before the CLI package reaches its timeout.
`TestRunCommandInjectsReservedMetadataIntoStaticSSH` reproduces the same failure
on unchanged upstream `e4ca5858` in a clean detached worktree.
The run also catches the changed help checksum; its corrected contract passes.
The full Go gate is not a pass.

The macOS script failure is in `image-publisher-auth-check.test.mjs` under
Bash 3.2. Its unchanged 44-test suite passes in the Linux container.

Reproduction commands for the focused race test and local Docker E2E:

```sh
GOTOOLCHAIN=go1.26.5 go test -race -timeout=3m ./internal/cli ./internal/providers/azure \
  -run 'Azure|azure|^TestProvidersDescribeBuiltBinaryContract$' -count=1
CRABBOX_BIN="$PWD/bin/crabbox" GOTOOLCHAIN=go1.26.5 go test -tags localcontainer \
  -run '^TestLocalContainerProviderE2E$' -count=1 -v -timeout=20m ./cmd/crabbox
```

Public evidence omits subscription and tenant IDs, account names, public IPs,
resource names, lease/run IDs, local paths, and credential or SSH material.
Raw logs remain local. No coordinator deployment or release is part of this proof.
