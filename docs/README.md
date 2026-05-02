# 🦀 Crabbox Docs

**Warm a box, sync the diff, run the suite.**

## What Crabbox is

Crabbox is a shared remote testbox system for OpenClaw maintainers and AI agents. The goal is to keep the local developer story unchanged - edit, save, run - while moving compute and tests onto owned cloud capacity.

A `crabbox run` command leases a Linux machine, syncs your tracked and nonignored local files, executes the command remotely, streams stdout and stderr back, and releases the machine. Behind the scenes a small Cloudflare-hosted broker owns provider credentials, lease state, cleanup, usage, and cost guardrails so individual machines and CLIs never need to.

## How it fits together

```text
your laptop                Cloudflare Worker            cloud provider
-------------              ------------------           --------------
crabbox CLI    -- HTTPS --> Fleet Durable Object  -->   Hetzner / AWS Spot
   |                         lease + cost state              |
   |                                                         |
   +------------ SSH + rsync to leased runner <--------------+
```

The CLI is a Go binary. The broker is a Cloudflare Worker plus a single Durable Object. Runners are vanilla Ubuntu boxes prepared by cloud-init with SSH, Git, rsync, curl, jq, and `/work/crabbox`. Project runtimes come from Actions hydration or repo-owned setup. Runners hold no broker credentials - they are leaf nodes.

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
- `crabbox_history`
- `crabbox_events`
- `crabbox_logs`
- `crabbox_results`
- `crabbox_usage`

The plugin shells out to the configured `crabbox` binary with argv arrays, so local Crabbox config, broker login, repo claims, and sync behavior stay owned by the CLI. Configure `plugins.entries.crabbox.config.binary` if the binary is not on `PATH`. Set `plugins.entries.crabbox.config.allowInspection: false` to disable run history, event, log, result, and usage inspection tools.

## Where to read next

Pick whichever matches your intent:

- **Get the mental model:** [How Crabbox Works](how-it-works.md), [Architecture](architecture.md), [Orchestrator](orchestrator.md).
- **Use the CLI:** [CLI](cli.md), [Commands](commands/README.md), [Features](features/README.md), [Actions hydration](features/actions-hydration.md).
- **Operate it:** [Operations](operations.md), [Observability](observability.md), [Troubleshooting](troubleshooting.md), [Performance](performance.md).
- **Set it up or audit it:** [Infrastructure](infrastructure.md), [Security](security.md), [Source Map](source-map.md), [MVP Plan](mvp-plan.md).

## About these docs

Markdown in this directory is the user-facing documentation source. Implementation truth stays in code; [Source Map](source-map.md) lists the files behind each documented behavior. The GitHub Pages site at <https://openclaw.github.io/crabbox/> is generated from these Markdown files by `scripts/build-docs-site.mjs` and deployed by `.github/workflows/pages.yml`. Pages must be enabled on the repository or organization for the workflow to publish.

Build the docs site locally:

```sh
npm run docs:check
open dist/docs-site/index.html
```
