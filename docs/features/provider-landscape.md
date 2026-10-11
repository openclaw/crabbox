# Provider Landscape

Read when:

- comparing Crabbox with adjacent sandbox, dev-environment, or microVM tools;
- deciding whether a competitor should become a first-class provider;
- planning provider capability work without live provider credentials.

Use this architecture map to compare execution contracts. It is not a
benchmark; check the linked provider documentation for current behavior.

## Core Position

Crabbox should own the provider-neutral contract: selecting a target, syncing a
checkout, running commands, returning proof, exposing reachability, and cleaning
up capacity. It should not copy every adjacent product's control plane.

That means:

- support stable execution substrates as providers;
- expose shared workflow capabilities through `crabbox providers` and
  `crabbox providers recommend`;
- keep runtime-shape labels provider-neutral (`managed-sandbox`,
  `delegated-command`, `ssh-host`, `worker-module`) instead of copying one
  vendor's control-plane terms;
- keep reachability labels provider-neutral (`provider-url`, `ssh-tunnel`,
  `tailnet-peer`, `tailnet-egress`) so preview URL, tailnet, and SSH tunnel
  workflows can be compared without provider-specific docs scraping;
- use `ssh` or `external` for tools that already produce a normal host contract;
- defer first-class support for runtimes whose useful behavior is still too
  product-specific to test offline.

## Landscape

| Segment | External systems | Crabbox fit today | Improve next |
| --- | --- | --- | --- |
| CI proof runners | Blacksmith Testbox, Semaphore, GitHub Actions-style runners | Strong fit. Crabbox already separates proof-runner semantics from generic VM semantics through `ci-proof-runner` providers and `providers recommend ci-proof`. | Keep run proof, artifacts, downloads, and status records normalized across providers. |
| Hosted agent sandboxes | E2B, Vercel Sandbox, Modal Sandboxes, Cloudflare Sandbox SDK, OpenSandbox, smolvm, Upstash Box | Good fit when the provider owns process execution and files. These map to `delegated-run` better than SSH leases. | Improve artifact/download parity, preview URL reporting, timeout/error taxonomy, and optional MCP attachment routing. |
| Remote developer environments | Coder, Daytona, Namespace Devbox, CodeSandbox, Morph, OpenComputer, Codespaces-like tools | Good fit when Crabbox can either SSH into the workspace or delegate a command with archive sync. `providers recommend remote-dev` is the routing surface. | Add clearer live smoke docs per provider, surface pause/resume support, and keep local-editor, remote-compute flows distinct from CI proof. |
| Forkable/versioned workspaces | Mitos, Firecracker snapshot systems, local-container, Parallels | Crabbox has provider-neutral checkpoint/fork/restore capabilities and checkpoint fan-out via `checkpoint fork --count`. The catalog advertises workspace fork on Daytona, Hetzner, Incus, local-container, Machine0, and Parallels; these are not uniform live-memory forks. | Harden `versioned-workspace` behavior before adding any runtime-specific fork API. Do not add Mitos-only flags. |
| Worker and module runtimes | Cloudflare Dynamic Workers, Cloudflare Sandbox SDK, Vercel/edge-adjacent runtimes | Narrow fit. `cloudflare-dynamic-workers` is a module-run provider; generic container sandboxes need a separate lifecycle and file/process contract. | Keep worker-runtime separate from Linux sandbox execution unless the provider can expose files, process status, logs, preview URLs, and cleanup. |
| Self-hosted virtualization | Proxmox, XCP-ng, Incus, KubeVirt, local VMs | Strong fit when Crabbox gets a normal SSH lease and lifecycle hooks. | Keep provider-specific reconciliation behind adapters; no provider-specific branching in core. |
| GPU and ML execution | RunPod, NVIDIA Brev, W&B, Modal, cloud GPU VMs | Mixed fit. SSH leases are best for debugging; delegated providers are best for provider-owned jobs. | Normalize result evidence and cost/usage metadata before chasing more GPU-specific launch paths. |

## Mitos Decision

Do not add first-class Mitos support yet.

Mitos targets forkable Firecracker microVMs, MCP, and copy-on-write fan-out.
A Crabbox adapter would need to keep its Kubernetes/KVM requirements behind the
provider boundary and prove the lifecycle on a real cluster.

Develop the shared contracts first: `workspace-checkpoint`, `workspace-fork`,
`workspace-restore`, `provider-snapshot`, `mcp-attachments`, and portable run
evidence (`run-proof`, `run-artifacts`, `run-downloads`, `url-bridge`). The
[runtime, lifecycle, and reachability filters](../commands/providers.md#providers-filters)
already describe execution and access without vendor-specific CLI modes.
`fanout-testing` (alias `best-of-n`) selects fork-capable providers;
`checkpoint fork --count` fans out archive or native checkpoints without
promising live-memory cloning.

Revisit Mitos when a concrete workload needs live microVM forking and the
adapter has offline coverage plus an opt-in live smoke.

## Support Matrix

| System | Support stance | Why |
| --- | --- | --- |
| Mitos | Observe, do not support directly yet. | Its main differentiator is live microVM forking. Crabbox needs generic fork/checkpoint semantics hardened before a Mitos adapter would be clean. |
| E2B | Supported as `e2b`. | Delegated sandbox execution maps cleanly to provider-owned sessions and URL bridge behavior. |
| Vercel Sandbox | Supported as `vercel-sandbox`. | Ephemeral Linux sandbox execution fits `delegated-run`; keep adding evidence and artifact parity before more surface area. |
| Modal | Supported as `modal`. | Provider-owned container execution fits delegated runs, especially Python and ML-shaped workloads. |
| Cloudflare Sandbox SDK | Supported through [`cloudflare-sandbox`](../providers/cloudflare-sandbox.md). | Delegated Linux commands and archive sync use an operator-deployed Sandbox bridge; separate from Cloudflare Containers and Dynamic Workers. |
| Cloudflare Dynamic Workers | Supported as `cloudflare-dynamic-workers`. | Worker/module execution is a separate target from Linux sandbox execution. |
| Daytona | Supported as [`daytona`](../providers/daytona.md). | Direct ordinary runs use SDK/toolbox APIs; script runs and brokered execution use SSH. |
| Namespace Devbox | Supported as `namespace-devbox`. | SSH lease plus sync/cleanup behavior fits remote-dev routing. |
| CodeSandbox | Supported as `codesandbox`. | Delegated workspace execution and pause/resume fit remote-dev routing. |
| Morph | Supported as `morph`. | Managed SSH lease fits local-editor, remote-compute workflows. |
| OpenComputer | Supported as `opencomputer`. | Delegated Linux execution fits remote-dev and sandbox routing, but evidence parity should improve. |
| DevPod | Do not support directly. | It is already a provider-agnostic dev environment layer. Use the resulting SSH/container target through `ssh`, `local-container`, or `external`. |
| Coder | Supported as `coder`. | The built-in contract is intentionally narrow: local Coder CLI auth, Linux SSH proxy execution, stop-by-default release, delete only by opt-in, and cleanup only for locally claimed workspaces. Use `ssh` or `external` for existing workspaces that Crabbox should not manage. |
| Kubernetes Agent Sandbox | Supported as `agent-sandbox`. | SandboxClaim-style delegated execution belongs behind the existing Kubernetes-native adapter. |
| OpenSandbox | Supported as [`opensandbox`](../providers/opensandbox.md). | Delegated execution through compatible OpenSandbox control and exec endpoints, with archive sync and owned sandbox cleanup. |
| Microsandbox, Moru-like runtimes | Candidate only. | Add only when one has a stable lifecycle and evidence contract that is meaningfully different from existing delegated sandboxes. |

## Roadmap

1. Keep recommendations centered on workflows such as `ci-proof`,
   `run-evidence`, `fast-feedback`, `remote-dev`, and `team-cloud`. Use the
   [command reference](../commands/providers.md#providers-recommend) for the full
   list, aliases, filters, and scoring.
2. Improve delegated-provider evidence parity: proof, artifacts, downloads,
   preview URLs, retained logs, reusable sessions, and error status. Access and
   evidence metadata should describe what users can inspect after a run.
3. Document an opt-in [live smoke](provider-live-smoke.md) for each provider,
   including prerequisites and verified cleanup. Offline recommendations do not
   establish working credentials, capacity, or a security boundary.
4. Strengthen checkpoint, fork, restore, and snapshot semantics before adding
   runtime-specific forking. `workspace-reuse` and `fanout-testing` expose these
   workflows without assuming live-memory cloning.
5. Keep `worker-runtime` module execution distinct from Linux shells and
   filesystem sandboxes in both targets and feature declarations.

## Add-Provider Bar

Follow [Provider authoring](provider-authoring.md) for backend selection,
capability declarations, credential handling, tests, and documentation. A new
adapter must prove create, inspect, run or attach, release, and cleanup behavior
under the [live smoke contract](provider-live-smoke.md#lifecycle-contract).
Use `ssh` or `external` for a manual integration until that contract is ready.
