# providers

`crabbox providers` prints the provider capability matrix that the CLI compiles
in. It is a static report: it reads each registered provider's declared spec and
does not contact any cloud, check credentials, or query quota. Use
[`doctor`](doctor.md) when you need live readiness checks.

```sh
crabbox providers
crabbox providers --json
crabbox providers recommend ci-proof
crabbox providers recommend agent-sandbox --json
crabbox providers recommend versioned-workspace
```

## Flags

- `--json`: emit the matrix as a JSON array instead of grouped text.

The matrix form takes no positional arguments. Use `providers recommend` for
workflow-oriented selection guidance.

## `providers recommend`

`crabbox providers recommend` ranks the compiled provider inventory for a
specific workflow without contacting any provider. It uses each provider's
declared targets and features plus the checked-in provider category metadata.
It is selection guidance, not a readiness check; run `crabbox doctor --provider
<name>` before a live workflow.

```sh
crabbox providers recommend
crabbox providers recommend ci-proof
crabbox providers recommend linux-vm --limit 8
crabbox providers recommend versioned-workspace
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
- `gpu`: GPU-oriented execution providers.
- `linux-vm`: general Linux VM or SSH-lease execution.
- `local`: local containers, VMs, or local sandboxes.
- `macos`: macOS targets.
- `self-hosted`: private virtualization, external providers, and BYO SSH.
- `versioned-workspace`: providers with native checkpoint, fork, restore, or
  snapshot-reference capabilities.
- `windows`: native Windows and WSL2 targets.
- `worker-runtime`: Worker/module-runtime execution.

Recommendation flags:

- `--use-case <name>`: pass the use case by flag instead of positionally.
- `--limit <n>`: maximum recommendations to print. Defaults to `5`.
- `--json`: emit recommendations as JSON.

## Output

Text output lists every provider as a block of indented fields:

```text
aws
  family: aws
  kind: ssh-lease
  targets: linux,windows/normal,windows/wsl2,macos
  features: ssh,crabbox-sync,cleanup,desktop,browser,code
  coordinator: supported

parallels
  family: parallels
  kind: ssh-lease
  targets: linux,macos,windows/normal,windows/wsl2
  features: ssh,crabbox-sync,cleanup,desktop,browser,code,workspace-checkpoint,workspace-fork,workspace-restore,provider-snapshot
  workspace: checkpoint,fork,restore,snapshot-ref
  coordinator: never

wandb
  family: wandb
  kind: delegated-run
  targets: linux
  features: -
  coordinator: never
  aliases: weights-and-biases

module-runtime-example
  family: module-runtime-example
  kind: delegated-run
  targets: worker-runtime
  features: module-run
  coordinator: never

hostinger
  family: hostinger
  kind: ssh-lease
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
    "targets": ["linux"],
    "features": ["ssh", "crabbox-sync", "cleanup"],
    "coordinator": "never"
  },
  {
    "provider": "parallels",
    "family": "parallels",
    "kind": "ssh-lease",
    "targets": ["linux", "macos", "windows/normal", "windows/wsl2"],
    "features": ["ssh", "crabbox-sync", "cleanup", "desktop", "browser", "code", "workspace-checkpoint", "workspace-fork", "workspace-restore", "provider-snapshot"],
    "workspace": ["checkpoint", "fork", "restore", "snapshot-ref"],
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
