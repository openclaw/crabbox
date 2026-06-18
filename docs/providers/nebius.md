# Nebius Provider

Read this when you are:

- choosing `provider: nebius`;
- validating a direct Nebius Compute lease;
- changing `internal/providers/nebius` or the guarded live smoke.

Nebius is a Linux-only **SSH lease** provider. Crabbox shells out to the local
`nebius` CLI, creates a VM in a configured parent/project and subnet, injects a
per-lease SSH key through cloud-init, labels the VM with Crabbox ownership
metadata, waits for a public IPv4 address and SSH readiness, then uses the
normal Crabbox SSH sync, `run`, `status`, `list`, `stop`, and `cleanup` path.

Crabbox does not store or accept Nebius secrets. Authentication stays in Nebius
CLI profiles or service-account profiles managed by the Nebius CLI.

Nebius is **direct-only** in this release. It does not run through the Crabbox
coordinator, so the local CLI process must have Nebius credentials and direct
cleanup remains the operator's responsibility.

## Prerequisites

- Install the Nebius CLI and authenticate it with the profile you want Crabbox
  to use.
- Confirm the CLI can list instances in the target parent/project:

  ```sh
  nebius compute instance list --parent-id <parent-id> --format json
  ```

- Choose a subnet that can attach dynamic public IPv4 addresses.
- Keep OpenSSH and `rsync` available locally for Crabbox's SSH workflow.
- Use an account with enough VM, disk, network, public IP, and optional GPU
  quota for the configured platform and preset.

Do not pass service-account JSON, OAuth tokens, API keys, or private keys to
Crabbox config or command-line flags. Configure those through Nebius-owned
credential stores and select them with `nebius.profile` or `--nebius-profile`.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes, Crabbox-managed SSH |
| Crabbox sync (rsync over SSH) | Yes |
| Provider-managed sync | No |
| Desktop / browser / code | No |
| Actions hydration | Yes, as a normal Linux SSH lease |
| Coordinator (broker) | No - direct only |
| Tailscale | No in phase 1 |
| Cleanup | Yes, for complete Crabbox-owned Nebius VMs in the configured scope |

Aliases: none.

## Configuration

```yaml
provider: nebius
target: linux
nebius:
  cli: nebius
  profile: ""
  parentId: project-example
  subnetId: subnet-example
  platform: cpu-d3
  preset: 4vcpu-16gb
  imageFamily: ubuntu24.04-driverless
  diskType: network_ssd
  diskSizeGiB: 50
  user: crabbox
  publicIP: dynamic
  securityGroupIds: []
  serviceAccountId: ""
  recoveryPolicy: fail
```

Required for lifecycle operations:

- `nebius.parentId`
- `nebius.subnetId`

Defaults:

- `cli`: `nebius`
- `platform`: `cpu-d3`
- `preset`: `4vcpu-16gb`
- `imageFamily`: `ubuntu24.04-driverless`
- `diskType`: `network_ssd`
- `diskSizeGiB`: `50`
- `user`: `crabbox`
- `publicIP`: `dynamic`
- `recoveryPolicy`: `fail`

Provider flags:

```text
--nebius-cli
--nebius-profile
--nebius-parent-id
--nebius-subnet-id
--nebius-platform
--nebius-preset
--nebius-image-family
--nebius-disk-type
--nebius-disk-size-gib
--nebius-user
--nebius-public-ip
--nebius-security-group-ids
--nebius-service-account-id
--nebius-recovery-policy
```

Environment overrides:

```text
CRABBOX_NEBIUS_CLI
CRABBOX_NEBIUS_PROFILE
CRABBOX_NEBIUS_PARENT_ID
CRABBOX_NEBIUS_SUBNET_ID
CRABBOX_NEBIUS_PLATFORM
CRABBOX_NEBIUS_PRESET
CRABBOX_NEBIUS_IMAGE_FAMILY
CRABBOX_NEBIUS_DISK_TYPE
CRABBOX_NEBIUS_DISK_SIZE_GIB
CRABBOX_NEBIUS_USER
CRABBOX_NEBIUS_PUBLIC_IP
CRABBOX_NEBIUS_SECURITY_GROUP_IDS
CRABBOX_NEBIUS_SERVICE_ACCOUNT_ID
CRABBOX_NEBIUS_RECOVERY_POLICY
```

`--nebius-cli` and `nebius.cli` are trusted-local settings. Project-local
untrusted config cannot redirect the Nebius CLI path. Authentication material is
never accepted as a Crabbox flag or config key.

## Lifecycle

### Doctor

`doctor` is read-only:

```sh
crabbox doctor --provider nebius
crabbox doctor --provider nebius --json
```

It validates the configured CLI surface, parent/project inventory access, target
subnet, image family, and the selected platform or preset without creating or
deleting a VM.

### Acquire

`warmup` and `run` create cost-bearing Nebius VMs:

```sh
crabbox warmup --provider nebius --slug my-app --keep --ttl 20m
crabbox run --provider nebius --no-sync -- echo ok
```

Acquire flow:

1. list instances in `nebius.parentId` to allocate a Crabbox slug;
2. generate a per-lease SSH key in the local Crabbox testbox key store;
3. render cloud-init for `nebius.user`;
4. create a Nebius Compute instance with the configured platform, preset, image
   family, boot disk type, disk size, subnet, optional security groups, optional
   service account, and dynamic public IP by default;
5. wait until the instance is running and exposes a public IPv4 address;
6. wait for SSH readiness;
7. update labels from provisioning to ready;
8. write a local Crabbox lease claim and run the normal SSH workflow.

Crabbox names Nebius instances with its normal `crabbox-<slug>-<lease-suffix>`
pattern and labels them with both generic direct-lease labels and Nebius-specific
scope labels.

### GPU and image selection

The default Nebius profile is CPU-oriented. To use a GPU VM, select a Nebius
platform, preset, image family, and disk size that match the desired accelerator
and account quota:

```sh
crabbox run \
  --provider nebius \
  --nebius-platform gpu-h100 \
  --nebius-preset 1gpu-16vcpu-200gb \
  --nebius-image-family ubuntu22.04-cuda \
  --nebius-disk-size-gib 120 \
  -- nvidia-smi
```

Crabbox does not infer GPU driver readiness beyond the selected Nebius image.
Use a CUDA-ready image family or bootstrap the workload yourself.

### Public IP and Tailscale

`nebius.publicIP` supports `dynamic` and `none` at configuration validation time,
but the phase 1 provider requires a reachable public IPv4 address before it can
complete acquisition. Use `dynamic` for normal Crabbox runs.

Tailscale routing is rejected for `provider=nebius` in this release. The provider
is direct public-SSH only until a private-network path is implemented and live
proven.

### Release and cleanup

Stop deletes the owned Nebius VM and removes local lease state:

```sh
crabbox stop --provider nebius my-app
```

`--id` accepts the canonical lease id, friendly slug, Nebius instance id, or
Nebius instance name when it resolves to a complete Crabbox-owned VM in the
configured scope.

Cleanup only mutates VMs with a complete Crabbox Nebius ownership predicate:

```sh
crabbox cleanup --provider nebius --dry-run
crabbox cleanup --provider nebius
```

The ownership predicate includes the Crabbox marker, provider, target, lease,
slug, expiration, normalized Nebius parent/project id, optional profile, and a
scope hash derived from provider, profile, parent/project, and subnet. Foreign,
partial, malformed, or differently scoped Crabbox-looking VMs are skipped or
refused rather than deleted.

If create returns an indeterminate Nebius CLI error, Crabbox retains a recovery
claim when possible. Retry `crabbox stop --provider nebius <lease-or-slug>` after
checking Nebius inventory rather than deleting same-named resources manually.

## Examples

Run a short CPU command and delete the VM afterward:

```sh
crabbox run --provider nebius --no-sync -- echo ok
```

Warm a reusable VM, run a command, then release it:

```sh
crabbox warmup --provider nebius --slug my-app --keep --ttl 30m
crabbox run --provider nebius --id my-app --no-sync -- uname -a
crabbox status --provider nebius --id my-app --wait
crabbox stop --provider nebius my-app
```

Use a named Nebius profile and explicit network scope:

```sh
crabbox run \
  --provider nebius \
  --nebius-profile dev \
  --nebius-parent-id project-example \
  --nebius-subnet-id subnet-example \
  -- echo ok
```

## Guarded live smoke

The repeatable live check is opt-in because it creates a billable Nebius VM:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nebius scripts/live-nebius-smoke.sh
```

The script builds `bin/crabbox` when needed, checks `doctor`, creates a small
CPU-default lease with a unique `nebius-smoke-*` slug, waits for ready status,
runs `echo ok`, verifies `list --json`, stops the lease, runs dry-run cleanup,
and verifies the slug is absent from the final list output.

Final classifications include:

```text
classification=live_nebius_smoke_passed
classification=environment_blocked
classification=quota_blocked
classification=validation_failed
```

The script exits without live operations unless both `CRABBOX_LIVE=1` and
`CRABBOX_LIVE_PROVIDERS=nebius` are present. Missing credentials, CLI profile,
parent/project, subnet, or local tools are reported as `environment_blocked`.
Quota, capacity, rate-limit, or limit errors are reported as `quota_blocked`.

If cleanup fails, use the reported slug with:

```sh
crabbox list --provider nebius --json
crabbox stop --provider nebius <slug>
crabbox cleanup --provider nebius --dry-run
```

## Gotchas

- Nebius is direct-only. Coordinator secrets and broker-side cost controls do
  not cover these VMs.
- Dynamic public IP is the practical phase 1 mode. `publicIP: none` prevents the
  provider from completing SSH acquisition.
- `parentId`, `subnetId`, and optional `profile` are part of cleanup scope.
  Switching them can make existing local claims unresolvable until the original
  scope is restored.
- `securityGroupIds` must allow inbound SSH from the operator network.
- `recoveryPolicy` currently supports `fail` only.
- The generated provider matrix is updated from the live CLI provider spec plus
  `docs/providers/provider-metadata.json`; do not hand-edit the generated table.

## Related Docs

- [Provider reference](README.md)
- [Provider feature guide](../features/providers.md)
- [Operations guide](../operations.md)
