# 🦀 Crabbox Docs

**Warm a box, sync the diff, run the suite.**

## What Crabbox is

Crabbox is a shared agent workspace control plane for software maintainers and
AI agents. The goal is to keep the local developer story unchanged - edit,
save, run - while moving compute, tests, and review evidence onto owned or
provider-backed remote capacity.

A `crabbox run` command leases a brokered cloud machine, reuses a static SSH
host, or delegates to a sandbox provider, syncs your tracked and nonignored
local files, executes the command remotely, streams stdout and stderr back, and
releases or unclaims the target. Behind the scenes a small Cloudflare-hosted
broker owns cloud provider credentials, lease state, cleanup, usage, and cost
guardrails so individual machines and CLIs never need to.

## How it fits together

```text
your laptop                Cloudflare Worker            cloud provider
-------------              ------------------           --------------
crabbox CLI    -- HTTPS --> Fleet Durable Object  -->   Hetzner / AWS EC2 / Azure
   |                         lease + cost state              |
   |                                                         |
   +------------ SSH + rsync to leased runner <--------------+
```

The CLI is a Go binary. The broker is a Cloudflare Worker plus a single Durable Object. Brokered Linux runners are vanilla Ubuntu boxes prepared by cloud-init with SSH, Git, rsync, curl, jq, and `/work/crabbox`; AWS can also broker managed Windows/WSL2 and EC2 Mac desktop targets, while Azure can broker native Windows SSH/sync/run targets. Static hosts are existing SSH machines selected with `provider: ssh`. Project runtimes come from Actions hydration or repo-owned setup. Runners hold no broker credentials - they are leaf nodes.

## A run, end to end

1. CLI loads config from flags, env, repo, user, defaults.
2. CLI mints a per-lease SSH key and slug, then calls `POST /v1/leases` on the broker.
3. Worker checks active-lease and monthly spend caps, reserves worst-case TTL cost, provisions a server, returns host / port / user / workdir / expiry / slug.
4. CLI waits for `crabbox-ready`, seeds remote Git when possible, rsyncs the Git file-list manifest, runs sync guardrails and sanity checks, hydrates the configured base ref.
5. CLI runs the command over SSH, streams output, records run events, sends heartbeats/touches.
6. CLI releases the lease unless `--keep` is set; kept leases still auto-release after idle timeout, and the broker frees reserved cost when the lease closes.

See [How Crabbox Works](how-it-works.md) for the full picture, including warm-machine reuse and the brokered vs direct provider paths. See [Source Map](source-map.md) when you need to trace a documented behavior back to code.

## Install

```sh
brew install openclaw/tap/crabbox
```

Verify with `crabbox --version`.

## Quick start

```sh
# log in once per machine - stores a broker token in user config
crabbox login

# one-shot run on a fresh leased box
crabbox run -- pnpm test

# keep a warm box around for repeated runs; output includes an ID and slug
crabbox warmup
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

`crabbox doctor` validates local config, network reachability, and SSH key availability before you commit to a long workflow. `crabbox usage` summarizes recent spend by user, org, provider, and server type.

## OpenClaw plugin

The repository root is also a native OpenClaw plugin package. Once installed in OpenClaw, it exposes Crabbox operations as agent tools:

- `crabbox_run`
- `crabbox_warmup`
- `crabbox_status`
- `crabbox_list`
- `crabbox_stop`

The plugin shells out to the configured `crabbox` binary with argv arrays, so local Crabbox config, broker login, repo claims, and sync behavior stay owned by the CLI. Configure `plugins.entries.crabbox.config.binary` if the binary is not on `PATH`.

Run history and inspection are intentionally handled by the Crabbox CLI and repo skill, not extra plugin tools. Use `crabbox history`, `crabbox events --after --limit`, `crabbox attach`, `crabbox logs`, `crabbox results`, and `crabbox usage` from a shell-capable agent.

## Where to read next

Pick whichever matches your intent:

- **Start here:** [Getting started](getting-started.md), [How Crabbox Works](how-it-works.md), [Concepts and glossary](concepts.md).
- **Get the mental model:** [Architecture](architecture.md), [Orchestrator](orchestrator.md).
- **Use the CLI:** [CLI](cli.md), [Commands](commands/README.md), [Features](features/README.md), [Configuration](features/configuration.md), [Jobs](features/jobs.md), [Actions hydration](features/actions-hydration.md), [Browser portal](features/portal.md), [Telemetry](features/telemetry.md).
- **Pick or add a target:** [Provider reference](providers/README.md), [Providers feature overview](features/providers.md), [Provider authoring](features/provider-authoring.md), [Provider backends](provider-backends.md), [AWS](providers/aws.md), [Azure](providers/azure.md), [Hetzner](providers/hetzner.md), [Static SSH](providers/ssh.md), [Blacksmith Testbox](providers/blacksmith-testbox.md), [Namespace Devbox](providers/namespace-devbox.md), [Semaphore](providers/semaphore.md), [Sprites](providers/sprites.md), [Daytona](providers/daytona.md), [Islo](providers/islo.md), [E2B](providers/e2b.md), [Interactive desktop and VNC](features/interactive-desktop-vnc.md).
- **Operate it:** [Operations](operations.md), [Observability](observability.md), [Troubleshooting](troubleshooting.md), [Performance](performance.md).
- **Set it up or audit it:** [Infrastructure](infrastructure.md), [Security](security.md), [Source Map](source-map.md), [MVP Plan](mvp-plan.md).

## About these docs

Markdown in this directory is the user-facing documentation source. Implementation truth stays in code; [Source Map](source-map.md) lists the files behind each documented behavior. The GitHub Pages site at <https://openclaw.github.io/crabbox/> is generated from these Markdown files by `scripts/build-docs-site.mjs` and deployed by `.github/workflows/pages.yml`. Pages must be enabled on the repository or organization for the workflow to publish.

Build the docs site locally:

```sh
npm run docs:check
open dist/docs-site/index.html
```
