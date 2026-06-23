# CodeSandbox Provider

Read when:

- choosing `provider: codesandbox` (aliases: `csb`, `code-sandbox`);
- configuring CodeSandbox SDK auth, templates, privacy, or preview URLs;
- changing `internal/providers/codesandbox`.

CodeSandbox is a delegated-run provider. Crabbox shells out to a local
Node-compatible SDK bridge for sandbox lifecycle, archive sync, command
execution, pause/resume, and preview URL discovery. CodeSandbox owns the Linux
VM and SDK transport; Crabbox owns local config, env-only auth loading, sync
manifests, local ownership claims, slugs, timing summaries, and normalized
list/status rendering.

## When To Use

Use CodeSandbox when you want a managed CodeSandbox Linux development
environment for short-lived or reusable delegated commands. Use AWS, Hetzner,
Static SSH, or Daytona when you need Crabbox-managed SSH access, rsync, VNC,
browser provisioning, code-server, or Actions runner hydration.

## Prerequisites

- A CodeSandbox account with SDK/API access.
- A CodeSandbox API key in the environment as
  `CRABBOX_CODESANDBOX_API_KEY` or `CSB_API_KEY`.
- Node.js on `PATH`, or another Node-compatible command configured with
  `--codesandbox-bridge-command`.
- `npm` on `PATH` so Crabbox can prepare its trusted local SDK bridge cache.

Load the API key from a prompt or secret manager so it never appears in shell
history:

```sh
export CRABBOX_CODESANDBOX_API_KEY="$(
  python3 -c 'import getpass; print(getpass.getpass("CodeSandbox API key: "))'
)"
```

Crabbox never persists the key in config and never places it on argv. The Go
provider passes the token to the local SDK bridge through the child process
environment only.

## Commands

```sh
crabbox doctor --provider codesandbox
crabbox warmup --provider codesandbox --slug live-smoke --keep
crabbox run --provider codesandbox -- echo ok
crabbox run --provider codesandbox --id live-smoke --sync-only
crabbox status --provider codesandbox --id live-smoke
crabbox pause --provider codesandbox live-smoke
crabbox resume --provider codesandbox live-smoke
crabbox ports --provider codesandbox --id live-smoke --publish 3000
crabbox list --provider codesandbox --json
crabbox stop --provider codesandbox live-smoke
```

## Config

```yaml
provider: codesandbox
target: linux
codeSandbox:
  templateId: ""                  # optional CodeSandbox template id
  workdir: /project/workspace     # command cwd and sync target
  vmTier: ""                      # pico, nano, micro, small, medium, large, xlarge
  privacy: private                # public, unlisted, private, or public-hosts
  hibernationTimeoutSecs: 0       # 0 uses CodeSandbox default
  automaticWakeupHttp: true       # allow host URL access to wake hibernated sandboxes
  automaticWakeupWebSocket: false
  bridgeCommand: node
  sdkPackage: "@codesandbox/sdk@2.4.2"
  doctorListLimit: 1
  operationTimeoutSecs: 30
```

Provider flags:

```text
--codesandbox-template-id
--codesandbox-workdir
--codesandbox-vm-tier
--codesandbox-privacy
--codesandbox-hibernation-timeout-secs
--codesandbox-automatic-wakeup-http
--codesandbox-automatic-wakeup-websocket
--codesandbox-bridge-command
--codesandbox-sdk-package
--codesandbox-doctor-list-limit
--codesandbox-operation-timeout-secs
```

Configuration flags have matching `CRABBOX_CODESANDBOX_*` environment
overrides, for example `CRABBOX_CODESANDBOX_WORKDIR`,
`CRABBOX_CODESANDBOX_PRIVACY`, and
`CRABBOX_CODESANDBOX_OPERATION_TIMEOUT_SECS`. The API key is intentionally not
a config field and has no command-line flag.

`codeSandbox.workdir` must be an absolute path under `/project/workspace`.
Crabbox rejects broad paths such as `/` and `/project`.

## Lifecycle

1. `warmup` or `run` without `--id` creates a CodeSandbox sandbox through the
   SDK bridge with Crabbox ownership tags and writes a local claim as
   `csbx_<sandbox-id>`.
2. By default `run` archive-syncs the working tree: Crabbox builds a
   `git ls-files`-driven gzipped tar locally, uploads it through the SDK file
   bridge, and extracts it under `codeSandbox.workdir`. `--no-sync` skips the
   archive upload; `--sync-only` syncs and exits without running a command.
   Manifest, guardrail, archive, staging, replacement, and cleanup orchestration
   use Crabbox's provider-neutral delegated archive-sync core. The root
   `/project/workspace` mount is replaced in place because CodeSandbox does not
   permit renaming that mount; configured subdirectories retain atomic rename.
3. Commands run through the SDK command client with `cwd` set to the configured
   workdir. Selected `--allow-env` and `--env-from-profile` values are sent to
   the local SDK bridge in the request body and staged through a temporary
   remote env file before the command runs, rather than embedded in local argv
   or remote command text.
4. `list`, `status`, `pause`, `resume`, `ports`, and `stop` start from local
   Crabbox claims and verify the remote ownership tag before mutating a
   sandbox. Raw user-created CodeSandbox IDs are rejected.
5. `stop` deletes the sandbox through the SDK and removes the local claim.
   `pause` hibernates the sandbox and keeps the local claim so `resume`, `run`,
   `status`, and `stop` can target it later.

## Ports And Preview URLs

CodeSandbox exposes HTTP ports through provider-owned `csb.app` hosts.
Crabbox's `ports` command supports:

- `crabbox ports --provider codesandbox --id <lease>` to list currently open
  SDK ports.
- `crabbox ports --provider codesandbox --id <lease> --publish 3000` to wait
  for sandbox port `3000` and print the SDK host URL.
- `--json` to return objects with `port`, `host`, and `url` fields.

CodeSandbox port specs are sandbox port numbers only. Host-port mappings such
as `41000:3000` are Docker-style mappings and are rejected for this provider.
`--unpublish` is not supported because the SDK ports surface observes running
processes rather than owning a close-port operation; stop the process inside
the sandbox instead.

Private sandboxes use CodeSandbox host tokens. The SDK bridge asks the
provider's host API for the URL instead of constructing strings locally, so
returned URLs come from CodeSandbox. Accessing a preview URL may automatically
wake a hibernated sandbox when HTTP wakeup is enabled.

## Capabilities

- SSH: not driven by Crabbox.
- Crabbox sync: archive sync through the SDK bridge; no rsync.
- Env forwarding: yes — local off-argv request payload, remote temporary env
  file before command execution.
- Pause/resume: yes — `pause` calls CodeSandbox hibernate and `resume` calls
  CodeSandbox resume.
- Run session: yes, `run` returns a reusable Crabbox lease/session handle for
  `--lease-output`, retained failure inspection, and later `run --id`,
  `status`, `ports`, `pause`, `resume`, or `stop`.
- URL bridge: yes — `ports` returns SDK-owned host URLs for open HTTP ports.
- Desktop / browser / code: no Crabbox VNC, browser provisioning, or
  code-server surface.
- Actions hydration: no.
- Coordinator: no — CodeSandbox always runs direct from the CLI through the SDK
  bridge and never goes through the broker.

## Live Smoke

The live smoke helper is opt-in by credential: without
`CRABBOX_CODESANDBOX_API_KEY` or `CSB_API_KEY` it exits successfully with
`classification=environment_blocked`.

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
node scripts/live-codesandbox-smoke.test.js
```

With auth present, the helper uses an isolated temporary repo and state
directory, creates one Crabbox-owned sandbox, verifies successful and expected
nonzero command exits, runs sync-only checks, exercises pause/resume and ports,
stops the sandbox, and verifies the local CodeSandbox list no longer contains
the smoke slug. Provider quota, rate limit, auth, capacity, or network failures
are classified without printing token values; SDK contract failures remain
diagnostic failures.

## Gotchas

- The bridge command runs from a Crabbox-managed cache directory, not the
  repository. Crabbox installs exact `@codesandbox/sdk@2.4.2` there on first
  use with install scripts and optional dependencies disabled. A clean measured
  install is about 117 MB and 232 dependency packages; the SDK package itself
  is about 6.8 MB unpacked. The dependency is required for CodeSandbox's
  command, filesystem, and port transports. Set
  `CRABBOX_CODESANDBOX_SDK_PACKAGE` or `--codesandbox-sdk-package` to a trusted
  npm package spec only when intentionally overriding the pinned bridge SDK.
- `doctor` is non-mutating. It only checks env auth and a bounded sandbox list.
- `operationTimeoutSecs` applies to each SDK bridge call. Increase it for slow
  creates, resumes, or port waits.
- `run` with an existing `--id` validates the local claim before reusing the
  sandbox. Use `--reclaim` only when intentionally updating a repo claim for a
  Crabbox-owned sandbox.
- Preview URLs can wake hibernated sandboxes. Disable
  `automaticWakeupHttp` when that behavior is not wanted, and resume explicitly
  before opening a preview.
- Host URLs may include short-lived provider access material. Treat smoke
  output as transient and avoid posting URLs publicly.

Related docs:

- [ports](../commands/ports.md)
- [pause](../commands/pause.md)
- [resume](../commands/resume.md)
- [Provider backends](../provider-backends.md)
