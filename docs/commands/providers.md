# providers

`crabbox providers` prints the provider capability matrix that the CLI compiles
in. It is a static report: it reads each registered provider's declared spec and
does not contact any cloud, check credentials, or query quota. Use
[`doctor`](doctor.md) when you need live readiness checks.

```sh
crabbox providers
crabbox providers --json
```

## Flags

- `--json`: emit the matrix as a JSON array instead of grouped text.

The command takes no positional arguments.

## Output

Text output lists every provider as a block of indented fields:

```text
aws
  family: aws
  kind: ssh-lease
  targets: linux,windows/normal,windows/wsl2,macos
  features: ssh,crabbox-sync,cleanup,desktop,browser,code
  coordinator: supported

wandb
  family: wandb
  kind: delegated-run
  targets: linux
  features: -
  coordinator: never
  aliases: weights-and-biases

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
    "provider": "aws",
    "family": "aws",
    "kind": "ssh-lease",
    "targets": ["linux", "windows/normal", "windows/wsl2", "macos"],
    "features": ["ssh", "crabbox-sync", "cleanup", "desktop", "browser", "code"],
    "coordinator": "supported"
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
- `targets`: supported OS and Windows mode combinations, such as `linux`,
  `macos`, `windows/normal`, and `windows/wsl2`.
- `features`: advertised capability flags. Possible values include `ssh`,
  `crabbox-sync`, `archive-sync`, `cleanup`, `desktop`, `browser`, `code`,
  `tailscale`, `url-bridge`, `workspace-checkpoint`, `workspace-fork`,
  `workspace-restore`, `provider-snapshot`, `run-proof`, and `run-session`.
- `coordinator`: whether the provider can route leases through the broker.
  - `supported`: the provider can be brokered through the coordinator when a
    broker URL is configured; otherwise it runs direct from the CLI.
  - `never`: the provider always runs direct from the CLI.

## Related docs

- [doctor](doctor.md) — local and broker/provider readiness checks.
- [run](run.md) — sync a checkout and run a command on a lease.
- [Provider reference](../providers/README.md) — per-provider setup and config.
