# job

Run named, repo-local jobs defined in your Crabbox config.

A job bundles lease routing, optional GitHub Actions hydration, a command, and a
cleanup policy under one name, then expands into the equivalent `warmup`,
`actions hydrate`, `run`, and `stop` commands at runtime. Crabbox owns lease
lifecycle, dirty-checkout sync, execution, timing/log output, and cleanup; the
repository owns the command string, toolchain setup, test environment, and
workflow names.

For the concept and authoring guidance, see [jobs](../features/jobs.md).

## Subcommands

| Command | Description |
| --- | --- |
| `crabbox job list` | List configured jobs and their key routing fields. |
| `crabbox job run <name>` | Run a configured job. |

Running `crabbox job` with no subcommand is equivalent to `crabbox job list`.

## job list

```sh
crabbox job list
```

Prints each configured job, sorted by name, with its `provider`, `target`,
`hydrate_actions`, and `stop` policy. Prints `no jobs configured` when the
merged config defines no jobs.

## job run

```sh
crabbox job run <name>
crabbox job run --dry-run <name>
crabbox job run --id <lease-or-slug> <name>
crabbox job run --stop never <name>
crabbox job run --github-runner <name>
```

`job run` takes exactly one positional argument: the job name. The job must
exist in the merged config and must define a `command` (or set `syncOnly`).

When `--id` is omitted, `job run` first creates a lease (via `warmup --keep`)
using the job's routing fields, then runs the job against it. When `--id` is
supplied, the job runs against that existing lease.

### Flags

| Flag | Description |
| --- | --- |
| `--id <lease-or-slug>` | Run against an existing lease instead of creating one. |
| `--no-hydrate` | Skip configured Actions hydration for this run. |
| `--github-runner` | Hydrate by registering a GitHub self-hosted runner instead of local SSH execution. Forces `--github-runner` on the nested `actions hydrate` call. |
| `--stop <policy>` | Override the job's stop policy: `auto`, `always`, `success`, `failure`, or `never`. |
| `--dry-run` | Print the planned `crabbox` commands without running them. |

### Stop policies

The stop policy resolves from `--stop` (if set) otherwise the job's `stop`
field, defaulting to `auto`. Behavior depends on whether the lease was created
by this run:

- `auto`: stop leases this run created; leave existing (`--id`) leases running.
- `always`: stop after success or failure.
- `success`: stop only after the command succeeds.
- `failure`: stop only after the command fails.
- `never`: never stop.

### Dry run

`--dry-run` prints the exact `warmup`, `actions hydrate`, `run`, and `stop`
commands the job expands to, so you can review routing and flags before
spending cloud capacity. Created leases show as a `<lease>` placeholder.

## Configuration

Jobs live under the `jobs` key in any merged config source (the user config, or
`crabbox.yaml` / `.crabbox.yaml` in the repo). Each job is keyed by its name:

```yaml
jobs:
  test-wsl2:
    provider: aws
    target: windows
    architecture: amd64
    windows:
      mode: wsl2
    class: beast
    market: on-demand
    idleTimeout: 240m
    hydrate:
      actions: true
      githubRunner: false
      waitTimeout: 45m
      keepAliveMinutes: 240
    actions:
      workflow: hydrate.yml
      job: hydrate
    shell: true
    command: >
      corepack enable &&
      pnpm install --frozen-lockfile &&
      CI=1 pnpm test
    stop: always
```

### Supported fields

Routing and lease creation:

- `provider`, `target` (or `targetOS`), `windows.mode`, `profile`, `class`, `architecture`.
- `type` (or `serverType`), `market` (or `capacity.market`).
- `ttl`, `idleTimeout`, `desktop`, `desktopEnv`, `browser`, `code`, `network`.

Actions hydration:

- `hydrate.actions`, `hydrate.githubRunner`, `hydrate.waitTimeout`, `hydrate.keepAliveMinutes`.
- `actions.repo`, `actions.workflow`, `actions.job`, `actions.ref`, `actions.fields` (each `fields` entry maps to a `--field key=value` arg).

Command and sync:

- `shell`, `command`, `noSync`, `syncOnly`, `checksum`, `forceSyncLarge`,
  `junit`, `label`, `artifactGlobs`, `requiredArtifacts`, `downloads`.
- `stop`: `auto`, `always`, `success`, `failure`, or `never`.

### Command execution

With `shell: true` the `command` string runs through a remote shell unchanged,
so `&&`, pipes, and shell variables work. Without `shell`, the command is split
on whitespace into argv and run without a shell, so shell operators are not
interpreted — set `shell: true` for any multi-step command.

### Hydration opt-out

When `hydrate.actions` is omitted or `false`, `job run` passes `--no-hydrate` to
the nested `run`, keeping the opt-out effective even when a global profile
configures `actions.workflow`. Passing `--no-hydrate` on the command line
applies the same opt-out for a single run.
