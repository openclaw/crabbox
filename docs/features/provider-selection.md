# Provider Selection

Read when:

- choosing where a workload should run;
- comparing a Crabbox provider with an external sandbox or dev-environment tool;
- deciding whether a new provider belongs in Crabbox.

Crabbox should support provider families, not every interesting runtime by name.
Prefer the provider that fits the workflow and its evidence requirements. Add a
new first-class provider only when the runtime exposes a stable lifecycle,
execution, authentication, cleanup, and proof contract that Crabbox can test
without special operator knowledge.

## Fast path

Start with the command-backed recommendation list:

```sh
crabbox providers recommend
crabbox providers recommend ci-proof
crabbox providers recommend linux-vm --limit 8
crabbox providers recommend agent-sandbox --json
crabbox providers recommend forkable-workspace --workspace fork
```

The recommendation command uses the built-in provider spec and checked-in
selection metadata only. It does not contact live providers, inspect quota, or
prove credentials. Treat it as routing advice, then run:

```sh
crabbox doctor --provider <name>
```

before spending real capacity.

## Selection rules

Use these rules before adding a new adapter:

1. If the provider can expose a stable SSH target, prefer an SSH-lease backend.
   That keeps sync, Actions hydration, VNC, code-server, cache, result parsing,
   and downloads in core.
2. If the provider owns filesystem sync and command execution, use a delegated
   run backend and declare only the features the provider actually supports.
3. If the provider only starts, stops, or inspects an existing service, use a
   service-control provider. Do not route arbitrary `crabbox run` there.
4. If the integration depends on local scripts or a private fleet contract,
   start with `external` or `ssh` before adding built-in provider code.
5. If the provider cannot be validated without live credentials, add offline
   unit tests and docs first; keep live proof as an opt-in smoke.

## Workflow map

| Workflow | Prefer | Why |
| --- | --- | --- |
| CI reproduction with durable proof | `blacksmith-testbox`, `semaphore` | They map to CI/proof-runner semantics instead of generic devbox semantics. |
| Run evidence and previews | `blacksmith-testbox`, `islo`, `e2b` | They advertise normalized proof, artifact, download, or preview-url capabilities in `crabbox providers` and `providers recommend run-evidence`. |
| Fast feedback with reusable caches | `local-container`, `apple-container`, `multipass`, `blacksmith-testbox` | They advertise cache-volume, sync, cleanup, or reusable proof/session capabilities in `crabbox providers` and `providers recommend fast-feedback`. |
| Disposable isolated execution | `agent-sandbox`, `anthropic-sandbox-runtime`, `e2b`, `smolvm`, `vercel-sandbox` | They are delegated or local sandbox providers in `crabbox providers` and `providers recommend isolated-execution`. |
| Shared app reachability | `hetzner`, `azure`, `gcp`, `islo`, `e2b` | They advertise tailnet, URL bridge, or SSH tunnel planes in `crabbox providers` and `providers recommend reachability`. |
| Shared team cloud leases | `aws`, `azure`, `gcp`, `hetzner` | They advertise brokerable cloud, cleanup, SSH, and sync capabilities in `crabbox providers` and `providers recommend team-cloud`. |
| Generic Linux command execution | `aws`, `azure`, `gcp`, `hetzner`, `digitalocean`, `linode`, `ssh` | SSH leases keep the normal Crabbox sync/run/debug path. |
| Existing owned machine | `ssh` | No provider lifecycle is needed; Crabbox only syncs and runs. |
| Local disposable Linux | `local-container`, `apple-container`, `apple-vz`, `multipass` | Fast local iteration without cloud credentials. |
| Native desktop/browser/code-server | `aws`, `azure`, `hetzner`, `parallels`, `local-container`, `ssh` | These advertise the interactive lease features. |
| GPU-oriented run | `runpod`, `nvidia-brev`, cloud VM providers with GPU types, `modal`, `wandb` | Pick SSH leases for normal debugging, delegated runs for provider-owned ML execution. |
| Worker/module execution | `cloudflare-dynamic-workers` | It advertises the `worker-runtime` target and `module-run` feature. |
| Versioned workspace reuse | `parallels`, `local-container` | They advertise normalized checkpoint/fork/restore/snapshot-reference capabilities in `crabbox providers` and `providers recommend versioned-workspace`; `forkable-workspace` is an alias for the same workflow. |
| Self-hosted virtualization | `proxmox`, `xcp-ng`, `incus`, `kubevirt`, `external`, `ssh` | Keeps private infrastructure behind explicit provider boundaries. |

## Related external systems

External projects are useful comparison points, but they should not become
first-class providers just because they are adjacent.

| System | Crabbox stance |
| --- | --- |
| [Mitos](https://github.com/mitos-run/mitos) | Observe, do not support as a first-class provider yet. Its forkable microVM/Kubernetes model is interesting, but Crabbox should first harden generic snapshot, fork, workspace, and delegated-run contracts that any future adapter could use. |
| [E2B](../providers/e2b.md) | Already supported as delegated run. Use it for hosted sandbox execution where provider-owned templates, filesystem APIs, and session handles are the contract. |
| [Daytona](../providers/daytona.md) | Already supported as a direct sandbox/devbox provider. Use it when Daytona's sandbox lifecycle is the desired substrate and short-lived SSH is enough. |
| [Modal](../providers/modal.md) | Already supported as delegated run. Use it for provider-owned container execution, especially Python/GPU-shaped jobs. |
| [Morph](../providers/morph.md) | Already supported as an SSH lease. Use it when a managed Linux VM with provider-side state reuse fits better than a pure delegated sandbox. |
| [Kubernetes Agent Sandbox](../providers/agent-sandbox.md) | Already supported as delegated run. Use it for Kubernetes-hosted SandboxClaim workflows. |
| [Coder](https://github.com/coder/coder) | Do not mirror as a first-class provider unless there is a narrow lifecycle contract. For now, connect through `ssh` or an `external` provider when a Coder workspace exposes a stable host contract. |
| [DevPod](https://github.com/loft-sh/devpod) | Do not mirror as a first-class provider. It is already a provider-agnostic dev environment layer; use its resulting SSH/container target through `ssh`, `local-container`, or `external` when needed. |
| [Cloudflare Sandbox SDK](https://developers.cloudflare.com/sandbox/) | Keep separate from the existing Cloudflare providers until the runtime contract maps cleanly to a Crabbox backend. Prefer the current Cloudflare providers for built-in Worker/container flows. |

## Mitos decision

If Crabbox does not support Mitos directly, the user-facing behavior should be:

- no `provider: mitos` option;
- no Mitos-specific flags in core commands;
- no Mitos-specific branching in provider-neutral code;
- a clear note that Mitos is observed but unsupported;
- reusable capability work for snapshot, fork, durable workspace, MCP, preview
  URLs, run proof, and delegated artifacts. `crabbox providers --json` exposes
  normalized workspace and run-evidence capability names so this stays
  provider-neutral.

That preserves optionality. If Mitos later has real demand and the contract is
stable enough, it can arrive as either a delegated-run provider or an SSH-lease
provider without changing the CLI surface again.

## Support threshold

Add a new built-in provider only when the PR can prove:

- the execution model is honestly represented by `ProviderSpec.Kind`;
- declared targets and features match real behavior;
- credentials are read from documented env/config locations and never argv;
- cleanup behavior is explicit for success, failure, and keep/expiry paths;
- offline tests cover parsing, command rejection, status/list rendering, and
  provider errors;
- live smoke is documented and opt-in when credentials are required;
- the provider docs say what works, what does not, and which workflow it is for.

If that bar is too high for the first version, use `ssh` or `external` and
document the manual contract instead of baking an unstable provider into
Crabbox.
