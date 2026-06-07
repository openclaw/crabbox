# init

`crabbox init` onboards the current repository: it writes the minimal config
that `crabbox run` needs, plus an optional GitHub Actions hydration workflow and
an agent skill file. Run it once from the repo root.

```sh
crabbox init
crabbox init --detect
crabbox init --force
crabbox init --workflow .github/workflows/crabbox-test.yml
```

## Files it writes

`init` generates three files, all relative to the repository root:

```text
.crabbox.yaml                        repo defaults (profile, class, capacity, sync, env, ssh)
.github/workflows/crabbox.yml        Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md      agent-facing skill instructions
```

Each path is configurable with the `--config`, `--workflow`, and `--skill`
flags. By default `init` refuses to overwrite an existing file: if any target
path already exists it stops with an error and writes nothing further. Pass
`--force` to regenerate and overwrite the files in place.

## `.crabbox.yaml`

The generated config is a starting template that includes:

- `profile: <repo-name>-check` and `class: beast`;
- a `capacity` block requesting `spot` with a `most-available` strategy and
  `on-demand-after-120s` fallback;
- an `actions` block wired to the generated workflow (`job: hydrate`,
  `runnerLabels: [crabbox]`, `runnerVersion: latest`, `ephemeral: true`);
- a `sync` block with sane defaults: `delete`, `gitSeed`, and `fingerprint`
  enabled, a 15m timeout, warn/fail thresholds on file count and bytes, and an
  `exclude` list covering `.cache`, `.turbo`, `dist`, and `node_modules`;
- `env.allow` with conservative defaults (`CI`, `NODE_OPTIONS`);
- an `ssh` block (`user: crabbox`, `port: "2222"`, `fallbackPorts: ["22"]`).

Open the file after `init` and adjust it to match the repo:

- pick the right `class` for the workload;
- add repo-specific `sync.exclude` patterns;
- expand `env.allow` for project-specific tunables;
- set `sync.baseRef` if the project's default branch is not the rsync base.

See [Configuration](../features/configuration.md) for the full schema.

### Detection (`--detect`)

With `--detect`, `init` scans common project markers and, when it can infer a
broad check, writes a `jobs.detected` entry (a shell job with `stop: auto`) plus
matching `run.preflightTools`. Detection is intentionally conservative:

- `go.mod` adds `go test ./...`, including nested modules (each in its own
  subshell);
- `package.json` uses the first present script among `test:ci`, `test`,
  `check`, or `build` (skipping the npm placeholder "no test specified"), with
  an install + run command picked from the lockfile or `packageManager` field
  for npm, pnpm, Yarn, or Bun. Nested packages without their own lockfile are
  skipped;
- `Cargo.toml` adds `cargo test` and excludes `target`;
- a root `Makefile` (or `makefile`) with a `test` target adds `make test`.

The generated job is plain repo-local YAML. Edit it when the real project check
needs services, secrets, sharding, or a narrower command. If nothing matches,
`init` prints a note and leaves the jobs section out — define jobs by hand.

## `.github/workflows/crabbox.yml`

The generated workflow is a conservative starting point for repo-specific
hydration, not a full replacement for CI. Edit it to install dependencies, start
service containers, and warm caches before agents begin repeated `crabbox run`
calls.

The workflow follows the contract used by [`crabbox actions hydrate`](../features/actions-hydration.md):

- it is `workflow_dispatch`-triggered and accepts the Crabbox lease ID
  (`crabbox_id`), a dynamic runner label (`crabbox_runner_label`), the
  hydration job identifier (`crabbox_job`, default `hydrate`), and a keep-alive
  window (`crabbox_keep_alive_minutes`, default 90);
- the `hydrate` job runs on `[self-hosted, <runner label>]`;
- it writes a ready marker and env snapshot under `$HOME/.crabbox/actions`;
- a final step keeps the job alive until the deadline or a stop marker appears
  (used for the self-hosted GitHub-runner fallback).

If the repo has no Actions hydration plans, delete the workflow. `crabbox run`
works fine without it; hydration is optional.

## `.agents/skills/crabbox/SKILL.md`

Repo-local agent instructions. The generated skill explains how an agent should
operate Crabbox in this repo: warm a box early, reuse the returned slug for
interactive checks while keeping the `cbx_` id in scripts/logs, run checks with
`crabbox run --id <slug> -- <command>`, inspect with `crabbox ssh`, and stop with
`crabbox stop <slug>` when finished. When `--detect` finds a check, the skill
also points agents at `crabbox job run detected`.

Edit this file to match how you want agents to operate in the repo. The skill is
read by OpenClaw and similar agent runtimes that auto-discover `.agents/skills/`.

## Flags

```text
--force             overwrite generated files
--detect            detect repo test commands and write a jobs.detected entry
--config <path>     repo config path (default .crabbox.yaml)
--workflow <path>   Actions workflow path (default .github/workflows/crabbox.yml)
--skill <path>      agent skill path (default .agents/skills/crabbox/SKILL.md)
```

## Re-running

Without `--force`, `init` will not touch existing files: it stops at the first
target that already exists and reports it, so an established repo is never
silently overwritten. With `--force`, it regenerates all three files.

## After init

```sh
crabbox doctor              # validate the config
crabbox sync-plan           # preview what would sync
crabbox warmup              # acquire a lease
crabbox run -- pnpm test    # run a command
crabbox job run detected    # run the detected job, when generated
```

Related docs:

- [Configuration](../features/configuration.md)
- [Repository onboarding](../features/repository-onboarding.md)
- [Actions hydration](../features/actions-hydration.md)
- [Sync](../features/sync.md)
- [Getting started](../getting-started.md)
