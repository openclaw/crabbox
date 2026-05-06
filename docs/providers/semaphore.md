# Provider: Semaphore

SSH lease provider that creates Semaphore CI jobs as testbox environments via
the Semaphore REST API. Crabbox handles sync and command execution over SSH.

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
