# Run Cloud Provider

Read when:

- choosing `provider: run-cloud`;
- configuring the Run Cloud CLI, image, region, or workdir;
- changing `internal/providers/runcloud`.

[Run Cloud](https://run.cloud) provides persistent Linux microVM sandboxes.
Crabbox uses the public `runcloud` CLI for sandbox lifecycle, exposes each
Crabbox-owned sandbox so the CLI's authenticated WebSocket SSH proxy is
available, installs a per-lease Crabbox SSH key, and then uses the normal
Crabbox rsync and command path.

## When To Use

Use Run Cloud when a repository needs a managed Linux sandbox with full
Crabbox SSH, sync, Actions hydration, and warm-lease workflows. The provider is
direct-only: the local `runcloud` login owns authentication and Crabbox never
copies the Run Cloud API key into repository config or command arguments.

## Prerequisites

- Install the CLI with `npm install -g runcloud`.
- Authenticate once with `runcloud login`.
- Verify sandbox access with `runcloud account --json`.
- Custom images must provide `tar` and `python3`, plus either `git` and `rsync`
  or Debian-compatible `apt-get` with root/passwordless-sudo access. Crabbox
  installs missing `git` and `rsync` in the default `runcloud/agent-base` image.

## Commands

```sh
crabbox warmup --provider run-cloud
crabbox run --provider run-cloud -- pnpm test
crabbox run --provider run-cloud --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider run-cloud --id blue-lobster
crabbox stop --provider run-cloud blue-lobster
```

## Config

```yaml
provider: run-cloud
target: linux
runCloud:
  cliPath: runcloud
  image: runcloud/agent-base
  region: eu-north
  workdir: /home/runcloud/crabbox
```

`region` is optional and is omitted by default so Run Cloud can apply account
and platform placement defaults.

Provider flags:

```text
--run-cloud-cli
--run-cloud-image
--run-cloud-region
--run-cloud-workdir
```

Environment overrides:

```text
CRABBOX_RUN_CLOUD_CLI
CRABBOX_RUN_CLOUD_IMAGE
CRABBOX_RUN_CLOUD_REGION
CRABBOX_RUN_CLOUD_WORKDIR
```

Run Cloud's own `RUN_CLOUD_HOME` and login configuration continue to be read by
the `runcloud` CLI.

## Lifecycle

1. `crabbox warmup --provider run-cloud` creates a persistent sandbox through
   `runcloud sandbox create --persistent --json`. Crabbox passes its lease TTL
   as the sandbox's explicit maximum lifetime and requests the `2 vCPU / 24 GiB`
   reservation used by Run Cloud's exposed-sandbox contract. It waits for the
   sandbox to reach `running` and records the exact sandbox ID in the local lease
   claim.
2. Crabbox publishes guest port `3000` with `runcloud sandbox expose`. The
   published application port is incidental to this adapter; SSH itself travels
   through `runcloud sandbox proxy` and does not expose public port 22.
3. Crabbox installs its per-lease public key for the `runcloud` user, verifies
   the sync prerequisites over SSH, and uses the standard SSH lease workflow
   for repository sync and commands.
4. Reusing an ID or slug resolves the local claim, resumes a paused or archived
   sandbox, refreshes the public key, and reconnects through the proxy.
5. `crabbox stop` calls `runcloud sandbox rm`, which removes the public hostname
   and sandbox, then removes the local claim and per-lease key. An already
   missing sandbox is treated as released.

## Ownership And Safety

Crabbox lists and releases only Run Cloud sandbox IDs recorded in local
`provider: run-cloud` claims. A raw Run Cloud sandbox ID or name must be adopted
explicitly with `--reclaim`; a `crabbox-` name prefix alone is not accepted as
proof of ownership.

The CLI credential stays in Run Cloud's credential store. The SSH ProxyCommand
contains only the configured CLI path and sandbox ID. Public SSH keys may
appear in the provider bootstrap command, but private keys never leave the
Crabbox per-lease key directory.

## Limitations

- The adapter does not expose Run Cloud CPU, memory, disk, desktop, snapshot,
  or custom-domain controls. Use the `runcloud` CLI directly when those product
  capabilities are needed outside Crabbox's lease workflow.
- `--class` and `--type` are not supported. Use a prepared Run Cloud image when
  a workflow needs additional tools.
- Custom images must preserve the `runcloud` SSH user and provide the bootstrap
  prerequisites described above.
- Run Cloud currently makes its authenticated SSH proxy available through an
  exposed sandbox. The adapter therefore publishes guest port `3000`; do not
  bind a sensitive unauthenticated service to that port while the lease exists.

## Live Validation

The guarded live smoke creates one Run Cloud sandbox with a 15-minute maximum
lifetime, waits for SSH, syncs the selected repository, runs a small command,
and removes the exact sandbox. Its exit trap retries targeted cleanup if a
later assertion fails.

```sh
CRABBOX_LIVE=1 \
  CRABBOX_LIVE_PROVIDERS=run-cloud \
  CRABBOX_LIVE_COORDINATOR=0 \
  CRABBOX_LIVE_REPO=/path/to/a/repository \
  scripts/live-smoke.sh
```

This command consumes real Run Cloud capacity. If it is interrupted before
cleanup completes, list the remaining sandbox with `runcloud sandbox list` and
remove it with `runcloud sandbox rm <sandbox-id>`.
