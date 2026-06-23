# providers

`crabbox providers` prints the provider capability matrix that the CLI compiles
in. It is a static report: it reads each registered provider's declared spec and
does not contact any cloud, check credentials, or query quota. Use
[`doctor`](doctor.md) when you need live readiness checks.

```sh
crabbox providers
crabbox providers --json
crabbox providers filters
crabbox providers filters --json
crabbox providers --reachability provider-url --evidence preview-url
crabbox providers --lifecycle cleanup --lifecycle workspace-state
crabbox providers --target linux --workspace checkpoint --workspace fork --json
crabbox providers recommend ci-proof
crabbox providers recommend agent-sandbox --json
crabbox providers recommend run-evidence
crabbox providers recommend run-evidence --reachability provider-url --evidence preview-url
crabbox providers recommend versioned-workspace
```

## Flags

- `--json`: emit the matrix as a JSON array instead of grouped text.
- `--kind <kind>`: keep providers with this driver kind. Repeatable. Current
  values include `ssh-lease`, `delegated-run`, and `service-control`.
- `--category <category>`: keep providers in this checked-in provider category,
  such as `delegated-sandbox`, `direct-cloud`, `brokerable-cloud`,
  `ci-proof-runner`, `gpu-cloud`, `local-vm`, or
  `self-hosted-virtualization`. Repeatable.
- `--target <target>`: keep providers that advertise this target, such as
  `linux`, `macos`, `windows/normal`, `windows/wsl2`, or `worker-runtime`.
  Repeatable.
- `--feature <feature>`: keep providers that advertise this raw feature flag,
  such as `ssh`, `crabbox-sync`, `run-proof`, or `url-bridge`. Repeatable.
- `--runtime <capability>`: keep providers that advertise this normalized
  runtime capability, such as `ssh-host`, `delegated-command`,
  `managed-sandbox`, `local-runtime`, `ci-runner`, `remote-dev`,
  `worker-module`, or `interactive`. Repeatable.
- `--reachability <capability>`: keep providers that advertise this normalized
  access-plane capability, such as `ssh-tunnel`, `tailnet-peer`,
  `tailnet-egress`, or `provider-url`. Repeatable.
- `--workspace <capability>`: keep providers that advertise this normalized
  workspace capability, such as `checkpoint`, `fork`, `restore`, or
  `snapshot-ref`. Repeatable.
- `--evidence <capability>`: keep providers that advertise this normalized
  evidence capability, such as `proof`, `artifacts`, `downloads`,
  `preview-url`, or `session`. Repeatable.
- `--lifecycle <capability>`: keep providers that advertise this normalized
  lifecycle capability, such as `cleanup`, `pause-resume`, `run-session`,
  `workspace-state`, or `coordinator-governed`. Repeatable.

Repeated filters are combined with AND semantics. Comma-separated values are also
accepted, so `--workspace checkpoint,fork` means the same thing as passing
`--workspace checkpoint --workspace fork`. Filter values are checked against the
compiled provider matrix; unknown values fail before printing partial output.
The same filters can be passed to `providers recommend` to rank only matching
providers for a workflow.

Run `crabbox providers filters` to print the exact filter values accepted by
the current binary.

The matrix form takes no positional arguments. Use `providers recommend` for
workflow-oriented ranked selection guidance.

## `providers filters`

`crabbox providers filters` prints allowed filter values from the compiled
provider matrix. It does not contact provider APIs. Use it before composing
matrix or recommendation filters in scripts.

```sh
crabbox providers filters
crabbox providers filters --json
```

Text output groups values by flag:

```text
provider filter values:
  kind: delegated-run,service-control,ssh-lease
  category: brokerable-cloud,byo-ssh,ci-proof-runner,delegated-sandbox,direct-cloud,external-provider,gpu-cloud,local-runtime,local-sandbox,local-vm,self-hosted-virtualization,service-control
  target: linux,macos,windows/normal,windows/wsl2,worker-runtime
  feature: archive-sync,browser,cache-volume,cleanup,code,crabbox-sync,desktop,mcp-attachments,module-run,pause-resume,provider-snapshot,run-artifacts,run-downloads,run-proof,run-session,ssh,tailscale,url-bridge,workspace-checkpoint,workspace-fork,workspace-restore
  runtime: ci-runner,delegated-command,interactive,local-runtime,local-sandbox,managed-sandbox,remote-dev,service-control,ssh-host,worker-module
  reachability: provider-url,ssh-tunnel,tailnet-egress,tailnet-peer
  workspace: checkpoint,fork,restore,snapshot-ref
  evidence: artifacts,downloads,preview-url,proof,session
  lifecycle: cleanup,coordinator-governed,pause-resume,run-session,workspace-state
```

JSON output returns one object with `kind`, `category`, `target`, `feature`,
`runtime`, `reachability`, `workspace`, `evidence`, and `lifecycle` arrays.

## `providers recommend`

`crabbox providers recommend` ranks the compiled provider inventory for a
specific workflow without contacting any provider. It uses each provider's
declared targets and features plus the checked-in provider category metadata.
It is selection guidance, not a readiness check; run `crabbox doctor --provider
<name>` before a live workflow.

```sh
crabbox providers recommend
crabbox providers recommend artifact-download
crabbox providers recommend ci-proof
crabbox providers recommend code-interpreter
crabbox providers recommend cost-control
crabbox providers recommend disposable-execution
crabbox providers recommend fast-feedback --feature cache-volume
crabbox providers recommend failure-diagnostics
crabbox providers recommend fanout-testing --workspace fork
crabbox providers recommend interactive-debug
crabbox providers recommend isolated-execution
crabbox providers recommend linux-vm --limit 8
crabbox providers recommend live-smoke
crabbox providers recommend mcp-sandbox
crabbox providers recommend network-isolation
crabbox providers recommend offline-validation
crabbox providers recommend pause-resume
crabbox providers recommend preview-url
crabbox providers recommend reachability
crabbox providers recommend remote-dev
crabbox providers recommend resource-observability
crabbox providers recommend run-evidence
crabbox providers recommend run-evidence --reachability provider-url --evidence preview-url
crabbox providers recommend run-session
crabbox providers recommend team-cloud
crabbox providers recommend workspace-reuse
crabbox providers recommend versioned-workspace
crabbox providers recommend warm-start
crabbox providers recommend web-app-smoke
crabbox providers recommend forkable-workspace --workspace fork
crabbox providers recommend versioned-workspace --target macos --workspace fork
crabbox providers recommend worker-runtime --json
```

With no use case, the command lists supported use cases and examples.

Supported use cases:

- `agent-sandbox`: delegated sandboxes and managed devboxes for agent code
  execution.
- `artifact-download`: providers that can collect run artifacts or materialize
  downloads from provider-owned execution.
- `byo-ssh`: existing SSH hosts.
- `ci-proof`: CI proof runners and providers that return run proof or
  artifacts.
- `code-interpreter`: delegated or local sandboxes for generated-code and
  script execution with sessions, archive sync, retained outputs, preview URLs,
  MCP attachments, or module execution. Aliases include `python-sandbox`,
  `ai-code-runner`, `generated-code`, and `script-runner`.
- `cost-control`: providers with local execution, coordinator governance,
  cleanup, cache reuse, reusable state, or retained proof to reduce quota and
  hot-capacity waste.
- `desktop`: providers with desktop/browser/code-server capabilities.
- `disposable-execution`: delegated or local sandboxes that advertise cleanup
  for temporary workloads, with optional archive sync, sessions, retained
  outputs, preview URLs, or pause/resume before release. Aliases include
  `ephemeral-sandbox`, `throwaway-sandbox`, and `auto-cleanup`.
- `fast-feedback`: providers suited to repeated test loops with reusable cache
  volumes, checkout sync, cleanup, or reusable validation evidence.
- `failure-diagnostics`: providers with proof, sessions, artifacts, downloads,
  preview URLs, or SSH/sync support useful for debugging failed runs. Aliases
  include `failed-run`, `failure-triage`, and `run-debugging`.
- `fanout-testing`: providers that can fork a prepared workspace for parallel
  branch, best-of-N, or snapshot-fanout experiments. Aliases include
  `best-of-n`, `parallel-testing`, and `snapshot-fanout`. This is provider
  selection guidance over existing fork/checkpoint capabilities, not a
  Mitos-style live microVM swarm API.
- `gpu`: GPU-oriented execution providers.
- `interactive-debug`: providers with live inspection surfaces such as
  synced SSH, browser/code/desktop access, reusable sessions, provider URLs, or
  retained evidence after debugging. Aliases include `live-debug`,
  `debug-session`, `ssh-debug`, and `browser-debug`.
- `isolated-execution`: delegated and local sandbox providers for disposable or
  untrusted command execution. This is routing guidance, not a security
  certification for a specific provider.
- `linux-vm`: general Linux VM or SSH-lease execution.
- `live-smoke`: providers with enough lifecycle, sync, cleanup, or evidence
  signals to be good candidates for opt-in live smoke validation. Local
  runtimes are ranked high so operators without cloud credentials still get a
  useful smoke path.
- `local`: local containers, VMs, or local sandboxes.
- `macos`: macOS targets.
- `mcp-sandbox`: sandboxes that can attach MCP server references when creating
  the run environment.
- `network-isolation`: delegated and local sandboxes for contained untrusted
  execution when network exposure should stay narrow. This is routing guidance,
  not a security certification for a specific provider.
- `offline-validation`: local, BYO SSH, or external-provider paths for
  validation when cloud/provider credentials are unavailable. Aliases include
  `no-credentials`, `credentialless`, and `local-first`. This is selection
  guidance; local providers may still need Docker, a VM runtime, or other local
  engine software installed.
- `pause-resume`: providers that can pause and resume provider-owned runtime or
  workspace state.
- `preview-url`: providers that can expose provider-native preview URLs for app
  or service smoke workflows.
- `reachability`: providers with a bidirectional tailnet plane, provider-native
  HTTPS endpoints, outbound-only tailnet egress, or operator-side SSH tunnels.
- `remote-dev`: managed developer environments and SSH-capable remote
  workspaces for local-editor, remote-compute workflows.
- `resource-observability`: providers with coordinator-backed usage/cost
  visibility, SSH resource telemetry, run proof, retained outputs, sessions, or
  preview URLs for later inspection. Aliases include `telemetry`,
  `usage-observability`, `metering`, and `cost-visibility`.
- `run-evidence`: providers that can return run proof, collect artifacts,
  materialize downloads, or expose preview URLs.
- `run-session`: providers that return reusable run/session handles for later
  inspection, logs, previews, artifacts, or downloads.
- `self-hosted`: private virtualization, external providers, and BYO SSH.
- `team-cloud`: brokerable or direct cloud providers for shared team workflows,
  coordinator-mediated spend/cleanup, and normal SSH debugging.
- `versioned-workspace`: providers with native checkpoint, fork, restore, or
  snapshot-reference capabilities. Aliases include `workspace-reuse`,
  `forkable-workspace`, `durable-workspace`, and `stateful-workspace`.
- `warm-start`: providers with local runtimes, reusable cache volumes, retained
  sessions, pause/resume, or workspace-state features that can reduce repeated
  setup overhead. Aliases include `warm-pool`, `prewarm`, and
  `low-latency-start`. This is provider selection guidance over existing
  reuse signals, not a guarantee of native warm-pool APIs.
- `web-app-smoke`: providers that can expose or reach app/service smoke
  targets through provider-native URLs, SSH tunnels, tailnet planes,
  browser/code/desktop access, sessions, or retained outputs. Aliases include
  `web-smoke`, `app-smoke`, `service-smoke`, and `browser-smoke`.
- `windows`: native Windows and WSL2 targets.
- `worker-runtime`: Worker/module-runtime execution.

Recommendation flags:

- `--use-case <name>`: pass the use case by flag instead of positionally.
- `--limit <n>`: maximum recommendations to print. Defaults to `5`.
- `--json`: emit recommendations as JSON.
- `--kind`, `--category`, `--target`, `--feature`, `--runtime`,
  `--reachability`, `--workspace`, `--evidence`, `--lifecycle`: filter the
  candidate provider matrix before scoring. These flags use the same values and
  repeat/comma semantics as the base `providers` matrix command.

## Output

Text output lists every provider as a block of indented fields:

```text
aws
  family: aws
  kind: ssh-lease
  category: brokerable-cloud
  targets: linux,windows/normal,windows/wsl2,macos
  features: ssh,crabbox-sync,cleanup,desktop,browser,code
  runtime: ssh-host,interactive
  reachability: ssh-tunnel
  coordinator: supported

parallels
  family: parallels
  kind: ssh-lease
  category: local-vm
  targets: linux,macos,windows/normal,windows/wsl2
  features: ssh,crabbox-sync,cleanup,desktop,browser,code,workspace-checkpoint,workspace-fork,workspace-restore,provider-snapshot
  runtime: ssh-host,local-runtime,interactive
  reachability: ssh-tunnel
  workspace: checkpoint,fork,restore,snapshot-ref
  lifecycle: cleanup,workspace-state
  coordinator: never

blacksmith-testbox
  family: blacksmith
  kind: delegated-run
  category: ci-proof-runner
  targets: linux
  features: cache-volume,run-proof,run-session,run-artifacts
  runtime: delegated-command,ci-runner
  evidence: proof,artifacts,session
  lifecycle: run-session
  coordinator: never
  aliases: blacksmith

e2b
  family: e2b
  kind: delegated-run
  category: delegated-sandbox
  targets: linux
  features: url-bridge,run-session
  runtime: delegated-command,managed-sandbox
  reachability: provider-url
  evidence: preview-url,session
  lifecycle: run-session
  coordinator: never

wandb
  family: wandb
  kind: delegated-run
  category: gpu-cloud
  targets: linux
  features: -
  runtime: delegated-command
  coordinator: never
  aliases: weights-and-biases

module-runtime-example
  family: module-runtime-example
  kind: delegated-run
  category: -
  targets: worker-runtime
  features: module-run
  runtime: delegated-command,worker-module
  coordinator: never

hostinger
  family: hostinger
  kind: ssh-lease
  category: direct-cloud
  targets: linux
  features: ssh,crabbox-sync,cleanup
  runtime: ssh-host
  reachability: ssh-tunnel
  lifecycle: cleanup
  coordinator: never
```

The `aliases` line appears only when the provider declares alternate names. A
dash (`-`) means the field has no entries (for example, a provider that
advertises no features).

Direct self-hosted SSH-lease providers such as `proxmox` and `xcp-ng` report
`coordinator: never`, `targets: linux`, and features including `ssh`,
`crabbox-sync`, and `cleanup`.

`--json` returns one object per provider:

```json
[
  {
    "provider": "hostinger",
    "family": "hostinger",
    "kind": "ssh-lease",
    "category": "direct-cloud",
    "targets": ["linux"],
    "features": ["ssh", "crabbox-sync", "cleanup"],
    "runtime": ["ssh-host"],
    "reachability": ["ssh-tunnel"],
    "lifecycle": ["cleanup"],
    "coordinator": "never"
  },
  {
    "provider": "blacksmith-testbox",
    "family": "blacksmith",
    "kind": "delegated-run",
    "category": "ci-proof-runner",
    "aliases": ["blacksmith"],
    "targets": ["linux"],
    "features": ["cache-volume", "run-proof", "run-session", "run-artifacts"],
    "runtime": ["delegated-command", "ci-runner"],
    "evidence": ["proof", "artifacts", "session"],
    "lifecycle": ["run-session"],
    "coordinator": "never"
  }
]
```

## Fields

- `provider`: canonical provider name (the value you pass to `--provider`).
- `family`: provider family that the adapter belongs to. Several adapters can
  share one family (for example, `azure` and `azure-dynamic-sessions` are both
  in the `azure` family). Present in both text and JSON output.
- `aliases`: accepted alternate names for the provider, when any are declared.
  Omitted from JSON when empty.
- `kind`: how Crabbox drives the provider.
  - `ssh-lease`: Crabbox provisions or connects to an SSH-reachable box and
    runs the full lease lifecycle (sync, run, release).
  - `delegated-run`: a sandbox or proof runner that owns sync and execution
    itself; there is no SSH lease.
  - `service-control`: Crabbox can inspect or stop a provider-owned service but
    cannot execute arbitrary run commands there.
- `category`: checked-in provider selection category used by recommendations and
  benchmark grouping. Examples include `brokerable-cloud`, `direct-cloud`,
  `delegated-sandbox`, `ci-proof-runner`, `gpu-cloud`, `local-runtime`,
  `local-vm`, `local-sandbox`, `self-hosted-virtualization`, `byo-ssh`,
  `external-provider`, and `service-control`. Omitted from JSON when no category
  is known; text output prints `-`.
- `targets`: supported OS, Windows mode, or runtime category combinations, such
  as `linux`, `macos`, `windows/normal`, `windows/wsl2`, and
  `worker-runtime`. `worker-runtime` means a hosted module or Worker-isolate
  runtime, not a Linux shell or SSH-reachable machine.
- `features`: advertised capability flags. Possible values include `ssh`,
  `crabbox-sync`, `archive-sync`, `cleanup`, `desktop`, `browser`, `code`,
  `tailscale`, `url-bridge`, `workspace-checkpoint`, `workspace-fork`,
  `workspace-restore`, `provider-snapshot`, `run-proof`, `run-session`,
  `mcp-attachments`, and `module-run`. `module-run` means delegated
  `crabbox run --script` or `--script-stdin` source-module execution; it does
  not imply POSIX command argv, archive sync, ports, or SSH access.
- `runtime`: normalized execution-shape capabilities derived from kind,
  category, targets, and features. Possible values are `ssh-host`,
  `delegated-command`, `managed-sandbox`, `local-runtime`, `local-sandbox`,
  `ci-runner`, `remote-dev`, `worker-module`, `interactive`, and
  `service-control`. These are routing hints, not isolation certifications.
- `reachability`: normalized access-plane capabilities derived from provider
  transport features. Possible values are `ssh-tunnel`, `tailnet-peer`,
  `tailnet-egress`, and `provider-url`. These say how an operator or workflow
  can reach a lease or provider-owned endpoint; they are not exposure or
  isolation guarantees.
- `workspace`: normalized versioned-workspace capabilities derived from feature
  flags. Possible values are `checkpoint`, `fork`, `restore`, and
  `snapshot-ref`. The field is omitted when a provider does not advertise any
  native workspace capabilities.
- `evidence`: normalized run-evidence and preview capabilities derived from
  feature flags. Possible values are `proof`, `artifacts`, `downloads`,
  `preview-url`, and `session`. The field is omitted when a provider does not
  advertise any run evidence or preview capabilities.
- `lifecycle`: normalized lifecycle controls derived from provider metadata and
  feature flags. Possible values are `cleanup`, `pause-resume`, `run-session`,
  `workspace-state`, and `coordinator-governed`. The field is omitted when a
  provider does not advertise any lifecycle controls.
- `coordinator`: whether the provider can route leases through the broker.
  - `supported`: the provider can be brokered through the coordinator when a
    broker URL is configured; otherwise it runs direct from the CLI.
  - `never`: the provider always runs direct from the CLI.

Recommendation JSON returns ranked objects:

```json
[
  {
    "provider": "blacksmith-testbox",
    "kind": "delegated-run",
    "category": "ci-proof-runner",
    "targets": ["linux"],
    "features": ["cache-volume", "run-proof", "run-session", "run-artifacts"],
    "runtime": ["delegated-command", "ci-runner"],
    "evidence": ["proof", "artifacts", "session"],
    "lifecycle": ["run-session"],
    "score": 158,
    "reasons": [
      "CI proof runner",
      "returns provider run proof",
      "can reuse provider run sessions",
      "can collect provider run artifacts or downloads"
    ]
  }
]
```

Scores are relative within one use case. They are intentionally heuristic and
stable enough for selection help, not a benchmark or live availability signal.

## Related docs

- [doctor](doctor.md) — local and broker/provider readiness checks.
- [run](run.md) — sync a checkout and run a command on a lease.
- [Provider decision matrix](../providers/README.md#provider-decision-matrix) —
  richer provider selection guidance, including substrate, access, GPU,
  lifecycle, cleanup, best fit, and caveats.
- [Provider selection](../features/provider-selection.md) — workflow selection
  rules and adjacent-system guidance.
- [Provider reference](../providers/README.md) — per-provider setup and config.
