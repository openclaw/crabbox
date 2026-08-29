# Machine0 Provider

Machine0 is a built-in direct Linux SSH-lease provider. Crabbox uses the
`machine0` CLI for VM lifecycle and its stable JSON surfaces, then uses the
normal Crabbox OpenSSH transport for sync, commands, desktop tunnels, WebVNC,
and code-server.

**Targets:** Linux.

**Capabilities:** SSH, Crabbox sync, cleanup, desktop/browser/code, explicit
pause/resume, provider-native versioned image checkpoints, and live CPU/GPU
size and cost discovery.

## Install and authenticate

Install the Machine0 CLI yourself; Crabbox never installs or upgrades it:

```sh
npm install -g @machine0/cli
machine0 --version
machine0 login
```

For non-interactive environments, Machine0 also supports
`MACHINE0_API_TOKEN`. Authentication remains owned by Machine0: Crabbox passes
the current process environment to the CLI but never reads, logs, or persists
the token. `crabbox doctor --provider machine0` checks the CLI, authentication,
inventory, and live size catalog without creating a VM.

## Quick start

```sh
machine0 sizes --json

crabbox providers sizes machine0
crabbox warmup --provider machine0 --slug linux-ci
crabbox warmup --provider machine0 --class fast --slug larger-ci
crabbox run --provider machine0 --id linux-ci -- go test ./...
crabbox ssh --provider machine0 --id linux-ci
crabbox stop --provider machine0 linux-ci
```

`stop` destroys the Machine0 VM by default. This is deliberate: Machine0 VMs
are persistent, and retaining either compute or snapshots can continue to cost
money.

## Configuration

```yaml
provider: machine0
machine0:
  cliPath: machine0
  image: ubuntu-24-04-loaded
  imageVersion: 0       # 0 selects the active image version
  desktopImage: ""      # optional prepared image used only with --desktop
  size: large
  region: eu
  key: ci-key           # optional; empty uses Machine0's default SSH key
  workRoot: ""          # dynamic: /home/<resolved-ssh-user>/crabbox
  releasePolicy: destroy
  createTimeout: 15m
  pollInterval: 60s
```

The equivalent flags are:

```text
--machine0-cli
--machine0-image
--machine0-image-version
--machine0-desktop-image
--machine0-size
--machine0-region
--machine0-key
--machine0-work-root
--machine0-release-policy destroy|suspend
--machine0-create-timeout
--machine0-poll-interval
```

Environment overrides use the same names with the `CRABBOX_MACHINE0_` prefix,
for example `CRABBOX_MACHINE0_SIZE`, `CRABBOX_MACHINE0_REGION`,
`CRABBOX_MACHINE0_IMAGE`, `CRABBOX_MACHINE0_KEY`, and
`CRABBOX_MACHINE0_RELEASE_POLICY`.

An omitted or empty `machine0.workRoot` is a dynamic sentinel. After resolving
the VM, Crabbox uses the same effective SSH username for access and storage:
`/home/ubuntu/crabbox` for Ubuntu and `/home/nix/crabbox` for NixOS. The
resolved root is recorded on the server and claim, then reused for repository
sync, later runs, resolves, and checkpoint workdirs. `crabbox config show`
renders the unresolved default as
`<dynamic:/home/<resolved-ssh-user>/crabbox>`.

An explicit `machine0.workRoot`, `--machine0-work-root`, or
`CRABBOX_MACHINE0_WORK_ROOT` is preserved exactly and replaces the dynamic
home-root behavior.

Machine0's `key` is the registered public or managed key name supplied to
`machine0 new --key`. When the VM details return `key.fileName`, that filename
is authoritative because it identifies the key actually injected into the VM.
When inventory omits the filename but retains a managed key name, Crabbox
derives Machine0's canonical `machine0__<key-name>` filename instead. It
resolves the provider-owned filename under `SSH_KEY_PATH` or the default
`~/.ssh` directory before readiness checks. Crabbox runs
`machine0 ssh <vm> true` only when the resolved private-key file is absent, then
verifies that the CLI actually materialized the file. Existing key files skip
the Machine0 CLI entirely, so ordinary resume and checkpoint restart do not
depend on the CLI's global `known_hosts`; Crabbox continues to perform its own
direct SSH host-key handling. The provider-owned path overrides a generic
`--ssh-key`, which remains available only when Machine0 provides no usable
provider-owned key identity. Set `SSH_KEY_PATH` when Machine0 uses a custom key
directory.

Before creating a VM with a PUBLIC key, Crabbox checks the selected key's local
filename and rejects a proven mismatch with the registered public key. It uses
noninteractive `ssh-keygen -y` on the private file, not its `.pub` sidecar.
Encrypted or unsupported keys and missing public metadata remain unverified;
this check does not prove that SSH authentication will succeed. Crabbox never
changes the selected key or downloads PUBLIC private keys to repair a mismatch.

Machine0 SSH targets use a provider-isolated host-trust file under
`<key-directory>/crabbox/machine0/known_hosts.d/`, keyed by a hash of the
immutable Machine0 machine ID. The directory is private (`0700`), OpenSSH host
checking remains enabled with `accept-new`, and Crabbox never edits the user's
shared `~/.ssh/known_hosts`. Ordinary acquire and resolve operations preserve
the per-machine file for continuity. After a provider-authorized checkpoint
restart or suspended-machine resume, Crabbox removes only that machine's
isolated file before its SSH readiness check so a rotated host key can be
accepted even when Machine0 reuses the same IP.

## Live sizes, GPUs, and cost

Crabbox publishes the following convenience mappings for its standard machine
classes, using shapes from the Machine0 size catalog:

| Class | Machine0 size | vCPUs | RAM |
| --- | --- | ---: | ---: |
| `tiny` | `large` | 2 | 4 GB |
| `small` | `xl` | 4 | 8 GB |
| `standard` | `xxl` | 8 | 16 GB |
| `fast` | `xxxl` | 16 | 64 GB |
| `large` | `4xl` | 32 | 128 GB |
| `beast` | `5xl` | 48 | 192 GB |

The static `classCatalog` in `crabbox providers --json` and
`crabbox providers describe machine0 --json` reports these mappings. Machine0
does not appear in the legacy top-level `classes` compatibility projection.
These aliases are conditional creation choices, not a guarantee of current
availability or a way to resize an existing machine. The live native size
catalog is authoritative for sizes, resources, prices, and available regions.

An explicitly configured `machine0.size`, `CRABBOX_MACHINE0_SIZE`, or
`--machine0-size` always wins over a portable class, even when the selected size
is the normal `large` default. Native sizes follow the usual configuration
precedence: YAML, then environment, then CLI. They may name any live Machine0
size, including GPU, NVMe, premium, and larger CPU shapes outside this
convenience catalog. An explicitly selected class supplies its mapped size only
when no native size was configured; omitting both preserves the existing
`large` default. Crabbox never hardcodes a reduced allowed-size enum or price
table: before creation it reads `machine0 sizes --all --json` and verifies that
the selected size exists and is currently offered in the requested region.

```sh
crabbox providers sizes machine0 --json
crabbox providers sizes machine0 --all --refresh
```

The JSON output preserves exact hourly microcurrency integers
(`1_000_000 = 1` currency unit), vCPU, RAM, boot disk, transfer allowance,
estimated snapshot size, default image, available regions, and GPU label, VRAM,
and scratch-disk capacity. Human output converts the exact integer to a
currency-unit hourly value only for display.

The current Machine0 region names are `us-east`, `us-west`, `uk`, `eu`, and
`asia`, but the live size catalog is authoritative. Capacity can still fail
regionally after validation; Crabbox preserves the CLI diagnostic and removes a
partially created VM unless `--keep` was explicit.

## Lifecycle and reuse

Native size, region, image, image version, desktop image, and registered key
select a new machine. Their flags report `creationOnly: true` in
`crabbox providers describe machine0 --json`. Machine0 has no supported in-place
resize: changing these selectors or a generic class does not resize or replace
an existing lease. Reuse reports the observed machine size and retains its
immutable Machine0 ID. Config values and class labels are selection intent,
not evidence of an existing machine's capacity. Creating or forking a separate
machine remains a distinct lifecycle operation.

Prewarm uses these selectors for creation but does not forward their flags to
hydration or probes, which reuse the created lease. It validates the provider
configuration those follow-ups will reload before allocating a machine. A
creation override does not repair an invalid saved value for a follow-up;
correct the saved configuration first.

CLI path, work root, release policy, polling interval, and creation timeout
remain usable with existing machines. Despite its name, creation timeout also
bounds resume, suspend, and checkpoint waits.

`machine0 new` returns before a VM is usable. Crabbox polls `get --json` through
`CREATING` and `STARTING`, requires a `RUNNING` VM with an IP, and treats
`ERRORED` and `UNAVAILABLE` as terminal failures with the Machine0 diagnostic.
Provisioning can take 20 minutes or longer, so set `machine0.createTimeout`
to cover the expected creation window when necessary.

The Machine0 resource ID—not its IP—is the stable lease identity. Every
resolve refreshes the VM JSON and SSH endpoint. This matters after suspend:
stop/start preserves an IP, while suspend removes compute and a later start can
assign a different IP. Crabbox always prefers `defaultSSHUsername` returned by
Machine0, falling back to `ubuntu` for Ubuntu and `nix` for NixOS.

For canonical dashed UUID lookups (`8-4-4-4-12` hexadecimal digits), Crabbox
reads `machine0 ls --json`, validates the inventory and every row's UUID, and
requires exactly one matching UUID, ignoring hexadecimal letter case. It then
fetches full details with `machine0 get <current-name> --json` and requires both
the original UUID and the selected name to match. Inventory summaries are never
returned as full details: they can omit the SSH key, default SSH username, and
other metadata. Ordinary name lookups, including named readiness polls, remain
direct detail reads without an extra inventory request.

Only a successful, complete, valid inventory with no exact UUID match reports
the UUID as absent from the current authorized inventory. Missing or malformed
IDs, duplicate matches, authentication/read/JSON errors, and failed detail
reads fail closed; they do not establish absence. A reused name or a UUID/name
change between inventory and detail reads is rejected. The name is only a
transport address: CloudID, ImmutableID, and claim scope remain bound to the
UUID, and lookup does not delete or rebind claims. This verifies identity across
the lookup reads; it does not make later name-addressed mutations atomic with
those reads or remove the existing remote name-reuse race before mutation.

Use `--keep` to keep a normal Crabbox run lease for later reuse. Adopt an
existing unclaimed Machine0 VM only through an explicit `--reclaim` reuse;
destructive release requires an exact local claim bound to the Machine0 ID.

If a local claim is missing, a canonical Crabbox lease ID can suggest a
Crabbox-named VM from Machine0 inventory, but the short name hash cannot prove
the full lease identity. Even a unique match fails closed with a candidate-name
hint. Inspect that machine and use its explicit name with `--reclaim` to adopt
it; Crabbox fetches its full details, including its SSH key and endpoint.
Discovery alone never authorizes reuse or deletion.

Machine0 VM names are limited to 31 lowercase letters, digits, and hyphens.
Crabbox truncates only the human slug portion and retains an eight-character
lease hash, so long requested slugs remain deterministic. The short hash can
collide; durable claims, not name hashes, establish lease ownership.

### Fixed-ID replay

`warmup --lease-id cbx_<12 lowercase hex>` makes direct Machine0 acquisition
idempotent across process restarts. Before `machine0 new`, Crabbox writes the
normalized create intent and exact create request to the ordinary durable lease
claim under its existing cross-process lock. The intent is bound to the
deterministic VM name derived from the lease ID and allocated slug. The durable
create attempt authorizes the first visible matching VM and records its exact
Machine0 resource ID; every later adoption must match that ID. A matching name
or slug alone never authorizes reuse.

An ambiguous create remains pinned to its original name, size, region, image,
image version, and key. Replay fails closed without another `machine0 new` call
while that attempt has no visible machine, deliberately accepting a false
negative if the process stopped after persisting the attempt but before sending
the request. When `machine0 new` returns an error, the original invocation polls
inventory for a bounded 60-second reconciliation window. A machine that appears
is retained and adopted; a definite no-machine result clears the attempt so an
ordinary capacity failure remains retryable.

The `machine0 ls --json` summary can omit a VM's entire SSH-key object. When a
fixed replay has a durable selected key but its exactly owned inventory entry
has no usable key identity, Crabbox performs one `machine0 get <vm> --json`
detail read. The detail must identify the same VM and selected key; missing or
mismatched identity or conflicting key types fail closed. Explicitly public keys
retain public-key semantics, and replay without a selected provider key keeps
generic SSH-key fallback without an additional detail read.

Once creation may have succeeded, later readiness or SSH failures never roll the
VM back: the caller's fixed lease identity remains bound to it, and a matching
replay can finish adoption. Repository binding is also durable; `--reclaim` is
the explicit override for replay from a different repository. With
`machine0.releasePolicy: suspend`, release keeps the live fixed claim and replay
starts the exact suspended machine before refreshing its endpoint.

Destroy release replaces the live claim with a compact terminal tombstone, so a
fixed ID is single-use and automatic cleanup never makes it replayable. Fixed
claims and tombstones use the downgrade-safe local discriminator
`machine0-fixed-v1`: current clients map it to runtime provider `machine0`, while
older clients see an unknown provider and skip or refuse destructive cleanup
instead of erasing fixed identity state.

Machine0 stop and suspend are intentionally distinct:

- `machine0 stop` preserves the instance and IP and continues billing compute.
  Do not treat it as a cost-saving operation.
- `crabbox pause --provider machine0 <lease>` calls `machine0 suspend --yes`,
  polls until Machine0 reports exact `SUSPENDED`, then clears the recorded SSH
  endpoint. This preserves the boot disk as billed snapshot storage while
  releasing compute.
- `crabbox resume --provider machine0 <lease>` calls `machine0 start`, waits for
  `RUNNING`, and records the refreshed IP.
- `crabbox stop` destroys with `machine0 rm --yes` unless
  `machine0.releasePolicy: suspend` or the matching flag is explicitly active.

## Images and checkpoints

Machine0 named images are reusable, versioned snapshots. Crabbox exposes them
through the provider-neutral checkpoint commands:

```sh
crabbox checkpoint create --id linux-ci --mode native --strategy image --name ci-baseline
crabbox checkpoint inspect <checkpoint-id> --verify
crabbox checkpoint fork <checkpoint-id> --slug experiment
crabbox checkpoint delete <checkpoint-id>
```

Verified inspection and listing (`--verify`), deletion, and pruning use the
current Crabbox configuration, including `machine0.cliPath` (or
`CRABBOX_MACHINE0_CLI`) and `machine0.pollInterval`. A custom executable does
not need to be on `PATH`. Provider-backed deletion keeps the local checkpoint
record if configuration or resource verification fails.

Creation flushes a running source over SSH, calls `machine0 stop`, waits for
exact `STOPPED`, and then uses `machine0 images save <vm> <image>`. The VM stays
stopped while Crabbox polls the exact metadata-matching version until its
underlying snapshot reports `READY`. Only then does Crabbox start a source it
stopped, wait for `RUNNING` and SSH, and atomically refresh the endpoint in the
lease claim. A source that was already `STOPPED` also waits for snapshot
readiness but remains stopped afterward; Crabbox never pretends it owned that
state transition.

Machine0's stopped-snapshot barrier is mandatory even with checkpoint
`--wait=false`: that flag may allow the returned version to remain `DRAFT`, but
its underlying snapshot must be `READY` before a running source can restart.
The barrier uses `--wait-timeout` when it is positive, otherwise
`machine0.createTimeout`. Stop continues billing compute, so this is a
consistency requirement rather than a cost-saving lifecycle action.

Reusing an image name creates a new version, which Crabbox records and passes
back through `--image-version` when forking. A draft becomes usable only after
its underlying snapshot reports `READY`; `DRAFT` status by itself is not
readiness. Deletion is always an explicit checkpoint operation: Crabbox removes
a whole image only when that checkpoint created the name, the exact owned
version metadata still matches, and it remains the image's only version. If
later versions exist, Crabbox refuses whole-image deletion so it cannot erase
unrelated work. For an existing image name, deletion targets only the recorded
draft version. Machine0 suspend snapshots such as
`suspended-<machine>-<timestamp>` remain lifecycle artifacts, not reusable
named checkpoint records.

Image and suspended-snapshot storage is billed separately. Removing or
destroying a VM does not imply that unrelated named images should be deleted.
Image-version storage prices may be fractional values; Crabbox preserves their
JSON decimal representation in checkpoint metadata instead of coercing them to
integer microcurrency.

## Rate limits and troubleshooting

Machine0 accounts have finite hourly API read quotas, and one CLI status or
inventory invocation can perform multiple API requests. The default
`machine0.pollInterval` of `60s` reduces polling volume about twelvefold
compared with `5s`; a 20-minute boot needs roughly 20 polls instead of roughly
240, leaving headroom for final inspection and cleanup. At most 55 seconds of
additional readiness-observation latency is negligible beside the documented
15-minute timeout and observed 10–21-minute creation windows.

When the CLI returns either exact `Rate limited. Please wait a moment and try
again.` or `The cloud provider is temporarily unavailable. Please try again
shortly.` response, Crabbox quietly retries read-only inventory and status
calls (`get`, `ls`, sizes, and image reads) until the current command's context
expires or is canceled. The retry cadence follows `machine0.pollInterval`
(default `60s`, with a one-second minimum) and prints only one concise warning.
A cached-credentials warning preceding either response does not prevent
recognition.

Crabbox never automatically retries Machine0 mutations such as create, start,
stop, suspend, remove, image save/delete, or SSH key priming. A failed mutation
may already have reached Machine0, so inspect the exact VM or image identity
before deciding whether a manual retry is safe.

## Optional live smoke

The dedicated live runner builds the current Crabbox binary unless
`CRABBOX_BIN` is explicit, performs read-only Machine0 auth/catalog/key
preflight, creates one short-lived VM, proves no-sync execution and ID-stable
pause/resume with an IP change, exercises a native checkpoint by default, then
destroys the VM and verifies the machine, image, and local claim are gone.

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=machine0 \
CRABBOX_LIVE_MACHINE0_SIZE=medium \
CRABBOX_LIVE_MACHINE0_IMAGE=ubuntu-24-04 \
CRABBOX_LIVE_MACHINE0_REGION=eu \
CRABBOX_LIVE_MACHINE0_KEY=ci-key \
scripts/live-smoke.sh
```

Use `CRABBOX_LIVE_MACHINE0_SIZE`, `CRABBOX_LIVE_MACHINE0_IMAGE`,
`CRABBOX_LIVE_MACHINE0_REGION`, and `CRABBOX_LIVE_MACHINE0_CLI` to select the
safe test capacity. Defaults are `medium`, `ubuntu-24-04`, and `eu`.
`CRABBOX_LIVE_MACHINE0_CHECKPOINT=0` skips the checkpoint lane; checkpoint
coverage defaults on. The runner never enables desktop/VNC or opens ports.

## Desktop and network security

Machine0 does not provide a built-in desktop. `--desktop`, `--browser`, and
`--code` use Crabbox's existing Linux SSH preparation and tunnel flow. An
optional `machine0.desktopImage` can select a prepared reusable desktop image;
when empty, Crabbox's automatic package preparation currently assumes the
Ubuntu/default apt-compatible image. NixOS and custom images should provide a
prepared `machine0.desktopImage` with the required desktop, VNC, and WebVNC
packages instead of relying on automatic setup.

```sh
crabbox warmup --provider machine0 --desktop --browser
crabbox webvnc --provider machine0 --id <lease> --open
```

Crabbox does not open VNC `5901` or WebVNC `6080` publicly. Access remains
inside the SSH tunnel, and the provider never attaches a Machine0 profile or
forwards profile credentials during creation. Machine0's authenticated HTTPS
URL proxies the VM's port 80 and is retained in provider metadata for
inspection; it is not a substitute for the SSH-tunneled desktop path.

Machine0 images can contain credentials written into the guest, and those
credentials survive snapshots. Keep image contents generic, review them before
sharing, and use Crabbox's normal allowlisted run-time environment forwarding
instead of baking secrets into a reusable image.

## CLI contract

The adapter uses the documented CLI. Prior lifecycle testing used
`@machine0/cli` 1.0.155:

- `machine0 new`, `get --json`, `ls --json`, `start`, `suspend --yes`, and
  `rm --yes` for VM lifecycle;
- `machine0 ssh` only to verify/materialize Machine0-managed key access;
- `machine0 sizes --all --json` for live availability and prices;
- `machine0 images ls/get/save/rm` and image-version removal for native
  checkpoints.

On 2026-08-28, read-only checks with `@machine0/cli` 1.0.164 observed native
`get <UUID>` returning `No such procedure` for an existing VM, while inventory
and full details by name succeeded under the same authentication. The repaired
Crabbox UUID lookup returned that same VM's verified identity and SSH username;
an absent UUID returned a clean absence error. A missing-procedure error alone
neither diagnoses authentication failure nor establishes resource existence.
These checks did not verify the full 1.0.164 lifecycle. The
[public 1.0.164 package](https://registry.npmjs.org/@machine0/cli/-/cli-1.0.164.tgz)
routes canonical UUIDs to `machines.getMachineById`, names to
`machines.getByName`, and inventory to `machines.list` through the same CLI
authentication client. The [native machine command documentation](https://docs.machine0.io/cli/machines)
describes `get <vm>`; the separately documented MCP UUID lookup uses a different
transport and is not Crabbox's CLI contract.

Crabbox therefore resolves canonical UUIDs through inventory and verified
name-addressed full details without issuing native UUID `get` first or matching
an error message to trigger a fallback. Both reads retain the existing outage
retry behavior. Mutations remain single-attempt, with existing ownership guards
unchanged; no direct API or MCP authentication path is added.

Crabbox does not use Machine0's published OpenAPI document because it is not
the VM control-plane contract. All command execution is behind an injectable
runner so unit tests require neither a Machine0 account nor network access.
