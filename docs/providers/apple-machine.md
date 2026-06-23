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

## Behavior and limits

- `warmup` maps to `container machine create`.
- `run` maps to `container machine run` and preserves the host repository path.
- `run --lease-output <path>` writes the Apple Machine lease ID, slug,
  reuse/retention state, and exact cleanup command for orchestration handoff.
- `status` and `list` use machine JSON inspection.
- `stop` deletes the machine and its persistent storage with `container machine rm`.
- The home directory is mounted read-write. Use `apple-container` when a narrower
  disposable filesystem boundary is more important than persistence.
- The default is `alpine:latest`. Custom images must include `/sbin/init`, as
  required by Apple Container's machine runtime.
- Explicit sync, patch upload, fresh-PR preparation, desktop, browser, Tailscale,
  and coordinator routing are not supported.
