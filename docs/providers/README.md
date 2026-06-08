# Provider Reference

Read when:

- choosing a Crabbox provider for a repo or one-off command;
- debugging provider-specific provisioning, sync, or command execution;
- changing provider registration, flags, config, or backend behavior.

## Provider model

Every provider registers a backend with one of two kinds:

- **SSH lease** — Crabbox provisions or connects to an SSH-reachable box and owns
  the full lease lifecycle (warmup, sync, run, ssh, cleanup). Core does the
  rsync/command execution directly to the box over SSH.
- **Delegated run** — a sandbox or proof runner. The provider owns sync and
  command execution end to end; there is no SSH lease and no local rsync.

SSH-lease providers further differ by how they reach the cloud:

- **Brokered cloud** — `aws`, `azure`, `gcp`, and `hetzner` can run through the
  Crabbox coordinator (the Cloudflare Worker broker). The Worker owns cloud
  credentials, cost state, cleanup alarms, and lease accounting. This is the
  normal shared-team path. Set with `config set-broker` and a broker URL
  (`CRABBOX_COORDINATOR`).
- **Direct cloud** — the same four providers without a configured broker, plus
  cloud providers that never broker (e.g. `proxmox`, `runpod`, `namespace-devbox`,
  `semaphore`, `sprites`, `exe-dev`, `daytona`). The CLI talks to the provider
  API itself and cleans up best-effort via provider labels.
- **Static SSH** — `ssh` connects to a preexisting machine you supply; no
  provisioning, no cleanup.
- **Local runtime** — `local-container` starts a labeled Linux container through
  a Docker-compatible local runtime (Docker Desktop, OrbStack, Colima),
  `apple-container` uses Apple's native `container` runtime on Apple silicon
  macOS, `multipass` launches local Ubuntu VMs through Canonical Multipass,
  and `tart` runs macOS VMs on Apple Silicon via Cirrus Labs tart.
- **Delegated sandbox** — managed sandbox/proof runners that execute remotely
  without an SSH lease (e.g. `e2b`, `modal`, `islo`, `cloudflare`,
  `azure-dynamic-sessions`, `docker-sandbox`).

Select a provider per command with `--provider <name>` (env `CRABBOX_PROVIDER`),
or set `provider: <name>` in config. Provider flags are registered before
command parsing, so provider-specific flags work even when that provider is not
the default. Most names accept aliases (listed below).

## Provider pages

Each page below maps to an adapter under `internal/providers/<dir>`. The
**Provider id** is the canonical `--provider` value; **Aliases** also resolve.

Some provider pages also preserve environment-specific validation runbooks when
the built-in adapter needs a separate local smoke contract.

### SSH lease

| Page | Provider id | Aliases | Targets | Brokered? |
| --- | --- | --- | --- | --- |
| [AWS](aws.md) | `aws` | — | Linux, macOS, Windows | yes |
| [Azure](azure.md) | `azure` | — | Linux, Windows | yes |
| [Google Cloud](gcp.md) | `gcp` | `google`, `google-cloud` | Linux | yes |
| [Hetzner](hetzner.md) | `hetzner` | — | Linux | yes |
| [Proxmox](proxmox.md) | `proxmox` | — | Linux | no (direct) |
| [Incus](incus.md) | `incus` | — | Linux | no (direct) |
| [Parallels](parallels.md) | `parallels` | — | Linux, macOS, Windows | no (direct) |
| [Static SSH](ssh.md) | `ssh` | `static`, `static-ssh` | Linux, macOS, Windows | no (static) |
| [Local Container](local-container.md) | `local-container` | `docker`, `container`, `local-docker` | Linux | no (local) |
| [Apple Container](apple-container.md) | `apple-container` | `apple`, `applecontainer` | Linux | no (local) |
| [Multipass](multipass.md) | `multipass` | `mp`, `canonical-multipass` | Linux | no (local) |
| [Tart](tart.md) | `tart` | `local-tart`, `macos-vm` | macOS | no (local) |
| [exe.dev](exe-dev.md) | `exe-dev` | `exe`, `exedev` | Linux | no (direct) |
| [KubeVirt](kubevirt.md) | `kubevirt` | `kubernetes-vm` | Linux | no (direct) |
| [External](external.md) | `external` | `exec-provider` | Linux | no (direct) |
| [Namespace Devbox](namespace-devbox.md) | `namespace-devbox` | `namespace`, `namespace-devboxes` | Linux | no (direct) |
| [Semaphore](semaphore.md) | `semaphore` | `sem` | Linux | no (direct) |
| [Sprites](sprites.md) | `sprites` | — | Linux | no (direct) |
| [Tenki](tenki.md) | `tenki` | — | Linux | no (direct) |
| [Daytona](daytona.md) | `daytona` | — | Linux | no (direct) |
| [RunPod](runpod.md) | `runpod` | `run-pod`, `runpodio` | Linux | no (direct) |
| [ASCII Box](ascii-box.md) | `ascii-box` | `ascii`, `asciibox` | Linux | no (direct) |

### Delegated run

| Page | Provider id | Aliases | Targets |
| --- | --- | --- | --- |
| [Azure Dynamic Sessions](azure-dynamic-sessions.md) | `azure-dynamic-sessions` | — | Linux |
| [Blacksmith Testbox](blacksmith-testbox.md) | `blacksmith-testbox` | `blacksmith` | Linux |
| [Cloudflare](cloudflare.md) | `cloudflare` | `cf` | Linux |
| [Docker Sandbox](docker-sandbox.md) | `docker-sandbox` | — | Linux |
| [E2B](e2b.md) | `e2b` | — | Linux |
| [Islo](islo.md) | `islo` | — | Linux |
| [Modal](modal.md) | `modal` | — | Linux |
| [Railway](railway.md) | `railway` | `rail`, `railwayapp` | Linux |
| [Tensorlake](tensorlake.md) | `tensorlake` | `tl`, `tensorlake-sbx` | Linux |
| [Upstash Box](upstash-box.md) | `upstash-box` | `upstash`, `box`, `upstashbox` | Linux |
| [W&B Sandboxes](wandb.md) | `wandb` | `weights-and-biases` | Linux |

Run `crabbox providers` (`--json`) to see the live capability set the binary
reports.

## Notes on families and capabilities

- The Azure family ships two backends: the default VM SSH lease
  (`provider: azure`) and the delegated `azure-dynamic-sessions` provider
  (Azure Container Apps dynamic sessions). They share the `azure` family but are
  distinct adapters.
- Tensorlake is Crabbox's Firecracker-backed delegated provider; Crabbox does
  not provision raw Firecracker instances directly.
- Docker Sandbox is a delegated-run provider driven by the standalone `sbx`
  CLI. It has no aliases, so `docker`, `container`, and `local-docker` remain
  Local Container aliases.
- ASCII Box is an SSH-lease provider. Crabbox uses the documented `box --json`
  CLI for lifecycle/status/delete, then runs normal sync and commands over SSH.
- `incus` is a direct Linux SSH-lease provider that stores Crabbox ownership and
  expiry metadata in Incus `user.crabbox.*` instance config keys. Real Apple
  Silicon smoke still follows the separate local testbed contract documented on
  the provider page.
- Capability flags (`--desktop`, `--browser`, `--code`, VNC) are validated
  against each provider's declared feature set. Among the SSH-lease providers,
  desktop/browser/code surfaces are richest on `aws`, `azure`, `hetzner`,
  `parallels`, `ssh`, and `local-container`; `multipass` exposes local VM SSH
  and sync only in its first implementation, and most direct sandbox/delegated
  providers expose `ssh` and Crabbox sync only.
- Actions runner hydration requires a normal SSH lease on Linux. Use a
  Linux-capable SSH-lease provider for that path.

```sh
crabbox warmup --provider aws --class beast
crabbox run --provider hetzner -- pnpm test
crabbox run --provider docker -- pnpm test
crabbox run --provider docker-sandbox -- go test ./...
crabbox run --provider multipass -- go test ./...
crabbox run --provider blacksmith-testbox --id tbx_123 -- pnpm test
crabbox run --provider namespace-devbox --id blue-lobster -- pnpm test
```

## Implementation

Provider implementation lives under `internal/providers/<name>`; registration is
in `internal/providers/all/all.go`. Command orchestration and the renderer
surface stay in `internal/cli`.

Related docs:

- [Provider backends](../provider-backends.md)
- [Feature overview](../features/providers.md)
- [Source map](../source-map.md)
