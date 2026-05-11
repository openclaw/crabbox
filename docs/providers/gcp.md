# Google Cloud Provider

Read when:

- choosing `provider: gcp`;
- setting up Compute Engine credentials for Crabbox;
- debugging GCP quotas, machine types, firewall rules, labels, or cleanup;
- changing `internal/providers/gcp`, `internal/cli/gcp.go`, or `worker/src/gcp.ts`.

Google Cloud is a managed SSH lease provider for Linux Compute Engine VMs.
Crabbox provisions the VM, SSH metadata, labels, boot disk, public IP, and a
Crabbox-managed SSH firewall rule. After the VM exists, the normal Crabbox SSH
path owns readiness, sync, command execution, results, touch labels, release,
and cleanup.

## When To Use

Use GCP when:

- your billing, quota, or compliance boundary is already in Google Cloud;
- you want Linux Compute Engine capacity behind the shared coordinator;
- you need direct local testing with Google Application Default Credentials.

Use AWS for Windows, Windows WSL2, macOS, image bake/promote, or Linux desktop
leases. GCP is Linux-only today and does not provide a Crabbox image
bake/promote path yet.

Provider names:

```text
gcp
google
google-cloud
```

`google` and `google-cloud` are aliases. Crabbox canonicalizes them to `gcp`
before direct or brokered lease requests, including class default selection.

## Quick Start

Direct local smoke:

```sh
gcloud auth application-default login
gcloud services enable compute.googleapis.com --project <project-id>

export GOOGLE_CLOUD_PROJECT=<project-id>
export GOOGLE_APPLICATION_CREDENTIALS="$HOME/.config/gcloud/application_default_credentials.json"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
printf 'provider: gcp\n' > "$tmp"
env -u CRABBOX_COORDINATOR -u CRABBOX_COORDINATOR_TOKEN \
  CRABBOX_CONFIG="$tmp" \
  crabbox run --provider gcp --type e2-micro --market on-demand --no-sync -- \
  echo gcp-ok
```

Normal class-based lease:

```sh
crabbox warmup --provider gcp --class standard
crabbox run --provider gcp --class fast -- pnpm test
crabbox ssh --provider gcp --id blue-lobster
crabbox stop --provider gcp blue-lobster
crabbox cleanup --provider gcp
```

`--type` is exact, for example `--type c4-standard-32` or `--type e2-micro`.
Use `--class` when you want Crabbox to retry the provider's class candidates.

## Config

```yaml
provider: gcp
target: linux
class: beast
gcp:
  project: example-project-123
  zone: europe-west2-a
  image: projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64
  network: default
  subnet: ""
  tags:
    - crabbox-ssh
  sshCIDRs: []
  rootGB: 400
  serviceAccount: ""
```

Project resolution order is `CRABBOX_GCP_PROJECT`, `gcp.project`,
`GOOGLE_CLOUD_PROJECT`, then `GCP_PROJECT_ID`. Brokered requests only forward
the Crabbox-specific project sources; ambient ADC project variables stay local
so Worker defaults can apply.

Direct-mode environment:

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

Capacity environment:

```text
CRABBOX_CAPACITY_MARKET
CRABBOX_CAPACITY_FALLBACK
CRABBOX_CAPACITY_AVAILABILITY_ZONES
```

`capacity.availabilityZones` controls GCP zone fallback. `capacity.regions`
does not expand into zones for GCP today.

## Direct Auth

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
gcloud services enable compute.googleapis.com --project <project-id>
gcloud compute zones list --project <project-id> --filter='name=europe-west2-a'
```

Common blockers:

- Compute Engine API disabled: enable `compute.googleapis.com`.
- Billing disabled: attach billing before enabling Compute.
- Missing IAM: the active account needs enough Compute permissions to create,
  label, list, and delete instances, plus permission to manage the shared
  firewall rule.
- Service Usage denied: the account may still run Compute calls, but cannot
  list or enable APIs.

For a cheap live smoke, use `--type e2-micro --market on-demand
--no-sync --ttl 20m --idle-timeout 5m`. This proves instance creation, SSH
metadata, cloud-init, SSH readiness, command execution, and release/delete
without syncing a repository.

## Brokered Auth

Brokered mode uses Worker-side service-account credentials. Developer machines
do not need Google credentials when the coordinator owns provisioning.
The Worker uses Compute REST calls with the configured service account and
lists pool state through aggregated instance listing with partial success
enabled, so one unhealthy zone does not hide healthy Crabbox VMs elsewhere.

Required Worker secrets:

```text
GCP_PROJECT_ID
GCP_CLIENT_EMAIL
GCP_PRIVATE_KEY
```

Worker defaults:

```text
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

Run:

```sh
crabbox doctor --provider gcp
```

The readiness check reports missing secret names without exposing values.
Lease creation fails with `provider_not_configured` until the Worker has the
service-account credentials.

## Lifecycle

1. Resolve project, zone, image, network, disk, tags, and credentials.
2. Ensure a Crabbox-managed SSH firewall exists for the configured network,
   SSH ports, CIDRs, and target tags.
3. Create a Compute Engine instance with Ubuntu cloud-init, SSH metadata, and
   Crabbox labels.
4. Attach an optional service account when `gcp.serviceAccount` or
   `CRABBOX_GCP_SERVICE_ACCOUNT` is set.
5. For Spot leases, set GCP scheduling to `SPOT`, `TERMINATE`, and
   termination action `DELETE`.
6. Wait for the public IP, then wait for SSH and the Crabbox ready marker.
7. Touch labels during active runs.
8. Delete the VM on release unless the lease is kept.

## Machine Classes

```text
standard  c4-standard-32, c3-standard-22, n2-standard-32, n2d-standard-32
fast      c4-standard-64, c3-standard-44, n2-standard-64, n2d-standard-64, c4-standard-32
large     c4-standard-96, c3-standard-88, n2-standard-80, n2d-standard-96, c4-standard-64
beast     c4-standard-192, c4-standard-96, c3-standard-176, c3-standard-88, n2d-standard-224, n2-standard-128
```

`capacity.market: spot` maps to GCP Spot VMs. If `capacity.fallback` starts
with `on-demand`, Crabbox retries the same zone/type candidates as on-demand
after retryable Spot capacity or quota failures.

Explicit `--type` disables class candidate fallback. Zone fallback and
Spot-to-on-demand fallback still apply to the exact requested type when GCP
returns a quota, capacity, rate-limit, or unavailable-type error.

## Networking

The provider uses `gcp.network` and optional `gcp.subnet`.

- If either value is a full self link, Crabbox uses it as-is.
- Otherwise `gcp.network` becomes
  `projects/<project>/global/networks/<name>`.
- Otherwise `gcp.subnet` becomes
  `projects/<project>/regions/<region>/subnetworks/<name>`, where region is
  derived from the zone.

The default firewall allows SSH ingress from `0.0.0.0/0` when no CIDRs are
configured. Set `gcp.sshCIDRs` or `CRABBOX_GCP_SSH_CIDRS` for tighter ingress.
The default VPC/default ingress policy uses firewall rule `crabbox-ssh`;
custom networks use `crabbox-ssh-<network>`. Non-default CIDRs, tags, or SSH
ports add a policy hash suffix so leases with different ingress settings do
not rewrite each other's SSH access.

Explicit `gcp.tags` or `CRABBOX_GCP_TAGS` replace the default target tags for
that lease. They are not merged with the default `crabbox-ssh` tag.

Crabbox refuses to update an existing matching firewall unless its description
marks it as Crabbox-managed. Rename the firewall, change tags, or adopt it
intentionally if an older rule already exists.

## Labels And Cleanup

GCP labels must be lowercase and label keys must start with a letter. Crabbox
sanitizes keys separately from values so numeric lease metadata remains
parseable.

Important cleanup labels:

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

Cleanup lists Crabbox-labeled instances across the visible project zones by
using aggregated instance listing with partial success enabled. It deletes
expired or released leases in the zone recorded on the VM. Brokered cleanup is
coordinator-owned; direct cleanup is best-effort label cleanup.

## Troubleshooting

`Compute Engine API has not been used ... or it is disabled`

Enable the Compute API for the selected project:

```sh
gcloud services enable compute.googleapis.com --project <project-id>
```

`Billing account ... is not found`

Attach billing to the project before enabling Compute or creating instances.

`PERMISSION_DENIED` from Service Usage

The active account cannot enable or list APIs for the project. Use an account
with Service Usage permissions, or ask a project admin to enable Compute.

`get gcp firewall` or `create gcp firewall` fails

Check network name, IAM, and whether a non-Crabbox firewall already owns the
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

- Linux only.
- No GCP Windows, WSL2, macOS, VNC/browser/code, or image bake/promote yet.
- No provider pricing lookup yet; cost uses the generic managed-provider
  fallback rate.
- OS Login must not block metadata SSH keys. Keep OS Login disabled for the
  project or instance until Crabbox grows an OS Login integration.

Related docs:

- [Provider reference](README.md)
- [Provider overview](../features/providers.md)
- [Provider backends](../provider-backends.md)
- [Capacity and fallback](../features/capacity-fallback.md)
