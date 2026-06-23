# Provider Landscape

Read when:

- comparing Crabbox with adjacent sandbox, dev-environment, or microVM tools;
- deciding whether a competitor should become a first-class provider;
- planning provider capability work without live provider credentials.

This is a product and architecture map, not a benchmark. External products move
quickly, so use this page to decide the next Crabbox seam to harden, then verify
current behavior against the provider docs before adding code.

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
| Remote developer environments | Daytona, Namespace Devbox, CodeSandbox, Morph, OpenComputer, Codespaces-like tools | Good fit when Crabbox can either SSH into the workspace or delegate a command with archive sync. `providers recommend remote-dev` is the routing surface. | Add clearer live smoke docs per provider, surface pause/resume support, and keep local-editor, remote-compute flows distinct from CI proof. |
| Forkable/versioned workspaces | Mitos, Firecracker snapshot systems, local-container, Parallels | Partial fit. Crabbox already has provider-neutral checkpoint/fork/restore capability names, but only local providers advertise them today. | Harden `versioned-workspace` behavior before adding any runtime-specific fork API. Do not add Mitos-only flags. |
| Worker and module runtimes | Cloudflare Dynamic Workers, Cloudflare Sandbox SDK, Vercel/edge-adjacent runtimes | Narrow fit. `cloudflare-dynamic-workers` is a module-run provider; generic container sandboxes need a separate lifecycle and file/process contract. | Keep worker-runtime separate from Linux sandbox execution unless the provider can expose files, process status, logs, preview URLs, and cleanup. |
| Self-hosted virtualization | Proxmox, XCP-ng, Incus, KubeVirt, local VMs | Strong fit when Crabbox gets a normal SSH lease and lifecycle hooks. | Keep provider-specific reconciliation behind adapters; no provider-specific branching in core. |
| GPU and ML execution | RunPod, NVIDIA Brev, W&B, Modal, cloud GPU VMs | Mixed fit. SSH leases are best for debugging; delegated providers are best for provider-owned jobs. | Normalize result evidence and cost/usage metadata before chasing more GPU-specific launch paths. |

## Mitos Decision

Do not add first-class Mitos support yet.

Mitos is strategically interesting because it targets forkable Firecracker
microVM swarms, MCP, and copy-on-write fan-out from a warm running machine. That
is a different contract from most Crabbox providers. If Crabbox adds `mitos`
today, the likely failure mode is ugly: Mitos-specific flags, Kubernetes and KVM
assumptions leaking into core, and tests that cannot prove the useful behavior
without a live cluster.

The better path is to make the generic seams real first:

- `workspace-checkpoint`, `workspace-fork`, `workspace-restore`, and
  `provider-snapshot` keep forkable workspace language provider-neutral.
- `runtime` filter values like `managed-sandbox`, `delegated-command`, and
  `worker-module` describe the execution shape without implying Mitos-style
  live microVM forking.
- `mcp-attachments` stays a capability, not a Mitos-only command mode.
- `run-proof`, `run-artifacts`, `run-downloads`, and `url-bridge` keep evidence
  portable across delegated sandboxes and proof runners.
- `reachability` filter values describe access planes without claiming a
  provider-specific network isolation model.
- `remote-dev`, `mcp-sandbox`, `isolated-execution`, and
  `versioned-workspace` keep recommendations workflow-oriented.
- `fanout-testing` and its `best-of-n` alias route operators to existing
  fork-capable providers without adding a Mitos-only live-fork API.

Revisit Mitos only when there is a concrete use case that needs live microVM
forking and the provider contract can be tested with offline unit coverage plus
an opt-in live smoke. Until then, document it as an observed adjacent system.

## Support Matrix

| System | Support stance | Why |
| --- | --- | --- |
| Mitos | Observe, do not support directly yet. | Its main differentiator is live microVM forking. Crabbox needs generic fork/checkpoint semantics hardened before a Mitos adapter would be clean. |
| E2B | Supported as `e2b`. | Delegated sandbox execution maps cleanly to provider-owned sessions and URL bridge behavior. |
| Vercel Sandbox | Supported as `vercel-sandbox`. | Ephemeral Linux sandbox execution fits `delegated-run`; keep adding evidence and artifact parity before more surface area. |
| Modal | Supported as `modal`. | Provider-owned container execution fits delegated runs, especially Python and ML-shaped workloads. |
| Cloudflare Sandbox SDK | Candidate, not the same as current Cloudflare providers. | The SDK can be a good delegated-run backend if the lifecycle, file, process, preview, and cleanup contract is stable enough. |
| Cloudflare Dynamic Workers | Supported as `cloudflare-dynamic-workers`. | Worker/module execution is a separate target from Linux sandbox execution. |
| Daytona | Supported as `daytona`. | Managed dev environment with SSH-shaped execution fits remote-dev routing. |
| Namespace Devbox | Supported as `namespace-devbox`. | SSH lease plus sync/cleanup behavior fits remote-dev routing. |
| CodeSandbox | Supported as `codesandbox`. | Delegated workspace execution and pause/resume fit remote-dev routing. |
| Morph | Supported as `morph`. | Managed SSH lease fits local-editor, remote-compute workflows. |
| OpenComputer | Supported as `opencomputer`. | Delegated Linux execution fits remote-dev and sandbox routing, but evidence parity should improve. |
| DevPod | Do not support directly. | It is already a provider-agnostic dev environment layer. Use the resulting SSH/container target through `ssh`, `local-container`, or `external`. |
| Coder | Do not support directly by default. | A Coder workspace should enter Crabbox as a stable host or external provider unless there is a narrow lifecycle contract to own. |
| Kubernetes Agent Sandbox | Supported as `agent-sandbox`. | SandboxClaim-style delegated execution belongs behind the existing Kubernetes-native adapter. |
| OpenSandbox, Microsandbox, Moru-like runtimes | Candidate only. | Add only when one has a stable lifecycle and evidence contract that is meaningfully different from existing delegated sandboxes. |

## Roadmap

Ship these in small PRs:

1. Keep recommendation surfaces workflow-first.
   `ci-proof`, `run-evidence`, `fast-feedback`, `isolated-execution`,
   `mcp-sandbox`, `remote-dev`, `team-cloud`, and `versioned-workspace` should
   stay the public routing vocabulary.
2. Improve evidence parity for delegated sandboxes.
   The biggest practical gap versus hosted sandbox products is not launch; it is
   consistent proof, artifacts, downloads, preview URLs, logs, and error status.
   Use `crabbox providers recommend run-session` when the workflow needs a
   reusable provider session handle for later inspection.
   Use `crabbox providers recommend artifact-download` when a workflow needs
   retained files or downloadable results from provider-owned execution.
   Use `crabbox providers recommend preview-url` when the workflow specifically
   needs provider-native app or service URLs.
   Use `crabbox providers --reachability provider-url` or
   `crabbox providers recommend reachability --reachability tailnet-peer` when
   the operator needs an explicit access plane.
   Use `crabbox providers recommend network-isolation` when the workflow runs
   untrusted code and network exposure should stay inside a delegated or local
   sandbox boundary.
3. Add live smoke docs where credentials are required.
   Provider adapters can be valuable before broad live access exists, but each
   one needs an opt-in smoke contract that says what real behavior proves. Use
   `crabbox providers recommend live-smoke` to pick candidates from offline
   lifecycle, sync, cleanup, and evidence metadata before spending capacity.
   Use `crabbox providers recommend cost-control` when a workflow should prefer
   local runtimes, coordinator governance, cleanup, cache reuse, or retained
   proof before spending provider quota.
   Use `crabbox providers recommend pause-resume` when long-running sandbox or
   dev-environment state needs to be parked and resumed.
4. Strengthen workspace reuse before runtime-specific forking.
   Checkpoint, fork, restore, and provider snapshot semantics should be testable
   through the CLI before adding live microVM fan-out. Use
   `crabbox providers recommend workspace-reuse` as the workflow entry point
   when the operator cares about reusable state more than a specific fork API.
   Use `crabbox providers recommend fanout-testing` when the workflow needs
   parallel branch or best-of-N experimentation from forkable state.
5. Keep edge/worker execution separate from Linux sandboxes.
   A Worker module runtime is not the same thing as a Linux shell. Make the
   target and feature names say that clearly.

## Add-Provider Bar

Add a first-class provider when the PR can prove:

- stable lifecycle: create, inspect, run or attach, release, cleanup;
- honest `ProviderSpec.Kind`, category, targets, and feature flags;
- documented auth from env/config, never command-line secrets;
- offline tests for parsing, command construction, status/list rendering, and
  provider errors;
- opt-in live smoke when credentials or quota are required;
- docs that explain when to use the provider and when to choose an existing
  workflow instead.

If the bar is not met, use `ssh` or `external` and document the manual contract.
