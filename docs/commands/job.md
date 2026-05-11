# job

Run named repo-local Crabbox jobs from `crabbox.yaml` or `.crabbox.yaml`.

Jobs are generic orchestration. Crabbox owns lease creation, optional GitHub
Actions hydration, dirty checkout sync, command execution, timing/log output,
and cleanup. The repo owns the actual command, package-manager setup, test
environment variables, and workflow names.

## Usage

```sh
crabbox job list
crabbox job run <name>
crabbox job run --dry-run <name>
crabbox job run --id <lease-or-slug> <name>
crabbox job run --stop never <name>
```

`job run` creates a lease when `--id` is omitted. For created leases the default
cleanup policy is `auto`, which stops the lease after the job. For existing
leases, `auto` leaves the lease running.

Stop policies:

- `auto`: stop created leases, keep existing leases.
- `always`: stop after success or failure.
- `success`: stop only after success.
- `failure`: stop only after failure.
- `never`: do not stop.

## Config

```yaml
jobs:
  openclaw-wsl2:
    provider: aws
    target: windows
    windows:
      mode: wsl2
    class: beast
    market: on-demand
    idleTimeout: 240m
    hydrate:
      actions: true
      waitTimeout: 45m
      keepAliveMinutes: 240
    actions:
      workflow: hydrate.yml
      job: hydrate
    shell: true
    command: >
      corepack enable &&
      pnpm install --frozen-lockfile &&
      CI=1 NODE_OPTIONS=--max-old-space-size=4096 pnpm test
    stop: always
```

Supported job fields:

- `provider`, `target`, `windows.mode`, `profile`, `class`, `type` or `serverType`.
- `market` or `capacity.market`.
- `ttl`, `idleTimeout`, `desktop`, `browser`, `code`, `network`.
- `hydrate.actions`, `hydrate.waitTimeout`, `hydrate.keepAliveMinutes`.
- `actions.repo`, `actions.workflow`, `actions.job`, `actions.ref`, `actions.fields`.
- `shell`, `command`, `noSync`, `syncOnly`, `checksum`, `forceSyncLarge`, `junit`, `downloads`.
- `stop`: `auto`, `always`, `success`, `failure`, or `never`.

Use `--dry-run` to inspect the exact `warmup`, `actions hydrate`, `run`, and
`stop` commands before spending cloud capacity.
