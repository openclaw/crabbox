# Repository Onboarding

Read when:

- changing `crabbox init`;
- adding repo-local config keys;
- changing generated workflow or agent instructions.

`crabbox init` prepares a repository for Crabbox without requiring users to remember the full command surface.
Use `crabbox init --detect` when the first config should include a detected
remote check based on the repository's language/tooling markers.

Generated files:

```text
.crabbox.yaml
.github/workflows/crabbox.yml
.agents/skills/crabbox/SKILL.md
```

Repo-local config should hold project-specific behavior:

- default profile;
- class;
- sync excludes;
- sync options;
- base ref for changed-test hydration;
- environment allowlist.

Generated agent instructions should point agents toward warmup/reuse flows and project-specific test commands. Generated workflow stubs are the bridge for Actions-backed hydration: Crabbox executes supported setup steps locally by default, with the GitHub runner path available when full Actions semantics are required.

`--detect` adds a `jobs.detected` config entry when it recognizes a runnable
check. The detector currently looks for Go modules, Node package scripts, Rust
crates, and a root Makefile `test` target. It also writes matching
`run.preflightTools` so `crabbox run --preflight` checks the tools the detected
job expects.

Related docs:

- [init command](../commands/init.md)
- [Actions hydration](actions-hydration.md)
- [Sync](sync.md)
- [CLI](../cli.md)
