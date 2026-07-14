# OVHcloud Provider

Read this when you are:

- choosing `provider: ovh`;
- validating a direct OVHcloud Public Cloud lease;
- changing `internal/providers/ovh` or the guarded live smoke.

OVHcloud is a Linux-only **SSH lease** provider. Crabbox creates a Public Cloud
instance, creates a per-lease OVH SSH key, records Crabbox ownership metadata in
a local lease claim, waits for a public IPv4 address and SSH bootstrap
readiness, and then uses the normal Crabbox SSH sync/run/stop/cleanup path.

OVHcloud is **direct-only** in this release. It does not run through the
Cloudflare Worker broker, so the local CLI must have OVH credentials and direct
cleanup remains the operator's responsibility.

## When To Use It

Use OVHcloud for simple Linux lease work when an OVH Public Cloud instance is
the desired execution surface and direct local credentials are acceptable.
Prefer AWS, Azure, GCP, or Hetzner when you need a brokered team path,
coordinator-side credentials, or Crabbox cost accounting.

## Commands

```sh
crabbox doctor --provider ovh
crabbox warmup --provider ovh --class standard
crabbox run --provider ovh --type b3-8 -- pnpm test
crabbox ssh --provider ovh --id my-app
crabbox stop --provider ovh my-app
crabbox cleanup --provider ovh --dry-run
```

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the OVH
instance id. `--type` is the exact OVH flavor name or id; there is no separate
OVH flavor flag for the generic lease commands.

## Configuration

```yaml
provider: ovh
target: linux
ovh:
  endpoint: https://api.us.ovhcloud.com/1.0
  projectId: "<public-cloud-project-id>"
  region: BHS5
  image: "Ubuntu 24.04"
  flavor: b3-8
```

Config keys under `ovh:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `endpoint` | `cfg.OVH.Endpoint` | `https://api.us.ovhcloud.com/1.0` | Must be an HTTPS OVH or OVHcloud API host. Endpoint aliases `ovh-us`, `ovh-ca`, and `ovh-eu` are accepted by the client. Repo config may not redirect an inherited endpoint. |
| `projectId` | `cfg.OVH.ProjectID` | empty | Required for doctor, list, warmup, status, stop, and cleanup. |
| `region` | `cfg.OVH.Region` | empty | Required when acquiring a new instance. Doctor verifies it is returned by the project when set. |
| `image` | `cfg.OVH.Image` | `Ubuntu 24.04` | Public active Debian or Ubuntu image name or exact id. Names must be unique; non-public images require an exact id. Other Linux distributions are rejected because the managed bootstrap currently uses Debian-family package and service conventions. Explicit values bypass the portable `--os` mapping guard. |
| `flavor` | `cfg.OVH.Flavor` | `b3-8` | Available Linux flavor name or exact id with remaining project quota. Names must be unique. `--type` overrides this value for new leases. |

Provider-specific flags:

```text
--ovh-endpoint <url-or-alias>
--ovh-project-id <project-id>
--ovh-region <region>
--ovh-image <image-name-or-id>
--ovh-flavor <flavor-name-or-id>
```

Environment overrides:

```text
OVH_ENDPOINT                  Override the OVH API endpoint
OVH_APPLICATION_KEY           OVH application key
OVH_APPLICATION_SECRET        OVH application credential secret
OVH_CONSUMER_KEY              OVH consumer key
CRABBOX_OVH_PROJECT_ID        Override the Public Cloud project id
CRABBOX_OVH_REGION            Override the region
CRABBOX_OVH_IMAGE             Override the image name or id
CRABBOX_OVH_FLAVOR            Override the flavor name or id
```

Do not pass OVH credentials as command-line arguments. Keep them in the
environment or in a local secret manager. Crabbox config stores only non-secret
OVH settings.

## Credential Scope

The provider signs requests with OVH application credentials and operates only
inside the configured Public Cloud project. It calls the Public Cloud project
APIs for regions, flavors, images, SSH keys, and instances, and calls
`/auth/time` for request signing.

The credentials must be able to:

```text
GET    /auth/time
GET    /cloud/project
GET    /cloud/project/{projectId}/region
GET    /cloud/project/{projectId}/flavor
GET    /cloud/project/{projectId}/flavor/{flavorId}
GET    /cloud/project/{projectId}/image
GET    /cloud/project/{projectId}/image/{imageId}
GET    /cloud/project/{projectId}/sshkey
GET    /cloud/project/{projectId}/sshkey/{keyId}
POST   /cloud/project/{projectId}/sshkey
DELETE /cloud/project/{projectId}/sshkey/{keyId}
GET    /cloud/project/{projectId}/instance
GET    /cloud/project/{projectId}/instance/{instanceId}
POST   /cloud/project/{projectId}/instance
DELETE /cloud/project/{projectId}/instance/{instanceId}
```

If a live smoke fails with a permission error, keep the error output
secret-safe and adjust the OVH credential grants before retrying. Do not broaden
permissions inside scripts.

## Lifecycle

1. Validate required project and region configuration.
2. Resolve exact ids before names, reject ambiguous image or flavor names, and
   verify Debian/Ubuntu image status plus flavor availability and project quota
   before creating credentials.
3. Generate a per-lease SSH key under the Crabbox testbox key directory.
4. Create a matching OVH project SSH key.
5. Create an instance with region, flavor, image, SSH key, and cloud-init
   `userData`.
6. Wait for a public IPv4 address and Crabbox SSH bootstrap readiness.
7. Record a local claim for the lease and run normal Crabbox sync/run/ssh
   workflows over SSH.
8. Delete the instance and managed OVH SSH key on `stop`; `cleanup` deletes only
   resources with complete Crabbox OVH ownership metadata and a matching local
   claim.

If instance creation returns an indeterminate error after a key or instance may
exist, Crabbox records a recovery claim so `crabbox stop --provider ovh
<lease-or-slug>` can retry cleanup instead of leaving a billed resource without
its SSH key.

## Ownership And Cleanup

Crabbox-owned OVH leases use a local claim with ownership metadata such as:

```text
crabbox=true
created_by=crabbox
provider=ovh
lease=cbx_abcdef123456
slug=my-app
target=linux
state=ready
ovh_project=<public-cloud-project-id>
ovh_region=BHS5
ovh_ssh_key_id=<ovh-ssh-key-id>
ovh_ssh_key_owned=true
expires_at=<unix-seconds>
```

Release and cleanup require a complete ownership predicate: Crabbox marker,
provider marker, lease id, slug, project identity, and a matching local claim.
Instances with partial, foreign, or malformed Crabbox-like metadata are skipped
or refused.

Direct mode has no coordinator alarm. Use:

```sh
crabbox list --provider ovh --json
crabbox cleanup --provider ovh --dry-run
crabbox cleanup --provider ovh
```

## Guarded Live Smoke

The repeatable live check is opt-in:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=ovh scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=ovh scripts/live-ovh-smoke.sh
```

The top-level script dispatches to the provider-specific script. The
provider-specific script builds `bin/crabbox` unless `CRABBOX_BIN` is set, reads
OVH credentials and project settings from the environment, requires an empty
Crabbox-owned OVH inventory, creates a small `b3-8` instance by default, waits
for ready status, runs `echo ok`, verifies `list --json`, stops the lease, runs
dry-run cleanup, and verifies the Crabbox-owned inventory is empty afterward.

Optional smoke overrides:

```text
CRABBOX_LIVE_OVH_ENDPOINT
CRABBOX_LIVE_OVH_PROJECT_ID
CRABBOX_LIVE_OVH_REGION
CRABBOX_LIVE_OVH_IMAGE
CRABBOX_LIVE_OVH_FLAVOR
```

Final classifications include:

```text
classification=live_ovh_smoke_passed
classification=environment_blocked
classification=quota_blocked
classification=validation_failed
```

If cleanup fails, use the reported slug and local claim metadata to inspect the
instance in `crabbox list --provider ovh --json` or the OVHcloud console.

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Tailscale**: yes through the standard Linux cloud-init path when a direct
  Tailscale auth key is configured.
- **Desktop / browser / code**: not advertised in Phase 1.
- **Cleanup**: yes, claim-backed only.
- **Coordinator**: never; direct CLI only.

## Gotchas

- `ovh` has no aliases; use the canonical provider name.
- `ovh` is direct-only. Worker broker secrets and cost accounting do not cover
  these instances.
- `--type` must be a valid OVH flavor name or id such as `b3-8`.
- Portable `--os` currently maps only the default `ubuntu:24.04` flow unless
  `ovh.image`, `CRABBOX_OVH_IMAGE`, or `--ovh-image` sets an explicit image.
- OVH endpoint URLs are restricted to OVH or OVHcloud API hosts.
- Restrict SSH exposure through project/network policy where needed, and prefer
  short TTLs for live validation.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Operations](../operations.md)
