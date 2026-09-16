# Apple Container Machine Provider

Use `provider: apple-machine` for persistent Linux development environments on
Apple silicon macOS with Apple Container 1.0 or newer.

Unlike `apple-container`, this provider uses the native `container machine`
lifecycle. It does not install or expose SSH. Apple mounts the macOS user's home
directory into the machine, and Crabbox executes directly with
`container machine run`.

## Prerequisites

- Apple silicon with macOS 26 or newer.
- Apple Container 1.0 or newer.
- Start the service once with `container system start`.
- Keep the repository under the current user's home directory.

## Usage

```sh
crabbox warmup --provider apple-machine --slug linux-dev
crabbox run --provider apple-machine --id linux-dev -- pnpm test
crabbox status --provider apple-machine --id linux-dev
crabbox stop --provider apple-machine linux-dev
```

A one-shot run creates and removes the machine automatically:

```sh
crabbox run --provider apple-machine -- go test ./...
```

## Configuration

The provider shares the `appleContainer` image and runtime settings with the
disposable Apple Container provider:

```yaml
provider: apple-machine
appleContainer:
  cliPath: container
  image: alpine:latest
  cpus: 4
  memory: 8G
```

Provider flags:

```text
--apple-machine-cli <path-or-name>
--apple-machine-image <image>
--apple-machine-cpus <n>
--apple-machine-memory <size>
```

These four flags remain separate from Apple Container's seven-flag surface;
they do not expose its user, work-root, or extra-run-argument flags. Both surfaces
share configuration values and image explicitness, but retain their distinct
post-flag defaults and runtime behavior. See the [shared input rules](apple-container.md#configuration)
for file and environment semantics; displayed configuration is not a claim about
the native machine's defaults.

## Behavior and limits

- `warmup` maps to `container machine create`.
- `run` maps to `container machine run` and preserves the host repository path.
- `run --lease-output <path>` writes the Apple Machine lease ID, slug,
  reuse/retention state, and exact cleanup command for orchestration handoff.
- Automatic deletion failure after a successful command fails the run with exit
  1 and keeps its session and claim available for recovery. A primary command or
  transport failure retains its outcome and inspectable cause when cleanup or
  reporting also fails; secondary diagnostics remain visible.
- Only a plain matching native process exit establishes a failed command's
  exit code. Transport, cancellation, deadline, and I/O errors remain failures
  with exit 1 even when the native result also carries a nonzero code.
- `--keep-on-failure` covers environment or command preparation failures after
  acquisition as well as failed commands. Reused machines remain kept. Private
  environment-file cleanup runs on every path, including retained runs.
- Timing is finalized after automatic cleanup, with the existing delegated,
  skipped-sync fields for the home-mounted workspace. A reporting failure after
  successful deletion cannot retroactively retain the machine. A failed timing
  writer may leave no usable timing record, but still fails the run without
  replacing an earlier failure.
- `status` and `list` use machine JSON inspection.
- `stop` deletes the machine and its persistent storage with `container machine rm`.
- New leases bind the exact machine name and daemon-reported storage root to a
  private ownership marker in the machine bundle. Reuse, status, and deletion
  verify that binding without booting the machine to inspect it. Cleanup holds
  the unchanged local claim until a complete inventory response and missing
  bundle confirm deletion; uncertainty retains the claim. A later `stop` can
  confirm an already-absent machine without issuing another deletion.
- Older leases without this binding are not adopted or deleted automatically,
  even with `--reclaim`. Inspect them with `container machine inspect <name>`
  and, only after confirming ownership and accepting loss of persistent storage,
  remove them manually with `container machine rm <name>`. Create a new Crabbox
  lease to obtain an ownership binding. Do not copy or regenerate ownership
  markers for replacement machines.
- The storage root comes from `container system status --format json`, not the
  calling shell's `CONTAINER_APP_ROOT`. Missing daemon storage evidence or an
  unexpected bundle layout fails closed. Apple currently stores machine bundles
  under `appRoot/plugin-state/machine-apiserver/machines`.
- Apple's native removal API does not offer an atomic expected-identity check.
  Crabbox fences its own claim changes and rejects observed replacements, but
  external tools must not replace machines concurrently with lifecycle commands.
- Caller cancellation also bounds waiting for claim fences during publication,
  reuse, status, list verification, and explicit stop. Read-only lookups do not
  refresh or adopt claims. The existing readiness budget includes its claim wait.
- Resource cleanup has one 30-second budget starting before its claim fence and
  covering identity verification, native removal, and confirmed absence. Failed
  acquisition rollback receives a fresh uncanceled budget, including when no
  claim was published; a successor claim never authorizes deletion of the
  original machine. Each standalone native control command still has its own
  30-second limit. These bounds cover cooperative waits and subprocesses, not
  forcible interruption of filesystem syscalls.
- Native control failures preserve cancellation/deadline causes alongside their
  existing messages and exit codes. A later rollback failure cannot replace the
  original exit code or hide the retained-machine recovery diagnostic. Once a
  guarded action succeeds, its durable claim publication/removal still completes.
- The home directory is mounted read-write. Use `apple-container` when a narrower
  disposable filesystem boundary is more important than persistence.
- The default is `alpine:latest`. Custom images must include `/sbin/init`, as
  required by Apple Container's machine runtime.
- Explicit sync, patch upload, fresh-PR preparation, desktop, browser, Tailscale,
  and coordinator routing are not supported.
