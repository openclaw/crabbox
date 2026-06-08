# 🦀 Crabbox Docs

**Warm a box, sync the diff, run the suite.**

## What Crabbox is

Crabbox is a generic remote software testing and execution control plane. It
keeps the local developer story unchanged — edit, save, run — while moving the
actual compute, tests, and review evidence onto owned or provider-backed remote
capacity. A maintainer or an AI agent issues one command; Crabbox leases a box,
ships the working tree, runs the command, streams the output back, and cleans
up.

A `crabbox run` leases a brokered cloud machine, reuses a static SSH host, or
delegates to a sandbox provider; syncs your tracked, non-ignored local files;
executes the command remotely; streams stdout and stderr back; and then
releases or unclaims the target. A small Cloudflare-hosted broker owns cloud
provider credentials, lease state, cleanup, usage accounting, and cost
guardrails so individual machines and CLIs never hold those.

## How it fits together

```text
your laptop                Cloudflare Worker            cloud provider
-------------              ------------------           --------------
crabbox CLI    -- HTTPS --> Fleet Durable Object  -->   Hetzner / AWS / Azure / GCP
   |                         lease + cost state              |
   |                                                         |
   +------------ SSH + rsync to leased runner <--------------+
```

The CLI is a Go binary (`cmd/crabbox`, `internal/cli`). The broker is a
Cloudflare Worker plus a single Durable Object (`worker/src`). Lease lifecycle
calls go through the broker over HTTPS, but the data plane — SSH, rsync, and
command execution — goes **directly from the CLI to the runner host**. Runners
hold no broker credentials; they are leaf nodes.

Crabbox selects one of three execution modes per provider:

- **Brokered** — for `aws`, `azure`, `gcp`, and `hetzner` when a broker URL is
  configured (`CRABBOX_COORDINATOR`). The Worker provisions and tracks leases;
  the CLI still drives sync and command execution over SSH.
- **Direct SSH** — the same SSH-lease providers without a broker, plus static
  hosts (`provider: ssh`) and self-hosted/local providers. The CLI talks to the
  cloud or host API itself.
- **Delegated** — sandbox/proof runners (for example dynamic-session and
  Firecracker providers) that own sync and run end to end; there is no SSH lease.

Brokered Linux runners are vanilla Ubuntu boxes prepared by cloud-init with SSH,
Git, rsync, and `/work/crabbox`. AWS and Azure can also broker Windows
(normal and WSL2) and, on AWS, EC2 Mac desktop targets. Project runtimes come
from Actions hydration or repo-owned setup.

## A run, end to end

1. The CLI loads config from flags, env, repo, user, and defaults.
2. The CLI mints a per-lease SSH key and slug, then `POST /v1/leases` on the
   broker (brokered mode) or provisions directly (direct mode).
3. The Worker checks active-lease and monthly spend caps, reserves worst-case
   TTL cost, provisions a server with region/market fallback, and returns host /
   port / user / workdir / expiry / slug.
4. The CLI waits for the `crabbox-ready` marker, seeds remote Git when possible,
   rsyncs the Git file-list manifest, runs sync guardrails, and hydrates the
   configured base ref.
5. The CLI runs the command over SSH, streams output, records run events, and
   sends heartbeats.
6. The CLI releases the lease unless `--keep` is set. Kept leases still
   auto-release after the idle timeout, and the broker frees reserved cost when
   the lease closes.

See [How Crabbox Works](how-it-works.md) for the full picture, including
warm-box reuse and the brokered-vs-direct paths. See the
[Source Map](source-map.md) to trace any documented behavior back to code.

## Install

```sh
brew install openclaw/tap/crabbox
```

Verify with `crabbox --version`.

## Quick start

```sh
# log in once per machine — stores a broker token in user config
crabbox login --url https://broker.example.com

# one-shot run on a fresh leased box
crabbox run -- pnpm test

# keep a warm box around for repeated runs; output includes an id and a slug
crabbox warmup
crabbox run --id swift-crab -- pnpm test:changed
crabbox ssh --id swift-crab
crabbox stop swift-crab
```

Each lease has a canonical id (`cbx_<12 hex>`) and a friendly slug
(`<adjective>-<noun>`); most commands accept either via `--id`. Run
`crabbox doctor` to validate local config, broker/provider reachability, and SSH
key availability before a long workflow, and `crabbox usage` to summarize recent
spend by user, org, provider, and server type.

## Where to read next

Pick whichever matches your intent:

- **Start here:** [Getting started](getting-started.md),
  [How Crabbox Works](how-it-works.md),
  [Concepts and glossary](concepts.md).
- **Get the mental model:** [Vision](vision.md),
  [Architecture](architecture.md), [Orchestrator](orchestrator.md),
  [Broker auth and routing](features/broker-auth-routing.md),
  [Coordinator](features/coordinator.md).
- **Use the CLI:** [CLI overview](cli.md),
  [Command reference](commands/README.md),
  [Feature reference](features/README.md),
  [Configuration](features/configuration.md), [Jobs](features/jobs.md),
  [Pond](features/pond.md),
  [Actions hydration](features/actions-hydration.md),
  [Capsules](features/capsules.md), [Checkpoints](features/checkpoints.md),
  [Browser portal](features/portal.md),
  [Capabilities](features/capabilities.md),
  [Interactive desktop and VNC](features/interactive-desktop-vnc.md),
  [Telemetry](features/telemetry.md), [Sync](features/sync.md).
- **Pick or add a target:** [Provider reference](providers/README.md),
  [Providers feature overview](features/providers.md),
  [Provider authoring](features/provider-authoring.md),
  [Provider backends](provider-backends.md),
  [Capacity fallback](features/capacity-fallback.md),
  [Network](features/network.md), [Tailscale](features/tailscale.md).
  Per-provider: [AWS](providers/aws.md), [Azure](providers/azure.md),
  [Azure Dynamic Sessions](providers/azure-dynamic-sessions.md),
  [Google Cloud](providers/gcp.md), [Hetzner](providers/hetzner.md),
  [Proxmox](providers/proxmox.md), [Incus](providers/incus.md), [Parallels](providers/parallels.md),
  [Local Container](providers/local-container.md),
  [Multipass](providers/multipass.md),
  [Static SSH](providers/ssh.md), [Railway](providers/railway.md),
  [RunPod](providers/runpod.md),
  [Blacksmith Testbox](providers/blacksmith-testbox.md),
  [KubeVirt](providers/kubevirt.md), [External](providers/external.md),
  [Namespace Devbox](providers/namespace-devbox.md),
  [Semaphore](providers/semaphore.md), [Sprites](providers/sprites.md),
  [Tenki](providers/tenki.md),
  [Daytona](providers/daytona.md), [Islo](providers/islo.md),
  [E2B](providers/e2b.md), [Modal](providers/modal.md),
  [Tensorlake](providers/tensorlake.md), [Upstash Box](providers/upstash-box.md),
  [Weights & Biases](providers/wandb.md), [Cloudflare](providers/cloudflare.md).
- **Operate it:** [Operations](operations.md),
  [Observability](observability.md), [Troubleshooting](troubleshooting.md),
  [Performance](performance.md), [Cost and usage](features/cost-usage.md),
  [Lifecycle and cleanup](features/lifecycle-cleanup.md).
- **Set it up or audit it:** [Infrastructure](infrastructure.md),
  [Security](security.md), [Auth and admin](features/auth-admin.md),
  [Repository onboarding](features/repository-onboarding.md),
  [SSH keys](features/ssh-keys.md), [Source Map](source-map.md).

## About these docs

Markdown in this directory is the user-facing documentation source.
Implementation truth stays in code; the [Source Map](source-map.md) lists the
files behind each documented behavior. The GitHub Pages site at
<https://openclaw.github.io/crabbox/> is generated from these Markdown files by
`scripts/build-docs-site.mjs` and deployed by `.github/workflows/pages.yml`.
Pages must be enabled on the repository or organization for the workflow to
publish.

Build and check the docs site locally:

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```
