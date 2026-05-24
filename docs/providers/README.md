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
| [Parallels](parallels.md) | SSH lease | Linux, macOS, Windows | local or remote Mac Parallels template/fleet clones |
| [Local Container](local-container.md) | SSH lease | Linux | zero-cloud Linux and desktop/browser smoke tests through Docker-compatible runtimes |
| [Static SSH](ssh.md) | SSH lease | Linux, macOS, Windows | reusing an existing host |
| [exe.dev](exe-dev.md) | SSH lease | Linux | disposable exe.dev VMs with Crabbox sync |
| [Blacksmith Testbox](blacksmith-testbox.md) | delegated run | Linux | existing Blacksmith Testbox workflows |
| [Namespace Devbox](namespace-devbox.md) | SSH lease | Linux | Namespace-managed dev environments with Crabbox sync |
| [Semaphore](semaphore.md) | SSH lease | Linux | Semaphore CI environments with project secrets and cache |
| [Sprites](sprites.md) | SSH lease | Linux | fast Sprites microVMs through `sprite proxy` |
| [Daytona](daytona.md) | hybrid delegated run + SSH | Linux | Daytona snapshot sandboxes |
| [Islo](islo.md) | delegated run | Linux | Islo-owned sandbox execution |
| [E2B](e2b.md) | delegated run | Linux | E2B-owned sandbox execution |
| [Modal](modal.md) | delegated run | Linux | Modal Sandbox execution through the local Python client |
| [Upstash Box](upstash-box.md) | delegated run | Linux | Upstash Box execution through the Box REST API |
| [Tensorlake](tensorlake.md) | delegated run | Linux | Tensorlake Firecracker sandbox execution via the `tensorlake` CLI |
| [Cloudflare](cloudflare.md) | delegated run | Linux | Cloudflare execution through a Worker and container runner |
| [Railway](railway.md) | delegated run | Linux | redeploy and stream logs for an existing Railway service via the GraphQL API |
| [RunPod](runpod.md) | SSH lease | Linux | disposable RunPod pods provisioned via the REST API and accessed over public SSH |
| [W&B Sandboxes](wandb.md) | delegated run | Linux | [wandb.ai](https://wandb.ai/) (by CoreWeave) — the only provider an AI researcher can use with the `wandb login` credential they already have |

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
crabbox run --provider docker -- pnpm test
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
- Parallels clones local or remote Mac Parallels Desktop VMs through `prlctl`.
- Local Container starts labeled Linux containers through a Docker-compatible
  local runtime such as Docker Desktop, OrbStack, or Colima.
- exe.dev creates and deletes VMs through the exe.dev SSH API.
- Blacksmith uses the authenticated Blacksmith CLI.
- Daytona uses Daytona API and SDK/toolbox APIs.
- Islo uses the Islo API and SDK auth.
- E2B uses E2B's sandbox REST and envd APIs.
- Modal uses the local Modal Python client and Modal Sandbox APIs.
- Upstash Box uses the [Upstash Box](https://upstash.com/blog/upstash-box)
  REST API for sandbox lifecycle, archive upload, and command streaming.
- Sprites uses the authenticated `sprite` CLI plus Sprites API.
- Tensorlake uses the `tensorlake` CLI (`tensorlake sbx ...`) for sandbox lifecycle and command exec.
- Cloudflare uses a deployed Worker runner backed by a Cloudflare
  Containers image.
- Railway uses the [Railway](https://railway.com) GraphQL API (`deploymentRedeploy`,
  `deploymentLogs`, `deploymentStop`) against a pre-existing service the user
  owns. The user's command argument is logged; Railway runs the service's own
  start command — there is no synchronous exec endpoint.
- RunPod uses the [RunPod](https://runpod.io) REST API (`/v1/pods`) to
  provision a pod that exposes SSH on port 22. Once `publicIp` and
  `portMappings["22"]` report the public TCP mapping, Crabbox reuses its
  normal SSH sync/run path against `root@<pod-ip>:<public-port>`.

Local Container, Namespace Devbox, and Semaphore are SSH lease providers that do
not use the Crabbox coordinator. Local Container provisions through `docker`;
Namespace provisions through the authenticated `devbox` CLI; Semaphore
provisions through the Semaphore REST API; Sprites provisions through the
Sprites API and reaches SSH through `sprite proxy`; exe.dev provisions through
`ssh exe.dev` and returns a normal VM SSH target.

## Feature Matrix

| Provider | `run` | `warmup` | `ssh` | VNC/code | Crabbox sync | Provider sync |
| --- | --- | --- | --- | --- | --- | --- |
| AWS | yes | yes | yes | yes | yes | no |
| Azure | yes | yes | yes | Linux/Windows VNC; Linux code | yes | no |
| Google Cloud | yes | yes | yes | no | yes | no |
| Hetzner | yes | yes | yes | Linux VNC/code | yes | no |
| Proxmox | yes | yes | yes | no | yes | no |
| Parallels | yes | yes | yes | host-dependent | yes | no |
| Local Container | yes | yes | yes | local VNC/WebVNC; no code | yes | no |
| Static SSH | yes | resolves host | yes | host-dependent | yes | no |
| exe.dev | yes | yes | yes | no | yes | no |
| Blacksmith Testbox | yes | yes | no | no | no | yes |
| Namespace Devbox | yes | yes | yes | no | yes | no |
| Semaphore | yes | yes | yes | no | yes | no |
| Sprites | yes | yes | yes | no | yes | no |
| Daytona | yes | yes | yes | no | archive via Daytona toolbox | no |
| Islo | yes | yes | no | no | no | yes |
| E2B | yes | yes | no | no | archive via E2B envd | no |
| Modal | yes | yes | no | no | archive via Modal Sandbox exec | no |
| Upstash Box | yes | yes | no | no | archive via Box file upload | no |
| Tensorlake | yes | yes | no | no | archive via `tensorlake sbx cp` | no |
| Cloudflare | yes | yes | no | no | archive via Worker runner | no |
| Railway | yes | no | no | no | no | no |
| RunPod | yes | yes | yes | no | yes | no |

Actions runner hydration requires a normal SSH lease on Linux and is core-over-SSH.
Use AWS, Google Cloud, Hetzner, Proxmox, Parallels, Static SSH, exe.dev,
Namespace Devbox, Semaphore, Sprites, or RunPod for that path.

## Implementation

Provider implementation lives under `internal/providers/<name>`. The command
orchestration and renderer surface stays in `internal/cli`.

Related docs:

- [Provider backends](../provider-backends.md)
- [Feature overview](../features/providers.md)
- [Source map](../source-map.md)
