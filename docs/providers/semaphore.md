# Semaphore Provider

Read when:

- choosing `provider: semaphore` (alias `sem`);
- configuring Semaphore CI testboxes, API auth, machine types, or OS images;
- changing `internal/providers/semaphore`.

Semaphore is an SSH lease provider that turns a standalone Semaphore CI job into
a warm testbox. Crabbox talks to the Semaphore REST API directly (no agent
binary) to create a job, wait for it to start, and pull the job's debug SSH key.
Semaphore owns the job, project secret context, caches, machine type, and OS
image; Crabbox owns the local repo claim, friendly slug, per-lease SSH key,
sync, command execution, timing summary, and normalized list/status rendering.

## When To Use

Use Semaphore when a repo already runs on Semaphore CI and you want a lease that
inherits that project's machine types, OS images, and secret context. Reach for
AWS, Azure, Hetzner, or the static SSH provider instead when the box should be
independent managed cloud capacity, or when you need VNC/desktop/code, brokered
fleet accounting, or provider firewall control.

## Commands

```sh
crabbox warmup --provider semaphore --semaphore-host example-org.semaphoreci.com --semaphore-project my-app
crabbox run --provider semaphore --semaphore-machine f1-standard-4 -- pnpm test
crabbox ssh --provider semaphore --id swift-crab
crabbox status --provider semaphore --id swift-crab
crabbox stop --provider semaphore swift-crab
```

## Live Smoke

Run a live smoke when changing Semaphore provisioning, API polling, SSH key
retrieval, or release behavior. Keep the token in the environment or user
config; do not pass it as a command-line argument.

```sh
export CRABBOX_SEMAPHORE_HOST=example-org.semaphoreci.com
export CRABBOX_SEMAPHORE_PROJECT=my-app
export CRABBOX_SEMAPHORE_TOKEN=...

go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=semaphore CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

The shared harness exits before any Semaphore `warmup`, `run`, `list`, or `stop`
command when host, project, or token configuration is missing. With those values
configured, it creates one short-lived Semaphore testbox, waits for SSH, verifies
one no-sync command, lists normalized Semaphore inventory, and stops the lease.

For manual debugging, run the same lifecycle directly:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox warmup --provider semaphore --semaphore-idle-timeout 10m
lease=<slug-or-sem_id-from-warmup-output>

bin/crabbox status --provider semaphore --id "$lease" --wait
bin/crabbox run --provider semaphore --id "$lease" --no-sync -- echo crabbox-semaphore-ok
bin/crabbox list --provider semaphore
bin/crabbox stop --provider semaphore "$lease"
```

Expected results:

- `warmup` creates a standalone Semaphore job named `crabbox testbox`, prints a
  `sem_<job-id>` lease ID and a slug, and retrieves the debug SSH key.
- `status --wait` reports a running Linux lease with SSH host details.
- The no-sync run prints `crabbox-semaphore-ok`.
- `list` shows the running Crabbox-managed Semaphore job while it is active.
- `stop` posts the job stop request and removes the local lease claim and key.

## Backend Kind

SSH lease. The provider creates a standalone Semaphore job, polls until the job
is `RUNNING` with an agent IP and an `ssh` port, then fetches the debug SSH key
and returns a standard SSH `LeaseTarget` (user `semaphore`). Crabbox handles all
sync and command execution over that SSH connection.

## Configuration

```yaml
provider: semaphore
semaphore:
  host: example-org.semaphoreci.com   # required; host name, not an API URL
  # Prefer CRABBOX_SEMAPHORE_TOKEN / SEMAPHORE_API_TOKEN over committing a token.
  token: ...                          # required (config or environment)
  project: my-app                     # required
  machine: f1-standard-2              # optional, default f1-standard-2
  osImage: ubuntu2204                 # optional, default ubuntu2204
  idleTimeout: 30m                    # optional, default 30m (Go duration)
```

Flags: `--semaphore-host`, `--semaphore-project`, `--semaphore-machine`,
`--semaphore-os-image`, `--semaphore-idle-timeout`.

Environment variables:

```text
CRABBOX_SEMAPHORE_HOST          (or SEMAPHORE_HOST)
CRABBOX_SEMAPHORE_TOKEN         (or SEMAPHORE_API_TOKEN)
CRABBOX_SEMAPHORE_PROJECT       (or SEMAPHORE_PROJECT)
CRABBOX_SEMAPHORE_MACHINE
CRABBOX_SEMAPHORE_OS_IMAGE
CRABBOX_SEMAPHORE_IDLE_TIMEOUT
```

`CRABBOX_*` values win over their `SEMAPHORE_*` equivalents, which win over
config-file values. Generate a token at `https://<host>/me/api-tokens`. For
machine types see the [Semaphore machine types reference](https://docs.semaphore.io/reference/machine-types).

## Lifecycle

1. Resolve the project name to an ID (`GET /api/v1alpha/projects/<name>`, with a
   paginated list fallback).
2. `POST /api/v1alpha/jobs`: create a `crabbox testbox` job whose single command
   prepares `/work/crabbox`, prints `crabbox-testbox-ready`, then sleeps for the
   idle timeout (keepalive).
3. Poll `GET /api/v1alpha/jobs/:id` (up to ~4 minutes) until the job is `RUNNING`
   and exposes an agent IP plus an `ssh` port.
4. `GET /api/v1alpha/jobs/:id/debug_ssh_key`: fetch the SSH key and store it
   under the lease's per-box key path.
5. Crabbox syncs and runs commands over SSH.
6. `POST /api/v1alpha/jobs/:id/stop`: release the job; the local claim and stored
   key are removed.

## Capabilities

- SSH: yes.
- Crabbox sync: yes, standard SSH/rsync path.
- Desktop / browser / code: no.
- Actions hydration: no.
- Coordinator (broker): no — always direct from the CLI.

## Limitations

- Linux only.
- No coordinator/broker integration.
- No VNC, desktop, or code-server.
- `--type` is ignored; choose capacity with `semaphore.machine` or
  `--semaphore-machine`.
- Idle timeout is enforced by the in-job keepalive sleep, not a heartbeat;
  `crabbox` touch is a no-op for this provider.
- Cleanup depends on the Semaphore job stop endpoint.

## Gotchas

- `host` must be the Semaphore organization host (for example
  `example-org.semaphoreci.com`), not an API URL; URLs with a path, query, or
  fragment are rejected.
- The token must belong to that host and have access to the configured project.
  A `401 Unauthorized` usually means the token and host do not match.
- `idleTimeout` uses Go duration syntax (`30m`, `1h`); it must be positive.
- Only jobs named `crabbox testbox` are treated as Crabbox-managed by `list`,
  `status`, and `resolve`.
- Local claims are provider-scoped: a slug claimed by another provider does not
  resolve as a Semaphore job. Resolve a lease by its full `sem_<job-id>` ID or by
  a slug from a recent warmup.

## Related Docs

- [Feature: Semaphore](../features/semaphore.md)
- [Provider backends](../provider-backends.md)
