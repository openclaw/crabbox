# Harnesses

Harnesses add intent, grounding, and compliance evidence around ordinary
Crabbox runs. They do not change what the remote command does: Crabbox still
leases or reuses a box, syncs the checkout, runs the command, streams output,
and records artifacts. The harness wraps that run with a small contract that a
human or agent can review later.

Use harnesses when a run needs more than "exit code 0": a plan, scoped files,
required evidence, JUnit expectations, or a compliance report that can be linked
from a PR.

## Harness file

A harness is a Markdown file with optional YAML frontmatter and a human-readable
plan body:

```md
---
version: "1"
template: regression
job: full-ci
plan_file: docs/plan/login-regression.md
scope:
  - apps/web/**
  - packages/auth/**
compliance:
  require_plan: true
  require_junit: true
  required_artifacts:
    - junit
---

## Plan

Reproduce the login regression, run the browser flow, and collect JUnit plus
screenshots.
```

Supported frontmatter keys are `version`, `template`, `job`, `plan_file`,
`scope`, `validate`, and `compliance`. Unknown top-level keys fail validation so
agents do not silently misspell evidence requirements.

## CLI

Validate a harness without leasing a box:

```sh
crabbox harness validate HARNESS.md
crabbox harness validate --json HARNESS.md
```

Attach a harness to a direct run:

```sh
crabbox run --harness HARNESS.md -- pnpm test
```

Attach a harness to a job:

```sh
crabbox job run --harness HARNESS.md full-ci
```

When a harness is present, `--index` defaults to `light`. Use `--index none` to
skip remote grounding upload while still writing local harness evidence.

## Evidence

Completed harness runs write local evidence under
`.crabbox/runs/<run-or-lease>/`:

- `harness.md`
- `grounding.json`
- `compliance-report.md`
- `compliance-report.json`

For SSH-backed runs with `--index light`, Crabbox also uploads
`harness.md` and `grounding.json` under remote `.crabbox/grounding/` after the
workspace is synced.

Timing JSON includes a `harness` object plus artifact entries with kinds
`harness`, `grounding`, `compliance-report`, and `compliance-json`.

## Compliance status

With a harness, command exit `0` is necessary but not always sufficient. If the
harness requires plan evidence, passing JUnit evidence, or artifacts and they
are missing or failing, Crabbox writes the compliance report and returns a
nonzero CLI result. Failed commands still write compliance reports when enough
run context exists.

## Boundary

Harnesses intentionally stay lightweight:

- no LLM judging;
- no vector index;
- no scenario database;
- no autonomous state machine;
- no extra remote daemon.

Agents and maintainers own the plan. Crabbox owns grounding, execution, and
proof.

## Related

- [run command](../commands/run.md)
- [job command](../commands/job.md)
- [Jobs](jobs.md)
- [OpenClaw plugin](openclaw-plugin.md)
