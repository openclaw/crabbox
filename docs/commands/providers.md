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
crabbox providers --category delegated-sandbox --evidence preview-url
crabbox providers --target linux --workspace checkpoint --workspace fork --json
crabbox providers recommend ci-proof
crabbox providers recommend agent-sandbox --json
crabbox providers recommend run-evidence
crabbox providers recommend run-evidence --category delegated-sandbox --evidence preview-url
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
- `--workspace <capability>`: keep providers that advertise this normalized
  workspace capability, such as `checkpoint`, `fork`, `restore`, or
  `snapshot-ref`. Repeatable.
- `--evidence <capability>`: keep providers that advertise this normalized
  evidence capability, such as `proof`, `artifacts`, `downloads`,
  `preview-url`, or `session`. Repeatable.

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
  feature: archive-sync,browser,cache-volume,cleanup,code,crabbox-sync,desktop,module-run,pause-resume,provider-snapshot,run-artifacts,run-downloads,run-proof,run-session,ssh,tailscale,url-bridge,workspace-checkpoint,workspace-fork,workspace-restore
  workspace: checkpoint,fork,restore,snapshot-ref
  evidence: artifacts,downloads,preview-url,proof,session
```

JSON output returns one object with `kind`, `category`, `target`, `feature`,
`workspace`, and `evidence` arrays.

## `providers recommend`

`crabbox providers recommend` ranks the compiled provider inventory for a
specific workflow without contacting any provider. It uses each provider's
declared targets and features plus the checked-in provider category metadata.
It is selection guidance, not a readiness check; run `crabbox doctor --provider
<name>` before a live workflow.

```sh
crabbox providers recommend
crabbox providers recommend ci-proof
crabbox providers recommend fast-feedback --feature cache-volume
crabbox providers recommend isolated-execution
crabbox providers recommend linux-vm --limit 8
crabbox providers recommend reachability
crabbox providers recommend run-evidence
crabbox providers recommend run-evidence --category delegated-sandbox --evidence preview-url
crabbox providers recommend versioned-workspace
crabbox providers recommend versioned-workspace --target macos --workspace fork
crabbox providers recommend worker-runtime --json
```

With no use case, the command lists supported use cases and examples.

Supported use cases:

- `agent-sandbox`: delegated sandboxes and managed devboxes for agent code
  execution.
- `byo-ssh`: existing SSH hosts.
- `ci-proof`: CI proof runners and providers that return run proof or
  artifacts.
- `desktop`: providers with desktop/browser/code-server capabilities.
- `fast-feedback`: providers suited to repeated test loops with reusable cache
  volumes, checkout sync, cleanup, or reusable validation evidence.
- `gpu`: GPU-oriented execution providers.
- `isolated-execution`: delegated and local sandbox providers for disposable or
  untrusted command execution. This is routing guidance, not a security
  certification for a specific provider.
- `linux-vm`: general Linux VM or SSH-lease execution.
- `local`: local containers, VMs, or local sandboxes.
- `macos`: macOS targets.
- `reachability`: providers with a bidirectional tailnet plane, provider-native
  HTTPS endpoints, outbound-only tailnet egress, or operator-side SSH tunnels.
- `run-evidence`: providers that can return run proof, collect artifacts,
  materialize downloads, or expose preview URLs.
- `self-hosted`: private virtualization, external providers, and BYO SSH.
- `versioned-workspace`: providers with native checkpoint, fork, restore, or
  snapshot-reference capabilities.
- `windows`: native Windows and WSL2 targets.
- `worker-runtime`: Worker/module-runtime execution.

Recommendation flags:

- `--use-case <name>`: pass the use case by flag instead of positionally.
- `--limit <n>`: maximum recommendations to print. Defaults to `5`.
- `--json`: emit recommendations as JSON.
- `--kind`, `--category`, `--target`, `--feature`, `--workspace`,
  `--evidence`: filter the candidate provider matrix before scoring. These flags
  use the same values and repeat/comma semantics as the base `providers` matrix
  command.

## Output

Text output lists every provider as a block of indented fields:

```text
aws
  family: aws
  kind: ssh-lease
  category: brokerable-cloud
  targets: linux,windows/normal,windows/wsl2,macos
  features: ssh,crabbox-sync,cleanup,desktop,browser,code
  coordinator: supported

parallels
  family: parallels
  kind: ssh-lease
  category: local-vm
  targets: linux,macos,windows/normal,windows/wsl2
  features: ssh,crabbox-sync,cleanup,desktop,browser,code,workspace-checkpoint,workspace-fork,workspace-restore,provider-snapshot
  workspace: checkpoint,fork,restore,snapshot-ref
  coordinator: never

blacksmith-testbox
  family: blacksmith
  kind: delegated-run
  category: ci-proof-runner
  targets: linux
  features: cache-volume,run-proof,run-session,run-artifacts
  evidence: proof,artifacts,session
  coordinator: never
  aliases: blacksmith

e2b
  family: e2b
  kind: delegated-run
  category: delegated-sandbox
  targets: linux
  features: url-bridge,run-session
  evidence: preview-url,session
  coordinator: never

wandb
  family: wandb
  kind: delegated-run
  category: gpu-cloud
  targets: linux
  features: -
  coordinator: never
  aliases: weights-and-biases

module-runtime-example
  family: module-runtime-example
  kind: delegated-run
  category: -
  targets: worker-runtime
  features: module-run
  coordinator: never

hostinger
  family: hostinger
  kind: ssh-lease
  category: direct-cloud
  targets: linux
  features: ssh,crabbox-sync,cleanup
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
    "evidence": ["proof", "artifacts", "session"],
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
  `workspace-restore`, `provider-snapshot`, `run-proof`, `run-session`, and
  `module-run`. `module-run` means delegated `crabbox run --script` or
  `--script-stdin` source-module execution; it does not imply POSIX command
  argv, archive sync, ports, or SSH access.
- `workspace`: normalized versioned-workspace capabilities derived from feature
  flags. Possible values are `checkpoint`, `fork`, `restore`, and
  `snapshot-ref`. The field is omitted when a provider does not advertise any
  native workspace capabilities.
- `evidence`: normalized run-evidence and preview capabilities derived from
  feature flags. Possible values are `proof`, `artifacts`, `downloads`,
  `preview-url`, and `session`. The field is omitted when a provider does not
  advertise any run evidence or preview capabilities.
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
    "evidence": ["proof", "artifacts", "session"],
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
