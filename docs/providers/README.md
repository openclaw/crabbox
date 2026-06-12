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
  cloud providers that never broker (e.g. `proxmox`, `hostinger`, `runpod`, `namespace-devbox`,
  `semaphore`, `sprites`, `exe-dev`, `daytona`, `morph`). The CLI talks to the provider
  API itself and cleans up best-effort via provider labels.
- **Static SSH** — `ssh` connects to a preexisting machine you supply; no
  provisioning, no cleanup.
- **Local runtime** — `local-container` starts a labeled Linux container through
  a Docker-compatible local runtime (Docker Desktop, OrbStack, Colima),
  `apple-container` uses Apple's native `container` runtime on Apple silicon
  macOS, `apple-vz` launches a headless Linux VM through Apple's
  `Virtualization.framework`, `multipass` launches local Ubuntu VMs through
  Canonical Multipass, `tart` runs macOS VMs on Apple Silicon via Cirrus Labs
  tart, and `hyperv` creates local Windows VMs through Microsoft Hyper-V.
- **Delegated sandbox** — managed sandbox/proof runners that execute remotely
  without an SSH lease (e.g. `e2b`, `modal`, `islo`, `cloudflare`,
  `azure-dynamic-sessions`, `docker-sandbox`). `anthropic-sandbox-runtime` is
  the local macOS/Linux delegated-run exception: Anthropic's `srt` executes on
  the current machine while still owning sync/run policy end to end.

Select a provider per command with `--provider <name>` (env `CRABBOX_PROVIDER`),
or set `provider: <name>` in config. Provider flags are registered before
command parsing, so provider-specific flags work even when that provider is not
the default. Most names accept aliases (listed below).

## Provider pages

Each page below maps to an adapter under `internal/providers/<dir>`. The first
code value is the canonical `--provider` value; parenthesized values are aliases.

Some provider pages also preserve environment-specific validation runbooks when
the built-in adapter needs a separate local smoke contract.

### SSH lease

| Provider and aliases | Runs on / mode |
| --- | --- |
| [AWS](aws.md) — `aws` | Linux, macOS, Windows · brokered |
| [Azure](azure.md) — `azure` | Linux, Windows · brokered |
| [Google Cloud](gcp.md) — `gcp` (`google`, `google-cloud`) | Linux · brokered |
| [Hetzner](hetzner.md) — `hetzner` | Linux · brokered |
| [DigitalOcean](digitalocean.md) — `digitalocean` | Linux · direct |
| [Hostinger](hostinger.md) — `hostinger` | Linux · direct |
| [Proxmox](proxmox.md) — `proxmox` | Linux · direct |
| [XCP-ng](xcp-ng.md) — `xcp-ng` | Linux · direct |
| [Incus](incus.md) — `incus` | Linux · direct |
| [Parallels](parallels.md) — `parallels` | Linux, macOS, Windows · direct |
| [Static SSH](ssh.md) — `ssh` (`static`, `static-ssh`) | Linux, macOS, Windows · static |
| [Local Container](local-container.md) — `local-container` (`docker`, `container`, `local-docker`) | Linux · local |
| [Apple Container](apple-container.md) — `apple-container` (`apple`, `applecontainer`) | Linux · local |
| [Apple Container Machine](apple-machine.md) — `apple-machine` (`applemachine`) | Linux · local |
| [Apple VZ](apple-vz.md) — `apple-vz` (`applevz`) | Linux ARM64 · local |
| [Multipass](multipass.md) — `multipass` (`mp`, `canonical-multipass`) | Linux · local |
| [Tart](tart.md) — `tart` (`local-tart`, `macos-vm`) | macOS · local |
| [Hyper-V](hyperv.md) — `hyperv` | Windows · local |
| [exe.dev](exe-dev.md) — `exe-dev` (`exe`, `exedev`) | Linux · direct |
| [KubeVirt](kubevirt.md) — `kubevirt` (`kubernetes-vm`) | Linux · direct |
| [External](external.md) — `external` (`exec-provider`) | Linux · direct |
| [Namespace Devbox](namespace-devbox.md) — `namespace-devbox` (`namespace`, `namespace-devboxes`) | Linux · direct |
| [Semaphore](semaphore.md) — `semaphore` (`sem`) | Linux · direct |
| [Sprites](sprites.md) — `sprites` | Linux · direct |
| [Tenki](tenki.md) — `tenki` | Linux · direct |
| [Daytona](daytona.md) — `daytona` | Linux · direct |
| [Morph](morph.md) — `morph` | Linux · direct |
| [RunPod](runpod.md) — `runpod` (`run-pod`, `runpodio`) | Linux · direct |
| [ASCII Box](ascii-box.md) — `ascii-box` (`ascii`, `asciibox`) | Linux · direct |

### Delegated run

| Provider and aliases | Runs on |
| --- | --- |
| [Azure Dynamic Sessions](azure-dynamic-sessions.md) — `azure-dynamic-sessions` | Linux |
| [Blacksmith Testbox](blacksmith-testbox.md) — `blacksmith-testbox` (`blacksmith`) | Linux |
| [Cloudflare](cloudflare.md) — `cloudflare` (`cf`) | Linux |
| [Docker Sandbox](docker-sandbox.md) — `docker-sandbox` | Linux |
| [E2B](e2b.md) — `e2b` | Linux |
| [Freestyle](freestyle.md) — `freestyle` | Linux |
| [Islo](islo.md) — `islo` | Linux |
| [Modal](modal.md) — `modal` | Linux |
| [Microsoft Execution Containers](mxc.md) — `mxc` (`execution-container`) | Windows |
| [OpenComputer](opencomputer.md) — `opencomputer` (`oc`, `open-computer`) | Linux |
| [OpenSandbox](opensandbox.md) — `opensandbox` | Linux |
| [Railway](railway.md) — `railway` (`rail`, `railwayapp`) | Linux |
| [Anthropic Sandbox Runtime](anthropic-sandbox-runtime.md) — `anthropic-sandbox-runtime` (`srt`) | macOS, Linux |
| [Tensorlake](tensorlake.md) — `tensorlake` (`tl`, `tensorlake-sbx`) | Linux |
| [Upstash Box](upstash-box.md) — `upstash-box` (`upstash`, `box`, `upstashbox`) | Linux |
| [W&B Sandboxes](wandb.md) — `wandb` (`weights-and-biases`) | Linux |

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
- OpenSandbox is a delegated-run provider using the OpenSandbox Go SDK for
  lifecycle, file upload, and execd command execution. It has no aliases in v1,
  so `osb` remains reserved.
- Anthropic Sandbox Runtime is a local one-shot delegated-run provider driven
  by the standalone `srt` CLI. It has no SSH lease, no persistent lifecycle,
  and no remote sync surface.
- ASCII Box is an SSH-lease provider. Crabbox uses the documented `box --json`
  CLI for lifecycle/status/delete, then runs normal sync and commands over SSH.
- XCP-ng is a direct SSH-lease provider for a self-hosted XCP-ng pool on
  dedicated 64-bit x86 server-class hardware. XCP-ng itself can host Linux,
  Windows, and BSD guests, but Crabbox's current `xcp-ng` adapter provisions
  normal leases from Linux templates only. Crabbox talks to XAPI from the CLI,
  uses `VM.copy` plus `VM.provision`, injects cloud-init through a FAT16
  `CIDATA` config drive, optionally moves all VIFs to the configured network,
  and uses guest metrics for IPv4 discovery. See the provider page for the
  separate Windows x86_64/x64 ISO E2E harness, and use the Tart provider on
  Apple hardware for macOS VM workflows.
- `incus` is a direct Linux SSH-lease provider that stores Crabbox ownership and
  expiry metadata in Incus `user.crabbox.*` instance config keys. Real Apple
  Silicon smoke still follows the separate local testbed contract documented on
  the provider page.
- DigitalOcean is a direct-only Linux Droplet provider. It uses
  `DIGITALOCEAN_TOKEN`, per-lease SSH keys, and Crabbox-owned flat tags; it does
  not run through the Worker broker in Phase 1.
- Hostinger is a direct-only Linux VPS provider. Purchases require explicit
  opt-in; release stops the VPS but does not cancel its subscription.
- Capability flags (`--desktop`, `--browser`, `--code`, VNC) are validated
  against each provider's declared feature set. Among the SSH-lease providers,
  desktop/browser/code surfaces are richest on `aws`, `azure`, `hetzner`,
  `parallels`, `ssh`, and `local-container`; `multipass` exposes local VM SSH
  and sync only in its first implementation, `apple-vz` does the same through a
  local helper and host-local SSH proxy, and most direct sandbox/delegated
  providers expose `ssh` and Crabbox sync only.
- Actions runner hydration requires a normal SSH lease on Linux. Use a
  Linux-capable SSH-lease provider for that path.

```sh
crabbox warmup --provider aws --class beast
crabbox run --provider hetzner -- pnpm test
crabbox run --provider digitalocean --type s-1vcpu-1gb -- pnpm test
crabbox doctor --provider hostinger
crabbox run --provider docker -- pnpm test
crabbox run --provider docker-sandbox -- go test ./...
crabbox run --provider apple-vz -- go test ./...
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
