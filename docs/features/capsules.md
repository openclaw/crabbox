# Capsules

Read when you want to:

- turn a failed GitHub Actions run into a repeatable Crabbox debug case;
- decide whether state belongs in a capsule, a [checkpoint](checkpoints.md), an
  [image](prebaked-images.md), or the [cache](cache.md);
- understand the `repo-build-replay` capsule contract and replay outcomes.

Capsules are local-first **failure replay manifests**. A capsule records what
failed, how to rerun it, what outcome counts as a reproduction, and the bounded
evidence needed to inspect the original failure. The manifest is a single
`capsule.yaml` you can commit, share, or throw away.

A capsule deliberately does **not** preserve a machine. Environment state lives
in the other Crabbox primitives:

| Primitive | Purpose |
| --- | --- |
| [`crabbox image`](prebaked-images.md) | Trusted base runner image for future leases. |
| [`crabbox checkpoint`](checkpoints.md) | Explicit prepared machine or workspace state you can fork later. |
| [`crabbox cache`](cache.md) | Package and build cache on a lease. |
| [`crabbox actions hydrate`](actions-hydration.md) | Repository-owned CI setup on a live lease. |
| `crabbox capsule` | Failure recipe, source evidence, replay oracle, replay history. |

The first replay class is intentionally narrow: GitHub Actions failures become
small `repo-build-replay` bundles that Crabbox reruns through
[`crabbox run`](../commands/run.md). There is no coordinator registry, remote
storage, automatic workflow parser, or training loop in this version.

## Commands

| Command | Purpose |
| --- | --- |
| `crabbox capsule from-actions <run-url>` | Capture a failed Actions run into a `capsule.yaml`. |
| `crabbox capsule replay <capsule.yaml>` | Replay the captured failure on a lease. |
| `crabbox capsule inspect <capsule.yaml>` | Print the manifest summary and replay history. |
| `crabbox capsule promote <capsule.yaml>` | Mark the local capsule as a regression. |

`replay`, `inspect`, and `promote` accept a path to either the `capsule.yaml`
file or the directory that contains it.

## Basic flow

Capture a failed run. The `--replay` command is required; it is the exact
command Crabbox reruns to reproduce the failure.

```sh
crabbox capsule from-actions https://github.com/example-org/my-app/actions/runs/123 \
  --replay 'go test ./...'
```

This writes `capsules/example-org-my-app-actions-123/capsule.yaml` by default.
On Unix, capsule directories use mode `0700`; manifests and captured logs use
mode `0600`. Reusing an existing output path repairs broader permissions.
Replay it on a normal lease:

```sh
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml --keep
crabbox ssh --id <printed-lease-or-slug>
```

Once the capsule has proven useful, mark it as a regression replay:

```sh
crabbox capsule promote capsules/example-org-my-app-actions-123/capsule.yaml --regression
```

## `capsule from-actions`

Accepts a GitHub Actions run URL (for example
`https://github.com/example-org/my-app/actions/runs/123`, optionally with an
`/attempts/2` suffix) or a numeric run id together with `--repo owner/name`.

Captured into the manifest:

- repository, run URL, run id, attempt, workflow name and path, commit SHA,
  branch, event, status, and conclusion;
- the failed job and failed step when GitHub exposes them;
- the explicit replay command supplied with `--replay`;
- bounded failed-step logs from `gh run view --log-failed` (when reachable),
  and a derived `oracle.failure_signature`;
- references to the run's non-expired GitHub artifacts (name, size, download
  URL — not the artifact contents).

Reconstructing arbitrary workflow YAML or inferring shell snippets from logs is
out of scope: the explicit `--replay` command is the contract.

The selected job must have a failure conclusion (`failure`, `timed_out`,
`cancelled`, or `action_required`); otherwise the command exits with an error.
When a run has more than one failed job, use `--job` to pick one.

Capture requires the [`gh` CLI](https://cli.github.com) to be installed and
authenticated for the repository.

### Flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--replay '<command>'` | — | **Required.** Command Crabbox reruns to reproduce the failure. |
| `--repo owner/name` | — | Repository; required only when the argument is a bare run id. |
| `--output <dir>` | `capsules/<owner>-<name>-actions-<run-id>` | Capsule output directory. |
| `--scenario <text>` | derived from workflow/job/step | Human-readable scenario label. |
| `--job <name>` | — | Preferred failed job when a run has multiple failures. |
| `--required-quality <q>` | `semantically_identical` | Required replay quality recorded in the manifest. |
| `--max-log-bytes <n>` | `262144` (256 KiB) | Maximum failed-log bytes kept locally (tail-trimmed). |
| `--no-logs` | `false` | Skip fetching failed Actions logs entirely. |

## `capsule replay`

Replay delegates to [`crabbox run --shell`](../commands/run.md) with the
manifest's `replay.command`, then appends a record to the manifest's `replays`
history.

### Flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--id <lease-or-slug>` | — | Replay on an existing lease instead of provisioning a fresh one. |
| `--keep` | `false` | Keep the lease alive after replay for SSH debugging. |
| `--junit <paths>` | — | Comma-separated remote JUnit XML paths to record as [results](test-results.md). |
| `--no-sync` | `false` | Skip the rsync of the local checkout. |
| `--reclaim` | `false` | Claim the lease for the current repo checkout. |

Use `--keep` when the goal is human or agent debugging. Crabbox keeps the lease
alive; attach with `crabbox ssh --id <id-or-slug>` using the id or slug printed
by the underlying run.

## Replay outcomes

Replay classifies each run into one of four outcomes and records it in the
manifest:

| Outcome | Meaning | Exit code |
| --- | --- | --- |
| `pass` | Replay command exited `0`; the original failure was **not** reproduced. | non-zero |
| `fail_reproduced` | Replay failed in the expected way (see signature rule below). | `0` |
| `fail_new` | Replay failed, but with a different signature than captured. | non-zero |
| `inconclusive_env_error` | Replay could not run (lease, sync, or tooling error). | error from `run` |

The signature rule: if the manifest has no `oracle.failure_signature`, any
non-zero exit counts as `fail_reproduced`. When a signature is present, the last
256 KiB of replay output must contain that signature to count as
`fail_reproduced`; a non-zero exit with a different signature is recorded as
`fail_new`.

`fail_reproduced` is the only outcome that exits `0`, so a successful
reproduction can gate scripts. `pass` and `fail_new` intentionally exit non-zero:
neither is an honest reproduction of the captured failure.

Each replay record holds `at`, `outcome`, `replay_quality`, `exit_code`,
`duration_ms`, and whether the lease was kept — the measurement surface for the
first gate: did the same failure reproduce?

## With Actions hydration

Use [hydration](actions-hydration.md) when the failing CI job depends on
repository-owned setup such as service containers, dependency installation, or
toolchain bootstrap:

```sh
crabbox warmup
crabbox actions hydrate --id blue-lobster
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml \
  --id blue-lobster \
  --keep
```

The hydrate workflow owns CI setup; the capsule owns the replay command and
oracle. `crabbox run` syncs local edits into the hydrated workspace before
running the replay.

## With checkpoints

Use a [checkpoint](checkpoints.md) when setup is expensive and should be reused
across many replays:

```sh
crabbox warmup --provider aws --class beast
crabbox actions hydrate --id blue-lobster
crabbox checkpoint create --id blue-lobster --name ci-go-ready

crabbox checkpoint fork chk_123 --class beast
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml \
  --id purple-whale
```

The checkpoint preserves the prepared environment. The capsule stays portable:
it can be replayed against the original lease, a forked checkpoint, or a fresh
lease if the replay command carries enough setup itself.

## Manifest contract

The manifest keeps the durable parts small and versioned. A capsule captured
from a failing Actions run looks like:

```yaml
capsule_version: 1
capsule_id: sha256:9f1c…
class: repo-build-replay
class_version: 0.1.0
scenario: Replay GitHub Actions CI job test step go test
tenant_scope: external_sanitized
source:
  kind: github_actions
  repo: example-org/my-app
  run_id: "123"
  run_url: https://github.com/example-org/my-app/actions/runs/123
  workflow_name: CI
  workflow_path: .github/workflows/ci.yml
  job_name: test
  failed_step: go test
  head_sha: 0a1b2c3d
  head_branch: main
  conclusion: failure
  captured_at: 2026-05-29T12:00:00Z
inputs:
  source_snapshot_digest: git:0a1b2c3d
  actions_run_digest: github_actions_run:example-org/my-app#123
oracle:
  type: deterministic_rerun
  success_condition: The replay command exits non-zero with the same failure signature.
  failure_signature: FAIL example-org/my-app/pkg [build failed]
  forbidden_success_modes:
    - passing by deleting or skipping the failing test
    - passing by removing the failing build target
    - passing by ignoring the replay command exit code
replay:
  command: go test ./...
  command_mode: shell
  required_quality: semantically_identical
  nondeterminism_budget: exit code and failure signature must match
cost:
  max_wall_time_sec: 3600
  max_spend_units: 1
  requires_exclusive_lease: false
safety:
  action_profile: build_debug_v1
  network: repo_default
  secrets: denied
artifacts:
  logs:
    - name: failed-actions-log
      path: logs/failed.log
      size: 4096
      digest: sha256:…
extensions:
  repo-build-replay:
    schema_version: 1
    source: github_actions
    replay_mode: explicit_command
```

When no failure signature can be derived, `oracle.success_condition` is the
weaker "The replay command exits non-zero." and the `failure_signature` field is
omitted; any non-zero replay then counts as a reproduction.

The core contract is deliberately small. Future replay classes add their own
data under `extensions` without changing the base manifest.

## Secret and evidence boundary

Capsules store local YAML, bounded logs, and GitHub artifact references. They do
not store secrets intentionally, but CI logs and artifacts can still contain
sensitive data if the source workflow wrote it. Treat capsule directories as
debug artifacts:

- keep `--max-log-bytes` bounded;
- use `--no-logs` for sensitive runs;
- do not commit capsule directories unless the logs were reviewed;
- delete local capsules when they stop being useful.

## Non-goals

- No RL training or reward loop.
- No emulator or hardware-in-loop implementation.
- No coordinator registry or worker storage.
- No automatic workflow command extraction.
- No machine snapshotting. Use [checkpoints](checkpoints.md) or
  [images](prebaked-images.md) for environment state.
- No secret capture. Capsules store bounded logs and references, not raw runtime
  state.

The strategic point is to make real CI failures replayable first. That delivers
a useful debug product immediately and leaves a clean path toward richer replay
catalogues once the failure catalogue is trustworthy.
