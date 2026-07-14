# Proxmox Provider

Read this when you:

- choose `provider: proxmox`;
- set up a Proxmox VE VM template for Crabbox;
- debug Proxmox API tokens, clone tasks, guest-agent IP discovery, or cleanup;
- change `internal/providers/proxmox` or `internal/cli/proxmox.go`.

Proxmox is a direct SSH-lease provider for Linux QEMU VMs on a self-hosted
Proxmox VE cluster. For each lease Crabbox clones a configured QEMU template,
injects a per-lease SSH key through cloud-init, uses the QEMU guest agent to
discover the VM's IPv4 address, runs the Crabbox Linux bootstrap over SSH, and
then drives the normal SSH sync/run/release path.

The provider is direct-only: it talks to the Proxmox API straight from the CLI.
The Crabbox coordinator (broker) does not provision or broker Proxmox capacity,
so brokered shared-team leases are not available here. Proxmox supports the
`ssh`, `crabbox-sync`, and `cleanup` features on `target=linux` only.

## When to use

Use Proxmox when:

- your test capacity lives on a private Proxmox VE cluster;
- you want reusable lab infrastructure instead of public-cloud VMs;
- your template already supports cloud-init and the QEMU guest agent.

Use [AWS](aws.md), [Azure](azure.md), [Google Cloud](gcp.md), or
[Hetzner](hetzner.md) when you need brokered, cost-capped shared-team leases.
Use the [Static SSH](ssh.md) provider when you already have a running host and
do not want Crabbox to clone or delete VMs.

Provider name:

```text
proxmox
```

## Template requirements

The configured template must be a Linux QEMU VM template with:

- a cloud-init drive configured;
- `net0` attached to an active bridge when `proxmox.bridge` is not set;
- the QEMU guest agent installed and enabled;
- DHCP networking (or an equivalent IP config the guest agent can report);
- outbound package access for `apt-get`;
- a cloud-init user with passwordless `sudo` so the bootstrap can run as root.

For each lease Crabbox sets the following on the clone:

- `ciuser` to the configured SSH user (`proxmox.user`, default `crabbox`);
- `sshkeys` to the generated per-lease public key;
- `ipconfig0=ip=dhcp`;
- `agent=enabled=1`;
- `net0=virtio,bridge=<bridge>` when `proxmox.bridge` is set;
- `description` carrying the Crabbox lease labels used for list/touch/cleanup;
- `tags=crabbox`.

If the guest agent never reports an IPv4 address, or SSH does not come up so the
bootstrap can run, provisioning fails and the cloned VM is deleted.

## Template build helper

For a reproducible starting point, the repo ships
`scripts/proxmox-build-template.sh`, which builds an Ubuntu 24.04 (Noble)
cloud-image template with cloud-init, OpenSSH, the QEMU guest agent, and common
sync/runtime tools preinstalled.

Run it from a Crabbox checkout on a Proxmox VE node as root:

```sh
CRABBOX_PROXMOX_TEMPLATE_ID=9400 \
CRABBOX_PROXMOX_TEMPLATE_NAME=crabbox-ubuntu-2404 \
CRABBOX_PROXMOX_STORAGE=local-lvm \
CRABBOX_PROXMOX_BRIDGE=vmbr0 \
CRABBOX_PROXMOX_USER=crabbox \
./scripts/proxmox-build-template.sh
```

The helper downloads a pinned Ubuntu Noble release image, verifies its built-in
SHA256 before conversion, customizes a local copy, imports it into the selected
storage, attaches a cloud-init drive, and converts the VM to a template. It does
not use Proxmox API tokens, Crabbox coordinator tokens, or lease SSH keys, and it
bakes no secrets into the image.

If the target VMID already exists, the helper stops before changing it. Set
`CRABBOX_PROXMOX_REPLACE_TEMPLATE=1` only when you intentionally want to destroy
and rebuild that local template.

Helper environment variables:

```text
CRABBOX_PROXMOX_TEMPLATE_ID        VMID to create (default: 9400)
CRABBOX_PROXMOX_TEMPLATE_NAME      template name (default: crabbox-ubuntu-2404)
CRABBOX_PROXMOX_STORAGE            target storage (default: local-lvm)
CRABBOX_PROXMOX_BRIDGE             network bridge (default: vmbr0)
CRABBOX_PROXMOX_USER               cloud-init user (default: crabbox)
CRABBOX_PROXMOX_IMAGE_URL          custom cloud image URL
CRABBOX_PROXMOX_IMAGE_SHA256       expected image sha256; required with a custom URL
CRABBOX_PROXMOX_CORES              template vCPU count (default: 2)
CRABBOX_PROXMOX_MEMORY_MB          template memory in MiB (default: 4096)
CRABBOX_PROXMOX_DISK_SIZE          root disk size, qm syntax (default: 32G)
CRABBOX_PROXMOX_REPLACE_TEMPLATE   destroy an existing VM/template first when 1
```

It requires Proxmox's `qm` and `pvesm` commands plus `qemu-img`,
`virt-customize`, and either `curl` or `wget`.

To update the built-in image, choose a dated directory under Ubuntu's
`https://cloud-images.ubuntu.com/releases/noble/` index, then update
`default_image_url` and `default_image_sha256` together from that directory's
signed `SHA256SUMS`. Do not point the built-in default at `current` or `release`;
those aliases change without a repository review.

The resulting Crabbox config looks like:

```yaml
provider: proxmox
proxmox:
  node: pve1
  templateId: 9400
  storage: local-lvm
  bridge: vmbr0
  user: crabbox
```

## Quick start

Create an API token in Proxmox and grant it permission to inspect the target
node, storage, network bridge, template VM, and QEMU inventory. Before the
first lease, run the non-mutating doctor check:

```sh
export CRABBOX_PROXMOX_API_URL=https://pve.example.com:8006
export CRABBOX_PROXMOX_TOKEN_ID='crabbox@pve!ci'
export CRABBOX_PROXMOX_TOKEN_SECRET='<token-secret>'
export CRABBOX_PROXMOX_NODE=pve1
export CRABBOX_PROXMOX_TEMPLATE_ID=9400

crabbox doctor --provider proxmox --json
crabbox warmup --provider proxmox --keep
crabbox run --provider proxmox --no-sync -- echo proxmox-ok
crabbox ssh --provider proxmox --id swift-crab
crabbox stop --provider proxmox swift-crab
crabbox cleanup --provider proxmox
```

For self-signed private clusters, set `CRABBOX_PROXMOX_INSECURE_TLS=1` or pass
`--proxmox-insecure-tls`.

`doctor --provider proxmox` is read-only. It checks authentication with
`/version`, then verifies the configured node status, storage list, network
bridge list, template VM/config, `/cluster/nextid`, and QEMU inventory. An
explicit target storage must support `images`, and every source storage
referenced by the template must be active and enabled. VM disk, cloud-init,
EFI, and TPM stores must support `images`; CD-ROM stores must support `iso`.
The configured template must have a cloud-init drive and, when no bridge
override is set, `net0` on an active bridge. A configured pool must be readable.
The inventory check requires propagated `VM.Audit` on `/vms` before reading
cluster-wide QEMU inventory. Release recovery checks the exact target
`/vms/<vmid>` path before accepting absence. Cluster reconciliation also fails
closed if any candidate VM config cannot be read, so a permission-filtered or
partially unreadable `/cluster/resources` response is not treated as complete.
The output includes `mutation=false`; it does not clone, configure, start,
stop, or delete VMs.

## Configuration

```yaml
provider: proxmox
target: linux
proxmox:
  apiUrl: https://pve.example.com:8006
  tokenId: crabbox@pve!ci
  tokenSecret: <token-secret>
  node: pve1
  templateId: 9400
  storage: local-lvm
  pool: crabbox
  bridge: vmbr0
  user: crabbox
  workRoot: /work/crabbox
  fullClone: true
  insecureTLS: false
```

`apiUrl`, `tokenId`, `tokenSecret`, `node`, and `templateId` are required for
leasing. Doctor can still construct the API client without `templateId` so it
can report a structured failed `template` check. `user` defaults to `crabbox`,
`workRoot` defaults to `/work/crabbox`, and `fullClone` defaults to `true`. The
API URL may include or omit a trailing `/api2/json`; Crabbox normalizes it.

Keep secret values in a private config file with `0600` permissions, in
`~/.profile`, or in a secret manager. Do not pass token secrets as CLI
arguments.

Environment variables (each overrides the matching config field):

```text
CRABBOX_PROXMOX_API_URL
CRABBOX_PROXMOX_TOKEN_ID
CRABBOX_PROXMOX_TOKEN_SECRET
CRABBOX_PROXMOX_NODE
CRABBOX_PROXMOX_TEMPLATE_ID
CRABBOX_PROXMOX_STORAGE
CRABBOX_PROXMOX_POOL
CRABBOX_PROXMOX_BRIDGE
CRABBOX_PROXMOX_USER
CRABBOX_PROXMOX_WORK_ROOT
CRABBOX_PROXMOX_FULL_CLONE
CRABBOX_PROXMOX_INSECURE_TLS
```

Provider flags mirror the non-secret config fields:

```text
--proxmox-api-url
--proxmox-node
--proxmox-template-id
--proxmox-storage
--proxmox-pool
--proxmox-bridge
--proxmox-user
--proxmox-work-root
--proxmox-full-clone
--proxmox-insecure-tls
```

There is intentionally no `--proxmox-token-secret` flag, so the token secret
never appears in shell history or process arguments. Supply it through
`CRABBOX_PROXMOX_TOKEN_SECRET` or the config file instead.

## Readiness and token permissions

`crabbox doctor --provider proxmox` is the readiness gate for this provider. In
JSON mode, provider checks use stable names and details:

```sh
crabbox doctor --provider proxmox --json
```

Expected Proxmox-specific checks:

```text
auth       /version accepts the API token
node       /nodes/<node>/status is readable
storage    clone target and all template source stores are image-capable
bridge     configured bridge or the template net0 bridge is active
template   /nodes/<node>/qemu and /config show templateId is a QEMU template
nextid     /cluster/nextid is readable
pool       configured /pools/<pool> is readable, when set
inventory  /vms has propagated VM.Audit and cluster inventory is readable
mutation   always reports mutation=false
```

Proxmox API tokens can authenticate successfully while lacking useful
authorization for node, storage, network, template, or next-id endpoints. When
that happens, doctor reports the failing endpoint class with a remediation hint,
for example `class=permission hint=grant_proxmox_node_audit`. The messages are
secret-safe and do not print token secret values.

For a separated automation token, start by granting read access needed by
doctor on these paths, then add clone/config/start/stop/delete permissions only
for lease lifecycle operations:

```text
/                         Sys.Audit for cluster allocation metadata
/nodes/<node>             Sys.Audit for node status and local network inventory
/storage/<storage>        Datastore.Audit for clone and template storage checks
/vms/<templateId>         VM.Audit for template inspection
/vms                     propagated VM.Audit for authoritative cluster inventory
/sdn                     SDN.Audit when the configured bridge is SDN-managed
/pool/<pool>              Pool.Audit when a pool is configured
```

The exact least-privilege role depends on the Proxmox VE version and local ACL
model. If doctor fails with `class=permission`, fix the named endpoint first and
rerun doctor before attempting `warmup` or `run`. Doctor requires propagated
`VM.Audit` on `/vms`. Release recovery checks the exact target path before
treating `/cluster/resources?type=vm` as authoritative for that VM. Lease
lifecycle operations additionally need the corresponding VM clone, allocation,
configuration, power-management, datastore-allocation, and pool-allocation
privileges.

For CI or lab smoke checks after building the local binary:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_LIVE_DOCTOR_PROVIDERS=proxmox CRABBOX_BIN=./bin/crabbox scripts/live-doctor-smoke.sh
```

The smoke script validates that `doctor --json` emits one JSON object with
`ok`, `provider`, and `checks`. It is read-only; failures usually mean missing
local config, unavailable Proxmox API, TLS/network problems, or token ACL gaps.

## Live proof runbook

Use `scripts/proxmox-live-smoke.sh` when you need redacted, PR-ready evidence
from a real Proxmox lab. The script is intentionally opt-in for mutation. With
no live flag, it runs only read-only checks:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_BIN=./bin/crabbox scripts/proxmox-live-smoke.sh
```

The same proof script can be selected through the guarded top-level live smoke
matrix:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=proxmox CRABBOX_LIVE_COORDINATOR=0 CRABBOX_BIN=./bin/crabbox scripts/live-smoke.sh
```

The read-only path runs `doctor --provider proxmox --json`, validates the JSON
shape, runs `list --provider proxmox --json`, and writes raw plus redacted logs
under a temporary proof directory. If you also set
`CRABBOX_PROXMOX_SSH_INVENTORY_HOST`, the script runs a read-only SSH inventory
against the Proxmox node using a temporary `UserKnownHostsFile` in that proof
directory. It does not change the user's real SSH trust store.

After doctor is green and the configured `templateId` is a ready QEMU template,
run one controlled lease proof:

```sh
CRABBOX_BIN=./bin/crabbox \
CRABBOX_PROXMOX_LIVE_SMOKE=1 \
CRABBOX_PROXMOX_LIVE_SMOKE_SLUG=proxmox-live-smoke \
scripts/proxmox-live-smoke.sh
```

The configured smoke slug is a prefix. The script appends the UTC epoch and a
random host-independent nonce so concurrent proofs do not share a requested
slug.

Live mode runs the public CLI surface: `warmup --keep`, `status --json`, `ssh`
to print the pasteable command, `stop`, `cleanup --dry-run`, and a final
`list --json`. The smoke script never runs provider-wide mutating cleanup; the
owned lease is released by `stop`, and the cleanup dry-run remains evidence that
other Proxmox leases would not be changed by the proof. The script records every
lease ID emitted by warmup and reconciles only those IDs; when no ID is emitted,
it may reconcile one uniquely new lease with the exact random requested slug.
It never treats a slug suffix alone as ownership proof. The final inventory
comparison runs after successful lifecycles too, so a recorded leaked warmup
attempt is reconciled before the proof can report completion.

Proof artifacts are written to `CRABBOX_PROXMOX_LIVE_SMOKE_DIR` when set, or a
new system temporary directory otherwise. The directory and logs use owner-only
permissions, endpoint URLs are redacted, and live mode refuses to mutate when a
readiness preflight fails. Files ending in `.raw.log` are private operator
evidence. Use only `.redacted.log` files and
`summary.redacted.log` for public PR text, and still review them before
posting. The redactor removes the configured API URL, token ID, token secret,
optional SSH inventory host, `PVEAPIToken=...` values, local home paths, and
known wrapper credential filenames.

## Lifecycle

1. Allocate a Crabbox lease ID and friendly slug.
2. Ask Proxmox for the next free VMID (`/cluster/nextid`).
3. Clone `proxmox.templateId` on `proxmox.node`, honoring `fullClone`,
   `storage`, and `pool`.
4. Configure cloud-init SSH user/key, DHCP, the optional `net0` bridge, the
   guest agent, `tags=crabbox`, and Crabbox labels in the VM description.
5. Start the VM and wait for the QEMU guest agent to report a non-loopback
   IPv4 address (skipping `lo`, `docker*`, and `veth*` interfaces).
6. Wait for SSH to come up, then run the Crabbox Linux bootstrap over SSH as
   root: it installs `openssh-server`, `ca-certificates`, `curl`, `git`,
   `rsync`, and `jq`, writes `/usr/local/bin/crabbox-ready`, and runs it.
7. Sync the checkout and run commands over SSH.
8. Touch the lease labels during runs; on release, delete the VM (stop, then
   `DELETE ... ?purge=1`) unless the lease is kept.

List, release reconciliation, and cleanup use cluster-wide inventory and follow
VMs that migrated away from the configured node. Cleanup reads the Crabbox
labels from VM descriptions and only deletes expired, Crabbox-managed VMs
(those named `crabbox-*` with `crabbox=true` and a matching `provider` label).
Failed acquisition cleanup removes the per-lease SSH key only after confirming
the VM is absent across the cluster.

## Troubleshooting

`proxmox apiUrl is required` / `proxmox tokenId/tokenSecret are required` /
`proxmox node is required` / `proxmox templateId is required`

Set the corresponding `proxmox.*` config field or `CRABBOX_PROXMOX_*`
environment variable. The API URL, token ID, token secret, and node are required
to contact the API. `templateId` is required before leasing; doctor reports it
as a failed `template` check when it is missing.

`class=permission hint=grant_proxmox_*`

The token authenticated but cannot read a prerequisite endpoint. Grant the
token read access for the endpoint named in the doctor details, such as the
node status, storage inventory, network bridge inventory, template VM config,
or `/cluster/nextid`, then rerun `crabbox doctor --provider proxmox --json`.

`timeout waiting for proxmox qemu guest agent` /
`no guest ipv4 address reported by qemu guest agent`

The VM started but the guest agent did not report a usable IPv4 address. Install
and enable `qemu-guest-agent` in the template, then check DHCP, bridge
selection, and VLANs. If you override `--proxmox-bridge`, make sure the template
NIC can boot on that bridge. If no bridge override is configured, doctor
requires the template NIC to reference an active Proxmox bridge.

`timeout waiting for proxmox ssh bootstrap transport`

The VM has an IP but SSH never became reachable. Confirm `openssh-server` is in
the template, the cloud-init user matches `proxmox.user`, and no firewall blocks
the SSH port.

`proxmox guest bootstrap exit=...`

SSH connected and the bootstrap ran but failed (typically an `apt-get` error).
SSH into a kept lease if available, or check the Proxmox task and guest logs.

## Related

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
