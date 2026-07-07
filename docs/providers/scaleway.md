# Scaleway Provider

Read this when you are:

- choosing `provider: scaleway`;
- validating Scaleway SDK config and auth material discovery;
- changing `internal/providers/scaleway` or guarded live smoke support.

Scaleway is registered as a Linux-only **SSH lease** provider. The provider
implements the normal Crabbox SSH, sync, cleanup, and Tailscale command
surface for Scaleway Instances. It validates Scaleway SDK credentials plus
non-secret provider config, creates per-lease Scaleway IAM SSH keys, provisions
Instances with Crabbox ownership tags and cloud-init bootstrap, and deletes only
resources whose live Scaleway tags still prove Crabbox ownership.

Scaleway is **direct-only**. It does not run through the coordinator, so the
local CLI must have Scaleway credentials and direct cleanup remains the
operator's responsibility.

## When To Use It

Use Scaleway for simple Linux lease work when a Scaleway Instance is the desired
execution surface and direct local credentials are acceptable. Prefer AWS,
Azure, GCP, or Hetzner when you need a brokered team path, coordinator-side
credentials, or integrated cloud cost accounting.

## Commands

SDK config and auth material discovery can be checked without creating
resources:

```sh
crabbox doctor --provider scaleway
```

The provider is registered for the normal SSH-lease command surface:

```sh
crabbox warmup --provider scaleway --class standard
crabbox run --provider scaleway --type DEV1-S -- pnpm test
crabbox ssh --provider scaleway --id my-app
crabbox stop --provider scaleway my-app
crabbox cleanup --provider scaleway --dry-run
```

Those commands create, inspect, resolve, touch, release, and clean up Scaleway
Instances through the local Scaleway SDK profile. They are cost-bearing when
they create live Instances, so use `doctor` and `cleanup --dry-run` before
running live workflows.

## Live Smoke

The provider-specific live smoke is guarded by `CRABBOX_LIVE=1` and an explicit
provider selection. It builds `bin/crabbox` unless `CRABBOX_BIN` points at an
existing binary, verifies the Crabbox-owned Scaleway inventory starts empty,
creates one short-lived Instance, proves status, command execution, list JSON,
cleanup, and final empty inventory.

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=scaleway CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
# or, directly:
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=scaleway scripts/live-scaleway-smoke.sh
```

The script emits `classification=environment_blocked`,
`classification=quota_blocked`, `classification=validation_failed`, or
`classification=cleanup_failed` on stderr when a required credential, quota,
provider response, or cleanup invariant blocks the smoke. `SCW_ACCESS_KEY` and
`SCW_SECRET_KEY` are redacted from captured command output before printing.

`--type` is the exact Scaleway Instances commercial type, such as `DEV1-S`.
There is no separate Scaleway size flag for the generic lease commands.

## Configuration

```yaml
provider: scaleway
target: linux
class: standard
scaleway:
  region: fr-par
  zone: fr-par-1
  image: ubuntu_noble
  type: DEV1-S
  projectId: "<scaleway-project-id>"
  organizationId: "<scaleway-organization-id>"
  securityGroup: ""
  sshCIDRs: []
```

Config keys under `scaleway:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `region` | `cfg.Scaleway.Region` | `fr-par` | Scaleway region. |
| `zone` | `cfg.Scaleway.Zone` | `fr-par-1` | Scaleway zone used for Instances and local image lookup. |
| `image` | `cfg.Scaleway.Image` | `ubuntu_noble` | Scaleway image label or ID. |
| `type` | `cfg.Scaleway.Type` | `DEV1-S` | Scaleway Instances commercial type. |
| `projectId` | `cfg.Scaleway.ProjectID` | empty | Required through config, env, or the active Scaleway SDK profile before the SDK client is usable. |
| `organizationId` | `cfg.Scaleway.OrganizationID` | empty | Optional override for SDK profile organization identity. |
| `securityGroup` | `cfg.Scaleway.SecurityGroup` | empty | Optional existing Scaleway security group ID attached at Instance creation. Crabbox does not create or mutate security groups. |
| `sshCIDRs` | `cfg.Scaleway.SSHCIDRs` | empty | Reserved for future security-group mutation. Non-empty values fail fast because this provider does not create ingress rules. |

The active Scaleway SDK profile and `SCW_DEFAULT_REGION`/`SCW_DEFAULT_ZONE`
select the location when Crabbox location settings are not explicit. A
`scaleway.region`/`scaleway.zone` config value, matching `CRABBOX_SCALEWAY_*`
environment override, or provider-specific flag takes precedence; the table
defaults apply only when neither source selects a value.

Provider-specific flags:

```text
--scaleway-region <region>
--scaleway-zone <zone>
--scaleway-image <image-label-or-id>
--scaleway-type <commercial-type>
--scaleway-project-id <project-id>
--scaleway-organization-id <organization-id>
--scaleway-security-group <security-group-id>
--scaleway-ssh-cidrs <cidr[,cidr...]>
```

The portable `--os ubuntu:24.04` selector maps to `ubuntu_noble`. Other
explicit portable OS selectors are rejected unless `scaleway.image`,
`CRABBOX_SCALEWAY_IMAGE`, or `--scaleway-image` provides an explicit Scaleway
image label or ID.

Scaleway leases default to `root` on SSH port `22` with no fallback port.
Explicit generic `ssh.user` and `ssh.port` values remain authoritative. The
effective values appear in `crabbox config show` without retaining defaults
from another provider when a command overrides the provider.

Environment overrides:

```text
CRABBOX_SCALEWAY_REGION             Override the region
CRABBOX_SCALEWAY_ZONE               Override the zone
CRABBOX_SCALEWAY_IMAGE              Override the image label or ID
CRABBOX_SCALEWAY_TYPE               Override the commercial type
CRABBOX_SCALEWAY_PROJECT_ID         Override the project ID
CRABBOX_SCALEWAY_ORGANIZATION_ID    Override the organization ID
CRABBOX_SCALEWAY_SECURITY_GROUP     Override the security group ID
CRABBOX_SCALEWAY_SSH_CIDRS          Comma-separated SSH CIDRs; currently fails fast when non-empty
```

## Credentials

Crabbox uses the official Scaleway SDK config surfaces and environment profile.
It does not store Scaleway secrets in Crabbox config and does not accept them as
provider-specific flags. `doctor` proves that required auth material and
project configuration are discoverable enough to construct the SDK client; it
does not create resources or perform a live authorization probe.

Supported Scaleway credential and profile inputs include:

```text
SCW_ACCESS_KEY
SCW_SECRET_KEY
SCW_DEFAULT_ORGANIZATION_ID
SCW_DEFAULT_PROJECT_ID
SCW_DEFAULT_REGION
SCW_DEFAULT_ZONE
SCW_PROFILE
SCW_CONFIG_PATH
```

`SCW_ACCESS_KEY` and `SCW_SECRET_KEY`, or equivalent Scaleway SDK profile
credentials, are required. A project ID is also required through
`SCW_DEFAULT_PROJECT_ID`, `CRABBOX_SCALEWAY_PROJECT_ID`, or the active SDK
profile's `default_project_id`.

Do not pass Scaleway credentials as command-line arguments. Keep them in the
environment, the Scaleway SDK config, or a local secret manager. Crabbox
redacts configured Scaleway env values from SDK client errors before returning
them.

## Lifecycle

The provider implements a direct Linux SSH lease over Scaleway Instances:

1. Generate a per-lease SSH key under the Crabbox testbox key directory.
2. Create the matching project-scoped Scaleway IAM SSH key.
3. Create a Scaleway Instance with `region`, `zone`, `image`, `type`, project,
   SSH key, optional security group, cloud-init user data, and Crabbox tags.
4. Wait for a public IPv4 address and Crabbox SSH bootstrap readiness.
5. Add ready-state Crabbox tags and claim the lease locally.
6. Run normal Crabbox sync/run/ssh workflows over SSH.
7. Update timeout tags on touch.
8. Delete owned Scaleway Instances and managed IAM SSH keys on `stop`; `cleanup`
   deletes only resources with complete Crabbox Scaleway ownership tags and an
   exact project-, zone-, and server-bound local claim.

`list` uses all-pages Scaleway inventory. `resolve` may inspect complete,
canonical live ownership tags without a claim, but reuse requires explicit
supported `--reclaim` adoption. Recovery claims retain enough Scaleway identity
to finish release or cleanup after interrupted acquire paths.

## Ownership And Cleanup

The foundation tag helpers encode intended ownership tags such as:

```text
crabbox
crabbox:provider:scaleway
crabbox:lease:cbx_abcdef123456
crabbox:slug:my-app
crabbox:target:linux
crabbox:expires_at:<unix-seconds>
```

Read-only inventory requires a complete ownership predicate: Crabbox marker,
provider marker, canonical lease id, slug, and Linux target. Reuse and deletion
also require an exact local claim for the same Scaleway project, zone, lease,
and server id. Resources with partial, foreign, malformed, claimless, or
mismatched ownership are skipped or refused. A local claim alone is not enough
for destructive release: Crabbox re-fetches the live Scaleway Instance by ID
and validates current tags before deletion. If Scaleway confirms the Instance
is already absent, release removes the managed SSH key and local claim
idempotently.

`crabbox cleanup --provider scaleway --dry-run` reports expired owned resources
without deleting them. Use the Scaleway console or `scw` only for manual account
inspection; do not treat manual cleanup as Crabbox lifecycle proof.

## Guarded Live Smoke

The guarded opt-in command builds `bin/crabbox`, verifies the current Scaleway
Crabbox inventory is empty, creates a short-lived `DEV1-S` lease by default,
waits for readiness, runs `echo ok`, verifies the active lease appears in
`list --json`, stops it, runs `cleanup --dry-run`, and verifies the final
inventory is empty.

Run it only when live Scaleway credentials, IDs, region, zone, and quota are
available:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=scaleway scripts/live-scaleway-smoke.sh
```

Required environment:

```text
CRABBOX_LIVE=1
CRABBOX_LIVE_PROVIDERS=scaleway
SCW_ACCESS_KEY
SCW_SECRET_KEY
SCW_DEFAULT_ORGANIZATION_ID or CRABBOX_SCALEWAY_ORGANIZATION_ID
SCW_DEFAULT_PROJECT_ID or CRABBOX_SCALEWAY_PROJECT_ID
SCW_DEFAULT_REGION or CRABBOX_SCALEWAY_REGION
SCW_DEFAULT_ZONE or CRABBOX_SCALEWAY_ZONE
```

Optional smoke defaults:

```text
CRABBOX_SCALEWAY_TYPE=DEV1-S
CRABBOX_SCALEWAY_IMAGE=ubuntu_noble
CRABBOX_SCALEWAY_CLEANUP_ATTEMPTS=65
```

Expected classifications:

```text
classification=environment_blocked reason=CRABBOX_LIVE_not_enabled
classification=environment_blocked reason=scaleway_not_selected
classification=environment_blocked reason=SCW_ACCESS_KEY_missing
classification=environment_blocked reason=SCW_SECRET_KEY_missing
classification=environment_blocked reason=SCW_DEFAULT_ORGANIZATION_ID_missing
classification=environment_blocked reason=SCW_DEFAULT_PROJECT_ID_missing
classification=quota_blocked
classification=validation_failed
classification=cleanup_failed
classification=live_scaleway_smoke_passed
```

`environment_blocked` means local live gates, credentials, IDs, region, or zone
are unavailable. `quota_blocked` means Scaleway rejected the create path with
quota, capacity, rate-limit, or account-limit language. `validation_failed`
means the smoke's safety checks failed, such as non-empty initial inventory or
unexpected list JSON. `cleanup_failed` means the targeted stop retry loop could
not prove cleanup. The script redacts the configured Scaleway access and secret
keys from captured command output before printing.

## Capabilities

- **SSH** and **Crabbox sync**: implemented through the normal Linux SSH lease
  path.
- **Tailscale**: declared through the standard Linux cloud-init path.
- **Desktop / browser / code**: not advertised.
- **Cleanup**: implemented for Crabbox-owned Scaleway Instances and managed IAM
  SSH keys.
- **Coordinator**: never; direct CLI only.

## Gotchas

- `scaleway` has no aliases; use the canonical provider name.
- `scaleway` is direct-only. Coordinator secrets and cost accounting do not
  cover these Instances.
- `crabbox doctor --provider scaleway` checks SDK config and auth material
  discovery, but it does not create resources or prove credentials are
  authorized.
- `warmup`, `run`, `ssh`, `stop`, `list`, and `cleanup` are live command
  surfaces that can create or delete Scaleway resources.
- `--type` must be a valid Scaleway Instances commercial type such as `DEV1-S`.
- `securityGroup` must name an existing Scaleway security group. Non-empty
  `sshCIDRs` fails fast because this branch does not create or mutate Scaleway
  firewall/security-group rules.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Provider feature overview](../features/providers.md)
- [Operations](../operations.md)
