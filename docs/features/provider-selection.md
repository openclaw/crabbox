# Provider Selection

Read when:

- choosing where a workload should run;
- comparing a Crabbox provider with an external sandbox or dev-environment tool;
- deciding whether a new provider belongs in Crabbox.

For a deeper competitor and roadmap view, read
[Provider landscape](provider-landscape.md).

Crabbox should support provider families, not every interesting runtime by name.
Prefer the provider that fits the workflow and its evidence requirements. Add a
new first-class provider only when the runtime exposes a stable lifecycle,
execution, authentication, cleanup, and proof contract that Crabbox can test
without special operator knowledge.

## Fast path

Start with the command-backed recommendation list:

```sh
crabbox providers recommend
crabbox providers recommend artifact-download
crabbox providers recommend ci-proof
crabbox providers recommend cost-control
crabbox providers recommend linux-vm --limit 8
crabbox providers recommend agent-sandbox --json
crabbox providers recommend live-smoke
crabbox providers recommend mcp-sandbox
crabbox providers recommend network-isolation
crabbox providers recommend pause-resume
crabbox providers recommend preview-url
crabbox providers recommend remote-dev
crabbox providers recommend run-session
crabbox providers recommend workspace-reuse
crabbox providers recommend forkable-workspace --workspace fork
```

The recommendation command uses the built-in provider spec and checked-in
selection metadata only. It does not contact live providers, inspect quota, or
prove credentials. Treat it as routing advice, then run:

```sh
crabbox doctor --provider <name>
```

before spending real capacity. For the live proof expected from provider PRs,
see [Provider live smoke](provider-live-smoke.md).

Lifecycle commands require actionable provider intent: pass `--provider
<name>`, set `CRABBOX_PROVIDER`, or configure a provider in user, repository,
or broker config. The compiled provider value is retained only for compatible
default derivation and is never enough to initialize a backend. Existing lease
commands may instead route from an exact local claim, an unambiguous claimed
slug, or recorded run context. Exact claim and recorded routes are authoritative:
only an explicit routing flag may replace them. Archive checkpoint-fork and
shard dry-runs still require provider intent because the archive does not record
a destination provider. With no actionable selection, commands stop with shared
guidance before provider credential validation. Run `crabbox providers recommend`
when choosing a provider.

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
| Run artifacts and downloads | `blacksmith-testbox`, `islo` | They advertise artifact or download evidence and appear in `providers recommend artifact-download`. |
| Run evidence and previews | `blacksmith-testbox`, `islo`, `e2b` | They advertise normalized proof, artifact, download, or preview-url capabilities in `crabbox providers` and `providers recommend run-evidence`. |
| Inspectable run sessions | `local-container`, `blacksmith-testbox`, `islo`, `e2b`, `cloudflare-dynamic-workers` | They advertise reusable session evidence and appear in `providers recommend run-session`. |
| Pausable or resumable runtimes | `codesandbox`, `islo` | They advertise pause/resume capability and appear in `providers recommend pause-resume`. |
| Provider preview URLs | `islo`, `e2b`, `railway` | They advertise provider-native preview URL evidence and appear in `providers recommend preview-url`. |
| Provider live smoke | `blacksmith-testbox`, `apple-container`, `local-container`, `cloudflare`, `aws`, `sealos-devbox` | They advertise enough sync, cleanup, lifecycle, or evidence capability for `providers recommend live-smoke` to rank them as good opt-in smoke candidates, with local runtimes kept visible when cloud credentials are unavailable. |
| Cost- or quota-sensitive validation | `local-container`, `apple-container`, `multipass`, `aws`, `azure` | They advertise local execution, cache reuse, cleanup, or coordinator-governed cloud controls and appear in `providers recommend cost-control`. |
| Fast feedback with reusable caches | `local-container`, `apple-container`, `multipass`, `blacksmith-testbox` | They advertise cache-volume, sync, cleanup, or reusable proof/session capabilities in `crabbox providers` and `providers recommend fast-feedback`. |
| Disposable isolated execution | `agent-sandbox`, `anthropic-sandbox-runtime`, `e2b`, `smolvm`, `vercel-sandbox` | They are delegated or local sandbox providers in `crabbox providers` and `providers recommend isolated-execution`. |
| Network-contained untrusted execution | `cloudflare`, `agent-sandbox`, `codesandbox`, `opensandbox`, `vercel-sandbox` | They keep execution inside delegated or local sandbox boundaries and appear in `providers recommend network-isolation`. |
| MCP-attached sandbox runs | `docker-sandbox` | It advertises `mcp-attachments` in `crabbox providers` and `providers recommend mcp-sandbox`. |
| Remote developer environments | `namespace-devbox`, `daytona`, `morph`, `sealos-devbox`, `codesandbox`, `opencomputer` | They are managed dev-environment substrates with checkout sync, SSH or provider-owned workspace access, and `providers recommend remote-dev` routing. |
| Shared app reachability | `hetzner`, `azure`, `gcp`, `islo`, `e2b` | They advertise tailnet, URL bridge, or SSH tunnel planes in `crabbox providers` and `providers recommend reachability`. |
| Shared team cloud leases | `aws`, `azure`, `gcp`, `hetzner` | They advertise brokerable cloud, cleanup, SSH, and sync capabilities in `crabbox providers` and `providers recommend team-cloud`. |
| Generic Linux command execution | `aws`, `azure`, `gcp`, `hetzner`, `digitalocean`, `linode`, `ssh` | SSH leases keep the normal Crabbox sync/run/debug path. |
| Existing owned machine | `ssh` | No provider lifecycle is needed; Crabbox only syncs and runs. |
| Local disposable Linux | `local-container`, `apple-container`, `apple-vm`, `multipass` | Fast local iteration without cloud credentials. |
| Native desktop/browser/code-server | `aws`, `azure`, `hetzner`, `parallels`, `local-container`, `ssh` | These advertise the interactive lease features. |
| GPU-oriented run | `runpod`, `nvidia-brev`, cloud VM providers with GPU types, `modal`, `wandb` | Pick SSH leases for normal debugging, delegated runs for provider-owned ML execution. |
| Worker/module execution | `cloudflare-dynamic-workers` | It advertises the `worker-runtime` target and `module-run` feature. |
| Versioned workspace reuse | `parallels`, `local-container` | They advertise normalized checkpoint/fork/restore/snapshot-reference capabilities in `crabbox providers` and `providers recommend versioned-workspace`; `workspace-reuse` and `forkable-workspace` are aliases for the same workflow. |
| Self-hosted virtualization | `proxmox`, `xcp-ng`, `incus`, `kubevirt`, `external`, `ssh` | Keeps private infrastructure behind explicit provider boundaries. |

## Related external systems

The [Provider landscape](provider-landscape.md#support-matrix) records support
status and comparisons with adjacent systems. Provider pages document the
implemented contracts. Prefer `ssh` or `external` for an existing host or a
private lifecycle integration that does not need a built-in adapter.

## Mitos decision

Mitos remains unsupported. See the
[landscape decision](provider-landscape.md#mitos-decision) for the rationale and
provider-neutral checkpoint, fork, and evidence capabilities to develop first.
There is no `provider: mitos` or Mitos-specific CLI mode.

## Support threshold

A built-in provider needs an accurate execution kind, target and capability
metadata, off-argv authentication, explicit ownership and cleanup, offline
regression tests, and a documented live smoke. Follow
[Provider authoring](provider-authoring.md) and the
[live smoke contract](provider-live-smoke.md#lifecycle-contract).
