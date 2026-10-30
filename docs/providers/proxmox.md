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
`ssh`, `crabbox-sync`, and `cleanup` features on `target=linux` only, plus
[opt-in prepared desktop and browser templates](#prepared-desktop-and-browser-templates). Direct
Proxmox also supports caller-supplied fixed lease IDs with
`warmup --lease-id cbx_<12 lowercase hex>`, and can run behind
[`crabbox adapter serve`](../commands/adapter.md) as described in
[Runtime adapter](#runtime-adapter).

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
VMs that migrated away from the configured node. Cleanup uses names and labels
only to discover candidates; they never authorize deletion by themselves.
Failed acquisition cleanup removes the per-lease SSH key only after confirming
the VM is absent across the cluster.

For an ordinary acquire, step 2 remains a per-create `/cluster/nextid` lookup.
For `warmup --lease-id`, Crabbox instead persists the normalized create intent,
selected VMID, source node, and cluster scope in the fixed lease claim before
submitting the clone, then passes that exact VMID as the clone API's `newid`.
Starting at Proxmox's `nextid`, fixed-ID reservation skips VMIDs bound by any
live local Proxmox claim and verifies the next free VMID against complete cluster
inventory, including unlabelled VMs, templates, and containers. Released fixed-ID
receipts no longer reserve a VMID. Other claims are never dropped or adopted.
Local fixed acquisitions serialize VMID selection through durable claim
publication, so concurrent callers cannot reserve the same free ID. The fence
ends before cloning or waiting for the guest; other hosts still rely on Proxmox
to reject a conflicting clone request.
An identical replay inspects the persisted VMID and adopts only the VM whose
lease labels, intent fingerprint, cluster scope, VMID, and native `vmgenid`
match. Slugs are never replay authority. A changed intent, copied labels,
different VMID or generation, duplicate match, or unresolved post-submit
attempt returns `lease_id_conflict` without issuing another clone.

The clone request carries the lease labels in its description. After clone
completion, Crabbox records the native generation before configuration and
bootstrap. Fixed attempts never use ordinary acquisition rollback. If cloning
or generation recording was interrupted, inspection is required: copied labels
cannot establish the missing generation binding. A bound but incomplete bootstrap
can be stopped safely; replay does not declare it ready merely because SSH works.

Successful fixed-ID release, including authoritative absence of a previously
acquired VM, retains a terminal local tombstone. Absence while a clone is still
uncertain does not prove completion and retains the attempt on ordinary stop.
Explicit prepared-claim recovery checks both cluster absence and active clone
tasks as described below. Checked deletion
records its phase before stopping/purging, so a retry can reconcile a lost delete
response without reopening acquisition. The fixed lease ID is single-use and
cannot allocate another VM after release.

### Automatic cleanup ownership

Cleanup requires exactly one local claim matching the provider, configured API
endpoint and original node scope, VMID, lease, slug, provider-key namespace,
and native VM generation ID (`vmgenid`). New claims record the generation from
the VM configuration. Cleanup refreshes cluster inventory under the unchanged
claim lock, follows a migrated VM to its current node, and checks ownership and
expiry before stop and again before purge. The claim is removed only after
cluster-wide absence is confirmed. Failed stop, unreadable inventory, changed
identity, or uncertain deletion retains the claim; an ambiguous purge response
may be reconciled by authoritative cluster reads while still reporting the error.

Legacy claims without an exact scope, VMID, or generation binding, and VMs with
missing, disabled, malformed, or changed `vmgenid`, are retained for manual
inspection. This also applies to clones of templates that disable generation
IDs. Cleanup does not enable this device, adopt a VM, backfill authority, or
retarget a claim to another VM with copied labels. Ordinary endpoint refreshes
preserve an existing generation binding and do not promote legacy claims.
Duplicate local bindings or remote lease labels are retained as ambiguous.

Automatic cleanup retains local SSH keys, even after successful VM deletion:
key creation precedes claim publication and is not fenced by the claim lock.
Inspect retained legacy VMs and keys explicitly before removing them. Use
`crabbox cleanup --provider proxmox --dry-run` to see eligible candidates without
mutation. The generation ID is a native incarnation witness, not protection
against an operator copying or rewriting the entire VM configuration. Proxmox's
VMID-based purge has no atomic identity precondition; do not concurrently replace
VMs through raw Proxmox operations while cleanup runs.

## Runtime adapter

`crabbox adapter serve --provider proxmox` uses the same direct provider and
fixed lease IDs. At startup the adapter reads the provider's controller identity
and refuses to listen unless the configuration is complete: API URL, node,
token ID, token secret, a positive `templateId` and `target=linux`. The API URL
must be an absolute HTTP(S) URL without userinfo, a query or a fragment.

The identity includes a versioned, opaque scope (`proxmox-v1:sha256:...`). It is a SHA-256 digest of the
normalized API endpoint, node, token ID, template, storage, pool, bridge, clone
mode, guest user and work root. The token secret must be present, but it is
never part of the scope.
The adapter stores the scope with each workspace before its first lifecycle
operation. Later `inspect`, `list`, `stop` and WebVNC subprocesses for that
workspace refuse to run when the current configuration produces another scope.

This means:

- You can replace the token secret for the same token ID without affecting
  existing workspaces.
- Changing the token ID, API endpoint, node or clone settings changes the scope.
  Drain the adapter's workspaces to confirmed cleanup under the old configuration
  before changing these settings. If settings have already changed, restore the
  original configuration to drain them. Crabbox does not migrate workspaces between
  Proxmox principals or clone profiles.
- TLS verification, TTL and idle timeout do not change the scope.

The scope is routing protection, not ownership proof: fixed-ID replay and exact
cleanup still verify the exact claim, VMID and native `vmgenid` under the rules in [Lifecycle](#lifecycle),
including the checks that need propagated `VM.Audit` on `/vms`. Proxmox can
reuse a VMID after a VM is deleted. If another workspace receives the same VMID
before the adapter confirms absence of the old one, the adapter keeps the old
workspace's local cleanup pending until that VMID is gone. It never treats the
new VM as the old workspace.

When the adapter confirms that a released VM is absent, it finishes cleanup and
keeps the released claim as the lease ID's receipt, as direct `stop` does. If an
acquisition failed before Crabbox wrote that claim, confirmed-absence cleanup
fails closed and the workspace stays `stopping` until an operator inspects it.

## Troubleshooting

`http 403: Permission check failed (..., SDN.Use)` during a fixed-ID clone

Grant the API token the named permission on the indicated resource (for example,
`SDN.Use` on the selected bridge), then retry the same `warmup --lease-id` request.
A definite HTTP 401/403 rejection from the clone endpoint, or an authorization
failure during inventory/planning before submission, removes the pending intent
and generated lease key. It does not reserve the VMID or consume the fixed ID.
The original API error preserves the missing permission and path.

Clone rejection requires a native Proxmox JSON error envelope with null data
and the Proxmox server header. Proxy-generated, malformed, or rewritten 401/403
responses retain the claim because the upstream clone may already have started.
Replies carrying task data also retain custody, regardless of HTTP status.

A 401/403 from task polling or VM identity reads after clone acceptance is
ambiguous: the VM may already exist. Crabbox retains the prepared claim and
refuses another clone. Restore read permissions and inspect the clone task and
VM before recovery. A bound generation can be released with ordinary `stop`;
an existing VM without a bound generation requires manual inspection and cannot
be adopted or deleted by name.

After a 403 on an older version, restore permissions and inspect the clone task,
then clear each retained prepared claim whose VM is absent with its own lease ID:

```sh
crabbox stop --provider proxmox --id cbx_123456abcdef --force
```

Recovery requires the original cluster/node scope, a canonical fixed lease ID,
and a prepared claim with no bound generation. Under the claim lock it verifies
there are no active `qmclone` tasks on the source node, then checks complete
cluster inventory for the reserved VMID, exact requested VM name, lease label,
and provider key. This requires `Sys.Audit` on `/nodes/<source-node>` and
propagated `VM.Audit` on `/vms`; missing permissions, unreadable inventory,
active clones, or any matching VM retain the claim. Recovery never deletes a
VM. Success removes the stored lease key and leaves a terminal receipt, freeing
the VMID for a new lease ID while keeping the recovered fixed ID single-use.
This checked recovery also applies to older claims without rejection evidence.
Fixed IDs no longer collide with locally bound VMIDs: new requests skip those
VMIDs. If no alternative can be reserved, the error names the conflicting claim
and its exact recovery command; unreadable inventory always prevents allocation.

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

The initial SSH readiness probe succeeded, but the subsequent bootstrap SSH
command failed. The error includes its observed native exit status and redacted
stdout/stderr diagnostics, even if acquisition rollback removes the VM. A later
SSH transport failure is still possible; exit `255` does not establish that a
guest setup command ran. The underlying process error remains inspectable, while
the CLI retains its ordinary bootstrap-failure exit `1`.

Successful bootstrap remains quiet. Diagnostic capture is limited to 16 KiB;
when that limit is exceeded, all captured text is omitted and a fixed truncation
notice is shown so a cut credential cannot leak. Check guest or Proxmox logs if
the diagnostic is unavailable. Existing rollback and kept-lease policy are
unchanged.

## Related

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)

## Prepared desktop and browser templates

Desktop and browser leases are opt-in, template-backed Linux features. Crabbox
never installs a desktop or browser during acquisition. Prepare and review the
image first, then authorize its exact VMID in **user configuration**, environment
variables, or explicit flags. Repository configuration cannot grant these
capabilities. For example, with a prepared template at VMID 9401:

```yaml
provider: proxmox
target: linux
proxmox:
  node: pve1
  templateId: 9401
  desktopTemplateId: 9401
  browserTemplateId: 9401
  user: crabbox
```

Both authorization fields default to `0` (disabled). `desktopTemplateId` enables
`--desktop` only when it equals the selected `templateId`; `browserTemplateId`
independently enables `--browser`, including headless browser leases. Environment
names are `CRABBOX_PROXMOX_DESKTOP_TEMPLATE_ID` and
`CRABBOX_PROXMOX_BROWSER_TEMPLATE_ID`; explicit flags are
`--proxmox-desktop-template-id` and `--proxmox-browser-template-id`.
`crabbox providers --configured --json` reports the features enabled by this configuration.
Changing either authorization changes the immutable adapter scope: drain old
workspaces before reconfiguring an adapter. Existing headless configuration and
scope remain unchanged when both fields are zero.

### Template preparation recipe

Start with a Debian 12 Linux QEMU guest satisfying the cloud-init, guest-agent,
SSH and passwordless-sudo requirements above. Inside that guest, from a reviewed
Crabbox checkout, install the existing X11 desktop setup **before** converting the
VM to a template:

```sh
sudo env CRABBOX_DESKTOP_USER=crabbox \
  CRABBOX_DESKTOP_DISPLAY=:99 CRABBOX_DESKTOP_GEOMETRY=1920x1080x24 \
  bash scripts/install-linux-desktop.sh
sudo apt-get update
sudo apt-get install -y chromium x11-utils
sudo install -d -m 0755 /usr/local/bin
sudo tee /usr/local/bin/crabbox-browser >/dev/null <<'BROWSER'
#!/bin/sh
set -eu
profile="$HOME/.cache/crabbox/browser-profile"
umask 077
mkdir -p "$profile"
chmod 0700 "$profile"
exec /usr/bin/chromium --no-first-run --no-default-browser-check \
  --disable-default-apps --hide-crash-restore-bubble \
  --user-data-dir="$profile" "$@"
BROWSER
sudo chmod 0755 /usr/local/bin/crabbox-browser
sudo tee /var/lib/crabbox/browser.env >/dev/null <<'ENV'
BROWSER=/usr/local/bin/crabbox-browser
CHROME_BIN=/usr/local/bin/crabbox-browser
ENV
sudo chmod 0644 /var/lib/crabbox/browser.env
sudo -H -u crabbox /usr/local/bin/crabbox-browser --version
```

A reviewed Chrome-based template can use its installed Chrome executable in the
wrapper instead. Do not bake logged-in browser profiles, SSH keys, or reusable
credentials into the image. Before sealing it, stop and disable the desktop
services and remove their temporary VNC credentials, so the clone cannot start
with the template's password:

```sh
sudo systemctl disable --now crabbox-desktop.service crabbox-xvfb.service
sudo rm -f /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
```

Shut down the guest and convert it to a Proxmox template with your normal image
preparation process. Authorize the resulting template VMID, not the build VM's
previous identity. The template must retain `crabbox-xvfb.service` (TigerVNC) and
`crabbox-desktop.service` (XFCE), running as the configured `proxmox.user`.

### Readiness and limits

At clone creation Crabbox generates fresh private VNC credentials, enables and starts the
prepared services, and verifies XFCE, TigerVNC, an accessible X11 display at
`:99`, VncAuth using the per-clone password file, and a VNC listener exclusively
on loopback port 5900. Both password files must belong to the configured guest
user with mode `0600`. A browser request also checks `browser.env`, the executable
`/usr/local/bin/crabbox-browser` wrapper, and its `--version` result as the guest
user. These checks are installed in `crabbox-ready` for subsequent readiness
probes. Failures name the missing or invalid component and require repairing the
template; acquisition does not download a replacement desktop stack. The existing
headless SSH toolchain bootstrap is unchanged.

Only Linux XFCE/X11 with TigerVNC is supported here. Wayland, GNOME, Windows,
macOS, arbitrary display numbers, and the 8-bit Xvfb/x11vnc alternative are not
part of this template contract. A failed initial bootstrap follows the existing
owned-clone cleanup/recovery path. Fixed-ID replay preserves capability labels
and rejects a changed desktop/browser request instead of silently reusing an
incompatible VM. Read-only inspect never installs packages or rotates passwords.

Use the existing [VNC](../features/vnc-linux.md) SSH tunnel or
[WebVNC](../commands/webvnc.md); do not expose port 5900 on the Proxmox bridge.
For coordinator inventory and WebVNC, use `broker.mode: registered` with the
existing [runtime adapter](#runtime-adapter). Proxmox still owns clone creation
and cleanup; the coordinator does not gain Proxmox API credentials.

### Live desktop proof

This feature's automated tests use fake Proxmox, SSH, service, process and socket
boundaries. A real proof requires a Proxmox host, API permissions and a prepared
VMID as described above. On that host, using the trusted configuration:

```sh
crabbox doctor --provider proxmox --json
crabbox warmup --provider proxmox --keep --desktop --browser \
  --lease-id cbx_0123456789ab --slug desktop-proof --ttl 20m --idle-timeout 10m
crabbox inspect --provider proxmox --id cbx_0123456789ab --json
crabbox run --provider proxmox --id cbx_0123456789ab --browser --no-sync -- \
  bash -lc 'crabbox-ready && "$BROWSER" --version'
crabbox desktop launch --provider proxmox --id cbx_0123456789ab \
  --browser --url https://example.com
crabbox webvnc --provider proxmox --id cbx_0123456789ab --open
crabbox stop --provider proxmox --id cbx_0123456789ab
crabbox list --provider proxmox --json --refresh --all
```

Success means the lease reports `desktop=true`, `desktop_env=xfce`, and
`browser=true`; the browser appears in WebVNC; VNC listens only on guest loopback;
and stop removes the exact cloned VM, its local claim/key and registration while
leaving the template intact. Repeat warmup with the same ID and capabilities
before stop and confirm that there is still only one VM; omit `--browser` on a
retry and expect `lease_id_conflict`. Repeat through `crabbox adapter serve
--provider proxmox` with registered mode to validate the controller path.
Finally, remove a required component in a separate test template and confirm the
named readiness error and owned-clone cleanup. Preserve any unresolved claim
until Proxmox inventory proves absence.

## Native per-lease Tailnet enrollment

Opt in with `--tailscale --network tailscale --lease-id cbx_<12 lowercase hex>`.
This bounded path requires Linux, ordinary SSH on port 22, a strictly verified
HTTPS Proxmox API, and a clean prepared template. It never falls back to LAN SSH,
uses Tailscale SSH, or embeds a key in cloud-init. Non-fixed allocation, direct
`tailscale.authKey`/auth-key environment values, exit nodes and insecure TLS are
rejected before allocation. Existing direct-network behavior is unchanged.

The template must already contain running QGA, cloud-init, OpenSSH, Python 3,
`/usr/bin/tailscale`, a running but **unenrolled** tailscaled, and the reviewed
[scripts/proxmox-enroll.py](../../scripts/proxmox-enroll.py) installed root-owned,
not writable by the lease user, at `/usr/local/libexec/crabbox-enroll`. The helper
rejects pre-enrolled state and sends its key to `tailscale up` through stdin,
with Tailscale SSH and advertised route acceptance disabled. Normal Tailnet DNS
acceptance stays enabled so private controller/MagicDNS HTTPS names can resolve.
Successful enrollment requires a nonempty Tailscale `Self.ID`. The helper saves
only non-secret individual fields for the common SSH metadata reader; its framing
accepts both newline-terminated and unterminated files, including absent optional
fields.
No enrolled tailscaled state, device identity, reusable credential, machine ID
or SSH host keys may be baked into a reusable template. The existing template
builder does **not** install this Tailnet extension automatically.

Example trusted user config (the broker path is ignored in repository config):

```yaml
proxmox:
  tailscaleBroker: /usr/local/libexec/example-tailnet-broker
network: tailscale
tailscale:
  enabled: true
  tags: [tag:worker]
  hostnameTemplate: cbx-{id}
```

Alternatively set `CRABBOX_PROXMOX_TAILSCALE_BROKER` in the controller's protected
service environment. There is deliberately no executable/credential argv flag.
Use the existing Tailscale flags/options for tags and hostname. Use a dedicated
worker tag and issuer, with controller-to-worker TCP 22 permitted. No fleet key,
coordinator allocation path or broad shared-team service is introduced.

### Broker protocol v1

The operator supplies a trusted local executable; this repository defines and
mock-tests its contract, **not a production issuer deployment**. It is executed
without arguments or a shell. One JSON request arrives on stdin; stdout is a
private bounded JSON response (16 KiB), stderr is discarded. It inherits the
controller's protected environment, where an operator wrapper may resolve issuer
credentials. The process deadline is 30 seconds. Do not log stdin/stdout, HTTP
bodies, keys or issuer credentials in that wrapper or its upstream transports.

An acquisition request contains only non-secret identity and policy:

```json
{
  "version": 1,
  "operation": "acquire",
  "endpoint": "https://pve.example.test:8006",
  "node": "pve1",
  "leaseId": "cbx_012345abcdef",
  "vmid": "417",
  "generation": "8be39656-32b0-4c47-b68c-0a9e1d3ef901",
  "hostname": "cbx-cbx-012345abcdef",
  "tags": ["tag:worker"]
}
```

The response must contain `authKey` (one Tailscale enrollment key), `expiresAt`
(RFC 3339, after now and no more than ten minutes ahead), `ephemeral:true`, and
`reusable:false`. Never supply a reusable fleet key. The broker must validate
endpoint/node, dedicated tag allowlist and hostname, and durably bind the issuance
and its device-cleanup ownership to **all** of endpoint/node/leaseId/vmid/generation
before returning a credential. Its issuer must be limited to the admitted worker
tag. Repeated requests for a bound attempt must not mint independent identities;
ambiguity, a changed generation or an unsupported request must fail closed.

`release` sends the same identity and policy with `operation:"release"`. Return
`{"released":true}` only after revoking any unused key and deleting/reconciling
its enrolled device(s), including an acquisition whose response was lost or
cancelled. The broker must fence in-flight issuance/enrollment, reconcile the
unique lease hostname and dedicated tag without touching unrelated devices, and
make retries idempotent. This requires actual device-cleanup permission in
addition to `auth_keys`; an auth-key-only issuer is insufficient. If the chosen
issuer cannot prove this bounded ownership/cleanup contract, do not enable the
path. Ephemeral-node eventual expiry is not a cleanup acknowledgment.

The CLI persists no key, broker response or issuer secret. Its fixed intent binds
the network choice, tags, hostname template and broker path. Before clone or key
issuance, admission requires 1–32 unique tags (`tag:` followed by a lowercase
alphanumeric name with optional hyphens, at most 63 name characters) and the
**final rendered** hostname (1–63 lowercase alphanumeric/hyphen characters,
starting with an alphanumeric character). Templates are not truncated to fit;
choose a shorter template or slug if rendering exceeds the helper's limit.
A fresh pre-submission validation rejection retires its unsubmitted claim and
lease key under the fixed transaction fence, allowing a corrected request to use
the same canonical ID; it cannot retire an earlier or generation-bound attempt.
The clone attempt also persists the trusted absolute broker path locally, not
in the VM description. After binding VMID and vmgenid, it checks isolated exec permission and QGA readiness, rechecks the VM
generation and exact claim-bound hostname/tags, acquires one key and invokes only
the prepared helper through native
`agent/exec` input-data. Native REST input-data is raw stdin; PVE performs QGA's
base64 encoding. Acquire requests use the original claim-checked clone policy,
not later mutable descriptions; hostname/tag drift fails before key issuance.
Helper output and error bodies are never exposed as diagnostics. The root helper
sets the checked metadata directory to `0755` and non-secret receipt files to
`0644` explicitly, independent of QGA's umask, so the lease SSH user can read them;
ownership checks and no-follow descriptor handling remain enforced.

After successful helper exit, live QGA discovery selects only an IPv4 on exact
`tailscale0` within `100.64.0.0/10`, deterministically if several are reported.
SSH bootstrap must succeed using the lease key. Provision, fixed replay,
resolve/inspect and run share that address policy and normal `TailscaleMetadata`;
missing QGA/address never reuses a stale saved address or chooses an underlay IP.
The provider pins that QGA-selected IP through common bootstrap/run/status route
selection: persisted `tailscale_fqdn` and hostname values cannot replace it.

Before acquisition returns, the CLI reads the prepared guest's enrollment receipt
over the per-lease SSH connection, requires its IPv4 to match live QGA discovery,
and durably binds its actual device ID to the canonical lease, cluster scope,
VMID and vmgenid in the fixed claim. Fresh-process `inspect --json` exposes that
receipt as `tailscale.deviceID`; it does not trust a device ID copied into mutable
Proxmox descriptions. Fixed replay revalidates the receipt against the bound
device without another clone or key issuance. A missing receipt, changed device
or generation fails closed; a claim without a bound device cannot adopt a mutable
label as proof. Metadata refresh implements the common persistence capability
and may confirm, but not replace, this identity. For native Tailnet leases the
receipt is a required access gate: common warmup, workspace-owner setup, sync and
command callers propagate receipt read, validation and persistence failures before
sending user commands or sync payloads. They do not restore cached metadata and
continue, reallocate a VM or request another enrollment key. Other providers keep
their existing optional, best-effort metadata refresh behavior.

Inventory listing remains diagnostic: an incomplete or unverifiable Tailnet lease
stays visible with `tailscale_state=unverified`, no reported device ID or access
address, and an error for recovery; it does not hide healthy leases. This does not
authorize resolve, replay, sync or command access. A local claim scan failure
makes ownership unclassifiable and fails the list operation; it does not turn
ordinary direct VMs into Tailnet leases or silently remove their public addresses.

A public-network override cannot downgrade an already Tailnet-bound lease.
Release does not require guest reachability. Failed/cancelled provisioning retains
its fixed claim; stop the same canonical lease, not another allocation. VM
absence is verified, then broker release must succeed before terminal settlement.
A broker failure leaves the deleting claim retryable even when the VM is gone.
Before deleting a VM (or settling an already absent VM), release checks the
locally bound broker path, even when the invocation omits `--tailscale`. A changed
or missing binding fails before deletion or broker invocation. Restoring the
bound executable **and its original issuance ledger** permits ordinary release
retry; changing the path to a new empty ledger is not cleanup.

#### Broker migration and legacy recovery

Do not switch issuance owners while leases remain active. Pause new allocations,
retain the original broker and ledger at the bound path, release each owned
lease through that broker, and verify its key/device cleanup acknowledgments
before configuring a new broker for new leases. A compatible wrapper at the
bound path must still reconcile the original ledger; a matching path alone
cannot prove that an operator has preserved executable contents or ledger data.
Never replace either in place with an unrelated issuer.

Older experimental Tailnet claims whose attempts lack `tailscaleBroker` remain
inspectable but cannot authorize CLI release, including when the VM is already
absent. Do not infer their cleanup owner from current configuration, backfill the
binding blindly, delete the claim, or mark it terminal. The operator must recover
the original issuance owner from trusted allocation records, fence issuance and
reconcile that owner's exact endpoint/node/lease/VMID/generation key and device
ledger. Preserve the claim and evidence for a separately reviewed explicit
migration/recovery; this change does not supply a legacy binding migration tool.

### Additional permission decision

An operator must explicitly approve `VM.GuestAgent.Unrestricted` on the isolated
worker VM(s) (for example through the dedicated worker pool's runtime principal),
not on unrelated VMs. It permits guest execution; `VM.GuestAgent.FileWrite` and
`VM.GuestAgent.Audit` alone cannot enroll a guest. The CLI checks the exact
`/vms/<vmid>` effective permission before requesting a key and never grants it.
Keep this privilege, the issuer and device-cleanup authority out of CI. Existing
clone/audit/config/start/delete/storage/network rights remain necessary. The
operator still owns strict TLS, clean-template proof, dedicated tag/ACL/issuer
approval and a real end-to-end allocation/cancellation/cleanup proof.
