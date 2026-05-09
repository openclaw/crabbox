# Provider: Semaphore

Read when:

- choosing `provider: semaphore`;
- configuring Semaphore CI testboxes, API auth, machine types, or OS images;
- changing `internal/providers/semaphore`.

Semaphore is an SSH lease provider that creates Semaphore CI jobs as testbox
environments via the Semaphore REST API. Crabbox handles sync and command
execution over SSH.

## When To Use

Use Semaphore when a repo already depends on Semaphore CI environments,
project secrets, caches, or machine images and you want a Crabbox lease that
matches that CI context. Use AWS, Azure, Hetzner, or Static SSH when the box
should be independent managed cloud capacity, or when VNC/desktop/code
workflows are required.

## Commands

```sh
crabbox warmup --provider semaphore --semaphore-host myorg.semaphoreci.com --semaphore-project my-app
crabbox run --provider semaphore -- pnpm test
crabbox ssh --provider semaphore --id blue-lobster
crabbox status --provider semaphore --id blue-lobster
crabbox stop --provider semaphore blue-lobster
```

## Live Smoke

Use a live smoke when changing Semaphore provisioning, API polling, SSH key
retrieval, or release behavior. Keep the token in the environment or user
config; do not pass it as a command-line argument.

```sh
export CRABBOX_SEMAPHORE_HOST=myorg.semaphoreci.com
export CRABBOX_SEMAPHORE_PROJECT=my-app
export CRABBOX_SEMAPHORE_TOKEN=...

go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox warmup --provider semaphore --semaphore-idle-timeout 10m
lease=<slug-or-sem_id-from-warmup-output>

bin/crabbox status --provider semaphore --id "$lease" --wait
bin/crabbox run --provider semaphore --id "$lease" --no-sync -- echo crabbox-semaphore-ok
bin/crabbox list --provider semaphore
bin/crabbox stop --provider semaphore "$lease"
```

Expected results:

- `warmup` creates a standalone Semaphore job, prints a `sem_...` lease ID and
  slug, and retrieves the debug SSH key.
- `status --wait` reports a running Linux lease with SSH host details.
- The no-sync run prints `crabbox-semaphore-ok`.
- `list` shows the running Crabbox-managed Semaphore job while it is active.
- `stop` posts the job stop request and removes the local lease claim and key.

## Backend kind

SSH lease. Provisions a standalone Semaphore job, retrieves SSH credentials via
the debug SSH key API, returns a standard `LeaseTarget`.

## Configuration

```yaml
provider: semaphore
semaphore:
  host: myorg.semaphoreci.com     # required
  # Prefer CRABBOX_SEMAPHORE_TOKEN or SEMAPHORE_API_TOKEN.
  # Use user config only; do not commit tokens in repo config.
  token: ...                       # required unless provided by env
  project: my-app                  # required
  machine: f1-standard-2           # optional, default: f1-standard-2
  osImage: ubuntu2204              # optional, default: ubuntu2204
  idleTimeout: 30m                 # optional, default: 30m
```

Flags: `--semaphore-host`, `--semaphore-project`, `--semaphore-machine`,
`--semaphore-os-image`, `--semaphore-idle-timeout`.

Environment variables:

```text
CRABBOX_SEMAPHORE_HOST
CRABBOX_SEMAPHORE_TOKEN
CRABBOX_SEMAPHORE_PROJECT
CRABBOX_SEMAPHORE_MACHINE
CRABBOX_SEMAPHORE_OS_IMAGE
CRABBOX_SEMAPHORE_IDLE_TIMEOUT
SEMAPHORE_HOST
SEMAPHORE_API_TOKEN
SEMAPHORE_PROJECT
```

Token: `https://<host>/me/api-tokens`

Machine types: see [Semaphore docs](https://docs.semaphore.io/reference/machine-types).

## Lifecycle

1. `POST /api/v1alpha/jobs` — create job with keepalive script
2. Poll `GET /api/v1alpha/jobs/:id` until `RUNNING`
3. `GET /api/v1alpha/jobs/:id/debug_ssh_key` — retrieve SSH key
4. Crabbox syncs + runs over SSH
5. `POST /api/v1alpha/jobs/:id/stop` — release

## Limitations

- Linux only.
- No coordinator integration.
- No VNC/desktop.
