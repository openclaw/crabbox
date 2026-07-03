# Vast Provider

Read this when you are:

- choosing `provider: vast`;
- validating a direct Vast.ai SSH lease;
- changing `internal/providers/vast` or the guarded live smoke.

Vast is a Linux-only **SSH lease** provider for Vast.ai GPU instances. Crabbox
searches Vast offers, creates one `ssh_direct` instance from the selected offer,
injects a per-lease SSH key, marks the instance with a compact Crabbox ownership
label, waits for the direct SSH endpoint, and then uses the normal Crabbox SSH
sync/run/status/list/stop/cleanup path.

Vast is **direct-only** in this release. It does not run through the Crabbox
coordinator, so the local CLI must have a Vast API key and direct cleanup
remains the operator's responsibility. Vast instances are billable while they
are running. The default release action destroys the instance.

## When To Use It

Use Vast when you need a direct Linux GPU lease and local Vast credentials are
acceptable. Prefer AWS, Azure, GCP, or Hetzner when you need brokered team
credentials, coordinator-side cost accounting, or non-GPU cloud VM coverage.
Prefer Lambda, Nebius, RunPod, or NVIDIA Brev when those provider catalogs,
images, or account policies are a better fit for the workload.

## Commands

```sh
crabbox doctor --provider vast
crabbox warmup --provider vast --vast-gpu-name "RTX 4090" --keep
crabbox run --provider vast --vast-gpu-count 1 --no-sync -- nvidia-smi
crabbox ssh --provider vast --id my-app
crabbox stop --provider vast my-app
crabbox cleanup --provider vast --dry-run
```

Aliases: `vast-ai`, `vastai`.

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the
Vast instance id when it resolves to a complete Crabbox-owned Vast instance.
`--class` and `--type` are not supported for `provider=vast`; use the
Vast-specific GPU, image, and offer-selection flags instead.

## Configuration

```yaml
provider: vast
target: linux
vast:
  apiUrl: https://console.vast.ai/api/v0
  instanceType: ondemand
  gpuName: ""
  gpuCount: 0
  image: nvidia/cuda:12.8.1-cudnn-devel-ubuntu22.04
  templateId: ""
  runtype: ssh_direct
  diskGB: 20
  maxDphTotal: 0
  minReliability: 0
  order: dlperf_per_dphtotal desc
  user: root
  workRoot: /work/crabbox
  releaseAction: destroy
```

Config keys under `vast:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `apiUrl` | `cfg.Vast.APIURL` | `https://console.vast.ai/api/v0` | Absolute Vast REST API URL without credentials, query strings, or fragments. HTTPS is required except for localhost test endpoints. |
| `instanceType` | `cfg.Vast.InstanceType` | `ondemand` | Offer type, `ondemand` or `interruptible`; `on-demand` is normalized. |
| `gpuName` | `cfg.Vast.GPUName` | empty | Optional Vast GPU name selector. |
| `gpuCount` | `cfg.Vast.GPUCount` | `0` | Minimum GPU count when greater than zero. |
| `image` | `cfg.Vast.Image` | `nvidia/cuda:12.8.1-cudnn-devel-ubuntu22.04` | Docker image requested from Vast for the instance. |
| `templateId` | `cfg.Vast.TemplateID` | empty | Optional Vast template id. |
| `runtype` | `cfg.Vast.Runtype` | `ssh_direct` | Only `ssh_direct` is supported. |
| `diskGB` | `cfg.Vast.DiskGB` | `20` | Requested disk size in GB. |
| `maxDphTotal` | `cfg.Vast.MaxDphTotal` | `0` | Maximum dollars per hour when greater than zero. |
| `minReliability` | `cfg.Vast.MinReliability` | `0` | Minimum reliability score from 0 to 1 when greater than zero. |
| `order` | `cfg.Vast.Order` | `dlperf_per_dphtotal desc` | Vast offer ordering expression. |
| `user` | `cfg.Vast.User` | `root` | SSH user. Explicit generic `ssh.user` still wins. |
| `workRoot` | `cfg.Vast.WorkRoot` | `/work/crabbox` | Remote work root for Crabbox sync and commands. |
| `releaseAction` | `cfg.Vast.ReleaseAction` | `destroy` | `destroy`/`delete`, `stop`, or `keep`. |

Provider flags:

```text
--vast-api-url
--vast-instance-type
--vast-gpu-name
--vast-gpu-count
--vast-image
--vast-template-id
--vast-runtype
--vast-disk-gb
--vast-max-dph-total
--vast-min-reliability
--vast-order
--vast-user
--vast-work-root
--vast-release-action
```

Environment overrides:

```text
CRABBOX_VAST_API_KEY         Vast API key for direct mode
VAST_API_KEY                 Fallback Vast API key
CRABBOX_VAST_API_URL         Override the API URL
VAST_API_URL                 Fallback API URL override
CRABBOX_VAST_INSTANCE_TYPE   Override `ondemand` or `interruptible`
CRABBOX_VAST_GPU_NAME        Override the GPU name selector
CRABBOX_VAST_GPU_COUNT       Override the minimum GPU count
CRABBOX_VAST_IMAGE           Override the Docker image
CRABBOX_VAST_TEMPLATE_ID     Override the template id
CRABBOX_VAST_RUNTYPE         Override the runtime type; must be `ssh_direct`
CRABBOX_VAST_DISK_GB         Override disk size in GB
CRABBOX_VAST_MAX_DPH_TOTAL   Override maximum dollars per hour
CRABBOX_VAST_MIN_RELIABILITY Override minimum reliability score
CRABBOX_VAST_ORDER           Override offer ordering
CRABBOX_VAST_USER            Override the SSH user
CRABBOX_VAST_WORK_ROOT       Override the remote work root
CRABBOX_VAST_RELEASE_ACTION  Override release action
```

Do not pass the Vast API key as a command-line argument or store it in
repository config. Crabbox reads it from `CRABBOX_VAST_API_KEY` or
`VAST_API_KEY` and sends it only in the `Authorization: Bearer ...` header.

## Token Scope

The provider uses Vast account identity, offer search, instances, instance
state updates, instance destroy, and instance SSH-key attach/detach APIs.
`crabbox doctor --provider vast` is read-only: it checks auth, lists instances,
counts Crabbox-owned Vast instances, and reports the default order, runtime, and
SSH user.

Keep API keys in the environment or a local secret manager. Do not commit Vast
keys, generated private keys, instance API keys, user data, or Jupyter token
URLs.

## Lifecycle

1. Load the Vast API key from `CRABBOX_VAST_API_KEY` or `VAST_API_KEY`.
2. List instances and allocate a Crabbox slug.
3. Generate a per-lease SSH key in the Crabbox testbox key store.
4. Search Vast offers with the configured type, GPU name/count, reliability,
   max dollars per hour, and ordering.
5. Create one `ssh_direct` Vast instance from the selected offer, with the
   configured image, template, disk, user, and Crabbox environment marker.
6. Attach the per-lease public SSH key to the instance.
7. Wait until the instance is running and exposes a direct SSH host and port.
8. Connect with a transport-only SSH probe and install `git`, `rsync`, `tar`,
   and `python3` through the image's package manager when they are missing.
9. Update the Vast label from provisioning to ready.
10. Wait for Crabbox SSH bootstrap readiness and write a local lease claim.
11. Run normal Crabbox SSH sync, command execution, status, list, and cleanup.

The provider requires Linux. It does not advertise desktop, browser, code-server,
Tailscale, coordinator, or provider-managed sync support in this release.
Actions hydration works only as normal command execution on the resulting Linux
SSH lease.

If create or bootstrap becomes indeterminate after a Vast instance id is known,
Crabbox records a local recovery claim when possible. Retry
`crabbox stop --provider vast <lease-or-slug>` before deleting resources
manually so Crabbox can reconcile the instance and local key material.

## Offer Selection

By default Crabbox searches `ondemand` verified, rentable, not-rented offers
with at least one direct SSH port and orders by `dlperf_per_dphtotal desc`.
Narrow the search when the default catalog is too broad:

```sh
crabbox run \
  --provider vast \
  --vast-instance-type interruptible \
  --vast-gpu-name "H100" \
  --vast-gpu-count 1 \
  --vast-max-dph-total 4.25 \
  --vast-min-reliability 0.95 \
  --no-sync \
  -- nvidia-smi
```

`vast.maxDphTotal` and `--vast-max-dph-total` are guardrails for offer search,
not a billing cap enforced by Crabbox after provisioning. Review the selected
offer and Vast account billing before running long jobs.

## Release And Cleanup

The default release action is `destroy`, which deletes the Vast instance,
detaches the Crabbox-managed instance SSH key when its key id is known, removes
the local claim, and removes the local per-lease key.

Release actions:

- `destroy` or `delete`: destroy the Vast instance on `stop` or one-shot release.
- `stop`: request Vast to stop the instance and keep the local Crabbox claim
  with `state=stopped` so later status, cleanup, or explicit destroy can
  reconcile the retained resource.
- `keep`: leave the instance and local claim untouched during release.

Use `stop` or `keep` only when you explicitly accept the retained resource and
its billing implications. Direct mode has no coordinator alarm.

Cleanup only mutates instances with a complete Crabbox Vast ownership label and
a matching local claim:

```sh
crabbox list --provider vast --json
crabbox cleanup --provider vast --dry-run
crabbox cleanup --provider vast
```

Crabbox refuses to operate on non-Crabbox Vast instances, changed ownership
labels, stale local claims, missing local claims for destructive release, or
instances whose provider identity no longer matches the local claim. Vast labels
are compact strings beginning with `cbx1|`; they encode the lease id, slug, and
state.

Each new local claim also records the authenticated Vast account id and the
configured API endpoint. Release validates both before accepting a remote 404,
stopping, or destroying an instance, so switching credentials or API endpoints
cannot silently discard cleanup state for a billable instance in another account.
Runtime state updates preserve that exact non-secret routing metadata. Generated
stop commands include the credential-free API endpoint and never include the API
key.

## Guarded Live Smoke

The repeatable live check is opt-in and billable:

```sh
CRABBOX_LIVE=1 \
  CRABBOX_LIVE_PROVIDERS=vast \
  CRABBOX_LIVE_VAST_GPU_COUNT=1 \
  CRABBOX_LIVE_VAST_MAX_DPH_TOTAL=0.50 \
  CRABBOX_LIVE_VAST_RELEASE_ACTION=destroy \
  scripts/live-vast-smoke.sh
```

The script builds `bin/crabbox`, reads `CRABBOX_VAST_API_KEY` or
`VAST_API_KEY`, requires an explicit positive hourly cost cap, positive minimum
GPU count, destroy release action, and an empty Crabbox-owned Vast inventory. It
creates one kept lease, waits for ready status, proves at least the requested GPU
count with `nvidia-smi -L`, records the actual count, verifies `list --json`,
destroys the lease, runs dry-run cleanup, and verifies the Crabbox-owned
inventory is empty afterward.

Optional live-smoke overrides:

```text
CRABBOX_LIVE_VAST_GPU_NAME         GPU name selector, default empty
CRABBOX_LIVE_VAST_GPU_COUNT        Required positive minimum GPU count for live proof
CRABBOX_LIVE_VAST_MAX_DPH_TOTAL    Required positive max dollars per hour for offer search
CRABBOX_LIVE_VAST_INSTANCE_TYPE    Offer type, default ondemand
CRABBOX_LIVE_VAST_IMAGE            Docker image, default from provider config
CRABBOX_LIVE_VAST_RELEASE_ACTION   Must be destroy, default destroy
```

Final classifications include:

```text
classification=live_vast_smoke_passed ... minimum_gpu_count=N actual_gpu_count=N max_dph_total=N release_action=destroy pre_owned=0 post_owned=0 cleanup=complete
classification=environment_blocked
classification=billing_blocked
classification=quota_blocked
classification=capacity_blocked
classification=validation_failed
classification=cleanup_failed
```

Missing opt-in flags, missing credentials, auth failures, disabled account API
access, missing GPU offers, billing blocks, quota blocks, and capacity blocks
are reported as classified outcomes. The script redacts `VAST_API_KEY`,
`CRABBOX_VAST_API_KEY`, `instance_api_key`, Jupyter tokens, user data, private
keys, and URLs carrying token-like query parameters from diagnostics.

If cleanup fails, use the reported slug and Vast instance id with
`crabbox list --provider vast --json`, `crabbox stop --provider vast <slug>`,
and the Vast console. Do not delete unrelated instances that only look similar.

## Capabilities

- **OS targets**: Linux only.
- **SSH**: yes, Crabbox-managed SSH over Vast direct SSH endpoints.
- **Crabbox sync**: yes, rsync over SSH.
- **Provider-managed sync**: no.
- **GPU**: yes, provider catalog dependent.
- **Coordinator**: no; direct CLI only.
- **Cleanup**: yes, ownership-label and local-claim guarded.
- **Desktop / browser / code-server**: not advertised in this release.
- **Tailscale**: not advertised in this release.

## Gotchas

- Vast is direct-only. Coordinator secrets, usage limits, and cost accounting do
  not cover these instances.
- Vast offers are capacity-sensitive. No matching offer, quota, billing, or
  capacity failures are external blockers, not docs-check failures.
- `ssh_direct` is required. Other Vast runtime types are rejected by config
  validation.
- The default image is CUDA-oriented. If it lacks workload dependencies, install
  them in your repo setup or select a different image/template.
- `stop` and `keep` can retain billable resources. Use `destroy` for normal
  one-shot Crabbox validation.
- Destructive release requires local Crabbox claim state. Keep the claim until
  cleanup is complete.
