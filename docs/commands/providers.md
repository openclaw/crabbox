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
crabbox providers describe local-container
crabbox providers describe docker --json
crabbox providers --reachability provider-url --evidence preview-url
crabbox providers --lifecycle cleanup --lifecycle workspace-state
crabbox providers --target linux --workspace checkpoint --workspace fork --json
crabbox providers recommend ci-proof
crabbox providers recommend agent-sandbox --json
crabbox providers recommend run-evidence
crabbox providers recommend run-evidence --reachability provider-url --evidence preview-url
crabbox providers recommend versioned-workspace
crabbox providers sizes machine0
crabbox providers sizes machine0 --all --refresh --json
crabbox providers sizes machine0 --with-context --class fast --json
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

## `providers sizes`

`crabbox providers sizes <provider>` reads the selected provider's live machine
catalog. Unlike the static provider matrix, it loads merged configuration and
may invoke an authenticated provider client. Providers without the narrow live
size-catalog capability fail clearly instead of returning a guessed catalog.

```sh
crabbox providers sizes machine0
crabbox providers sizes machine0 --json
crabbox providers sizes machine0 --all --refresh
```

Flags:

- `--json` emits the complete catalog as a JSON array.
- `--all` includes currently unavailable sizes whose `regions` is empty or
  `null`. Without it, those entries are omitted.
- `--refresh` asks the provider to bypass any catalog cache. Providers that
  always fetch live data already satisfy this request without retaining a
  cache.
- `--with-context` requires `--json` and wraps the same catalog in
  `{ "sizes": [...], "selection": { "selector": "type", "effectiveType": "xxxl", "region": "eu" } }`.
  Only providers explicitly advertising `sizeSelection: "type"` support this
  context. Each catalog `name` is an exact native `--type` selector.
- `--class <class>` resolves the existing portable class against the selected
  provider and merged configuration. Explicit native configuration still wins.

`selection` describes the effective selection without a session override, not
a capacity reservation. A configured type may be missing from the catalog or
unavailable; the response preserves that identifier without fabricating a row
or choosing a fallback. `--all` still controls inclusion of sizes unavailable
everywhere; callers can compare each row's `regions` with `selection.region`
for regional availability. Catalog errors fail the command without partial JSON.
Without `--with-context`, the JSON array and human table remain unchanged.

Human output includes size, vCPU, GPU label, RAM GB, disk GB, hourly cost,
and current regions. JSON preserves the provider's exact integer
`pricePerHourMicro` value, where `1_000_000` is one currency unit, rather than
rounding it for display. Entries use this shape:

```json
{
  "name": "gpu-h100-1",
  "vcpu": 20,
  "ramGb": 240,
  "diskGb": 720,
  "gpu": {
    "label": "1x H100",
    "vramGb": 80,
    "scratchDiskGb": 5000
  },
  "regions": ["eu", "us-east"],
  "pricePerHourMicro": 4851000,
  "transferGiBPerMonth": 9313,
  "estimatedSnapshotGb": 200,
  "defaultImage": "gpu-h100x1-base"
}
```

CPU sizes omit `gpu`; GPU sizes retain the provider's label, VRAM, and optional
scratch-disk capacity. Optional `providerMetadata` is present only when the
provider returns forward-compatible catalog fields not yet normalized by
Crabbox.

For Machine0, this live catalog is the authority for native creation choices;
filter by the intended region and pass the exact `name` through
`--machine0-size`. Static class aliases do not guarantee availability, and an
explicitly configured native size takes priority over a generic class. Neither
catalog offers an in-place resize operation for existing Machine0 leases.

## `providers describe`

`crabbox providers describe <provider>` reports the selected runnable
provider's canonical identity, normalized capabilities, and the exact flags
registered by `crabbox run`. An alias is accepted before or after `--json`:

```sh
crabbox providers describe local-container
crabbox providers describe docker --json
crabbox providers describe --json docker
```

Alias input is visible in both formats. Human output starts with the
canonicalization and keeps command-level and provider-owned flags separate:

```text
docker -> local-container
  kind: ssh-lease
  runnable: true
  family: container
  aliases: container,docker,local-docker
  deprecated: false
  replacement: -
  targets: linux
  features: browser,cache-volume,cleanup,crabbox-sync,desktop,run-session,ssh,workspace-checkpoint,workspace-fork
  runtime: interactive,local-runtime,ssh-host
  reachability: ssh-tunnel
  workspace: checkpoint,fork
  evidence: session
  lifecycle: cleanup,run-session,workspace-state
  coordinator: never

Shared run flags:
  --arch
    type: string; value shape: scalar; default: "amd64"; repeatable: false
    deprecated: false; replacement: -; routing: false; creation-only: false
    CPU architecture: amd64 or arm64
  ...

local-container flags:
  --local-container-volume
    type: string; value shape: string-list; default: []; repeatable: true
    deprecated: false; replacement: -; routing: false; creation-only: true
    bind-mount a host path into the container; host:container[:ro]; repeatable
```

Providers with no provider-owned flags still print `Shared run flags` and an
explicit `(none)` in their provider section. Shared flags are command-level
`run` workflow flags; their presence does not bypass ordinary capability,
target, provider-kind, or option-combination validation.

### JSON schema v2

Providers that opt into native size selection also include the optional
top-level `sizeSelection: "type"` field, as in the static matrix. This is a
provider declaration, not a live catalog or effective configuration. Schema
version 2 and the existing flag metadata remain unchanged.

`--json` emits one object. Every array is present, including empty arrays, and
identity aliases, targets, capability arrays, and flag records are sorted for
deterministic output:

```json
{
  "schemaVersion": 2,
  "provider": {
    "requested": "docker",
    "canonical": "local-container",
    "inputAlias": "docker",
    "aliases": ["container", "docker", "local-docker"],
    "deprecated": false,
    "replacement": ""
  },
  "runnable": true,
  "kind": "ssh-lease",
  "family": "container",
  "targets": ["linux"],
  "capabilities": {
    "features": ["browser", "cache-volume", "cleanup", "crabbox-sync", "desktop", "run-session", "ssh", "workspace-checkpoint", "workspace-fork"],
    "runtime": ["interactive", "local-runtime", "ssh-host"],
    "reachability": ["ssh-tunnel"],
    "workspace": ["checkpoint", "fork"],
    "evidence": ["session"],
    "lifecycle": ["cleanup", "run-session", "workspace-state"],
    "coordinator": "never"
  },
  "classCatalog": {"disposition": "unmapped", "profiles": []},
  "sharedFlags": [],
  "providerFlags": []
}
```

The abbreviated empty flag arrays above have this stable record shape when
populated:

```json
{
  "name": "local-container-volume",
  "type": "string",
  "valueShape": "string-list",
  "default": [],
  "repeatable": true,
  "usage": "bind-mount a host path into the container; host:container[:ro]; repeatable",
  "deprecated": false,
  "replacement": "",
  "routing": false,
  "creationOnly": true
}
```

Scalar `type` values are `string`, `bool`, `int`, `int64`, `float64`, or
`duration`; durations use canonical Go duration strings. A repeatable string
list has `type: "string"`, `valueShape: "string-list"`, a JSON string-array
default, and `repeatable: true`.

`creationOnly: true` marks provider-owned flags that select a new resource;
it does not advertise mutation of an existing lease. Machine0 marks its size,
region, image, image version, desktop image, and registered key selectors this
way. `false` means the flag is not annotated as creation-only, not that it can
resize a resource. In particular, shared `class` and `type` metadata are not
resize capabilities. Static flag defaults and class profiles do not describe
effective config or the observed capacity of an existing machine.

Defaults come from the compiled `baseConfig()` passed through the real `run`
flag registration. The command does not load config files or environment
overrides, inspect credentials, configure or apply a provider, create clients,
contact a network, read claims/state/locks, or write files. Consequently it
never serializes live config, credential, or environment values. Provider-level
deprecation is currently always `false` with an empty replacement because the
registry has equivalent aliases but no deprecated-provider contract. Flag
deprecations are explicit per flag and point to their canonical replacement.

Only `ssh-lease` and `delegated-run` providers are runnable. Unknown providers,
`service-control` providers, future unsupported provider kinds, missing names,
and extra names fail with exit 2 and produce no partial JSON.

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
  class standard: c7a.8xlarge (32 vCPU, 64 GB RAM)
  class fast: c7a.16xlarge (64 vCPU, 128 GB RAM)
  class large: c7a.24xlarge (96 vCPU, 192 GB RAM)
  class beast: c7a.48xlarge (192 vCPU, 384 GB RAM)

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

Blaxel reports as a direct delegated-run Linux provider with `archive-sync` and
`cleanup`, and `coordinator: never`.

The `aliases` line appears only when the provider declares alternate names. A
dash (`-`) means the field has no entries (for example, a provider that
advertises no features).

Direct self-hosted SSH-lease providers such as `firecracker`, `proxmox`, and
`xcp-ng` report `coordinator: never`, `targets: linux`, and features including
`ssh`, `crabbox-sync`, and `cleanup`.

`--json` returns one object per provider. The compatibility `classes` summary
is complete; the authoritative AWS `classCatalog` below is abbreviated to one
profile and one fallback to show the richer record shape:

```json
[
  {
    "provider": "aws",
    "family": "aws",
    "kind": "ssh-lease",
    "category": "brokerable-cloud",
    "targets": ["linux", "windows/normal", "windows/wsl2", "macos"],
    "features": ["ssh", "crabbox-sync", "cleanup", "desktop", "browser", "code"],
    "runtime": ["ssh-host", "interactive"],
    "reachability": ["ssh-tunnel"],
    "lifecycle": ["coordinator-governed", "cleanup"],
    "coordinator": "supported",
    "classes": [
      {"class": "standard", "type": "c7a.8xlarge", "vcpu": 32, "memoryGb": 64},
      {"class": "fast", "type": "c7a.16xlarge", "vcpu": 64, "memoryGb": 128},
      {"class": "large", "type": "c7a.24xlarge", "vcpu": 96, "memoryGb": 192},
      {"class": "beast", "type": "c7a.48xlarge", "vcpu": 192, "memoryGb": 384}
    ],
    "classCatalog": {
      "disposition": "mapped",
      "profiles": [
        {
          "class": "standard",
          "target": "linux",
          "architecture": "amd64",
          "primary": {"type": "c7a.8xlarge", "architecture": "amd64", "vcpu": 32, "memory": {"value": 64, "unit": "GiB"}},
          "fallbacks": [
            {"type": "c7i.8xlarge", "architecture": "amd64", "vcpu": 32, "memory": {"value": 64, "unit": "GiB"}}
          ]
        }
      ]
    }
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
    "coordinator": "never",
    "classCatalog": {"disposition": "unmapped", "profiles": []}
  }
]
```

`coder` appears as an SSH-lease provider with Linux target, `ssh`,
`crabbox-sync`, and `cleanup` features, and `coordinator: never`.

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
- `classes`: compatibility summary of the primary machine selected for each
  class on the default Linux/amd64 selector. It is present only for the five
  providers covered by the initial contract: AWS, Azure, GCP, Hetzner, and
  Namespace Instance. The array remains ordered as `standard`, `fast`, `large`,
  and `beast`, with nominal integral `vcpu` and `memoryGb` values when known.
- `classCatalog`: the authoritative provider-owned class catalog. It is always
  present in JSON and is deeply identical in `providers --json` and
  `providers describe --json`, including when the requested description used
  an alias.
  - `disposition` is `mapped` when an explicitly selected canonical class has
    a supported provider-owned machine profile. `unmapped` means no supported
    static class-to-machine profile is exposed; it does not promise uniform
    rejection across CLI, YAML, and environment inputs, or that legacy metadata
    can never contain a class string.
  - `profiles` is always an array. A mapped profile selects one exact canonical
    class, target, Windows mode (Windows only), and architecture. Windows with
    an empty mode normalizes to `normal`; architecture and Windows mode do not
    fall back. An explicit `mixed` profile is the only architecture fallback.
    Selecting an exact lowercase canonical class without an exact or `mixed`
    selector match fails with exit 2; it is never treated as a literal provider
    machine type. Uppercase, padded, and custom class strings retain their
    provider-specific legacy behavior.
  - `primary` is tried first and `fallbacks` follows in declared order.
    `fallbacks` is always an array and is never sorted.
  - Machine `architecture` is `amd64` or `arm64`. Unknown `vcpu` and `memory`
    are JSON `null`, never estimates. Non-null memory includes a numeric `value`
    and explicit provider-native unit: `MB`, `MiB`, `GB`, or `GiB`.

Profile order is deterministic: canonical class order (`standard`, `fast`,
`large`, `beast`), then target, Windows mode, and architecture. The catalog is
compiled static data; discovery does not read config or local state, inspect
credentials, contact a provider, or make network calls. The human-readable
provider output prints only the compatibility `classes` summary for the same
five providers; it does not dump authoritative profiles.

Profiles describe explicit canonical class intent. When class is inherited
rather than explicitly selected, provider-native defaults and overrides may
take precedence. Blacksmith is `unmapped`: its workflow chooses capacity
outside a supported static Crabbox class-to-machine catalog.

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
