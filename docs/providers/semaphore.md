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

## Backend kind

SSH lease. Provisions a standalone Semaphore job, retrieves SSH credentials via
the debug SSH key API, returns a standard `LeaseTarget`.

## Configuration

```yaml
provider: semaphore
semaphore:
  host: myorg.semaphoreci.com     # required
  token: ...                       # required
  project: my-app                  # required
  machine: f1-standard-2           # optional, default: f1-standard-2
  osImage: ubuntu2204              # optional, default: ubuntu2204
  idleTimeout: 30m                 # optional, default: 30m
```

Flags: `--semaphore-host`, `--semaphore-token`, `--semaphore-project`,
`--semaphore-machine`, `--semaphore-os-image`, `--semaphore-idle-timeout`.

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
