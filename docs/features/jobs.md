# Jobs

Jobs are named, repo-local validation flows. A repository describes a repeatable
remote run once in config, then anyone with the repo checked out runs it with a
single command:

```sh
crabbox job run <name>
```

A job expands into the same primitives you would invoke by hand — `warmup`,
`actions hydrate`, `run`, and `stop` — with all the routing and run options
filled in from config. Reach for a job when a flow needs more than one plain
`crabbox run` invocation, especially when it must warm a lease and run Actions
hydration before the actual command.

The split of responsibilities is deliberate. Crabbox owns the cloud lease
lifecycle, optional Actions hydration, dirty-checkout sync, command execution,
timing and log output, and cleanup. The repository owns the command string,
package-manager setup, test environment, workflow names, and any
project-specific parallelism.

## Boundary

Belongs in a Crabbox job:

- provider, target OS, architecture, Windows mode, profile, class/type, market, network;
- lease TTL, idle timeout, and stop policy;
- whether to run Actions hydration and how long to wait for it;
- the remote command and whether it runs through a shell;
- command-adjacent options such as labels, JUnit paths, required artifacts,
  artifact globs, downloads, and sync flags.

Belongs in the repository command or workflow:

- `pnpm install`, `npm ci`, `go test`, or similar dependency and build setup;
- service startup, database setup, and secrets;
- test sharding and project-specific environment variables;
- anything tied to a particular package-manager lockfile.

## Listing jobs

```sh
crabbox job list
```

Each line prints the job name plus its `provider`, `target`,
`hydrate_actions`, and resolved `stop` policy. With no jobs configured it prints
`no jobs configured`.

## Example

```yaml
jobs:
  test-live:
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

Preview the planned commands without touching a box:

```sh
crabbox job run --dry-run test-live
```

Run it:

```sh
crabbox job run test-live
```

Reuse an existing lease instead of warming a new one:

```sh
crabbox job run --id swift-crab test-live
```

## Lifecycle

`crabbox job run` expands the job into ordinary Crabbox commands and runs them in
order:

1. `warmup` creates a lease when `--id` is omitted (it is reused when supplied).
2. `actions hydrate` runs when `hydrate.actions: true` and `--no-hydrate` is not
   set.
3. `run` syncs the local checkout and executes the configured command.
4. `stop` runs according to the stop policy.

When the job omits `hydrate.actions`, that is an explicit opt-out: the nested
`run` is passed `--no-hydrate` so a global `actions.workflow` does not
auto-hydrate unexpectedly.

Set `hydrate.githubRunner: true`, or pass `crabbox job run --github-runner`, when
the hydrate workflow needs repository secrets, OIDC, services, job containers, or
other Actions features that local SSH execution cannot reproduce. This registers
the box as a GitHub self-hosted runner instead of driving the workflow over SSH.

### Stop policy

The default policy is `auto`. Override it per run with `--stop`. Behavior
depends on whether the job warmed a new lease or reused one passed with `--id`:

| Policy    | New lease (no `--id`)          | Existing lease (`--id`)        |
| --------- | ------------------------------ | ------------------------------ |
| `auto`    | stop after the run             | leave running                  |
| `always`  | stop after success or failure  | stop after success or failure  |
| `success` | stop only after success        | stop only after success        |
| `failure` | stop only after failure        | stop only after failure        |
| `never`   | leave running                  | leave running                  |

A new lease created by the job is stopped under `auto`; a lease you supplied is
left running so you can keep iterating against it.

## Config fields

A job requires either a `command` or `syncOnly: true`; otherwise it fails to
validate.

### Target and capacity

```yaml
provider: aws
target: windows        # alias: targetOS
windows:
  mode: wsl2           # normal | wsl2; sets --windows-mode
profile: project-check
class: beast
architecture: amd64    # amd64 | arm64; arm64 supports Linux on AWS/Azure/Apple Container and native Windows on Azure
type: m8i.4xlarge      # alias: serverType
market: on-demand      # alias: capacity.market
network: auto
ttl: 6h
idleTimeout: 240m
desktop: false
desktopEnv: xfce       # xfce | wayland | gnome
browser: false
code: false
```

`desktop`, `browser`, and `code` are tri-state: omit them to leave the default,
or set `true`/`false` to force the capability on or off for the lease.

### Hydration

```yaml
hydrate:
  actions: true
  githubRunner: false
  waitTimeout: 45m
  keepAliveMinutes: 240
actions:
  repo: example-org/my-app
  workflow: hydrate.yml
  job: hydrate
  ref: main
  fields:
    - suite=full
```

Each entry under `actions.fields` is passed to `actions hydrate` as a
`--field key=value` workflow input.

### Run options

```yaml
shell: true
command: pnpm test
noSync: false
syncOnly: false
checksum: false
forceSyncLarge: false
junit:
  - reports/junit.xml
label: nightly smoke
artifactGlobs:
  - reports/**
requiredArtifacts:
  - reports/summary.json
downloads:
  - out/report.json=artifacts/report.json
stop: auto
```

With `shell: true` the command is passed to a remote shell verbatim (so `&&`,
pipes, and environment assignments work). Without it, the command is split on
whitespace and executed directly. `artifactGlobs` and `requiredArtifacts`
forward to the matching repeatable `run` flags. Each `downloads` entry is a
`remote=local` pair pulled back after the run.

## Related

- [job command](../commands/job.md)
- [Configuration](configuration.md)
- [Actions hydration](actions-hydration.md)
- [Sync](sync.md)
- [Run command](../commands/run.md)
</content>
</invoke>
