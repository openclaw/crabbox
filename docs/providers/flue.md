# Flue Provider

Read when:

- choosing `provider: flue`;
- running a Crabbox checkout through a Flue workflow;
- changing `internal/providers/flue`.

Flue is a delegated-run provider. Crabbox builds a local archive of the current
Git checkout, writes a versioned request JSON file, and asks the local `flue`
CLI to run a named workflow. Flue owns the sandbox, command transport, and
workflow implementation. Crabbox owns provider selection, archive creation,
request-file cleanup, output parsing, timing summaries, and doctor/readiness
reporting.

This is not an SSH lease. It does not go through the Crabbox coordinator, does
not expose ports, does not create a persistent Crabbox session, and does not
support desktop, browser, code-server, Tailscale, retained leases, or remote
artifact promises in v1.

## When To Use

Use `flue` when you already have a local Flue project with a workflow that can
consume Crabbox's request-file protocol and run a one-shot Linux command:

```sh
crabbox doctor --provider flue --flue-root ./flue-runner
crabbox run --provider flue --flue-root ./flue-runner --flue-workflow crabbox-runner -- echo ok
crabbox run --provider flue --flue-root ./flue-runner --flue-workdir /workspace/my-app -- pnpm test
```

Use an SSH-lease provider such as `aws`, `hetzner`, `local-container`, or
`ssh` when you need a persistent machine, normal SSH access, Crabbox-managed
rsync, ports, copies, desktop/browser/code-server, or coordinator scheduling.

## Prerequisites

- Install Flue so `flue` is on `PATH`, or configure `--flue-cli` /
  `flue.cliPath`.
- Create a local Flue project root and pass it with `--flue-root`, or run
  Crabbox from a directory where the Flue CLI can discover the project.
- Add a workflow named `crabbox-runner`, or pass another workflow name with
  `--flue-workflow`.
- Keep the Flue target as `node`. Crabbox v1 passes host-local archive and
  request-file paths, so Cloudflare and remote server targets need a future
  upload or HTTP staging contract before they can work.
- Keep provider secrets in Flue/project/provider environment mechanisms such as
  `--flue-env`, not in Crabbox argv.

## Commands

```sh
crabbox doctor --provider flue --flue-root ./flue-runner
crabbox doctor --provider flue --flue-cli /opt/flue/bin/flue --flue-root ./flue-runner --json
crabbox run --provider flue --flue-root ./flue-runner --flue-workflow crabbox-runner -- echo ok
crabbox run --provider flue --flue-root ./flue-runner --flue-config flue.config.ts --flue-env .env -- pnpm test
```

Crabbox invokes Flue as:

```text
flue run workflow:<name> --target node --input '{"requestFile":"<temp-json>"}' [--root <path>] [--config <path>] [--env <path>] [--output <mode>]
```

The `--input` payload intentionally contains only a pointer to a temporary
request file. The full command, environment, archive path, timeout, and output
limits stay in that request file so secrets are not copied into the Flue argv.
Crabbox removes both the request file and the archive after the Flue process
returns.

## Config

```yaml
provider: flue
target: linux
flue:
  cliPath: flue
  root: ./flue-runner
  workflow: crabbox-runner
  target: node
  config: flue.config.ts
  envFile: .env
  output: json
  workdir: /workspace/crabbox
  timeoutSecs: 1800
```

Provider flags:

```text
--flue-cli <path>
--flue-root <path>
--flue-workflow <name>
--flue-target node
--flue-config <path>
--flue-env <path>
--flue-output <mode>
--flue-workdir <absolute-sandbox-path>
--flue-timeout-secs <seconds>
```

Environment overrides:

```text
CRABBOX_FLUE_CLI
CRABBOX_FLUE_ROOT
CRABBOX_FLUE_WORKFLOW
CRABBOX_FLUE_TARGET
CRABBOX_FLUE_CONFIG
CRABBOX_FLUE_ENV
CRABBOX_FLUE_OUTPUT
CRABBOX_FLUE_WORKDIR
CRABBOX_FLUE_TIMEOUT_SECS
```

Precedence follows the normal Crabbox order:

```text
flags > env > repo config > user config > defaults
```

## Request Protocol

Flue receives a small CLI input object:

```json
{"requestFile":"/tmp/crabbox-flue-request-123.json"}
```

The request file is `0600` and contains:

```json
{
  "protocolVersion": 1,
  "operation": "run",
  "workflow": "crabbox-runner",
  "target": "node",
  "workspaceArchive": "/tmp/crabbox-flue-sync-123.tgz",
  "workspace": "/workspace/crabbox",
  "command": ["echo", "ok"],
  "env": {},
  "timeoutMs": 1800000,
  "outputLimits": {
    "stdoutBytes": 10485760,
    "stderrBytes": 10485760
  }
}
```

The workflow must print a final JSON response on stdout. It may print progress
before that, but the final parseable JSON object should be last:

```json
{
  "protocolVersion": 1,
  "operation": "run",
  "exitCode": 0,
  "stdout": "ok\n",
  "stderr": "",
  "timing": {
    "runMs": 42,
    "totalMs": 150
  }
}
```

`exitCode` is the delegated command's exit code. `stdout` and `stderr` are
replayed by Crabbox onto the local Crabbox stdout/stderr streams. A non-zero
`exitCode` makes `crabbox run` exit non-zero with the same command failure
classification.

## Runner Example

The example runner at
[`docs/examples/flue/crabbox-runner.mjs`](../examples/flue/crabbox-runner.mjs)
is intentionally dependency-free Node code. It validates `protocolVersion`,
reads the request file, checks the archive member names before extraction,
stages the archive, runs the command, enforces the request timeout, returns
structured stdout/stderr/exit/timing, and avoids logging request contents or
environment values.

Use it as the command body inside a Flue workflow, or as the implementation
model for a TypeScript workflow that uses Flue's sandbox APIs:

```sh
node docs/examples/flue/crabbox-runner.mjs --input '{"requestFile":"/tmp/crabbox-flue-request.json"}'
```

The example is executable as a local protocol fixture. It is not a generated
Flue project and does not configure Cloudflare, server mode, retained sessions,
live streaming, or artifact upload/download.

## Doctor

`crabbox doctor --provider flue` is non-mutating. It does not run the configured
workflow and never creates a sandbox. The provider doctor checks:

- `flue --help` through the configured CLI path.
- `flue --version` as optional, non-authoritative context.
- configured `flue.root`, `flue.config`, and `flue.envFile` path readability.
- `flue.output` string sanity.
- `flue.target=node`.
- configured workflow name presence, while reporting workflow discovery as
  `unchecked` unless Flue exposes a safe read-only discovery command.

Example JSON check fields include `mutation=false`, `target=node`, and
`discoverability=unchecked`. A missing CLI, unreadable configured root, missing
configured config/env file, or non-`node` target fails doctor.

## Lifecycle And Capabilities

- Kind: delegated-run.
- Family: `flue`.
- Canonical provider: `flue`.
- Targets: Linux.
- Coordinator: never. Crabbox shells out to the local Flue CLI.
- Sync: archive-sync. Crabbox creates a tar archive and Flue extracts it.
- SSH: unsupported.
- Persistent lifecycle: unsupported. `list` returns no leases; `warmup`,
  `status`, and `stop` report one-shot unsupported errors.
- Desktop, browser, code-server, VNC, Tailscale, ports, copies, checkpoints,
  retained sessions, live streaming, run artifacts, and run downloads:
  unsupported in v1.

Unsupported generic sizing flags such as `--class` and `--type` are rejected.
`--no-sync`, `--sync-only`, `--keep`, `--keep-on-failure`, persistent `--id`,
desktop/browser/code-server, Tailscale, local patch application, scripts,
fresh-PR hydration, proof emission, and SSH-only capture options are rejected
unless a future provider version advertises a matching capability.

## Smoke

Build Crabbox and run the deterministic CLI surfaces:

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
(cd /tmp && /path/to/crabbox/bin/crabbox providers --json)
(cd /tmp && /path/to/crabbox/bin/crabbox doctor --provider flue --flue-root /path/to/flue-runner)
```

For a fake or fixture Flue CLI, make the fake `flue` command read the
`--input` pointer, invoke the runner example, and return the final response JSON:

```sh
crabbox run --provider flue --flue-cli /path/to/fake-flue --flue-root /path/to/fixture --flue-workflow crabbox-runner -- echo ok
```

If a real local Flue install or workflow prerequisites are unavailable, classify
that live proof as `environment_blocked`. Keep the fake CLI and Go protocol
tests as deterministic proof that Crabbox builds the request-file bridge,
cleans up temporary files, parses the response, and propagates command output
and exit status.
