# Providers

Read when:

- changing Hetzner, AWS, Azure, or Google Cloud provisioning;
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
- **Coordinator** — `supported` means the provider *may* be brokered through the
  Cloudflare Worker; `never` means it always runs direct from the CLI. Only
  `aws`, `azure`, `gcp`, and `hetzner` are `supported`, and even those run direct
  unless a broker URL and token are configured (see
  [Configuration](configuration.md) and `crabbox config set-broker`).
- **Targets** — which of Linux, macOS, and Windows the provider can lease.

`internal/cli/provider_backend.go` defines the kinds, coordinator modes, and
feature flags; `internal/cli/config.go` holds the per-provider config sections
and the class-to-machine-type maps.

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

## Brokered providers

Four providers can be brokered through the Worker. Brokering adds lease records,
cost guardrails, usage accounting, scheduled cleanup, and the web portal; the
data plane (SSH, rsync, command execution) still runs directly from the CLI to
the box.

```text
hetzner   Hetzner Cloud servers          (Linux)
aws       AWS EC2 instances              (Linux, native Windows, Windows WSL2, EC2 Mac)
azure     Azure Virtual Machines         (Linux, native Windows, Windows WSL2)
gcp       Google Cloud Compute Engine    (Linux)
```

The `azure` family can also route to the delegated Azure Container Apps Dynamic
Sessions backend with `azure.backend: dynamic-sessions` or
`--azure-backend dynamic-sessions`.

When no coordinator is configured, these providers still work in **direct mode**:
the CLI talks to the cloud API itself using local credentials (AWS SDK chain,
Azure credentials, Google Application Default Credentials,
`HCLOUD_TOKEN`/`HETZNER_TOKEN`, or `DIGITALOCEAN_TOKEN` for DigitalOcean).
Direct mode has no Durable Object alarm; cleanup
is best-effort through provider labels and manual `crabbox cleanup`. Prefer the
brokered path when a broker is available.

## Direct SSH-lease providers

These provision or attach an SSH-reachable box and use the standard Crabbox SSH
sync/run/release path. None of them go through the Worker.

```text
ssh              Existing SSH host (no provisioning)      Linux, macOS, Windows
digitalocean     DigitalOcean Droplets                    Linux
parallels        Parallels Desktop linked clones          Linux, macOS, Windows
proxmox          Proxmox VE QEMU VM clones                Linux
incus            Incus containers or VMs over SSH         Linux
local-container  Local Docker-compatible containers       Linux
apple-vz         Apple Virtualization.framework Linux VM  Linux ARM64
multipass        Canonical Multipass local Ubuntu VMs     Linux
daytona          Daytona sandboxes (short-lived SSH)      Linux
exe-dev          exe.dev managed VMs (public SSH)         Linux
kubevirt         Generic KubeVirt virtual machines        Linux
external         Configured executable provider           Linux
namespace-devbox Namespace Devboxes                       Linux
runpod           RunPod GPU pods (public SSH)             Linux
semaphore        Semaphore CI jobs                        Linux
sprites          Sprites microVMs through sprite proxy    Linux
```

## Delegated-run providers

These run the command inside a sandbox/proof runner; Crabbox does not lease or
SSH into a box. Local sync options (`--no-sync`, rsync flags) are rejected - the
provider owns sync. Most are Linux-only. `anthropic-sandbox-runtime` is local
to the current macOS or Linux host.

```text
cloudflare              Cloudflare Containers (Worker runtime)
azure-dynamic-sessions  Azure Container Apps custom-container Dynamic Sessions
docker-sandbox          Docker Sandboxes through the standalone sbx CLI
e2b                     E2B Firecracker sandboxes
islo                    Islo sandboxes
modal                   Modal Sandboxes
opencomputer            OpenComputer Linux VMs
anthropic-sandbox-runtime Anthropic Sandbox Runtime through the local srt CLI
tensorlake              Tensorlake Firecracker sandboxes
upstash-box             Upstash sandboxes
blacksmith-testbox      Blacksmith CI test runner (proof/session)
wandb                   Weights & Biases run sandboxes
```

## Service-control providers

These expose provider-native service inspection/control without an arbitrary
command execution contract.

```text
railway                 Railway service status and stop controls
```

## Provider pages

- [Provider reference](../providers/README.md): one page per built-in backend.
- [AWS](../providers/aws.md): EC2 Linux, Windows, WSL2, EC2 Mac, capacity, AMIs, security groups.
- [Azure](../providers/azure.md): Azure Linux/native Windows, shared infra, capacity, cleanup.
- [Azure Dynamic Sessions](../providers/azure-dynamic-sessions.md): delegated Azure Container Apps sandbox execution.
- [Google Cloud](../providers/gcp.md): GCP Compute Engine SSH leases.
- [Hetzner](../providers/hetzner.md): Linux-only managed provider, classes, cleanup.
- [DigitalOcean](../providers/digitalocean.md): direct Linux Droplet leases.
- [Static SSH](../providers/ssh.md): existing Linux, macOS, and Windows SSH hosts.
- [Parallels](../providers/parallels.md): local or remote Mac Parallels Desktop VM clones and small Mac fleets.
- [Proxmox](../providers/proxmox.md): direct Proxmox VE Linux QEMU VM clones.
- [Incus](../providers/incus.md): direct Incus Linux SSH leases plus an opt-in Apple Silicon / local live smoke contract.
- [Local Container](../providers/local-container.md): local Linux containers through Docker-compatible runtimes.
- [Apple VZ](../providers/apple-vz.md): local Linux VMs through Apple's `Virtualization.framework`.
- [Multipass](../providers/multipass.md): local Ubuntu VMs through Canonical Multipass.
- [Daytona](../providers/daytona.md): Daytona SDK/toolbox sandbox leases.
- [exe.dev](../providers/exe-dev.md): exe.dev VMs exposed as SSH leases.
- [KubeVirt](../providers/kubevirt.md): generic KubeVirt VMs over Kubernetes control-plane forwarding.
- [External](../providers/external.md): configured executable provider protocol for private integrations.
- [Namespace Devbox](../providers/namespace-devbox.md): Namespace Devbox SSH leases.
- [Railway](../providers/railway.md): Railway service status and stop controls.
- [RunPod](../providers/runpod.md): RunPod GPU pods over public SSH.
- [Semaphore](../providers/semaphore.md): Semaphore CI job leases.
- [Sprites](../providers/sprites.md): Sprites microVM SSH leases through `sprite proxy`.
- [Tenki](../providers/tenki.md): Tenki sandbox VM SSH leases through `tenki sandbox ssh-proxy`.
- [Cloudflare](../providers/cloudflare.md): delegated Cloudflare Containers execution.
- [Docker Sandbox](../providers/docker-sandbox.md): delegated Docker Sandbox execution through the standalone `sbx` CLI.
- [Anthropic Sandbox Runtime](../providers/anthropic-sandbox-runtime.md): local one-shot delegated execution through `srt` on macOS/Linux.
- [Islo](../providers/islo.md): delegated Islo sandbox execution.
- [E2B](../providers/e2b.md): delegated E2B sandbox execution.
- [Modal](../providers/modal.md): delegated Modal Sandbox execution.
- [OpenComputer](../providers/opencomputer.md): delegated OpenComputer Linux VM execution through the OpenComputer REST API.
- [Tensorlake](../providers/tensorlake.md): delegated Tensorlake Firecracker sandbox execution.
- [Upstash Box](../providers/upstash-box.md): delegated Upstash sandbox execution.
- [Blacksmith Testbox](../providers/blacksmith-testbox.md): delegated Blacksmith CI runner.
- [Weights & Biases](../providers/wandb.md): delegated W&B run sandbox execution.
- [Provider backends](../provider-backends.md): guide for adding a new provider/backend/plugin.

## Machine classes

`--class standard|fast|large|beast` (default `beast`) maps to an ordered list of
provider machine types. Crabbox tries each in turn, falling back when capacity or
quota rejects a request. The maps below come from `internal/cli/config.go` and
`internal/cli/gcp.go`:

```text
Hetzner
standard  ccx33, cpx62, cx53
fast      ccx43, cpx62, cx53
large     ccx53, ccx43, cpx62, cx53
beast     ccx63, ccx53, ccx43, cpx62, cx53

AWS (Linux)
standard  c7a.8xlarge, c7i.8xlarge, m7a.8xlarge, m7i.8xlarge, c7a.4xlarge
fast      c7a.16xlarge, c7i.16xlarge, m7a.16xlarge, m7i.16xlarge, c7a.12xlarge, c7a.8xlarge
large     c7a.24xlarge, c7i.24xlarge, m7a.24xlarge, m7i.24xlarge, r7a.24xlarge, c7a.16xlarge, c7a.12xlarge
beast     c7a.48xlarge, c7i.48xlarge, m7a.48xlarge, m7i.48xlarge, r7a.48xlarge, c7a.32xlarge, c7i.32xlarge, m7a.32xlarge, c7a.24xlarge, c7a.16xlarge

AWS Windows (normal)
standard  m7i.large, m7a.large, t3.large
fast      m7i.xlarge, m7a.xlarge, t3.xlarge
large     m7i.2xlarge, m7a.2xlarge, t3.2xlarge
beast     m7i.4xlarge, m7a.4xlarge, m7i.2xlarge

AWS Windows WSL2
standard  m8i.large, m8i-flex.large, c8i.large, r8i.large
fast      m8i.xlarge, m8i-flex.xlarge, c8i.xlarge, r8i.xlarge
large     m8i.2xlarge, m8i-flex.2xlarge, c8i.2xlarge, r8i.2xlarge
beast     m8i.4xlarge, m8i-flex.4xlarge, c8i.4xlarge, r8i.4xlarge, m8i.2xlarge

AWS macOS (all classes)
mac2.metal, mac2-m2.metal, mac2-m2pro.metal, mac-m4.metal, mac-m4pro.metal,
mac-m4max.metal, mac2-m1ultra.metal, mac-m3ultra.metal, then mac1.metal unless
`--type` is set

Google Cloud
standard  c4-standard-32, c3-standard-22, n2-standard-32, n2d-standard-32
fast      c4-standard-64, c3-standard-44, n2-standard-64, n2d-standard-64, c4-standard-32
large     c4-standard-96, c3-standard-88, n2-standard-80, n2d-standard-96, c4-standard-64
beast     c4-standard-192, c4-standard-96, c3-standard-176, c3-standard-88, n2d-standard-224, n2-standard-128

Namespace Devbox
standard  S
fast      M
large     L
beast     XL

Cloudflare Containers (any class -> standard-4)
lite, basic, standard-1, standard-2, standard-3, standard-4
```

An explicit `--type` is treated as an exact provider type request. If that type
is rejected, Crabbox fails clearly instead of silently choosing a different
instance type. Drop `--type` and use a class when you want fallback. See
[Capacity and fallback](capacity-fallback.md) for the full fallback model.

DigitalOcean maps every class to the smallest Phase 1 default size
`s-1vcpu-1gb`. Use `--type <droplet-size-slug>` when you need a larger exact
Droplet size.

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
  and `aws.macHostId` remain compatibility aliases);
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
- **runpod** — leases a RunPod GPU pod with public SSH (no Tailscale); auth from
  `RUNPOD_API_KEY`.
- **semaphore** — creates a standalone Semaphore job, waits for host/port metadata
  and a debug SSH key, then runs the standard SSH path. Use it to run in the same
  machine image, secret context, and cache plane as Semaphore CI.
- **sprites** — creates a sprite, installs OpenSSH and rsync inside it, and reaches
  SSH through `sprite proxy` for a fast Linux microVM on the standard SSH path.

Delegated-run providers (`cloudflare`, `azure-dynamic-sessions`, `e2b`, `islo`,
`modal`, `tensorlake`, `upstash-box`, `blacksmith-testbox`, `wandb`) do not use
the broker or an SSH lease; each owns sandbox lifecycle and command execution and
syncs through its own API (gzipped archive upload for most). See the linked
provider pages for per-provider auth and configuration.

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
