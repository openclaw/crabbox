# Providers

Read when:

- changing AWS, Azure, Daytona, Google Cloud, or Hetzner provisioning;
- adding or wiring a new backend;
- adjusting machine classes, fallback order, regions, or images.

A *provider* is the backend that supplies the remote box a lease runs on. Crabbox
selects one with `--provider <name>` or the `provider:` config key, normalizing
aliases on the way in. Every built-in adapter lives under
`internal/providers/<name>` and is registered for its side effects in
`internal/providers/all/all.go`; the source-of-truth list of identifiers and
aliases is each adapter's `Spec()` in `provider.go` (`ProviderSpec.Name` and
`ProviderSpec.Aliases`).

## How a provider is wired

Each adapter declares a `Spec` that drives how Crabbox treats it:

- **Kind** — `ssh-lease` (Crabbox provisions or connects to an SSH-reachable box
  and owns the full lifecycle, sync, run, and release), `delegated-run` (the
  provider owns sync and execution; there is no SSH lease), or
  `service-control` (Crabbox can inspect or stop a provider-owned service, but
  cannot execute arbitrary run commands there).
- **Coordinator** — `supported` means the provider *may* be brokered through
  either coordinator runtime; `never` means it always runs direct from the CLI. Only
  `aws`, `azure`, `daytona`, `gcp`, and `hetzner` are `supported`, and even those
  run direct unless a broker URL and token are configured (see
  [Configuration](configuration.md) and `crabbox config set-broker`).
- **Targets** — which runtime category the provider can satisfy. OS-backed
  providers advertise Linux, macOS, or Windows; module/runtime providers can
  advertise `worker-runtime` when they execute source in a hosted runtime
  without SSH, POSIX shell, or filesystem sync semantics.

`internal/cli/provider_backend.go` defines the kinds, coordinator modes, and
feature flags; `internal/cli/config.go` holds per-provider config sections.
Mapped provider adapters own their class-profile catalog and runtime candidate
order.

See [Provider authoring](provider-authoring.md) for registration and backend
contracts, and [Provider live smoke](provider-live-smoke.md) for validation.
A smoke needs runnable prerequisites and explicit provider routing; use a
manual runbook when the shared harness cannot exercise the provider.

## Choosing a provider

Use the [canonical provider decision matrix](../providers/README.md#provider-decision-matrix)
to compare every built-in provider by execution model, access semantics, target
OS, substrate, location, GPU orientation, cleanup behavior, best fit, and main
caveat. The matrix is generated from the live CLI provider spec plus checked-in
selection metadata, so registration and documentation drift fail the docs gate.

`crabbox providers --json` remains the low-level compiled spec. `crabbox doctor`
checks whether the selected provider is usable from the current environment.

## Direct lease expiry coverage

The shared Linux guest guard covers **AWS, Azure, GCP, Hetzner, DigitalOcean,
Vultr, Linode, OVHcloud, Scaleway, Nebius, Lambda, Tencent Cloud, Incus,
Multipass, and Firecracker**. See the [deadline source and reclaim action
table](lifecycle-cleanup.md#guest-side-expiry-guard). Coordinator-managed leases
and Windows/macOS targets are excluded.

The following providers do not use that shared Linux bootstrap and receive no
guard (some have provider-native expiry instead):

- `agent-sandbox`, `anthropic-sandbox-runtime`, `apple-container`, `apple-machine`, `apple-vm`, `ascii-box`, `aws-lambda-microvm`, `azure-dynamic-sessions`.
- `azure-sandbox`, `blacksmith-testbox`, `blaxel`, `boxd`, `cloud-run-sandbox`, `cloudflare`, `cloudflare-dynamic-workers`, `cloudflare-sandbox`.
- `coder`, `codesandbox`, `crownest`, `cua`, `cubesandbox`, `daytona`, `docker-sandbox`, `e2b`.
- `exe-dev`, `external`, `fastapi-cloud`, `freestyle`, `github-codespaces`, `hostinger`, `hyperv`, `islo`.
- `kubevirt`, `local-container`, `lume`, `machine0`, `modal`, `morph`, `mxc`, `namespace-devbox`.
- `namespace-instance`, `nomad`, `nvidia-brev`, `opencomputer`, `opensandbox`, `orgo`, `parallels`, `phala`.
- `proxmox`, `railway`, `runpod`, `sealos-devbox`, `semaphore`, `serve`, `smolvm`, `sprites`.
- `ssh`, `superserve`, `tart`, `tenki`, `tensorlake`, `unikraft-cloud`, `upstash-box`, `vast`.
- `vercel-sandbox`, `wandb`, `windows-sandbox`, `xcp-ng`.

In particular, prepared/custom-bootstrapped VMs such as Proxmox, XCP-ng,
Parallels, and Apple VM remain uncovered; their existing provider cleanup still
applies. This guard does not add a bootstrap path to hosted sandboxes or static
hosts. A guest shutdown is not deletion: stopped machines can remain billable
and still need `crabbox cleanup`.

To forbid new direct remote allocations, set
[`directLeases: deny`](configuration.md#direct-lease-policy), with optional
trusted `directLeaseAllow` exceptions. The always-exempt local/static set is
`ssh`, `local-container`, `docker-sandbox`, `apple-container`, `apple-machine`,
`apple-vm`, `anthropic-sandbox-runtime`, `mxc`, `multipass`, `firecracker`,
`lume`, `tart`, `hyperv`, and `windows-sandbox`.

## Machine classes

An explicitly selected canonical `--class tiny|small|standard|fast|large|beast` maps to an
ordered list of provider machine types. Crabbox tries the profile `primary`
first and then its declared `fallbacks` in order when provider policy permits a
capacity retry. An inherited global class does not override provider-native
defaults unless that provider already defines such behavior. The adapter's
compiled catalog is the source of truth for machine types,
resource sizes, selectors, and fallback order. Inspect it without credentials
or provider API calls:

```sh
crabbox providers describe aws --json | jq '.classCatalog'
crabbox providers describe hetzner --json | jq '.classCatalog'
crabbox providers --json | jq '.[] | {provider, classCatalog}'
```

See [`providers describe`](../commands/providers.md#providers-describe) for the
schema and [Capacity and fallback](capacity-fallback.md) for retry policy.
Provider-specific sizing and image notes live in the
[provider reference](../providers/README.md).

All six canonical classes are profiled per exact selector. AWS declares
Linux/amd64, Linux/arm64, Windows normal/amd64, Windows WSL2/amd64, and
macOS/mixed. Its macOS sequence is Apple Silicon first and ends with the Intel
`mac1.metal`; every canonical class exposes the same sequence. Azure declares
Linux/amd64, Linux/arm64, Windows normal/amd64, Windows normal/arm64, and
Windows WSL2/amd64. Windows WSL2/arm64 is rejected. GCP, Hetzner, Namespace Devbox,
Namespace Instance, Cloudflare, DigitalOcean, Linode, OVHcloud, Scaleway,
Vultr, Phala, Tencent Cloud, and direct Daytona declare Linux/amd64 profiles.
Windows with no mode normalizes to `normal`; selection prefers an exact
architecture and may fall back only to a declared `mixed` profile. It never
crosses Windows modes or amd64/arm64 profiles.
An exact lowercase canonical class with no exact or `mixed` selector match
fails with exit 2 rather than becoming a literal provider-native type.
Noncanonical uppercase, padded, and custom strings retain provider-specific
legacy handling.

Both `crabbox providers --json` and schema-v2
`crabbox providers describe <provider> --json` expose the same authoritative
`classCatalog`. `disposition` is `mapped` when explicit canonical class intent has a
supported provider-owned machine profile, and `unmapped` otherwise. Unmapped
does not promise that every input provenance rejects class or that no legacy
metadata can contain one. The `profiles` and every `fallbacks` field are always
arrays. Profiles are sorted by
canonical class, target, Windows mode, and architecture, while machine
fallbacks stay in runtime order. Unknown vCPU or memory metadata is `null`.
Known memory always carries its provider-native `MB`, `MiB`, `GB`, or `GiB`
unit; GCP decimal GB ratios are not relabeled as GiB. The catalog is compiled
static data and does not read config, credentials, provider state, local files,
or the network.

For compatibility, unversioned `crabbox providers --json` also keeps the
`classes` array introduced for AWS, Azure, GCP, Hetzner, and Namespace
Instance. It is a default Linux/amd64 summary derived from those providers'
authoritative profiles, not a second runtime mapping source. Other providers
omit it. Human `crabbox providers` prints this same five-provider projection;
target-aware and fallback-aware discovery belongs to `classCatalog`.

Azure full-caching ephemeral-disk eligibility remains a runtime policy layered
over its stable base profiles, not another profile dimension. Snapshot runs use
managed disks; supported stored types can still prepend; and Windows may expand
from the selected class to large then beast before applying the existing
full-caching filter.

An explicit `--type` bypasses class-profile selection. Provider-native override
fields and stored non-explicit types retain their documented precedence. Every
provider treats explicit type as exact. Drop `--type` and use a class when you
want the portable profile and capacity-fallback order. See
[Capacity and fallback](capacity-fallback.md).

Phala and Tencent Cloud apply these profiles when class is explicitly selected
through CLI, YAML, or environment provenance. Without explicit class intent,
Phala preserves its inexpensive `tdx.small`/provider-native default and Tencent
Cloud preserves its `SA5.MEDIUM2`/provider-native default. Explicit generic and
provider-native type overrides keep precedence.

Daytona maps adjacent classes to its three native container snapshot tiers.
With a custom or captured snapshot, `--class` validates the resource shape and
preserves the selected snapshot. YAML/environment class values only select a
tier when no snapshot is configured; existing snapshots retain their sizing.
It never sends resize overrides.
Brokered Daytona rejects `--class`; existing YAML/environment class values do
not override the coordinator's shared snapshot. See [Daytona configuration](daytona.md#config).

Vultr maps every canonical class to the smallest Phase 1 default plan `vc2-1c-1gb`.
Use `--type <vultr-plan-id>` when you need a different exact instance type.

[DigitalOcean](../providers/digitalocean.md), [Linode](../providers/linode.md),
and [Scaleway](../providers/scaleway.md) map classes to distinct machine sizes.
Inspect their compiled catalogs for the current types and fallback order;
`--type` selects an exact provider-native type.

## Brokered provider behavior

### Hetzner

The [Hetzner provider](../providers/hetzner.md) covers provisioning,
credentials, images, class fallback, and snapshots. See
[brokered cleanup confirmation](lifecycle-cleanup.md#brokered-hetzner-cleanup-confirmation)
for the coordinator's ownership checks and deletion journal.

### AWS

The [AWS provider](../providers/aws.md) covers Linux, Windows, and EC2 Mac
leases, key pairs, networking, tags, and host pinning. The
[AWS feature guide](aws.md) documents quota preflight, Spot/On-Demand selection,
pricing, and operator controls; [Capacity and fallback](capacity-fallback.md)
describes retry order.

`crabbox list` marks brokered machines as `orphan=no-active-lease` when their
provider label references a lease no longer active in the coordinator. This is an
operator hint only; listing does not delete machines. Recorded expiry still
applies regardless of `keep=true`.

The structured quota preflight and `provisioningAttempts` metadata belong to the
brokered Worker path; direct AWS fallback can still retry provider types but
without that telemetry.

## Direct provider notes

A minimal direct (no-coordinator) smoke looks like this:

```sh
tmp="$(mktemp)"
printf 'provider: hetzner\n' > "$tmp"
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= crabbox warmup --provider hetzner --class standard --ttl 15m --idle-timeout 4m
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= crabbox run --provider hetzner --id <slug> --no-sync -- echo direct-hetzner-ok
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= crabbox stop --provider hetzner <slug>
rm -f "$tmp"
```

Swap `--provider aws` (AWS SDK credentials) or `--provider gcp` (Google
Application Default Credentials) for direct cloud smoke. The direct GCP path uses
Google's Compute Go SDK and project-wide aggregated instance listing for resolve,
list, and cleanup.

Direct providers own their authentication and lifecycle contracts. Use the
[provider decision matrix](../providers/README.md#provider-decision-matrix)
for the full list, then the selected provider page for prerequisites, config,
commands, and cleanup behavior. Common entry points include
[Proxmox](../providers/proxmox.md), [Firecracker](../providers/firecracker.md),
[Parallels](../providers/parallels.md),
[Local Container](../providers/local-container.md),
[Multipass](../providers/multipass.md), [Daytona](../providers/daytona.md),
[exe.dev](../providers/exe-dev.md), [KubeVirt](../providers/kubevirt.md),
[External](../providers/external.md),
[Namespace Devbox](../providers/namespace-devbox.md),
[Nebius](../providers/nebius.md), [RunPod](../providers/runpod.md),
[Semaphore](../providers/semaphore.md), [Sprites](../providers/sprites.md),
and [Scaleway](../providers/scaleway.md).

Delegated-run providers own command execution and usually transfer files
through provider APIs. They do not use the coordinator for execution. Optional
SSH access, such as Islo's login helper, does not change the run/sync transport;
check the provider page before assuming SSH-lease features are available.

Module-runtime delegated providers are a narrower category for Worker-isolate
style runtimes. They should advertise `target=worker-runtime` and
`feature=module-run`, accept `crabbox run --script <file>` or
`--script-stdin` as source module input, and reject trailing `-- <command>`
argv rather than implying Linux shell semantics. A module-runtime target does
not imply SSH, rsync, archive sync, VNC, browser desktop, code-server, ports, or
POSIX filesystem behavior unless a provider explicitly documents and advertises
those capabilities.

`cloudflare-dynamic-workers` is the Cloudflare-family module-runtime provider.
It is distinct from `cloudflare`, which runs Linux commands in Cloudflare
Containers, and from `cloudflare-sandbox`, which runs Linux commands through a
configured Cloudflare Sandbox bridge. Dynamic Workers support `module-run`,
local claim cleanup, and run-session metadata, but not SSH, Crabbox sync,
Actions hydration, browser, desktop, code-server, ports, or container instance
classes.

## Static SSH targets

`provider: ssh` (aliases `static`, `static-ssh`) attaches to an existing host.
Crabbox does not provision or delete it. See the
[SSH provider configuration](../providers/ssh.md#configuration) for Linux,
macOS, native Windows, and WSL2 examples, and its
[host requirements](../providers/ssh.md#host-requirements) for shell and sync
tools. Optional [power hooks](../providers/ssh.md#power-hooks) require an explicit
dedicated-host contract.

## Tailscale is not a provider

Use `--tailscale` to add tailnet reachability to new managed Linux leases, or
point a static host at a MagicDNS name / `100.x` address when the host is already
on a tailnet. See [Tailscale](tailscale.md).

## Related docs

- [Infrastructure](../infrastructure.md)
- [Configuration](configuration.md)
- [Capacity and fallback](capacity-fallback.md)
- [Provider reference](../providers/README.md)
- [Provider backends](../provider-backends.md)
- [Tailscale](tailscale.md)
- [Runner bootstrap](runner-bootstrap.md)
- [Cost and usage](cost-usage.md)
