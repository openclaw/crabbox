# Repository Onboarding

Read this when:

- changing `crabbox init`;
- adding or renaming repo-local config keys;
- changing the generated workflow stub or agent instructions.

`crabbox init` prepares a repository for Crabbox so contributors and agents do not have to
memorize the full command surface. It writes a small set of repo-local files, then leaves
project-specific behavior in version control where it can be reviewed like any other config.

## What it generates

`crabbox init` writes three files (paths are overridable; defaults shown):

```text
.crabbox.yaml                       # --config
.github/workflows/crabbox.yml       # --workflow
.agents/skills/crabbox/SKILL.md     # --skill
```

Each path can be relocated with the matching flag, and existing files are protected: `init`
exits with an error if a target file already exists. Pass `--force` to overwrite. See the
[init command reference](../commands/init.md) for the full flag list.

## Repo-local config (`.crabbox.yaml`)

The generated config holds the project-specific defaults that should travel with the repo:

- `profile` (defaults to `<repo>-check`) and `class` (`beast`);
- `capacity` market/strategy/fallback defaults;
- `actions` hydration settings (workflow path, hydrate job, runner labels, runner version,
  ephemeral flag);
- `sync` options (delete, checksum, gitSeed, fingerprint, timeout) and the size guardrails
  (`warnFiles`/`warnBytes`/`failFiles`/`failBytes`);
- `sync.exclude` paths (seeds `.cache`, `.turbo`, `dist`, `node_modules`, plus any detected
  package-manager caches);
- `env.allow` (seeds `CI` and `NODE_OPTIONS`);
- `ssh` user/port and the ordered `fallbackPorts` list (`["22"]` by default; set `[]` to
  disable fallback).

## Generated agent instructions (`SKILL.md`)

The generated skill follows the open Agent Skills format with required `name`
and `description` frontmatter. It points agents at the warmup/reuse workflow
rather than ad-hoc commands: warm a box early with `crabbox warmup`, reuse the
returned slug for interactive checks, run checks with
`crabbox run --id <slug> -- <command>`, inspect a failure over `crabbox ssh`,
and `crabbox stop <slug>` when finished. It also instructs agents not to debug
product failures on a reused box that fails its sync sanity check: stop it,
warm a fresh box, and rerun.

The default `.agents/skills` location is shared by several coding-agent
clients. Repeat `--skill` during initialization when the repository also needs
a client-specific discovery path. Supplying the flag replaces the implicit
default, so list the `.agents` destination explicitly when retaining it. Each
repository-relative destination must end in `crabbox/SKILL.md`, because the
skill name must match its parent directory. See [AI Agents and
Harnesses](../integrations/agents.md).

## Detection (`--detect`)

`crabbox init --detect` inspects the repository for runnable checks and, when it finds any,
writes a `jobs.detected` entry plus matching `run.preflightTools` so `crabbox run --preflight`
can verify the tools the detected job expects. Run the resulting job with
`crabbox job run detected`.

The detector walks the repo (skipping `.git`, `node_modules`, `dist`, `bin`, `.cache`,
`.turbo`, `.venv`, `target`, and similar) and recognizes:

- **Go modules** (`go.mod`) -> `go test ./...`, preflight tool `go`.
- **Node packages** (`package.json`) -> install + run the first available script among
  `test:ci`, `test`, `check`, `build` (placeholder `test` scripts are ignored). The package
  manager is inferred from the `packageManager` field or lockfile (`npm`, `pnpm`, `yarn`,
  `bun`); preflight tools and cache excludes are set accordingly.
- **Rust crates** (`Cargo.toml`) -> `cargo test`, preflight tool `cargo`, excludes `target`.
- **Makefiles** with a root `test` target -> `make test`, preflight tool `make`.

Each detected command is scoped to its manifest directory, so monorepos with packages in
subdirectories get correctly nested commands. When nothing runnable is found, `init` reports
that and leaves the `jobs` section for you to fill in manually.

## How onboarding ties into hydration

The generated `.github/workflows/crabbox.yml` is the bridge to Actions-backed hydration. By
default Crabbox runs supported setup steps locally; the self-hosted GitHub runner path is used
when full Actions semantics are required. The stub exposes a `hydrate` job triggered by
`workflow_dispatch`, which checks out the requested ref, installs dependencies, writes the
hydration state file (`$HOME/.crabbox/actions/<lease-id>.env`) that Crabbox polls for
readiness, and keeps the job alive for the requested window. See
[Actions hydration](actions-hydration.md) for the runtime flow.

## Related docs

- [init command](../commands/init.md)
- [Actions hydration](actions-hydration.md)
- [Sync](sync.md)
- [Jobs](jobs.md)
- [CLI](../cli.md)
