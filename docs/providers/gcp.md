# Google Cloud Provider

Read this when you:

- choose `provider: gcp`;
- set up Compute Engine credentials for direct or brokered leases;
- debug GCP quota, machine types, firewall rules, labels, or cleanup;
- change `internal/providers/gcp`, `internal/cli/gcp.go`, or `worker/src/gcp.ts`.

Google Cloud is a managed SSH-lease provider for Linux Compute Engine VMs.
Crabbox provisions the instance, SSH metadata, labels, boot disk, public IP, and
a Crabbox-managed SSH firewall rule. Once the VM exists, the standard SSH path
owns readiness, sync, command execution, results, label touches, release, and
cleanup. GCP can run direct from the CLI (Application Default Credentials) or
brokered through the coordinator (`Coordinator: supported`).

## When to use

Use GCP when:

- your billing, quota, or compliance boundary already lives in Google Cloud;
- you want Linux Compute Engine capacity behind the shared coordinator;
- you want direct local testing with Google Application Default Credentials.

GCP is Linux-only. For Windows, WSL2, macOS, Linux desktop/browser/code leases,
or the AMI-style image bake-and-promote workflow, use AWS instead. GCP does
support [Tailscale](../features/tailscale.md) and native
[checkpoints](../features/checkpoints.md) (machine-image and disk-snapshot
fork/restore) — see below.

### Provider names

```text
gcp
google
google-cloud
```

`google` and `google-cloud` are aliases. Crabbox canonicalizes them to `gcp`
before direct or brokered lease requests, including class default selection.

## Quick start

Direct local smoke test:

```sh
gcloud auth application-default login
gcloud services enable compute.googleapis.com --project example-project-123

export GOOGLE_CLOUD_PROJECT=example-project-123
export GOOGLE_APPLICATION_CREDENTIALS="$HOME/.config/gcloud/application_default_credentials.json"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
printf 'provider: gcp\n' > "$tmp"
env -u CRABBOX_COORDINATOR -u CRABBOX_COORDINATOR_TOKEN \
  CRABBOX_CONFIG="$tmp" \
  crabbox run --provider gcp --type e2-micro --market on-demand --no-sync -- \
  echo gcp-ok
```

Normal class-based leases:

```sh
crabbox warmup --provider gcp --class standard
crabbox run --provider gcp --class fast -- pnpm test
crabbox ssh --provider gcp --id blue-lobster
crabbox stop --provider gcp blue-lobster
crabbox cleanup --provider gcp
```

`--type` is exact, for example `--type c4-standard-32` or `--type e2-micro`. Use
`--class` to let Crabbox retry the provider's class candidates.

## Configuration

```yaml
provider: gcp
target: linux
class: beast
gcp:
  project: example-project-123
  zone: europe-west2-a
  image: projects/ubuntu-os-cloud/global/images/family/ubuntu-2604-lts-amd64
  network: default
  subnet: ""
  tags:
    - crabbox-ssh
  sshCIDRs: []
  rootGB: 0 # automatic: class default, at least the image minimum
  serviceAccount: ""
```

Defaults applied when a field is unset: `zone` `europe-west2-a`, `network`
`default`, `tags` `[crabbox-ssh]`, automatic root sizing, and the Ubuntu 26.04 LTS image
above. `project` and `zone` are required for a direct lease.

Automatic boot disks are 40 GB for `tiny`, 80 GB for `small`, 150 GB for
`standard` and `fast`, 250 GB for `large`, and 400 GB for `beast`. Unset or zero
`gcp.rootGB` selects this policy. Direct and brokered creates read the source
image's disk size and raise the automatic default when necessary. Family aliases
are resolved to the exact inspected image. Project-relative references such as
`global/images/my-image` resolve in the selected project. Brokered disk-snapshot restores use
the same policy with the snapshot's size as the minimum. Provider credentials
need image read access (and snapshot read access for snapshot restores).

Set `gcp.rootGB: 400` or `CRABBOX_GCP_ROOT_GB=400` to
retain 400 GB. Positive explicit sizes are forwarded unchanged, including sizes
that GCP rejects as smaller than the source. The coordinator's
`CRABBOX_GCP_ROOT_GB` remains an explicit operator override when the request
leaves sizing unset. Config output reports `rootGB: 0` for automatic sizing;
the source minimum is resolved only during create. Upgrade the coordinator as
well as the client for class sizing in brokered mode.

Ready-pool borrows retain their existing disk; new members follow this policy.
Machine-image restores preserve their inherited disk sizes and types: GCP
[does not allow attached-disk property overrides during restore](https://cloud.google.com/compute/docs/machine-images/create-instance-from-machine-image#create-instance-from-image-override).
Use disk-snapshot checkpoints when a fork needs a different root size.

Project resolution order is `CRABBOX_GCP_PROJECT`, then `gcp.project`, then
`GOOGLE_CLOUD_PROJECT`, then `GCP_PROJECT_ID`. Brokered requests forward only the
Crabbox-specific project sources (`CRABBOX_GCP_PROJECT` / `gcp.project`); ambient
ADC project variables (`GOOGLE_CLOUD_PROJECT`, `GCP_PROJECT_ID`) stay local so
the Worker's own defaults apply.

### Direct-mode environment

```text
GOOGLE_APPLICATION_CREDENTIALS
GOOGLE_CLOUD_PROJECT
GCP_PROJECT_ID
CRABBOX_GCP_PROJECT
CRABBOX_GCP_ZONE
CRABBOX_GCP_IMAGE
CRABBOX_GCP_NETWORK
CRABBOX_GCP_SUBNET
CRABBOX_GCP_TAGS
CRABBOX_GCP_SSH_CIDRS
CRABBOX_GCP_ROOT_GB
CRABBOX_GCP_SERVICE_ACCOUNT
```

### Capacity environment

```text
CRABBOX_CAPACITY_MARKET
CRABBOX_CAPACITY_FALLBACK
CRABBOX_CAPACITY_AVAILABILITY_ZONES
```

`capacity.availabilityZones` controls GCP zone fallback. `capacity.regions` does
not expand into zones for GCP today.

## Direct auth

Direct mode uses Google's official Compute Go SDK
(`cloud.google.com/go/compute/apiv1`) and the credential sources supported by
Google Application Default Credentials.

Local setup:

```sh
gcloud auth application-default login
gcloud auth list
gcloud auth application-default print-access-token >/dev/null
```

Project setup:

```sh
gcloud services enable compute.googleapis.com --project example-project-123
gcloud compute zones list --project example-project-123 --filter='name=europe-west2-a'
```

Common blockers:

- Compute Engine API disabled — enable `compute.googleapis.com`.
- Billing disabled — attach billing before enabling Compute.
- Missing IAM — the active account needs enough Compute permissions to create,
  label, list, and delete instances, plus permission to manage the shared
  firewall rule.
- Service Usage denied — the account may still run Compute calls but cannot list
  or enable APIs.

For a cheap live smoke test, use
`--type e2-micro --market on-demand --no-sync --ttl 20m --idle-timeout 5m`. This
exercises instance creation, SSH metadata, cloud-init, SSH readiness, command
execution, and release/delete without syncing a repository.

## Brokered auth

Brokered mode uses coordinator-side Google credentials, so developer machines
do not need Google credentials when the coordinator owns provisioning. The
coordinator uses Compute REST calls and lists pool state through aggregated
instance listing with partial success enabled, so one unhealthy zone does not
hide healthy Crabbox VMs elsewhere.

The coordinator always needs a project:

```text
GCP_PROJECT_ID   (or CRABBOX_GCP_PROJECT)
```

For a coordinator running on Google Cloud infrastructure, prefer the attached
service account identity. GKE Workload Identity, Compute Engine, and similar
Google-hosted runtimes can provide short-lived tokens through the metadata
server, so no service-account private key is required in the coordinator
environment. Enable this credential source explicitly:

```text
CRABBOX_GCP_CREDENTIAL_SOURCE=metadata
```

The attached service account still needs the IAM permissions used by the
coordinator. On Compute Engine, configure the VM with the
`https://www.googleapis.com/auth/cloud-platform` access scope; VM access scopes
cap the permissions available to metadata-issued tokens even when IAM grants
the service account broader roles.

For portable coordinator deployments without metadata server credentials, set
both service-account key variables:

```text
GCP_CLIENT_EMAIL
GCP_PRIVATE_KEY
```

If either `GCP_CLIENT_EMAIL` or `GCP_PRIVATE_KEY` is set, both must be set.
When `CRABBOX_GCP_CREDENTIAL_SOURCE` is unset, brokered GCP uses the
service-account-key path. `CRABBOX_GCP_CREDENTIAL_SOURCE=service-account-key`
is accepted as an explicit spelling of the same default. Metadata token requests
use manual redirect handling supported by both Node and workerd, reject all `3xx`
responses without following them or retrying, reject responses without Google's metadata marker, refresh before
the metadata server's five-minute token-cache boundary, and retry
connection/startup failures plus transient `429`, `499`, and `5xx` responses
with bounded exponential backoff and a one-minute overall deadline.

Optional coordinator defaults (same names as the direct-mode environment):

```text
CRABBOX_GCP_ZONE
CRABBOX_GCP_IMAGE
CRABBOX_GCP_NETWORK
CRABBOX_GCP_TAGS
CRABBOX_GCP_ROOT_GB
CRABBOX_GCP_SERVICE_ACCOUNT
CRABBOX_GCP_CREDENTIAL_SOURCE
```

Explicit broker requests for `gcp.project`, `gcp.image`, `gcp.network`,
`gcp.subnet`, `gcp.tags`, or `gcp.serviceAccount` require admin-token
authentication. Normal broker users receive the coordinator-managed values.
Direct mode keeps these local configuration overrides.

Verify configuration:

```sh
crabbox doctor --provider gcp
```

Maintainers can exercise the coordinator's exact ownership boundary against a
disposable `e2-micro`: set the project plus either coordinator credential path
above, then set `LIVE_SSH_PUBLIC_KEY`, then run
`CRABBOX_GCP_RELEASE_LIVE=1 npm test --prefix worker -- --run test/gcp-release.live.test.ts`.
The smoke proves a foreign owner claim is denied, the exact owner claim is
deleted, and neither the instance nor its boot disk remains.

The readiness check reports missing configuration names without exposing values.
Lease creation fails with `provider_not_configured` until the coordinator has a
project plus either a complete service-account key pair or
`CRABBOX_GCP_CREDENTIAL_SOURCE=metadata`.

## Lifecycle

1. Resolve project, zone, image, network, disk, tags, and credentials.
2. Ensure a Crabbox-managed SSH firewall exists for the configured network, SSH
   ports, CIDRs, and target tags.
3. Create a Compute Engine instance with Ubuntu cloud-init, SSH metadata
   (`enable-oslogin=FALSE`), and Crabbox labels.
4. Attach a service account when `gcp.serviceAccount` or
   `CRABBOX_GCP_SERVICE_ACCOUNT` is set.
5. For Spot leases, set scheduling to provisioning model `SPOT`, on-host
   maintenance `TERMINATE`, automatic restart off, and termination action
   `DELETE`.
6. Wait for the public IP, then for SSH and the Crabbox ready marker.
   Public-IP observations and their five-second retry waits share a two-minute
   budget. Earlier cancellation stops discovery, and API errors fail immediately;
   this budget does not include the subsequent SSH readiness phase.
   Readiness cancellation retains the caller's original cause and diagnostic,
   including interrupted reads, while exposing the canonical cancellation or
   deadline for run classification. Budget expiry keeps the existing timeout
   message and CLI exit code 1. Completed ready responses and typed API errors
   retain precedence when they coincide with cancellation.
7. Touch labels during active runs.
8. Delete the VM on release unless the lease is kept.

If acquisition fails after creating a VM, successful rollback also removes the
lease's generated SSH credentials and host-trust files. Failed remote deletion
retains those files; a local artifact-cleanup error is reported and prevents an
automatic fresh-lease retry. Cleanup-client creation and remote deletion
completion share a fresh three-minute budget, independent of cancellation of the
acquisition request. [Compute Engine guest shutdown](https://docs.cloud.google.com/compute/docs/instances/suspend-stop-reset-instances-overview#stop_operation)
can take 120 seconds before resource deletion, so the budget includes time to
confirm completion. Uncertain creation outcomes are unchanged.

## Machine classes

```text
tiny      c4-standard-4, c3-standard-4, n2-standard-4, n2d-standard-4
small     c4-standard-8, c3-standard-8, n2-standard-8, n2d-standard-8, c4-standard-4
standard  c4-standard-32, c3-standard-22, n2-standard-32, n2d-standard-32
fast      c4-standard-64, c3-standard-44, n2-standard-64, n2d-standard-64, c4-standard-32
large     c4-standard-96, c3-standard-88, n2-standard-80, n2d-standard-96, c4-standard-64
beast     c4-standard-192, c4-standard-96, c3-standard-176, c3-standard-88, n2d-standard-224, n2-standard-128
```

Boot disks use Hyperdisk Balanced (`hyperdisk-balanced`) for the Hyperdisk-only
families A4, A4X, C4, C4A, C4D, C4N, G4, H4D, M4, M4N, N4, N4A, N4D, X4,
X5, and Z4D, plus the A3 Ultra (`a3-ultragpu`) variant.
Other families use Balanced Persistent Disk (`pd-balanced`), including C3, N2,
and N2D. N2 and N2D cannot use Hyperdisk Balanced, so the disk type follows each
candidate rather than the initially selected class type.

`capacity.market: spot` maps to GCP Spot VMs. If `capacity.fallback` starts with
`on-demand`, Crabbox retries the same zone and type candidates as on-demand after
retryable Spot capacity or quota failures.

Explicit `--type` disables class-candidate fallback. Zone fallback and
Spot-to-on-demand fallback still apply to the exact requested type when GCP
returns a quota, capacity, rate-limit, or unavailable-type error. See
[Capacity and fallback](../features/capacity-fallback.md).

The coordinator classifies retry eligibility from complete API error evidence;
displayed diagnostics remain bounded and redacted. Shortening a diagnostic does
not change which configured zone or market candidates may be attempted.

Disk-type/machine-type incompatibility also permits candidate fallback. For any
provider, a create the coordinator records as failed with no possible provider
resource returns HTTP 422 `provisioning_failed` with the diagnostic message.
Uncertain outcomes and failures requiring resource cleanup retain their existing
responses. An identical fixed-ID replay returns the same `422 provisioning_failed`
diagnostics without provider work or client cancellation.

## Networking

The provider uses `gcp.network` and optional `gcp.subnet`.

- If either value is a full self link, Crabbox uses it as-is.
- Otherwise `gcp.network` becomes `projects/<project>/global/networks/<name>`.
- Otherwise `gcp.subnet` becomes
  `projects/<project>/regions/<region>/subnetworks/<name>`, where the region is
  derived from the zone.

The default firewall allows SSH ingress from `0.0.0.0/0` when no CIDRs are
configured. Set `gcp.sshCIDRs` or `CRABBOX_GCP_SSH_CIDRS` for tighter ingress.
The default-network, default-policy rule is named `crabbox-ssh`; custom networks
use `crabbox-ssh-<network>`. Non-default CIDRs, tags, or SSH ports append a
policy-hash suffix so leases with different ingress settings do not rewrite each
other's SSH access.

Explicit `gcp.tags` or `CRABBOX_GCP_TAGS` replace the default target tags for
that lease; they are not merged with the default `crabbox-ssh` tag.

Crabbox refuses to update an existing firewall unless its description marks it as
Crabbox-managed. Rename the firewall, change tags, or adopt it intentionally if
an older rule already owns the name.

For Tailscale-meshed leases, see [Tailscale](../features/tailscale.md). Direct
`--tailscale` requires an auth key in the configured auth-key environment
variable; brokered mode uses coordinator OAuth secrets.

## Labels and cleanup

GCP labels must be lowercase and label keys must start with a letter. Crabbox
sanitizes keys separately from values so numeric lease metadata stays parseable.

Key cleanup labels:

```text
crabbox=true
provider=gcp
lease=<cbx_id>
state=ready
keep=false
created_at=<unix_seconds>
expires_at=<unix_seconds>
ttl_secs=<seconds>
zone=<gcp_zone>
```

Crabbox also records the immutable numeric Compute Engine instance ID in the
local lease claim. Direct release and cleanup require that unchanged claim to
match the exact project, zone, instance name and numeric ID, lease, slug, and
provider key before deletion. Labels and deterministic names remain discovery
hints, not destructive authority. Claimless or stale-claim resources are
skipped; recover or remove them through an explicit operator-controlled GCP
workflow instead of silently adopting cloud metadata.

After deletion or an exact lookup confirming the instance is absent, direct
release and cleanup remove the lease's generated SSH key and private host-trust
files before retiring its claim. Both local steps share the unchanged-claim
lock. A provider-deletion or SSH-artifact cleanup failure retains the claim for retry, and
`--dry-run` leaves the claim and SSH material untouched. Stale records without
a cloud resource identity retain their existing claim-only pruning behavior.
Waiting for the claim lock honors caller cancellation and deadlines. Cancellation
before lock admission preserves local state and does not begin deletion; after
confirmed deletion, bounded SSH-artifact cleanup and claim retirement still finish.

Direct cleanup:

```sh
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
printf 'provider: gcp\n' > "$tmp"

env -u CRABBOX_COORDINATOR -u CRABBOX_COORDINATOR_TOKEN \
  CRABBOX_CONFIG="$tmp" \
  crabbox cleanup --provider gcp --dry-run

env -u CRABBOX_COORDINATOR -u CRABBOX_COORDINATOR_TOKEN \
  CRABBOX_CONFIG="$tmp" \
  crabbox cleanup --provider gcp
```

Cleanup lists Crabbox-labeled instances across the project's visible zones using
aggregated instance listing with partial success enabled. Inventory accepts only
deterministically named instances with the canonical `crabbox`, `created_by`,
`provider`, and valid lease labels, then deletes expired or released leases in
the zone recorded on each VM. Brokered workspace recovery uses one authoritative
lookup for the persisted project, zone, and instance name, then requires the
unchanged numeric instance ID and canonical ownership labels. It does not fall
back to project inventory or adopt a replacement instance. For an interrupted
non-workspace create with an exact durable create-attempt binding but no cloud
identity, the coordinator may perform one canonical-name lookup, persist only
the observed numeric instance ID under the unchanged lease and attempt fences,
then require a second strict lookup before binding the resource for cleanup.
If that strict lookup fails transiently, recovery retries only with the same
captured ID; absence or replacement remains unresolved. Successful creates do
not publish cleanup custody until either the operation or an exact owned-instance
lookup supplies the numeric ID.
Unbound legacy, tokenless, active, registered, workspace, or already claimed
leases cannot use this interrupted-create path.

For a fully bound pre-upgrade brokered lease that lacks only the numeric instance
ID, manual deletion or expiry cleanup may perform one exact scoped lookup under
the unchanged cleanup claim. The lookup must match all ownership labels and
either the exact durable raw resource identity or, when that identity is absent,
corroborate the observed raw ID through the lease's positive, finite, integral
historical numeric server ID. The numeric comparison is intentionally lossy
corroboration, not exact identity proof. The coordinator persists the observed
raw ID, then performs the ordinary exact lookup again before deletion. It never
uses project inventory or this migration path for registered, workspace,
provisioning, or uncertain-create records. Absence completes cleanup only after
the same post-lookup lease and cleanup fences pass; replacement or ambiguity
retains the captured ID without deleting either instance.

Brokered release requires the persisted numeric ID and refuses deletion when it
is absent from or differs from the observed VM. These checks are defense-in-depth
against accidental or ambiguous resource adoption.
GCP labels are operator metadata, not an authorization boundary against another
principal that can already mutate instances in the same project. Brokered
cleanup is coordinator-owned; direct cleanup additionally requires the exact
local claim described above.

Three independent safety nets enforce expiry:

- Direct GCP VMs set Compute Engine `maxRunDuration` with termination action
  `DELETE`, so the TTL hard cap is enforced by the platform.
- Each VM installs a guest-side `crabbox-gcp-expiry-guard` systemd timer that
  reads live instance labels through the metadata service and Compute API, then
  self-deletes expired leases, including kept warmups, when the attached service
  account can delete the VM. New VMs receive this behavior; existing VMs retain
  their installed guard and must be recreated to receive the updated idle reaper.
  Manual `crabbox cleanup` continues to skip kept leases; use explicit `stop`
  to remove an existing kept VM.
- Cleanup removes stale local GCP claim files whose lease IDs no longer appear in
  provider inventory.

See [Lifecycle and cleanup](../features/lifecycle-cleanup.md) for the shared model.

## Typed ready-pool identity

Typed ready-pool identity generation and registration observe the exact
Crabbox-owned VM and its single persistent boot disk. Crabbox accepts only a
canonical global image or snapshot source with the immutable numeric resource
ID reported by Compute Engine. Machine images, foreign ownership, ambiguous
boot disks, and malformed or incomplete source evidence fail closed.
The coordinator binds the lease's exact numeric VM resource ID, requires the
disk self-link and sole `users` attachment to identify that VM, then rereads
both resources and rejects replacement, attachment, ownership, or provenance
drift before persisting evidence.

This observation runs only for typed `identity` and `register-identity`
requests after lease access, state, expiry, and request metadata validation.
Ordinary launches with automatic root sizing read image or snapshot size metadata.
Borrow/return/heartbeat and reconciliation do not read source metadata. The coordinator credentials need
`compute.instances.get` and `compute.disks.get` for typed GCP identity
operations.

Once persisted, typed cohort identity uses the source namespace
`projects/<project>/global/images` or
`projects/<project>/global/snapshots`, the immutable numeric source ID, and the
canonical architecture. The execution project remains required launch evidence
but is not substituted for the source project or included in the identity. The
launch zone is excluded so capacity fallback does not split an otherwise
identical cohort.

Only exact relative resource names and the lowercase official Compute API HTTPS
forms are accepted. Leading or trailing whitespace, family aliases, extra path
segments, ports, queries, fragments, percent-encoded ambiguity,
kind/collection mismatches, and non-numeric IDs fail closed. Machine images
remain valid checkpoint sources but cannot join typed ready-pool cohorts.

## Checkpoints

Brokered Linux GCP leases support native [checkpoints](../features/checkpoints.md):

- `--strategy image` captures a GCP machine image (`gcp-machine-image`). GCP
  [refuses machine images](https://docs.cloud.google.com/compute/docs/machine-images/create-machine-images)
  for instances with any Hyperdisk attached (Crabbox rejects Hyperdisk-booted
  leases before calling GCP) and for C3D, H3, A3, and most Z3 machine types even
  with a Persistent Disk boot; use the default disk-snapshot strategy for those.
- The default strategy captures a disk snapshot (`gcp-disk-snapshot`).

`checkpoint fork` and `checkpoint restore` rehydrate from either kind in the
recorded project and zone. Native checkpoints require a coordinator and a known
cloud instance ID; they are not available for direct-only leases.

Machine-image restore preserves the image's disk type without overriding its
disks. An incompatible candidate (such as C4 with a `pd-balanced` machine image)
falls through to the next configured candidate. Disk-snapshot restore selects
the boot disk type for the destination machine family and supports Hyperdisk.

New brokered GCP checkpoints are owned by the coordinator and manually retained
unless creation or `checkpoint policy` explicitly sets
`--expire-unused-after <duration>`. Ownership binds the exact project, global
resource kind/name, canonical resource ID, numeric immutable identity, and
source lease. Fork/shard use claims block expiry while provisioning is active.
Wrong-project, disabled-API, authorization, operation, or name-reuse failures
never count as successful deletion. Provider labels remain within GCP's
63-character value limit even when the coordinator checkpoint identifier is
long; a bounded ownership digest links those labels to the full durable ID.

Machine images remain valid checkpoint fork/restore sources, but Compute Engine
does not expose exact created-disk source evidence for them. They therefore
cannot supply typed ready-pool immutable provenance; use a boot image or disk
snapshot when that identity is required.

Generic `crabbox image delete <image-id> --provider gcp` refuses an image owned
by a managed checkpoint; use `checkpoint delete <id>` instead. GCP does not yet
have the AWS-style `image promote` bake pipeline. Direct and historical images
remain operator-managed.

## Troubleshooting

`Compute Engine API has not been used ... or it is disabled`

Enable the Compute API for the selected project:

```sh
gcloud services enable compute.googleapis.com --project example-project-123
```

`Billing account ... is not found`

Attach billing to the project before enabling Compute or creating instances.

`PERMISSION_DENIED` from Service Usage

The active account cannot enable or list APIs for the project. Use an account
with Service Usage permissions, or ask a project admin to enable Compute.

`get gcp firewall` or `create gcp firewall` fails

Check the network name, IAM, and whether a non-Crabbox firewall already owns the
`crabbox-ssh`, `crabbox-ssh-<network>`, or policy-suffixed firewall name.

SSH stays on port `22` and port `2222` never opens

Cloud-init may still be running. On very small instances, package update and
bootstrap can take several minutes. `crabbox run` falls back to SSH port `22`
when the configured port is not ready but the instance is otherwise reachable.

`crabbox cleanup --provider gcp` prints nothing

No expired Crabbox-labeled instances were found in the project zones visible to
the active credentials, or the command is still using the coordinator. Use a
temporary `CRABBOX_CONFIG` without broker settings for direct cleanup.

## Limitations

- Linux only — no GCP Windows, WSL2, or macOS.
- No desktop, browser, code-server, or VNC capabilities.
- No AWS-style image bake-and-promote pipeline (native checkpoints and
  `image delete` are supported).
- No provider pricing lookup yet; cost uses the generic managed-provider fallback
  rate.
- OS Login must not block metadata SSH keys. Crabbox sets `enable-oslogin=FALSE`
  on each instance; keep OS Login from being force-enabled at the project or org
  level until Crabbox grows an OS Login integration.

## Related docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Capacity and fallback](../features/capacity-fallback.md)
- [Tailscale](../features/tailscale.md)
- [Checkpoints](../features/checkpoints.md)
- [Lifecycle and cleanup](../features/lifecycle-cleanup.md)

## Brokered address readiness

The coordinator checks the instance address immediately, then waits 250 ms,
500 ms, 1 s, 2 s, 4 s, and at most 5 s between reads. The existing two-minute
address deadline is unchanged; the last wait is clipped and no new read starts
at the deadline. A response arriving after the deadline is not accepted as ready.
In-flight calls retain the provider client's existing transport/retry behavior.
Provider errors, including exhausted throttling responses, propagate unchanged;
the polling loop does not add rapid retries or override provider backoff.
This reduces the observation delay for an early address, not the VM's boot time.

## Coordinator preparation reuse

The coordinator caches an exact firewall verification in memory for up to five
minutes, scoped to its environment, credential generation, project, firewall
name, network, and desired policy. Creates with the same policy during that
window skip the firewall GET. Cache hits do not extend the window, and cold
coordinator instances always fetch. The cache is bounded to 128 policies and
contains no credentials or durable ownership claims.

Only a successful GET of a Crabbox-managed firewall can populate the cache. It
must exactly match the desired network, ingress direction, priority, enabled
state, source CIDRs, target tags, and TCP ports, with no additional selectors or
deny rules. Updates and inserts require a subsequent exact GET before reuse.
Changed policies and credentials require fresh verification. GCP request errors
(including authentication, missing resources, transport, and malformed responses),
operation errors, and address-readiness timeouts discard cached verifications;
an in-flight older read cannot refill an invalidated entry.

This introduces a bounded drift window: external firewall edits or deletion can
remain undetected for up to five minutes after verification starts. A cache hit
does not change or widen access; it proves only that the exact desired policy
recently existed, not that it is still unchanged. External widening can therefore
persist until the next uncached create reconciles it; external narrowing or
deletion can interrupt SSH. After expiry, the next create reads the firewall and
reconciles drift as before. Cleanup and instance ownership checks never use this
cache.

On a cache miss, an exact match still skips the update and global-operation
wait. Drift follows the existing update-and-wait path; unmanaged rules remain an
error.

OAuth tokens are reused in memory across provider instances belonging to the
same coordinator environment and credential generation. Service-account tokens
refresh one minute before expiry; metadata tokens refresh five minutes before
expiry. Credential changes discard the shared cache, concurrent misses share
one exchange, and failed exchanges are never cached. Tokens are not persisted.
Previously, each new provider instance started with an empty client cache, so
`gcp.token_mint` could recur on successive creates. Cold coordinator instances
and token expiry still incur that step.

`gcp.firewall_get`, `gcp.firewall_put`, `gcp.firewall_insert`, and
`gcp.disk_instance_insert` timings measure the Compute HTTP request and response
processing after token acquisition. Older coordinator versions included token
acquisition in these steps as well as in `gcp.token_mint`, so those durations
overlapped; they were not independent network-latency measurements. A firewall
cache hit emits no `gcp.firewall_get` step because no GET was sent.
