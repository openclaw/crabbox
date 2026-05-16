# Capsules

Read when:

- turning a failed GitHub Actions run into a repeatable Crabbox debug case;
- deciding whether data belongs in a capsule, checkpoint, image, or cache;
- reviewing the `repo-build-replay` capsule contract.

Capsules are local-first **failure replay manifests**. A capsule records what
failed, how to run the replay, what outcome counts as a reproduction, and the
bounded evidence needed to inspect the original failure.

Capsules do not preserve a machine. Environment state belongs to the existing
Crabbox primitives:

- `crabbox image`: trusted base runner image for future leases.
- `crabbox checkpoint`: explicit prepared machine or workspace state that can be
  forked later.
- `crabbox cache`: package/build cache state on a lease.
- `crabbox actions hydrate`: repository-owned CI setup on a live lease.
- `crabbox capsule`: failure recipe, source evidence, replay oracle, replay
  history.

The first delivery is intentionally narrow: GitHub Actions failures become small
`repo-build-replay` bundles that Crabbox reruns through `crabbox run`. There is
no coordinator registry, remote storage, automatic workflow parser, emulator
class, or training loop in this version.

## Basic Flow

Capture a failed run:

```sh
crabbox capsule from-actions https://github.com/example-org/my-app/actions/runs/123 \
  --replay 'go test ./...'
```

Replay it on a normal lease:

```sh
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml --keep
crabbox ssh --id <printed-lease-or-slug>
```

Mark the local manifest as a regression replay after the failure is useful:

```sh
crabbox capsule promote capsules/example-org-my-app-actions-123/capsule.yaml --regression
```

`from-actions` records:

- repository, run URL, run id, attempt, workflow name/path, commit SHA, branch,
  event, status, and conclusion;
- failed job and failed step when GitHub exposes them;
- the explicit replay command supplied with `--replay`;
- bounded failed-step logs when `gh run view --log-failed` can fetch them;
- GitHub artifact download references.

The explicit `--replay` command is the v1 contract. Crabbox does not try to
reconstruct arbitrary workflow YAML or infer shell snippets from logs.

## With Actions Hydration

Use hydration when the failing CI job depends on repository-owned setup such as
service containers, dependency installation, or toolchain bootstrap:

```sh
crabbox warmup
crabbox actions hydrate --id blue-lobster
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml \
  --id blue-lobster \
  --keep
```

The hydrate workflow owns CI setup. The capsule owns the replay command and
oracle. `crabbox run` syncs local edits into the hydrated workspace before
running the replay.

## With Checkpoints

Use a checkpoint when setup is expensive and should be reused across many
replays:

```sh
crabbox warmup --provider aws --class beast
crabbox actions hydrate --id blue-lobster
crabbox checkpoint create --id blue-lobster --name ci-go-ready

crabbox checkpoint fork chk_123 --class beast
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml \
  --id purple-whale
```

The checkpoint preserves the prepared environment. The capsule remains portable:
it can be replayed against the original lease, a forked checkpoint, or a fresh
lease if the command has enough setup built in.

## Manifest Contract

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

The core contract is deliberately small. Future replay classes can add their
own data under `extensions` without changing the base manifest.

## Replay Semantics

`capsule replay` delegates to the existing `crabbox run` path with `--shell`.
If the replay command exits non-zero and the manifest has no
`oracle.failure_signature`, Crabbox records `outcome: fail_reproduced`. When a
signature is present, the bounded replay output must contain that signature to
count as `fail_reproduced`. A nonzero replay with a different signature records
`outcome: fail_new` and returns nonzero because it is a new failure, not an
honest reproduction. If the command exits zero, Crabbox records `outcome: pass`
and returns nonzero because the original failure was not reproduced.

Use `--keep` when the goal is human or agent debugging. Crabbox keeps the lease
alive and the user can attach with `crabbox ssh --id <id-or-slug>` using the id
or slug printed by the underlying run.

Each replay appends a local record with `outcome`, `replay_quality`,
`exit_code`, `duration_ms`, and whether the lease was kept. That is the simple
measurement surface for the first gate: did the same failure reproduce?

## Secret And Evidence Boundary

Capsules store local YAML, bounded logs, and GitHub artifact references. They do
not store secrets intentionally, but CI logs and artifacts can still contain
sensitive data if the source workflow wrote it. Treat capsule directories as
debug artifacts:

- keep `--max-log-bytes` bounded;
- use `--no-logs` for sensitive runs;
- do not commit capsule directories unless the logs were reviewed;
- delete local capsules when they stop being useful.

## Non-Goals

- No RL training or reward loop.
- No emulator or hardware-in-loop implementation.
- No coordinator registry or worker storage.
- No automatic workflow command extraction.
- No machine snapshotting. Use checkpoints or images for environment state.
- No secret capture. Capsules store bounded logs and references, not raw runtime
  state.

The strategic point is to make real CI failures replayable first. That creates a
useful debug product immediately and leaves a clean path toward richer replay
catalogues once the failure catalogue is trustworthy.
