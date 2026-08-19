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
aliases is each adapter's `provider.go` (`Name()`, `Aliases()`, `Spec()`).

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

When an SSH-lease provider can be exercised from local credentials, add a
provider-specific path in `scripts/live-smoke.sh`. The smoke should use explicit
`--provider` routing for `warmup`, `status`, `run`, `list`, and `stop`, and its
remote command should not assume a particular project language unless it is
provider-specific.

If the provider is still unimplemented or the only credible proof is an
environment-specific local runbook, keep the smoke manual and document the real
acceptance contract first. Do not add a placeholder `scripts/live-smoke.sh`
branch that cannot run on a fresh operator machine with the documented
prerequisites.

Incus is the current example of an explicit opt-in local path: the default live
matrix still skips it, while `CRABBOX_LIVE_PROVIDERS=incus` and
`CRABBOX_LIVE_DOCTOR_PROVIDERS=incus` run the documented Apple Silicon / local
testbed contract when those prerequisites are actually present.

## Choosing a provider

Use the [canonical provider decision matrix](../providers/README.md#provider-decision-matrix)
to compare every built-in provider by execution model, access semantics, target
OS, substrate, location, GPU orientation, cleanup behavior, best fit, and main
caveat. The matrix is generated from the live CLI provider spec plus checked-in
selection metadata, so registration and documentation drift fail the docs gate.

`crabbox providers --json` remains the low-level compiled spec. `crabbox doctor`
checks whether the selected provider is usable from the current environment.

## Machine classes

An explicitly selected canonical `--class tiny|small|standard|fast|large|beast` maps to an
ordered list of provider machine types. Crabbox tries the profile `primary`
first and then its declared `fallbacks` in order when provider policy permits a
capacity retry. An inherited global class does not override provider-native
defaults unless that provider already defines such behavior. The catalog in
each mapped provider adapter is also the runtime source of truth:

```text
Hetzner
tiny      ccx13 (2 vCPU, 8 GB RAM), cpx22, cx23
small     ccx23 (4 vCPU, 16 GB RAM), cpx32, cx33
standard  ccx33 (8 vCPU, 32 GB RAM), cpx62, cx53
fast      ccx43 (16 vCPU, 64 GB RAM), cpx62, cx53
large     ccx53 (32 vCPU, 128 GB RAM), ccx43, cpx62, cx53
beast     ccx63 (48 vCPU, 192 GB RAM), ccx53, ccx43, cpx62, cx53

AWS (Linux)
tiny      m7a.large (2 vCPU, 8 GiB RAM), m7i.large, c7a.xlarge, c7i.xlarge, t3.small
small     c7a.2xlarge (8 vCPU, 16 GiB RAM), c7i.2xlarge, m7a.xlarge, m7i.xlarge, c7a.xlarge, t3.small
standard  c7a.8xlarge (32 vCPU, 64 GiB RAM), c7i.8xlarge, m7a.8xlarge, m7i.8xlarge, c7a.4xlarge, t3.small
fast      c7a.16xlarge (64 vCPU, 128 GiB RAM), c7i.16xlarge, m7a.16xlarge, m7i.16xlarge, c7a.12xlarge, c7a.8xlarge, t3.small
large     c7a.24xlarge (96 vCPU, 192 GiB RAM), c7i.24xlarge, m7a.24xlarge, m7i.24xlarge, r7a.24xlarge, c7a.16xlarge, c7a.12xlarge, t3.small
beast     c7a.48xlarge (192 vCPU, 384 GiB RAM), c7i.48xlarge, m7a.48xlarge, m7i.48xlarge, r7a.48xlarge, c7a.32xlarge, c7i.32xlarge, m7a.32xlarge, c7a.24xlarge, c7a.16xlarge, t3.small

AWS (Linux ARM64)
tiny      m7g.large, c7g.xlarge, r7g.large
small     c7g.2xlarge, m7g.xlarge, r7g.large, c7g.xlarge

AWS Windows (normal)
tiny      m7a.large, m7i.large, t3.large
small     c7a.2xlarge, c7i.2xlarge, m7a.xlarge, m7i.xlarge, t3.xlarge
standard  m7i.large, m7a.large, t3.large
fast      m7i.xlarge, m7a.xlarge, t3.xlarge, t3.large
large     m7i.2xlarge, m7a.2xlarge, t3.2xlarge, t3.large
beast     m7i.4xlarge, m7a.4xlarge, m7i.2xlarge, t3.large

AWS Windows WSL2
tiny      m8i.large, m8i-flex.large, c8i.xlarge, r8i.large
small     c8i.2xlarge, m8i.xlarge, m8i-flex.xlarge, r8i.large, c8i.xlarge
standard  m8i.large, m8i-flex.large, c8i.large, r8i.large
fast      m8i.xlarge, m8i-flex.xlarge, c8i.xlarge, r8i.xlarge, m8i.large
large     m8i.2xlarge, m8i-flex.2xlarge, c8i.2xlarge, r8i.2xlarge, m8i.large
beast     m8i.4xlarge, m8i-flex.4xlarge, c8i.4xlarge, r8i.4xlarge, m8i.2xlarge, m8i.large

AWS macOS (all classes)
mac2.metal, mac2-m2.metal, mac2-m2pro.metal, mac-m4.metal, mac-m4pro.metal,
mac-m4max.metal, mac2-m1ultra.metal, mac-m3ultra.metal, then mac1.metal unless
`--type` is set

Google Cloud
tiny      c4-standard-4 (4 vCPU, 15 GB RAM), c3-standard-4, n2-standard-4, n2d-standard-4
small     c4-standard-8 (8 vCPU, 30 GB RAM), c3-standard-8, n2-standard-8, n2d-standard-8, c4-standard-4
standard  c4-standard-32 (32 vCPU, 120 GB RAM), c3-standard-22, n2-standard-32, n2d-standard-32
fast      c4-standard-64 (64 vCPU, 240 GB RAM), c3-standard-44, n2-standard-64, n2d-standard-64, c4-standard-32
large     c4-standard-96 (96 vCPU, 360 GB RAM), c3-standard-88, n2-standard-80, n2d-standard-96, c4-standard-64
beast     c4-standard-192 (192 vCPU, 720 GB RAM), c4-standard-96, c3-standard-176, c3-standard-88, n2d-standard-224, n2-standard-128

Azure (Linux)
tiny      Standard_D2ads_v6 (2 vCPU, 8 GiB RAM), Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, Standard_F2s_v2
small     Standard_D8ads_v6 (8 vCPU, 32 GiB RAM), Standard_D8ds_v6, Standard_F8s_v2, Standard_D8ads_v5, Standard_D8ds_v5, Standard_D4ads_v6, Standard_D4ds_v6, Standard_F4s_v2
standard  Standard_D32ads_v6 (32 vCPU, 128 GiB RAM), Standard_D32ds_v6, Standard_F32s_v2, Standard_D32ads_v5, Standard_D32ds_v5, Standard_D16ads_v6, Standard_D16ds_v6, Standard_F16s_v2
fast      Standard_D64ads_v6 (64 vCPU, 256 GiB RAM), Standard_D64ds_v6, Standard_F64s_v2, Standard_D64ads_v5, Standard_D64ds_v5, Standard_D48ads_v6, Standard_D48ds_v6, Standard_F48s_v2, Standard_D32ads_v6, Standard_D32ds_v6, Standard_F32s_v2
large     Standard_D96ads_v6 (96 vCPU, 384 GiB RAM), Standard_D96ds_v6, Standard_D96ads_v5, Standard_D96ds_v5, Standard_D64ads_v6, Standard_D64ds_v6, Standard_F64s_v2, Standard_D48ads_v6, Standard_D48ds_v6, Standard_F48s_v2
beast     Standard_D192ds_v6 (192 vCPU, 768 GiB RAM), Standard_D128ds_v6, Standard_D96ads_v6, Standard_D96ds_v6, Standard_D96ads_v5, Standard_D96ds_v5, Standard_D64ads_v6, Standard_D64ds_v6, Standard_F64s_v2

Namespace Instance
tiny      1x2 (1 vCPU, 2 GB RAM)
small     2x4 (2 vCPU, 4 GB RAM)
standard  4x8 (4 vCPU, 8 GB RAM)
fast      8x16 (8 vCPU, 16 GB RAM)
large     16x32 (16 vCPU, 32 GB RAM)
beast     32x64 (32 vCPU, 64 GB RAM)

Azure (Linux)
tiny      Standard_D2ads_v6, Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, Standard_F2s_v2
small     Standard_D8ads_v6, Standard_D8ds_v6, Standard_F8s_v2, Standard_D8ads_v5, Standard_D8ds_v5, Standard_D4ads_v6, Standard_D4ds_v6, Standard_F4s_v2

Azure (Linux/Windows ARM64)
tiny      Standard_D2pds_v6, Standard_D2ps_v6
small     Standard_D8pds_v6, Standard_D8ps_v6, Standard_D4pds_v6, Standard_D4ps_v6

Azure Windows (normal/WSL2)
tiny      Standard_D2ads_v6, Standard_D2ds_v6, Standard_D2ads_v5, Standard_D2ds_v5, Standard_D2as_v6
small     Standard_D8ads_v6, Standard_D8ds_v6, Standard_D8ads_v5, Standard_D8ds_v5, Standard_D8as_v6

Namespace Devbox
tiny      S
small     S
standard  S
fast      M
large     L
beast     XL

Cloudflare Containers (each canonical class -> standard-4)
lite, basic, standard-1, standard-2, standard-3, standard-4
```

The four canonical classes are profiled per exact selector. AWS declares
Linux/amd64, Linux/arm64, Windows normal/amd64, Windows WSL2/amd64, and
macOS/mixed. Its macOS sequence is Apple Silicon first and ends with the Intel
`mac1.metal`; every canonical class exposes the same sequence. Azure declares
Linux/amd64, Linux/arm64, Windows normal/amd64, Windows normal/arm64, and
Windows WSL2/amd64. It intentionally has no Windows WSL2/arm64 profile because
that combination is rejected. GCP, Hetzner, Namespace Devbox,
Namespace Instance, Cloudflare, DigitalOcean, Linode, OVHcloud, Scaleway,
Vultr, Phala, and Tencent Cloud declare Linux/amd64 profiles.
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

DigitalOcean maps every canonical class to the smallest Phase 1 default size
`s-1vcpu-1gb`. Use `--type <droplet-size-slug>` when you need a larger exact
Droplet size.

Linode maps every canonical class to the smallest Phase 1 default size `g6-standard-1`.
Use `--type <linode-type-slug>` when you need a different exact instance type.

Vultr maps every canonical class to the smallest Phase 1 default plan `vc2-1c-1gb`.
Use `--type <vultr-plan-id>` when you need a different exact instance type.

Scaleway maps every canonical class to the smallest foundation default type `DEV1-S`.
Use `--type <scaleway-commercial-type>` when you need a different exact
Scaleway Instances commercial type. The live lifecycle backend is not
implemented yet, so this is a config/provider contract rather than a live
capacity fallback path in this branch.

## Brokered provider behavior

### Hetzner

- imports or reuses the lease SSH key;
- creates a server with Crabbox labels;
- uses the configured image and location;
- falls back across class server types when capacity or quota rejects a request;
- fetches server-type hourly prices when cost estimates need provider pricing.

### AWS

- signs EC2 Query API calls inside the Worker;
- imports or reuses an EC2 key pair;
- creates or reuses the `crabbox-runners` security group with SSH ingress limited
  to configured CIDRs or the request source IP;
- launches one-time Linux Spot or On-Demand instances;
- launches native Windows Server leases with EC2Launch PowerShell user data, then
  a post-SSH bootstrap for OpenSSH/Git/user setup; `--desktop` adds TightVNC,
  auto-logon, and first-network flyout suppression;
- launches EC2 Mac leases on available Dedicated Hosts with On-Demand capacity,
  optionally pinned by `CRABBOX_HOST_ID` or `hostId` (`CRABBOX_AWS_MAC_HOST_ID`
  and `aws.macHostId` remain compatibility aliases); brokered pinning requires
  admin authentication;
- tags instances, volumes, and Spot requests;
- falls back across broad C/M/R instance families, including account-policy and
  capacity rejections, and can fall back to a small burstable type when policy
  rejects high-core candidates;
- preflights applied Spot/On-Demand vCPU quotas in brokered mode when Service
  Quotas allows it, recording skipped candidates as quota attempts;
- honors `--market spot|on-demand` on `warmup` and `run` for one-off overrides;
- uses Spot placement score across configured regions in direct mode and can fall
  back to On-Demand after Spot capacity/quota failures when configured;
- fetches Spot price history when cost estimates need provider pricing.

`crabbox list` marks brokered machines as `orphan=no-active-lease` when their
provider label references a lease no longer active in the coordinator. This is an
operator hint only; `keep=true` machines are never deleted automatically.

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

- **proxmox** — clones a configured Linux QEMU template, injects SSH via
  cloud-init, discovers the IP and bootstraps through the QEMU guest agent, then
  uses normal Crabbox SSH sync/run/release. Configure with `CRABBOX_PROXMOX_*` /
  the `proxmox` config section.
- **firecracker** — currently exposes the self-hosted Linux KVM provider
  contract and a read-only doctor surface. It validates the host OS,
  `/dev/kvm`, the configured Firecracker binary, kernel/rootfs files, and CNI
  directories without starting a microVM. Configure with
  `CRABBOX_FIRECRACKER_*` / the `firecracker` config section.
- **parallels** — creates a linked clone from a configured source VM and optional
  snapshot, starts it, discovers the guest IP through `prlctl`, then uses normal
  SSH sync/run/release. Supports Linux, macOS, and Windows guests that already
  expose the matching SSH contract. Configure with `CRABBOX_PARALLELS_*`.
- **local-container** (alias `docker`) — starts a labeled container on a local
  Docker-compatible runtime, publishes SSH on loopback, syncs over SSH, and
  removes it on `stop`. It detects an installed `docker` or `podman` CLI; if
  both are present, `docker` is selected unless `localContainer.runtime` is set
  explicitly. Cache volumes use named volumes. It does not bind-mount the repo
  or the Docker-compatible socket by default. Reads `DOCKER_HOST` for socket
  pass-through.
- **multipass** (alias `mp`) — launches a local Ubuntu VM through Canonical
  Multipass with cloud-init, resolves the VM IP through `multipass info`, syncs
  over SSH, and deletes the VM with `multipass delete --purge`. Cache volumes
  are host directories mounted into the VM.
- **daytona** — creates a sandbox from `daytona.snapshot`, syncs and runs through
  Daytona's SDK/toolbox APIs, and mints short-lived SSH tokens only for explicit
  `crabbox ssh` access.
- **exe-dev** — exe.dev owns auth and lifecycle through `ssh exe.dev`; Crabbox
  treats the returned `ssh_dest` as a normal Linux SSH lease (public SSH only, no
  Tailscale).
- **kubevirt** — applies a standard KubeVirt `VirtualMachine`, controls it with
  `virtctl`, and carries SSH, rsync, and desktop tunnels through
  `virtctl port-forward --stdio`.
- **external** — invokes a configured executable for lifecycle operations and
  consumes the returned SSH target. Provider-specific logic and credentials
  remain outside Crabbox.
- **namespace-devbox** — Namespace owns Devbox auth and lifecycle through the
  `devbox` CLI; Crabbox treats the prepared Devbox as a normal Linux SSH lease.
- **nebius** — creates a Nebius Compute VM through the authenticated `nebius`
  CLI, injects a per-lease SSH key with cloud-init, waits for dynamic public
  IPv4 and SSH readiness, then uses normal Crabbox SSH sync/run/release.
- **runpod** — leases a RunPod GPU pod with public SSH (no Tailscale); auth from
  `RUNPOD_API_KEY`.
- **semaphore** — creates a standalone Semaphore job, waits for host/port metadata
  and a debug SSH key, then runs the standard SSH path. Use it to run in the same
  machine image, secret context, and cache plane as Semaphore CI.
- **sprites** — creates a sprite, installs OpenSSH and rsync inside it, and reaches
  SSH through `sprite proxy` for a fast Linux microVM on the standard SSH path.
- **scaleway** — is registered as a direct Linux SSH-lease provider for Scaleway
  Instances with Scaleway SDK credentials, per-lease managed IAM SSH keys,
  cloud-init bootstrap, Crabbox-owned tags, and direct cleanup. `doctor` checks
  SDK config and auth material discovery without creating resources.

Delegated-run providers (`cloudflare`, `cloudflare-sandbox`,
`azure-dynamic-sessions`, `blaxel`, `e2b`, `islo`, `modal`, `tensorlake`,
`upstash-box`, `blacksmith-testbox`, `wandb`, `opensandbox`, `superserve`, and
`vercel-sandbox`) do not use the broker for run execution; each owns sandbox
lifecycle and command execution and syncs through its own API (gzipped archive
upload for most). Islo also exposes a direct `crabbox ssh` login helper for
kept sandboxes at `<sandbox>.islo`, but Islo run/sync remains delegated. See the
linked provider pages for per-provider auth and configuration.

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

`provider: ssh` (aliases `static`, `static-ssh`) attaches to a preexisting host —
no provisioning, no cleanup:

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

```yaml
provider: ssh
target: windows
windows:
  mode: normal
static:
  host: win-dev.local
  user: alice
  port: "22"
  workRoot: C:\crabbox
```

`target: windows` supports `windows.mode: normal` and `windows.mode: wsl2`:

- **normal** uses PowerShell over OpenSSH and syncs the manifest as a tar archive.
- **wsl2** keeps the POSIX SSH contract: commands run through
  `wsl.exe --exec bash -lc`, rsync uses `wsl.exe rsync`, and `static.workRoot`
  should be a WSL path such as `/home/alice/crabbox`. Managed AWS WSL2 leases need
  nested virtualization, so they use the C8i/M8i/R8i families and enable nested
  virtualization at launch.

macOS also uses the POSIX contract and needs `git`, `rsync`, `tar`, and SSH.

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
