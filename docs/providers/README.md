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
| [Azure](azure.md) | SSH lease | Linux, Windows | Azure-backed Linux and Windows capacity |
| [Google Cloud](gcp.md) | SSH lease | Linux | GCP-backed Linux Compute Engine capacity |
| [Hetzner](hetzner.md) | SSH lease | Linux | fast Linux capacity at low cost |
| [Proxmox](proxmox.md) | SSH lease | Linux | private Proxmox VE QEMU VM templates |
| [Static SSH](ssh.md) | SSH lease | Linux, macOS, Windows | reusing an existing host |
| [Blacksmith Testbox](blacksmith-testbox.md) | delegated run | Linux | existing Blacksmith Testbox workflows |
| [Namespace Devbox](namespace-devbox.md) | SSH lease | Linux | Namespace-managed dev environments with Crabbox sync |
| [Semaphore](semaphore.md) | SSH lease | Linux | Semaphore CI environments with project secrets and cache |
| [Sprites](sprites.md) | SSH lease | Linux | fast Sprites microVMs through `sprite proxy` |
| [Daytona](daytona.md) | hybrid delegated run + SSH | Linux | Daytona snapshot sandboxes |
| [Islo](islo.md) | delegated run | Linux | Islo-owned sandbox execution |
| [E2B](e2b.md) | delegated run | Linux | E2B-owned sandbox execution |
| [Tensorlake](tensorlake.md) | delegated run | Linux | Tensorlake Firecracker sandbox execution via the `tensorlake` CLI |

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
crabbox run --provider namespace-devbox --id blue-lobster -- pnpm test
```

## Brokered Versus Direct

AWS, Azure, Google Cloud, and Hetzner can run through the Crabbox coordinator or directly
from the CLI.
Coordinator mode is the normal shared-team path: the Worker owns cloud
credentials, cost state, cleanup alarms, and lease accounting.

Direct mode is for local operator debugging or non-brokered setups. It uses local
provider credentials and best-effort cleanup through provider labels.

Proxmox and delegated providers do not use the Crabbox coordinator:

- Proxmox clones private QEMU VM templates through the Proxmox VE REST API.
- Blacksmith uses the authenticated Blacksmith CLI.
- Daytona uses Daytona API and SDK/toolbox APIs.
- Islo uses the Islo API and SDK auth.
- E2B uses E2B's sandbox REST and envd APIs.
- Sprites uses the authenticated `sprite` CLI plus Sprites API.
- Tensorlake uses the `tensorlake` CLI (`tensorlake sbx ...`) for sandbox lifecycle and command exec.

Namespace Devbox and Semaphore are SSH lease providers that do not use the
Crabbox coordinator. Namespace provisions through the authenticated `devbox`
CLI; Semaphore provisions through the Semaphore REST API; Sprites provisions
through the Sprites API and reaches SSH through `sprite proxy`.

## Feature Matrix

| Provider | `run` | `warmup` | `ssh` | VNC/code | Crabbox sync | Provider sync |
| --- | --- | --- | --- | --- | --- | --- |
| AWS | yes | yes | yes | yes | yes | no |
| Azure | yes | yes | yes | Linux/Windows VNC; Linux code | yes | no |
| Google Cloud | yes | yes | yes | no | yes | no |
| Hetzner | yes | yes | yes | Linux VNC/code | yes | no |
| Proxmox | yes | yes | yes | no | yes | no |
| Static SSH | yes | resolves host | yes | host-dependent | yes | no |
| Blacksmith Testbox | yes | yes | no | no | no | yes |
| Namespace Devbox | yes | yes | yes | no | yes | no |
| Semaphore | yes | yes | yes | no | yes | no |
| Sprites | yes | yes | yes | no | yes | no |
| Daytona | yes | yes | yes | no | archive via Daytona toolbox | no |
| Islo | yes | yes | no | no | no | yes |
| E2B | yes | yes | no | no | archive via E2B envd | no |
| Tensorlake | yes | yes | no | no | archive via `tensorlake sbx cp` | no |

Actions runner hydration requires a normal SSH lease on Linux and is core-over-SSH.
Use AWS, Google Cloud, Hetzner, Proxmox, Static SSH, Namespace Devbox,
Semaphore, or Sprites for that path.

## Implementation

Provider implementation lives under `internal/providers/<name>`. The command
orchestration and renderer surface stays in `internal/cli`.

Related docs:

- [Provider backends](../provider-backends.md)
- [Feature overview](../features/providers.md)
- [Source map](../source-map.md)
