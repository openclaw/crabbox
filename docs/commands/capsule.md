# capsule

`crabbox capsule` captures and replays lightweight failure capsules.

The first implementation is Actions-first and local-first. It writes a
`capsule.yaml` plus bounded local evidence, then reuses `crabbox run` for
replay.

A capsule is not a VM snapshot. Use `crabbox checkpoint` or `crabbox image` for
environment state. Use `crabbox capsule` for the failure source, replay command,
oracle, and replay history.

## Create From GitHub Actions

```sh
crabbox capsule from-actions <run-url> --replay '<command>'
crabbox capsule from-actions 123456 --repo example-org/my-app --replay 'go test ./...'
```

Useful flags:

```text
--repo <owner/name>             repository, required when the argument is only a run id
--replay <command>              explicit replay command, required
--output <dir>                  output directory, default capsules/<repo>-actions-<run-id>
--scenario <text>               human-readable scenario
--job <name>                    prefer a specific failed job when a run has several
--required-quality <quality>    default semantically_identical
--max-log-bytes <n>             cap retained failed log bytes, default 262144
--no-logs                       skip fetching failed Actions logs
```

The command records run metadata, failed job/step metadata, a bounded failed log
when available, GitHub artifact references, and the explicit replay command.
It does not infer commands from arbitrary workflow YAML.

## Replay

```sh
crabbox capsule replay <capsule.yaml> [--keep]
```

Useful flags:

```text
--id <lease-id-or-slug>   replay on an existing lease
--keep                    keep the lease after replay for SSH debugging
--junit <paths>           collect remote JUnit XML through crabbox run
--no-sync                 skip rsync
--reclaim                 claim an existing lease for the current repo
```

Replay runs the manifest's `replay.command` through `crabbox run --shell`. When
the manifest has `oracle.failure_signature`, a nonzero replay only records
`fail_reproduced` if the bounded replay output contains that signature. A
nonzero replay with a different signature records `fail_new` and returns
nonzero so the mismatch is visible. A zero exit records `pass` and returns
nonzero because the captured failure did not reproduce.

Replay can target an environment prepared by other Crabbox features:

```sh
crabbox actions hydrate --id blue-lobster
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml --id blue-lobster --keep

crabbox checkpoint fork chk_123 --class beast
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml --id purple-whale
```

## Inspect

```sh
crabbox capsule inspect <capsule.yaml>
crabbox capsule inspect <capsule.yaml> --json
```

`inspect` prints the source, replay command, oracle, last replay, and promotion
state.

## Promote

```sh
crabbox capsule promote <capsule.yaml> --regression
```

Promotion marks the local manifest as a regression replay. There is no remote
registry in this first slice.

Related docs:

- [Capsules](../features/capsules.md)
- [Actions hydration](../features/actions-hydration.md)
- [checkpoint](checkpoint.md)
- [run](run.md)
