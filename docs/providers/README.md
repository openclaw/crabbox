# Provider Reference

Read when:

- choosing a Crabbox provider for a repo or one-off command;
- debugging provider-specific provisioning, sync, or command execution;
- changing provider registration, flags, config, or backend behavior.

Crabbox supports managed SSH lease providers, delegated run providers, and one
static SSH provider for existing machines.

| Provider | Backend kind | Targets | Best for |
| --- | --- | --- | --- |
| [AWS](aws.md) | SSH lease | Linux, Windows, macOS | broad managed capacity, Windows, EC2 Mac |
| [Azure](azure.md) | SSH lease | Linux, Windows | Windows clien |
| [Hetzner](hetzner.md) | SSH lease | Linux | fast Linux capacity at low cost |
| [Static SSH](ssh.md) | SSH lease | Linux, macOS, Windows | reusing an existing host |
| [Blacksmith Testbox](blacksmith-testbox.md) | delegated run | Linux | existing Blacksmith Testbox workflows |
| [Daytona](daytona.md) | hybrid delegated run + SSH | Linux | Daytona snapshot sandboxes |
| [Islo](islo.md) | delegated run | Linux | Islo-owned sandbox execution |

## Shared Rules

Core Crabbox owns provider selection, config loading, friendly slugs, local repo
claims, timing summaries, command rendering, and normalized list/status output.
Providers own only their backend boundary: provisioning or delegated command
execution.

Use `--provider <name>` for one command, or set `provider: <name>` in Crabbox
config. Provider flags are registered by provider packages before command-line
parsing, so provider-specific flags work even when that provider is not the
default.

```sh
crabbox warmup --provider aws --class beast
crabbox run --provider hetzner -- pnpm test
crabbox run --provider blacksmith-testbox --id tbx_123 -- pnpm test
```

## Brokered Versus Direct

AWS, Azure, and Hetzner can run through the Crabbox coordinator or directly
from the CLI.
Coordinator mode is the normal shared-team path: the Worker owns cloud
credentials, cost state, cleanup alarms, and lease accounting.

Direct mode is for local operator debugging or non-brokered setups. It uses local
provider credentials and best-effort cleanup through provider labels.

Delegated providers do not use the Crabbox coordinator:

- Blacksmith uses the authenticated Blacksmith CLI.
- Daytona uses Daytona API and SDK/toolbox APIs.
- Islo uses the Islo API and SDK auth.

## Feature Matrix

| Provider | `run` | `warmup` | `ssh` | VNC/code | Crabbox sync | Provider sync |
| --- | --- | --- | --- | --- | --- | --- |
| AWS | yes | yes | yes | yes | yes | no |
| Azure | yes | yes | yes | Linux VNC/code | yes | no |
| Hetzner | yes | yes | yes | Linux VNC/code | yes | no |
| Static SSH | yes | resolves host | yes | host-dependent | yes | no |
| Blacksmith Testbox | yes | yes | no | no | no | yes |
| Daytona | yes | yes | yes | no | archive via Daytona toolbox | no |
| Islo | yes | yes | no | no | no | yes |

Actions runner hydration requires a normal SSH lease on Linux and is core-over-SSH.
Use AWS, Hetzner, or Static SSH for that path.

## Implementation

Provider implementation lives under `internal/providers/<name>`. The command
orchestration and renderer surface stays in `internal/cli`.

Related docs:

- [Provider backends](../provider-backends.md)
- [Feature overview](../features/providers.md)
- [Source map](../source-map.md)
