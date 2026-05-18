# init

`crabbox init` onboards a repository for agent-first remote verification.
It writes the minimum config needed for `crabbox run` and sets up the
optional Actions hydration bridge and agent skill.

```sh
crabbox init
crabbox init --force
crabbox init --workflow .github/workflows/crabbox-test.yml
```

## Files It Writes

```text
.crabbox.yaml                          repo defaults (provider, profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration stub (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

By default `init` will not overwrite existing files. `--force` overrides
that and replaces them with freshly generated content.

## `.crabbox.yaml`

A starting template that includes:

- a default `profile` and `class`;
- `sync.exclude` covering common heavy directories;
- `env.allow` with conservative defaults (`CI`, `NODE_OPTIONS`,
  `PROJECT_*`);
- `actions.workflow` pointing at the generated workflow stub;
- `cache` toggles for pnpm, npm, docker, and git.

Open the file after `init` and adjust it to match the repo:

- pick the right `class` for the workload;
- add repo-specific `sync.exclude` patterns;
- expand `env.allow` for project-specific tunables;
- pin `sync.baseRef` to the project's default branch.

See [Configuration](../features/configuration.md) for the full schema.

## `.github/workflows/crabbox.yml`

The generated workflow is intentionally conservative. It is a starting
point for repo-specific hydration, not a full replacement for CI. Edit it
to install dependencies, start service containers, and warm caches before
agents begin repeated `crabbox run` calls.

The workflow contract is the one used by `crabbox actions hydrate`:

- accepts the Crabbox lease ID and dynamic runner label;
- runs locally over SSH by default, or on the self-hosted runner when `--github-runner` is used;
- writes a ready marker under `$HOME/.crabbox/actions`;
- keeps the job alive only for the GitHub-runner fallback.

If the repo has no Actions hydration plans, you can delete the workflow.
`crabbox run` works fine without it - hydration is optional.

## `.agents/skills/crabbox/SKILL.md`

Repo-local agent instructions. The generated skill explains:

- when to use Crabbox vs running locally;
- how to acquire and reuse leases;
- which commands the agent should prefer (`warmup`, `run --id`, `stop`);
- what env vars the project allows;
- where to find repo-specific test commands.

Edit this file to match how you want agents to operate in the repo. The
skill is read by OpenClaw and similar agent runtimes that auto-discover
`.agents/skills/`.

## Flags

```text
--force                 overwrite generated files
--config <path>         repo config path (default ./.crabbox.yaml)
--workflow <path>       Actions workflow path (default .github/workflows/crabbox.yml)
--skill <path>          agent skill path (default .agents/skills/crabbox/SKILL.md)
```

## Idempotency

`init` is safe to re-run. Without `--force`, it leaves existing files
alone and exits with a summary of what would be created. With `--force`,
it replaces files atomically.

## After Init

```sh
crabbox doctor              # validate the config
crabbox sync-plan           # preview what would sync
crabbox warmup              # acquire a lease
crabbox run -- pnpm test    # run a command
```

Related docs:

- [Configuration](../features/configuration.md)
- [Repository onboarding](../features/repository-onboarding.md)
- [Actions hydration](../features/actions-hydration.md)
- [Sync](../features/sync.md)
- [Getting started](../getting-started.md)
