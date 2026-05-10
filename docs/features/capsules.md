# Capsules

Read when:

- creating the first replayable failure bundle from a GitHub Actions run;
- reviewing the `repo-build-replay` capsule contract;
- deciding whether a replay feature belongs in Crabbox or in a future registry.

Capsules are local-first replay manifests for failed engineering environments.
The first delivery is intentionally narrow: GitHub Actions failures become small
`repo-build-replay` bundles that Crabbox can rerun on a lease. There is no
coordinator registry, RL loop, emulator class, or worker-side storage in this
version.

## Actions-First Flow

```sh
crabbox capsule from-actions https://github.com/openclaw/crabbox/actions/runs/123 \
  --replay 'go test ./...'

crabbox capsule replay capsules/openclaw-crabbox-actions-123/capsule.yaml --keep
crabbox ssh --id <printed-lease-or-slug>
crabbox capsule promote capsules/openclaw-crabbox-actions-123/capsule.yaml --regression
```

`from-actions` records the essentials:

- repository, run URL, run id, attempt, workflow name/path, commit SHA, branch,
  event, status, and conclusion;
- failed job and failed step when GitHub exposes them;
- an explicit replay command supplied by the user;
- bounded failed-step logs when `gh run view --log-failed` can fetch them;
- artifact download references from the GitHub Actions API.

The explicit `--replay` command is the v1 escape hatch. Crabbox does not try to
perfectly parse arbitrary workflow YAML or infer shell snippets from logs.

## Contract

The manifest keeps the durable parts small and versioned:

```yaml
capsule_version: 1
class: repo-build-replay
class_version: 0.1.0
source:
  kind: github_actions
replay:
  command: go test ./...
  command_mode: shell
  required_quality: semantically_identical
oracle:
  type: deterministic_rerun
  success_condition: The replay command exits non-zero with the same failure signature.
safety:
  action_profile: build_debug_v1
  network: repo_default
  secrets: denied
extensions:
  repo-build-replay:
    schema_version: 1
    source: github_actions
    replay_mode: explicit_command
```

The schema includes `extensions` from day one so emulator, browser, hardware, or
agent-debug classes can add their own fields later without widening the core
contract. Those classes are deliberately not implemented in the Actions-first
slice.

## Replay Semantics

`capsule replay` delegates to the existing `crabbox run` path with `--shell`.
If the replay command exits non-zero and the manifest has no
`oracle.failure_signature`, Crabbox records `outcome: fail_reproduced`. When a
signature is present, the bounded replay output must contain that signature to
count as `fail_reproduced`. A non-zero replay with a different signature records
`outcome: fail_new` and returns nonzero because it is a new failure, not an
honest reproduction. If the command exits zero, Crabbox records `outcome: pass`
and returns nonzero because the original failure was not reproduced.

Use `--keep` when the goal is human or agent debugging. Crabbox keeps the lease
alive and the user can attach with `crabbox ssh --id <id-or-slug>` using the id
or slug printed by the underlying run.

Each replay appends a local record with `outcome`, `replay_quality`,
`exit_code`, `duration_ms`, and whether the lease was kept. That is the simple
measurement surface for the first gates: replay success, time to attach, bounded
cost intent from the manifest, and human repro time saved from the operator's
notes. A richer registry can consume these records later, but the first slice
does not need one.

## Non-Goals

- No RL training or reward loop.
- No emulator or hardware-in-loop implementation.
- No coordinator registry or worker storage.
- No automatic workflow command extraction.
- No secret capture. Capsules store bounded logs and references, not raw
  production state.

The strategic point is to make real CI failures replayable first. That creates a
useful debug product immediately and leaves a clean path toward eval and
trajectory datasets once the replay catalogue is trustworthy.
