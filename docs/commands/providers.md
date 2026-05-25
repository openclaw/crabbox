# providers

`crabbox providers` prints the provider capability matrix known to the CLI. It
does not contact clouds, check credentials, or scan quota; use `doctor` for
readiness checks.

```sh
crabbox providers
crabbox providers --json
```

## Output

Human output is grouped by provider:

```text
aws
  kind: ssh-lease
  targets: linux,windows/normal,windows/wsl2,macos
  features: ssh,crabbox-sync,cleanup,desktop,browser,code
  coordinator: supported

wandb
  kind: delegated-run
  targets: linux
  features: -
  coordinator: never
  aliases: weave
```

`--json` returns an array with stable provider records:

```json
[
  {
    "provider": "aws",
    "kind": "ssh-lease",
    "targets": ["linux", "windows/normal", "windows/wsl2", "macos"],
    "features": ["ssh", "crabbox-sync", "cleanup", "desktop", "browser", "code"],
    "coordinator": "supported"
  }
]
```

## Fields

- `provider`: canonical provider name.
- `aliases`: accepted alternate names, when any.
- `kind`: `ssh-lease` or `delegated-run`.
- `targets`: supported OS and Windows mode combinations.
- `features`: advertised capability flags such as `ssh`, `crabbox-sync`, `archive-sync`, `cleanup`, `desktop`, `browser`, `code`, `tailscale`, `workspace-checkpoint`, `workspace-fork`, `workspace-restore`, `provider-snapshot`, and `run-proof`.
- `coordinator`: whether the provider can use the coordinator for brokered leases.

Related docs:

- [doctor](doctor.md)
- [run](run.md)
- [Provider reference](../providers/README.md)
