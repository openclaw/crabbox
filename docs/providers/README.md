# Provider Reference

Read when:

- choosing a Crabbox provider for a repo or one-off command;
- debugging provider-specific provisioning, sync, or command execution;
- changing provider registration, flags, config, or backend behavior.

## Provider model

Every provider registers a backend with one of three kinds:

- **SSH lease** — Crabbox provisions or connects to an SSH-reachable box and owns
  the full lease lifecycle (warmup, sync, run, ssh, cleanup). Core does the
  rsync/command execution directly to the box over SSH.
- **Delegated run** — a sandbox or proof runner. The provider owns sync and
  command execution end to end; there is no SSH lease and no local rsync.
- **Service control** — Crabbox can inspect or stop a provider-owned service,
  but cannot execute arbitrary commands there.

SSH-lease providers further differ by how they reach the cloud:

- **Brokered cloud** — `aws`, `azure`, `gcp`, and `hetzner` can run through the
  Crabbox coordinator on Cloudflare or Node/PostgreSQL. The coordinator owns
  cloud credentials, cost state, cleanup scheduling, and lease accounting.
  This is the normal shared-team path. Set with `config set-broker` and a
  broker URL (`CRABBOX_COORDINATOR`).
- **Direct cloud** — the same four providers without a configured broker, plus
  cloud providers that never broker (e.g. `digitalocean`, `linode`, `vultr`,
  `proxmox`, `hostinger`, `runpod`, `namespace-devbox`, `namespace-instance`,
  `semaphore`, `sprites`, `exe-dev`, `daytona`, `morph`). The CLI talks to the
  provider API itself and cleans up best-effort via provider labels.
- **Static SSH** — `ssh` connects to a preexisting machine you supply; no
  provisioning, no cleanup.
- **Local runtime** — `local-container` starts a labeled Linux container through
  a Docker-compatible local runtime (Docker Desktop, OrbStack, Colima),
  `apple-container` uses Apple's native `container` runtime on Apple silicon
  macOS, `apple-vm` launches a headless Linux VM through Apple's
  `Virtualization.framework`, `multipass` launches local Ubuntu VMs through
  Canonical Multipass, `tart` runs macOS VMs on Apple Silicon via Cirrus Labs
  tart, and `hyperv` creates local Windows VMs through Microsoft Hyper-V.
- **Delegated sandbox** — managed sandbox/proof runners that execute remotely
  without an SSH lease (e.g. `blaxel`, `e2b`, `modal`, `islo`, `cloudflare`,
  `cloudflare-sandbox`, `azure-dynamic-sessions`, `docker-sandbox`, `smolvm`).
  `anthropic-sandbox-runtime` is the local macOS/Linux delegated-run exception:
  Anthropic's `srt` executes on the current machine while still owning sync/run
  policy end to end.
  `windows-sandbox` is the local Windows delegated-run exception.

Select a provider per command with `--provider <name>` (env `CRABBOX_PROVIDER`),
or set `provider: <name>` in config. Provider flags are registered before
command parsing, so provider-specific flags work even when that provider is not
the default. Most names accept aliases (listed below).

<!-- BEGIN GENERATED PROVIDER MATRIX -->

## Provider decision matrix

This table combines the live provider spec compiled into the CLI with curated
selection metadata. Regenerate it with `node scripts/generate-provider-matrix.mjs`.
`scripts/check-docs.sh` fails when provider registration, metadata, docs paths, or
this generated table drift.

Current built-in surface: 72 providers (42 SSH lease, 28 delegated run, 2 service control).

Access terms:

- **Crabbox-managed SSH**: SSH uses Crabbox's normal client; the sync column shows whether run and sync use that data plane.
- **Provider-specific SSH**: an adapter-specific login helper, not the normal Crabbox data plane.
- **No SSH**: the provider owns command execution end to end.

| Provider | Status / category | Execution / access | Targets / substrate | Location / GPU | Lifecycle / cleanup | Best fit | Main caveat |
| --- | --- | --- | --- | --- | --- | --- | --- |
| [agent-sandbox](agent-sandbox.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; Kubernetes Agent Sandbox warm pool | `self-hosted`; GPU: unknown | Agent Sandbox SandboxClaim; owned SandboxClaim delete | Kubernetes-hosted delegated Linux execution | Requires kubectl, Agent Sandbox v0.5.0rc1 v1beta1 CRDs, a warm pool, explicit context, and RBAC |
| [anthropic-sandbox-runtime](anthropic-sandbox-runtime.md) (`srt`) | built-in; `delegated-run` · local-sandbox | No SSH; `provider-owned` · direct only; features: none | `linux`, `macos`; Anthropic Sandbox Runtime process sandbox | `local`; GPU: no | local runtime; one-shot process exit | Local policy-constrained command execution | No persistent lease, remote box, or SSH access |
| [apple-container](apple-container.md) (`apple`, `applecontainer`) | built-in; `ssh-lease` · local-runtime | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `cache-volume` | `linux`; Apple container runtime | `local`; GPU: no | Crabbox; container delete | Local Linux containers on Apple silicon | Requires Apple's container CLI and macOS |
| [apple-machine](apple-machine.md) (`applemachine`) | built-in; `delegated-run` · local-vm | No SSH; `provider-owned` · direct only; features: `run-session` | `linux`; Apple container machine | `local`; GPU: no | Apple runtime; machine delete | Local delegated Linux machine execution | Delegated execution, not a normal SSH lease |
| [apple-vm](apple-vm.md) (`applevm`, `apple-vz`, `applevz`) | built-in; `ssh-lease` · local-vm | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Apple Virtualization.framework VM | `local`; GPU: no | Crabbox; VM delete | Headless Linux ARM64 VM on Apple silicon | Apple silicon macOS only |
| [ascii-box](ascii-box.md) (`ascii`, `asciibox`) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync` | `linux`; ASCII Box managed Linux box | `provider-managed`; GPU: unknown | provider CLI; provider delete | Managed Linux box over SSH | Requires the ASCII Box CLI and account |
| [aws](aws.md) | built-in; `ssh-lease` · brokerable-cloud | Crabbox-managed SSH; `crabbox-sync` · coordinator optional; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop`, `browser`, `code` | `linux`, `windows/normal`, `windows/wsl2`, `macos`; EC2 VM or dedicated Mac host | `cloud`; GPU: optional | Crabbox or coordinator; instance termination | Broad Linux, Windows, WSL2, and macOS cloud coverage | Largest configuration, quota, and cost surface |
| [aws-lambda-microvm](aws-lambda-microvm.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session`, `pause-resume` | `linux`; AWS Lambda Firecracker MicroVM | `cloud`; GPU: no | Crabbox and Lambda MicroVM API; MicroVM termination | Isolated stateful ARM64 command execution | Requires a compatible Crabbox runner image; launch Regions and lifetime are limited |
| [azure](azure.md) | built-in; `ssh-lease` · brokerable-cloud | Crabbox-managed SSH; `crabbox-sync` · coordinator optional; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop`, `browser`, `code`, `tailscale` | `linux`, `windows/normal`, `windows/wsl2`; Azure Virtual Machine | `cloud`; GPU: optional | Crabbox or coordinator; VM and owned resource delete | Linux or Windows workloads in Azure | Shared resource and identity setup is substantial |
| [azure-dynamic-sessions](azure-dynamic-sessions.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `run-session` | `linux`; Azure Container Apps Dynamic Session | `cloud`; GPU: no | Azure session pool; provider session expiry | Short delegated container sessions in Azure | No Crabbox-managed SSH lease |
| [blacksmith-testbox](blacksmith-testbox.md) (`blacksmith`) | built-in; `delegated-run` · ci-proof-runner | No SSH; `provider-owned` · direct only; features: `cache-volume`, `run-proof`, `run-session`, `run-artifacts` | `linux`; Blacksmith Testbox runner | `provider-managed`; GPU: no | Blacksmith; provider session cleanup | CI reproduction with proof and reusable sessions | Execution and artifacts follow the Testbox contract |
| [blaxel](blaxel.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; Blaxel managed Linux sandbox | `provider-managed`; GPU: unknown | Blaxel; owned sandbox delete | Managed delegated Linux sandbox execution | Requires Blaxel API credentials and workspace access |
| [cloudflare](cloudflare.md) (`cf`) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; Cloudflare Container | `cloud`; GPU: no | Cloudflare Worker; container delete | Fast delegated Linux container execution | Requires Worker deployment and container availability |
| [cloudflare-dynamic-workers](cloudflare-dynamic-workers.md) (`cf-dynamic`, `cfdw`) | built-in; `delegated-run` · delegated-sandbox | No SSH; `provider-owned` · direct only; features: `cleanup`, `module-run`, `run-session` | `worker-runtime`; Cloudflare Dynamic Worker | `cloud`; GPU: no | Cloudflare loader Worker; terminal metadata and local claim removal | Hosted Worker module execution | No shell, SSH, or filesystem sync; Dynamic Workers must be enabled |
| [cloudflare-sandbox](cloudflare-sandbox.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; Cloudflare Sandbox bridge | `cloud`; GPU: no | Cloudflare Sandbox bridge; sandbox delete | Cloudflare Sandbox Linux command execution through a bridge | Requires a configured bridge URL; no SSH, browser, Tailscale, URL sessions, mounts, or checkpoints |
| [coder](coder.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Coder workspace | `provider-managed`; GPU: unknown | Coder CLI; workspace stop or delete | Coder-backed Linux workspace over SSH proxy | Requires the coder CLI, login, template access, and workspace quota |
| [codesandbox](codesandbox.md) (`csb`, `code-sandbox`) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `pause-resume`, `run-session` | `linux`; CodeSandbox SDK sandbox | `provider-managed`; GPU: no | CodeSandbox; sandbox delete | Managed CodeSandbox Linux development environments | Requires env-only SDK auth and a local Node bridge |
| [crownest](crownest.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; Crownest Workspace Run sandbox | `cloud`; GPU: no | Crownest; sandbox delete | Hosted agent-ready Workspace Runs with archive sync and durable evidence | No SSH, env forwarding, artifacts, proof, downloads, or custom Dockerfile templates yet |
| [daytona](daytona.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `archive-sync` · coordinator optional; features: `ssh`, `crabbox-sync` | `linux`; Daytona sandbox | `provider-managed`; GPU: unknown | Daytona; sandbox delete | Managed development sandbox with direct toolbox execution or brokered SSH | SSH access is short-lived; direct run uses Daytona toolbox APIs while brokered run uses SSH |
| [digitalocean](digitalocean.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `tailscale` | `linux`; DigitalOcean Droplet | `cloud`; GPU: optional | Crabbox; Droplet and key delete | Simple direct Linux VM | Direct-only; no coordinator scheduling |
| [docker-sandbox](docker-sandbox.md) | built-in; `delegated-run` · local-sandbox | No SSH; `provider-owned` · direct only; features: `run-session`, `mcp-attachments` | `linux`; Docker Sandbox | `local`; GPU: no | Docker sbx CLI; sandbox delete | Local delegated sandbox with reusable session handles | Requires the standalone sbx CLI |
| [e2b](e2b.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `provider-owned` · direct only; features: `url-bridge`, `run-session` | `linux`; E2B Firecracker sandbox | `provider-managed`; GPU: no | E2B; sandbox kill or expiry | Hosted ephemeral code sandbox | URL bridge is provider-specific; no normal SSH lease |
| [exe-dev](exe-dev.md) (`exe`, `exedev`) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync` | `linux`; exe.dev managed VM | `provider-managed`; GPU: unknown | exe.dev; provider lifecycle | Fast managed Linux VM exposed over SSH | Public SSH only; provider CLI owns auth |
| [external](external.md) (`exec-provider`) | built-in; `ssh-lease` · external-provider | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop`, `browser`, `code` | `linux`; Configured executable contract | `byo`; GPU: unknown | external executable; contract-defined | Private or organization-specific provider integration | Safety and semantics depend on the configured executable |
| [fastapi-cloud](fastapi-cloud.md) (`fastapicloud`, `fastapi`) | specialized; `service-control` · service-control | SSH not applicable; `none` · direct only; features: none | `linux`; FastAPI Cloud app | `cloud`; GPU: unknown | FastAPI Cloud; not exposed | Inspecting FastAPI Cloud app deployment readiness | Cannot execute arbitrary Crabbox run commands or stop apps |
| [firecracker](firecracker.md) | built-in; `ssh-lease` · self-hosted-virtualization | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Firecracker microVM | `self-hosted`; GPU: no | Crabbox direct lifecycle; microVM and local artifact cleanup | Self-hosted Linux KVM host with prepared Firecracker kernel, rootfs, and CNI | Requires Linux, /dev/kvm, Firecracker assets, and a working CNI setup on the host |
| [freestyle](freestyle.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `run-session` | `linux`; Freestyle VM | `provider-managed`; GPU: unknown | Freestyle; provider VM cleanup | Hosted delegated Linux VM execution | No Crabbox-managed SSH path |
| [gcp](gcp.md) (`google`, `google-cloud`) | built-in; `ssh-lease` · brokerable-cloud | Crabbox-managed SSH; `crabbox-sync` · coordinator optional; features: `ssh`, `crabbox-sync`, `cleanup`, `tailscale` | `linux`; Google Compute Engine VM | `cloud`; GPU: optional | Crabbox or coordinator; instance and firewall cleanup | Linux compute with broad machine selection | Project, IAM, quota, and firewall setup required |
| [hetzner](hetzner.md) | built-in; `ssh-lease` · brokerable-cloud | Crabbox-managed SSH; `crabbox-sync` · coordinator optional; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop`, `browser`, `code`, `tailscale` | `linux`; Hetzner Cloud server | `cloud`; GPU: no | Crabbox or coordinator; server delete | Cost-effective high-CPU Linux VM | Linux-only and capacity varies by location |
| [hostinger](hostinger.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Hostinger VPS | `cloud`; GPU: no | Hostinger subscription; stop only | Direct Linux VPS with persistent subscription | Purchase needs opt-in and release does not cancel billing |
| [hyperv](hyperv.md) | built-in; `ssh-lease` · local-vm | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `windows/normal`; Microsoft Hyper-V VM | `local`; GPU: no | Crabbox; VM delete | Local native Windows VM | Windows host with Hyper-V required |
| [incus](incus.md) | built-in; `ssh-lease` · self-hosted-virtualization | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Incus container or VM | `self-hosted`; GPU: optional | Crabbox; instance delete | Self-hosted Linux containers or VMs | Requires an accessible Incus environment |
| [islo](islo.md) | built-in; `delegated-run` · delegated-sandbox | Provider-specific SSH; `provider-owned` · direct only; features: `ssh`, `url-bridge`, `run-session`, `tailscale`, `pause-resume`, `run-downloads` | `linux`; Islo sandbox | `provider-managed`; GPU: unknown | Islo; sandbox delete | Hosted delegated execution with keep, pause, and SSH helper | SSH feature is not Crabbox-managed sync/run |
| [kubevirt](kubevirt.md) (`kubernetes-vm`) | built-in; `ssh-lease` · self-hosted-virtualization | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop`, `browser`, `code` | `linux`; KubeVirt VirtualMachine | `self-hosted`; GPU: optional | Crabbox on Kubernetes; VirtualMachine delete | Kubernetes-hosted Linux VM | Needs KubeVirt, virtctl, and an SSH-ready template |
| [lambda](lambda.md) | built-in; `ssh-lease` · gpu-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `tailscale` | `linux`; Lambda Cloud on-demand instance | `cloud`; GPU: yes | Crabbox; instance and key termination | Direct GPU-backed Linux workload over SSH | Direct-only; billing, quota, and capacity are account-owned |
| [linode](linode.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `tailscale` | `linux`; Linode instance | `cloud`; GPU: optional | Crabbox; instance and key delete | Straightforward direct Linux VM | Direct-only; optional firewall must already exist |
| [local-container](local-container.md) (`docker`, `container`, `local-docker`) | built-in; `ssh-lease` · local-runtime | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop`, `browser`, `cache-volume`, `workspace-checkpoint`, `workspace-fork` | `linux`; Docker-compatible container | `local`; GPU: optional | Crabbox; container delete | Fast local Linux test environment | Isolation follows the local container runtime |
| [modal](modal.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `run-session` | `linux`; Modal Sandbox | `provider-managed`; GPU: optional | Modal; sandbox termination | Hosted Python or GPU-oriented delegated workloads | Provider owns execution; no normal SSH lease |
| [morph](morph.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync` | `linux`; Morph Cloud VM | `provider-managed`; GPU: unknown | Morph; pause by default; optional delete | Managed Linux VM over SSH | Release retains the paused instance unless deleteOnRelease is enabled |
| [multipass](multipass.md) (`mp`, `canonical-multipass`) | built-in; `ssh-lease` · local-vm | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `cache-volume` | `linux`; Canonical Multipass VM | `local`; GPU: no | Crabbox; VM delete and purge | Portable local Ubuntu VM | Ubuntu-only first implementation |
| [mxc](mxc.md) (`execution-container`) | built-in; `delegated-run` · local-sandbox | No SSH; `provider-owned` · direct only; features: none | `windows/normal`; Microsoft Execution Container | `local`; GPU: no | Windows runtime; container termination | Local isolated Windows command execution | Windows host and execution-container support required |
| [namespace-devbox](namespace-devbox.md) (`namespace`, `namespace-devboxes`) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Namespace Devbox | `provider-managed`; GPU: unknown | Namespace devbox CLI; stop by default; optional delete | Fast managed development box over SSH | Uses the devbox product, not Namespace Compute instances |
| [namespace-instance](namespace-instance.md) (`namespace-compute`) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Namespace Compute instance | `provider-managed`; GPU: unknown | Namespace nsc CLI; instance delete | Short-lived managed Linux compute over SSH | Requires the nsc CLI and direct provider credentials |
| [nebius](nebius.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Nebius Compute VM | `cloud`; GPU: optional | Nebius CLI; owned VM delete | Direct Linux VM lease with optional GPU selection | Requires Nebius CLI auth, project/subnet setup, quota, and public SSH |
| [nomad](nomad.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup` | `linux`; HashiCorp Nomad allocation | `self-hosted`; GPU: unknown | Nomad job; owned job deregister | Self-hosted delegated Linux execution on an existing Nomad cluster | Requires Nomad HTTP API access, allocation exec privileges, and an env-only token when ACLs are enabled |
| [nvidia-brev](nvidia-brev.md) (`brev`, `nvidia`) | built-in; `ssh-lease` · gpu-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; NVIDIA Brev GPU workspace | `provider-managed`; GPU: yes | NVIDIA Brev CLI; delete by default; optional stop | Managed NVIDIA GPU workspace over SSH | Requires Brev CLI auth, quota, and available GPU capacity |
| [opencomputer](opencomputer.md) (`oc`, `open-computer`) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `run-session` | `linux`; OpenComputer Linux VM | `provider-managed`; GPU: unknown | OpenComputer; VM delete | Hosted delegated Linux VM execution | REST execution contract, not an SSH lease |
| [opensandbox](opensandbox.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; OpenSandbox sandbox | `provider-managed`; GPU: unknown | OpenSandbox; sandbox delete | Hosted delegated sandbox through an open SDK | Requires compatible OpenSandbox control and exec endpoints |
| [ovh](ovh.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `tailscale` | `linux`; OVHcloud Public Cloud instance | `cloud`; GPU: optional | Crabbox; instance, key, and local claim delete | OVHcloud Public Cloud Linux VM | Direct-only; credentials use OVH signed requests and local claims |
| [parallels](parallels.md) | built-in; `ssh-lease` · local-vm | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop`, `browser`, `code`, `workspace-checkpoint`, `workspace-fork`, `workspace-restore`, `provider-snapshot` | `linux`, `macos`, `windows/normal`, `windows/wsl2`; Parallels linked-clone VM | `local`; GPU: no | Crabbox; clone delete | Local macOS, Linux, or Windows VM with snapshots | Requires prepared Parallels source VMs and SSH |
| [phala](phala.md) (`phala-cloud`, `dstack`) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Phala Cloud confidential Intel TDX CVM | `provider-managed`; GPU: no | Phala phala CLI; CVM delete | Short-lived confidential Linux compute over SSH | Requires the phala CLI and its stored auth; verifies Intel TDX attestation by default (needs outbound Intel PCS network; --phala-skip-attestation to opt out) |
| [proxmox](proxmox.md) | built-in; `ssh-lease` · self-hosted-virtualization | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Proxmox VE QEMU clone | `self-hosted`; GPU: optional | Crabbox; VM delete | Self-hosted Linux VM fleet | Needs a prepared template, guest agent, and network |
| [railway](railway.md) (`rail`, `railwayapp`) | specialized; `service-control` · service-control | SSH not applicable; `none` · direct only; features: `url-bridge` | `linux`; Railway service | `cloud`; GPU: unknown | Railway; service stop only | Inspecting or stopping an existing Railway service | Cannot execute arbitrary Crabbox run commands |
| [runpod](runpod.md) (`run-pod`, `runpodio`) | built-in; `ssh-lease` · gpu-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync` | `linux`; RunPod GPU pod | `cloud`; GPU: yes | RunPod; pod release | GPU-backed Linux workload over public SSH | Capacity, GPU pricing, and public SSH vary |
| [scaleway](scaleway.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `tailscale` | `linux`; Scaleway Instance | `cloud`; GPU: optional | Crabbox; instance and managed key delete | Direct Linux VM on Scaleway Instances | Direct-only; security groups must already allow SSH |
| [sealos-devbox](sealos-devbox.md) (`sealos`, `sealos-dev`) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Sealos DevBox CRD | `provider-managed`; GPU: unknown | Sealos DevBox CRD; pause or DevBox delete | Sealos DevBox Linux workspace over SSHGate or NodePort | Requires kubectl, explicit context, DevBox CRD/RBAC, image, and SSHGate or NodePort route configuration |
| [semaphore](semaphore.md) (`sem`) | built-in; `ssh-lease` · ci-proof-runner | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync` | `linux`; Semaphore CI job | `provider-managed`; GPU: optional | Semaphore; job stop | Debugging in the same image and secret plane as CI | Depends on debug SSH metadata from the job |
| [smolvm](smolvm.md) (`smol`, `smolmachines`, `smolfleet`) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `run-session` | `linux`; Smol Machines microVM | `provider-managed`; GPU: no | smolfleet; microVM delete | Lightweight hosted microVM execution | Delegated execution through smolfleet |
| [sprites](sprites.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync` | `linux`; Sprite microVM | `provider-managed`; GPU: no | Sprites; sprite delete | Fast Linux microVM over provider SSH proxy | SSH transport depends on sprite proxy |
| [ssh](ssh.md) (`static`, `static-ssh`) | built-in; `ssh-lease` · byo-ssh | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `desktop`, `browser`, `code` | `linux`, `windows/normal`, `windows/wsl2`, `macos`; Existing SSH host | `byo`; GPU: optional | user; none | Bring-your-own persistent Linux, macOS, or Windows host | Crabbox does not provision or clean up the host |
| [superserve](superserve.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; Superserve hosted sandbox | `provider-managed`; GPU: unknown | Superserve; sandbox delete | Hosted delegated Linux sandbox | Requires both control-plane and data-plane access |
| [tart](tart.md) (`local-tart`, `macos-vm`) | built-in; `ssh-lease` · local-vm | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `desktop` | `macos`; Tart Apple silicon VM | `local`; GPU: no | Crabbox; VM delete | Local macOS VM testing | Apple silicon host and prepared Tart image required |
| [tencentcloud](tencentcloud.md) (`tencent`, `tencent-cvm`, `cvm`) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup`, `tailscale` | `linux`; Tencent Cloud CVM instance | `cloud`; GPU: optional | Crabbox; instance termination | Linux SSH leases on Tencent Cloud CVM | Direct-only; requires CVM image, VPC/subnet/security-group planning, and Tencent Cloud API credentials |
| [tenki](tenki.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync` | `linux`; Tenki sandbox VM | `provider-managed`; GPU: unknown | Tenki; sandbox release | Managed Linux sandbox with SSH proxy | Gateway auth uses Tenki-managed key and certificate files |
| [tensorlake](tensorlake.md) (`tl`, `tensorlake-sbx`) | built-in; `delegated-run` · delegated-sandbox | No SSH; `provider-owned` · direct only; features: `run-session` | `linux`; Tensorlake Firecracker sandbox | `provider-managed`; GPU: unknown | Tensorlake; provider sandbox cleanup | Hosted Firecracker-backed delegated execution | Does not expose raw Firecracker provisioning |
| [upstash-box](upstash-box.md) (`upstash`, `box`, `upstashbox`) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `run-session` | `linux`; Upstash Box sandbox | `provider-managed`; GPU: no | Upstash; sandbox cleanup | Hosted short-lived delegated sandbox | No normal SSH access or coordinator routing |
| [vast](vast.md) (`vast-ai`, `vastai`) | built-in; `ssh-lease` · gpu-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Vast.ai direct GPU instance | `provider-managed`; GPU: yes | Crabbox; destroy by default; optional stop or keep | Direct Linux GPU lease from the Vast.ai offer market | Direct-only and billable; capacity, quota, and offer availability vary |
| [vercel-sandbox](vercel-sandbox.md) | built-in; `delegated-run` · delegated-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync`, `cleanup`, `run-session` | `linux`; Vercel Sandbox microVM | `provider-managed`; GPU: no | Vercel Sandbox; sandbox delete | Hosted delegated Linux microVM execution | Requires SDK bridge support and Vercel Sandbox auth |
| [vultr](vultr.md) | built-in; `ssh-lease` · direct-cloud | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; Vultr instance | `cloud`; GPU: optional | Crabbox; instance and key delete | Direct Linux VM on Vultr | Direct-only; firewall groups and VPCs must already exist |
| [wandb](wandb.md) (`weights-and-biases`) | built-in; `delegated-run` · gpu-cloud | No SSH; `provider-owned` · direct only; features: `run-session` | `linux`; Weights & Biases run sandbox | `provider-managed`; GPU: optional | Weights & Biases; run termination | Delegated ML or GPU run environment | Execution follows the W&B run contract |
| [windows-sandbox](windows-sandbox.md) (`wsb`, `windows-sandbox-provider`) | built-in; `delegated-run` · local-sandbox | No SSH; `archive-sync` · direct only; features: `archive-sync` | `windows/normal`; Windows Sandbox | `local`; GPU: optional | Windows host; sandbox close | Disposable native Windows command execution | Requires Windows Sandbox and local host automation |
| [xcp-ng](xcp-ng.md) | built-in; `ssh-lease` · self-hosted-virtualization | Crabbox-managed SSH; `crabbox-sync` · direct only; features: `ssh`, `crabbox-sync`, `cleanup` | `linux`; XCP-ng VM clone | `self-hosted`; GPU: optional | Crabbox; VM delete | Self-hosted Linux VM pool over XAPI | Normal leases require prepared Linux templates |

<!-- END GENERATED PROVIDER MATRIX -->

## Notes on families and capabilities

- The Azure family ships two backends: the default VM SSH lease
  (`provider: azure`) and the delegated `azure-dynamic-sessions` provider
  (Azure Container Apps dynamic sessions). They share the `azure` family but are
  distinct adapters.
- The Cloudflare family ships three delegated backends: `cloudflare` for
  Cloudflare Containers and Linux commands, `cloudflare-dynamic-workers` for
  Worker-runtime module execution, and `cloudflare-sandbox` for Cloudflare
  Sandbox bridge-backed Linux command execution. They are separate providers
  with separate runner configs and token env vars.
- Tensorlake is Crabbox's Firecracker-backed delegated provider. The separate
  `firecracker` provider is the self-hosted Linux KVM surface with direct
  lifecycle, normal Crabbox SSH sync/run, and local artifact cleanup.
- Docker Sandbox is a delegated-run provider driven by the standalone `sbx`
  CLI. It has no aliases, so `docker`, `container`, and `local-docker` remain
  Local Container aliases.
- OpenSandbox is a delegated-run provider using the OpenSandbox Go SDK for
  lifecycle, file upload, and execd command execution. It has no aliases in v1,
  so `osb` remains reserved.
- Superserve is a delegated-run provider using Superserve's control plane for
  sandbox lifecycle and a sandbox data plane for file upload and command
  execution. It has no aliases in v1.
- Vercel Sandbox is a delegated-run provider using Vercel's Sandbox SDK bridge
  for lifecycle, archive upload, command execution, session handles, and
  deletion. The `sandbox` CLI is used only for login/readiness checks and manual
  debugging because Crabbox does not rely on it as a stable lifecycle JSON
  contract.
- Anthropic Sandbox Runtime is a local one-shot delegated-run provider driven
  by the standalone `srt` CLI. It has no SSH lease, no persistent lifecycle,
  and no remote sync surface.
- ASCII Box is an SSH-lease provider. Crabbox uses the documented `box --json`
  CLI for lifecycle/status/delete, then runs normal sync and commands over SSH.
- XCP-ng is a direct SSH-lease provider for a self-hosted XCP-ng pool on
  dedicated 64-bit x86 server-class hardware. XCP-ng itself can host Linux,
  Windows, and BSD guests, but Crabbox's current `xcp-ng` adapter provisions
  normal leases from Linux templates only. Crabbox talks to XAPI from the CLI,
  uses `VM.copy` plus `VM.provision`, injects cloud-init through a FAT16
  `CIDATA` config drive, optionally moves all VIFs to the configured network,
  and uses guest metrics for IPv4 discovery. See the provider page for the
  separate Windows x86_64/x64 ISO E2E harness, and use the Tart provider on
  Apple hardware for macOS VM workflows.
- `incus` is a direct Linux SSH-lease provider that stores Crabbox ownership and
  expiry metadata in Incus `user.crabbox.*` instance config keys. Real Apple
  Silicon smoke still follows the separate local testbed contract documented on
  the provider page.
- DigitalOcean is a direct-only Linux Droplet provider. It uses
  `DIGITALOCEAN_TOKEN`, per-lease SSH keys, and Crabbox-owned flat tags; it does
  not run through the coordinator.
- Linode is a direct-only Linux instance provider. It uses `LINODE_TOKEN`,
  per-lease SSH keys, metadata user-data, optional attachment to an existing
  firewall, and Crabbox-owned tags; it does not run through the coordinator.
- Vultr is a direct-only Linux instance provider. It uses `VULTR_API_KEY`,
  per-lease SSH keys, cloud-init user data, optional attachment to existing
  firewall groups and VPCs, and Crabbox-owned tags; it does not run through the
  coordinator.
- Hostinger is a direct-only Linux VPS provider. Purchases require explicit
  opt-in; release stops the VPS but does not cancel its subscription.
- Capability flags (`--desktop`, `--browser`, `--code`, VNC) are validated
  against each provider's declared feature set. Among the SSH-lease providers,
  desktop/browser/code surfaces are richest on `aws`, `azure`, `hetzner`,
  `parallels`, `ssh`, and `local-container`; `multipass` exposes local VM SSH
  and sync only in its first implementation, `apple-vm` does the same through a
  local helper and host-local SSH proxy, and most direct sandbox/delegated
  providers expose `ssh` and Crabbox sync only.
- Actions runner hydration requires a normal SSH lease on Linux. Use a
  Linux-capable SSH-lease provider for that path.

```sh
crabbox warmup --provider aws --class beast
crabbox run --provider hetzner -- pnpm test
crabbox run --provider digitalocean --type s-1vcpu-1gb -- pnpm test
crabbox run --provider linode --type g6-standard-1 -- pnpm test
crabbox run --provider vultr --type vc2-1c-1gb -- pnpm test
crabbox doctor --provider hostinger
crabbox run --provider docker -- pnpm test
crabbox run --provider docker-sandbox -- go test ./...
crabbox run --provider apple-vm -- go test ./...
crabbox run --provider multipass -- go test ./...
crabbox run --provider blacksmith-testbox --id tbx_123 -- pnpm test
crabbox run --provider namespace-devbox --id blue-lobster -- pnpm test
```

## Implementation

Provider implementation lives under `internal/providers/<name>`; registration is
in `internal/providers/all/all.go`. Command orchestration and the renderer
surface stay in `internal/cli`.

Related docs:

- [Provider selection](../features/provider-selection.md)
- [Provider backends](../provider-backends.md)
- [Feature overview](../features/providers.md)
- [Source map](../source-map.md)
