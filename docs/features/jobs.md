# Jobs

Crabbox jobs are named repo-local workflows. They let a repository describe a
repeatable remote validation flow once, then run it with:

```sh
crabbox job run <name>
```

Jobs are intentionally generic. Crabbox owns cloud lease lifecycle, optional
GitHub Actions hydration, dirty checkout sync, command execution, timing/log
output, and cleanup. The repository owns the command string, package-manager
setup, test environment variables, workflow names, and project-specific
parallelism.

Use jobs when a flow needs more than a single `crabbox run` command, especially
when it needs a warmed lease plus Actions hydration before the actual command.

## Boundary

Belongs in a Crabbox job:

- provider, target OS, Windows mode, class/type, market, network;
- lease timeout and stop policy;
- whether to run Actions hydration and how long to wait;
- the remote command and whether it should run through the shell;
- command-adjacent options such as JUnit paths, downloads, and sync flags.

Belongs in the repository command or workflow:

- `pnpm install`, `npm ci`, `go test`, `xcodebuild`, or similar setup;
- service startup, database setup, and secrets;
- test sharding and project-specific environment variables;
- any dependency on a particular package-manager lockfile.

## Example

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
      CI=1 NODE_OPTIONS=--max-old-space-size=4096
      OPENCLAW_TEST_PROJECTS_PARALLEL=6
      OPENCLAW_VITEST_MAX_WORKERS=1
      pnpm test
    stop: always
```

Inspect before running:

```sh
crabbox job run --dry-run openclaw-wsl2
```

Then run:

```sh
crabbox job run openclaw-wsl2
```

Reuse an existing lease:

```sh
crabbox job run --id blue-lobster openclaw-wsl2
```

## Lifecycle

`crabbox job run` expands a job into normal Crabbox commands:

1. `warmup` creates a lease when `--id` is omitted.
2. `actions hydrate` runs when `hydrate.actions: true`.
3. `run` syncs the local checkout and executes the configured command.
4. `stop` runs according to the stop policy.

The default stop policy is `auto`:

- created leases are stopped after the job;
- existing leases passed with `--id` are left running.

Other policies:

- `always`: stop after success or failure.
- `success`: stop only after success.
- `failure`: stop only after failure.
- `never`: leave the lease running.

## Config Fields

Target and capacity:

```yaml
provider: aws
target: windows
windows:
  mode: wsl2
profile: project-check
class: beast
type: m8i.4xlarge
market: on-demand
network: auto
ttl: 6h
idleTimeout: 240m
desktop: false
browser: false
code: false
```

Hydration:

```yaml
hydrate:
  actions: true
  waitTimeout: 45m
  keepAliveMinutes: 240
actions:
  repo: owner/name
  workflow: hydrate.yml
  job: hydrate
  ref: main
  fields:
    - suite=full
```

Run options:

```yaml
shell: true
command: pnpm test
noSync: false
syncOnly: false
checksum: false
forceSyncLarge: true
junit:
  - reports/junit.xml
downloads:
  - out/report.json=artifacts/report.json
stop: auto
```

## Related

- [job command](../commands/job.md)
- [Configuration](configuration.md)
- [Actions hydration](actions-hydration.md)
- [Sync](sync.md)
- [Run command](../commands/run.md)
