# fal Provider

Read this when you are:

- choosing `provider: fal` or the `fal-ai` alias;
- validating a direct fal Compute SSH lease;
- changing `internal/providers/fal` or the guarded live smoke.

fal is a Linux-only **SSH lease** provider for fal Compute instances. Crabbox
creates one Compute instance through the fal API, sends a per-lease SSH public
key in the create request, records the instance in a local Crabbox claim, waits
for public SSH readiness, and then uses the normal Crabbox SSH sync, run, ssh,
stop, and cleanup paths.

fal is **direct-only** in this release. It does not run through the coordinator,
so the local CLI must have a fal API key and direct cleanup remains the
operator's responsibility. fal Compute instances are billable while they are
running; stopping a Crabbox lease deletes the instance instead of pausing it.

## When To Use It

Use fal when you need a direct Linux GPU instance and local fal credentials are
acceptable. Prefer AWS, Azure, GCP, or Hetzner when you need a brokered team
path, coordinator-side credentials, or cloud-specific cost accounting. Prefer a
non-GPU direct provider when a CPU-only Linux box is enough. fal Serverless and
Model APIs are not the first Crabbox provider path here because this provider
needs a long-lived SSH-reachable box for rsync and command execution.

## Commands

```sh
crabbox doctor --provider fal
crabbox warmup --provider fal --fal-instance-type gpu_1x_h100_sxm5
crabbox run --provider fal --fal-instance-type gpu_1x_h100_sxm5 -- go test ./...
crabbox ssh --provider fal --id my-app
crabbox list --provider fal --json
crabbox stop --provider fal my-app
crabbox cleanup --provider fal --dry-run
```

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the fal
instance id when that instance is backed by a local Crabbox fal claim. The
`fal-ai` provider alias is accepted for compatibility, but examples use the
canonical `fal` name.

## Configuration

```yaml
provider: fal
target: linux
fal:
  apiUrl: https://api.fal.ai/v1
  instanceType: gpu_1x_h100_sxm5
  user: root
  workRoot: /work/crabbox
```

Config keys under `fal:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `apiUrl` | `cfg.Fal.APIURL` | `https://api.fal.ai/v1` | fal Platform API base URL. HTTPS is required unless targeting localhost for tests. |
| `instanceType` | `cfg.Fal.InstanceType` | `gpu_1x_h100_sxm5` | fal Compute instance type. |
| `sector` | `cfg.Fal.Sector` | unset | fal Compute sector; set only for supported 8× H100 multi-node instance types. |
| `user` | `cfg.Fal.User` | `root` | SSH user for the instance. |
| `workRoot` | `cfg.Fal.WorkRoot` | `/work/crabbox` | Remote Crabbox work root. |

Provider flags:

```text
--fal-api-url
--fal-instance-type
--fal-sector
--fal-user
--fal-work-root
```

Environment overrides:

```text
CRABBOX_FAL_KEY             fal API key, preferred
FAL_KEY                     fal API key fallback
CRABBOX_FAL_API_URL         Override the fal API URL
CRABBOX_FAL_INSTANCE_TYPE   Override the Compute instance type
CRABBOX_FAL_SECTOR          Override the Compute sector
CRABBOX_FAL_USER            Override the SSH user
CRABBOX_FAL_WORK_ROOT       Override the remote work root
```

Do not pass the fal API key as a command-line argument. Crabbox intentionally
has no fal key flag, so the key cannot leak through shell history or process
listings.

## Token Scope

Crabbox sends the API key only in the `Authorization: Key ...` header to the fal
Compute endpoints used for instance list, get, create, and delete. Cross-origin
redirects are rejected before credentials or create bodies can be replayed to
another destination.

`crabbox doctor --provider fal` is non-mutating. It checks that credentials are
available and that the Compute instance list API is reachable. Missing API
keys, authorization failures, billing, quota, and capacity issues should be
treated as account or provider readiness problems, not as proof that a live
lease can be created.

## Lifecycle

1. Generate a per-lease SSH key under the Crabbox testbox key directory.
2. Create one fal Compute instance with `instanceType`, `sector`, and the
   per-lease public key.
3. Wait for the instance to report `ready` and expose an SSH host.
4. Wait for Crabbox SSH/bootstrap readiness over public SSH.
5. Claim the lease locally with fal instance id and SSH endpoint metadata.
6. Run normal Crabbox sync, run, ssh, status, and list workflows over SSH.
7. Delete the fal instance and remove the local claim and stored key on `stop`.

If creation or post-create readiness becomes indeterminate, Crabbox preserves a
local recovery claim when it has enough information to retry `crabbox stop
--provider fal <lease-or-slug>`. Recovery-pending claims are retained until the
operator can reconcile or remove them.

## Ownership And Cleanup

fal cleanup is intentionally local-claim based. Crabbox will not delete a fal
instance unless the local claim proves the provider, lease id, slug, target, and
fal instance id line up with the requested lease. This protects foreign fal
Compute instances and Crabbox-like resources that were not claimed by this CLI.

Use:

```sh
crabbox list --provider fal --json
crabbox cleanup --provider fal --dry-run
crabbox cleanup --provider fal
```

`cleanup --dry-run` prints what would be deleted without mutating fal or local
claims. If the provider says an instance is already absent, cleanup removes the
stale local claim only when not in dry-run mode. Recovery-pending claims are
skipped so the operator can investigate the uncertain create or readiness
outcome.

## Cost Discipline

fal Compute instances are billable while active. Keep live checks short, prefer
`--ttl` and `--idle-timeout`, use `cleanup --dry-run` before destructive
cleanup, and run live smoke only with explicit opt-in environment gates. If
cleanup fails, keep the reported slug and inspect local claims with
`crabbox list --provider fal --json` before using the fal console or API to
delete any remaining instance.

## Guarded Live Smoke

The repeatable live check is opt-in:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=fal scripts/live-fal-smoke.sh
```

The script builds `bin/crabbox`, reads `CRABBOX_FAL_KEY` or `FAL_KEY` from the
environment, requires an empty Crabbox-owned fal inventory, creates a kept
short-lived Compute lease, waits for readiness, runs `echo ok`, verifies
`list --json`, stops the lease, runs dry-run cleanup, and verifies the local fal
inventory is empty afterward.

Optional live-smoke overrides:

```text
CRABBOX_LIVE_FAL_INSTANCE_TYPE   Instance type for the smoke, default gpu_1x_h100_sxm5
CRABBOX_LIVE_FAL_SECTOR          Sector for the smoke; unset by default
CRABBOX_LIVE_FAL_API_URL         API URL for the smoke, default https://api.fal.ai/v1
```

Final classifications include:

```text
classification=live_fal_smoke_passed
classification=environment_blocked
classification=billing_blocked
classification=quota_blocked
classification=capacity_blocked
classification=validation_failed
classification=cleanup_failed
```

External blockers such as missing credentials, inactive billing, quota, or
capacity are reported as classified blocked outcomes. The smoke script redacts
fal keys, SSH key payload fields, private key material, and token-like URLs from
diagnostic output.

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Tailscale**: no; fal Compute exposes public SSH for this provider path.
- **Desktop / browser / code**: not advertised in this phase.
- **Cleanup**: yes, local-claim-owned fal Compute instances only.
- **Coordinator**: never; direct CLI only.

## Gotchas

- fal is direct-only. Coordinator secrets, team scheduling, and cost accounting
  do not cover these instances.
- Running Compute instances are billable. Use short TTLs, dry-run cleanup, and
  explicit live-smoke gates.
- `--fal-instance-type` must be a fal Compute instance type such as
  `gpu_1x_h100_sxm5`.
- `--tailscale` and non-Linux targets are rejected for this provider.
- The first provider path is fal Compute over SSH. fal Serverless and Model APIs
  are separate fal products and are not Crabbox lease backends here.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Provider feature overview](../features/providers.md)
- [Operations](../operations.md)
