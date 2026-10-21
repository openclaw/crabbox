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
| `crabbox job submit --id <lease> --job-id <job> -- <command...>` | Submit a detached command to an existing Linux SSH lease. |
| `crabbox job status --id <lease> --job-id <job>` | Read JSON status without interpreting SSH errors as workload exits. |
| `crabbox job logs --id <lease> --job-id <job> [--lines 100]` | Read a bounded tail of combined stdout and stderr. |
| `crabbox job push --id <lease> <local-path> <relative-path>` | Upload one workspace path while holding workspace ownership. |
| `crabbox job pull --id <lease> <local-path> <relative-path>` | Download one workspace path while holding workspace ownership. |

Running `crabbox job` with no subcommand is equivalent to `crabbox job list`.

## Detached commands on existing hosts

A detached command continues on the host after its initiating SSH connection
closes. Unlike named `job run`, these commands require an existing lease, never
provision or release a machine, and do not apply named-job configuration.
The first implementation supports native Linux SSH targets only. Lease expiry,
provider shutdown, host reboot, and operator cleanup can still interrupt a job;
detachment is not a lifetime extension or a restart service.

Prepare the ordinary repository workspace with `run --sync-only`, then submit:

```sh
crabbox run --id buildbox --no-hydrate --sync-only
crabbox job submit --id buildbox --job-id test-001 -- sh -c 'make test'
crabbox job status --id buildbox --job-id test-001
crabbox job logs --id buildbox --job-id test-001 --lines 50
crabbox job pull --id buildbox ./results results
```

Submission runs the exact command arguments in the ordinary repository workspace
(`workRoot/leaseID/repositoryName`). Use explicit `sh -c` for shell syntax.
No local environment values, named-job hooks, hydration, automatic artifact
collection, or machine cleanup are implied. Commands must remain in the
foreground and wait for their children; do not submit a daemonizing command.

IDs contain 1–64 letters, digits, underscores or hyphens, starting with a letter
or digit. Choose the ID **before** submission. Reusing an ID is refused even
after completion. If submission loses its acknowledgment, inspect the same ID:
do not invent another ID and retry. A successful submission reports
`{"id":"test-001","state":"submitted"}`, not successful workload completion.

Status reports `running` with a process ID (PID), `exited` with that PID and an
`exitCode`, `missing` when no record exists, or `unknown` when no durable exit
record proves completion and the recorded process cannot be confirmed. Process
identity includes its start time, not just its reusable PID. Even workload exit
255 is a normal `exited` observation. A successful status read exits zero;
automation must inspect `exitCode`. An SSH or malformed-record failure exits 7
and does **not** report that the workload failed. Status and logs have a
one-minute operation limit; log reads accept 1–10000 lines and do not follow
indefinitely. Repeat these reads with a caller-chosen deadline to monitor a job.

### Routing and transfers

These commands reuse resolved lease SSH transport, including existing external
provider `sshConfigProxy` aliases and `proxyCommand` routes; no second SSH
configuration format is introduced. See [external SSH routing](../providers/external.md)
and [SSH transport](../features/ssh-transport.md). Configure destination and
credential approval in trusted user configuration as those contracts require.

`push` and `pull` take the local operand **first** in both directions. The
remote operand is relative to the repository workspace. Absolute paths,
parent traversal, `.git`, `.crabbox`, and remote symlink traversal are refused.
Both operations use the existing [cp transport](cp.md): incremental rsync when
available, otherwise validated archive replacement of the selected subtree.
They do not delete the rest of the workspace. Explicit uploads copy the selected
operand, not Git's tracked/non-ignored manifest; review its contents before
uploading. Use `run --sync-only` for ordinary repository-aware source sync.

### Ownership and interrupted operations

Jobs and transfers share `run`'s remote workspace owner. Its existing scope is
one lease, deliberately serializing all repository workspaces within that lease.
Different leases use distinct workspace paths. Do not alias those paths through
symlinks or use different lease identities for the same workspace. This is
cooperative collision protection, not isolation from direct SSH commands or
other operating-system accounts. Ordinary `cp` does not take this reservation;
use `job push`/`job pull` for transfers that must exclude running jobs.

A persistent reservation prevents automatic takeover after an ambiguous
dispatch, lost transfer connection, missing supervisor, or expired local owner.
It is removed only after observed transfer success or completed foreground
execution and witness cleanup. A recorded workload exit can precede that cleanup;
it does not by itself prove that the reservation was released.

Job records and logs remain under `~/.crabbox/jobs/` on the host. A crash before
dispatch or during cleanup can leave the workspace reserved indefinitely.
Inspect the job's `supervisor.log`, the exact workspace-owner record, and host
processes before any manual recovery. Do not remove ownership state based only
on elapsed time, an unreachable host, or an absent PID. This release does not
provide automatic recovery, cancellation, or garbage collection for detached
jobs; preserve records until results have been recovered and ownership resolved.

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

The selected job's provider and platform fields apply before inherited defaults
are validated, just as they do in its generated child commands. This also applies
to `--dry-run`. An existing lease must still match explicit platform assertions;
job routing cannot replace a lease's recorded identity.

Blacksmith Testbox does not support `noSync: true`. Such jobs exit 2 before
warmup, hydration, run, or stop, without generating keys or changing claims.
This also applies to `--dry-run` and existing leases. Omit `noSync` (or set it
to `false`) to let Blacksmith manage source sync, or choose a provider that
supports skipping sync. Job provider overrides and existing-lease routing
determine which provider's policy applies.

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
