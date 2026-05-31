# OpenClaw Plugin

Read when:

- enabling Crabbox as a plugin inside OpenClaw;
- changing the plugin tools, parameter schemas, or wrapper behavior;
- understanding why some Crabbox surfaces are CLI-only and not plugin tools.

The Crabbox repository root is also a native OpenClaw plugin package
(`@openclaw/crabbox-plugin`). When OpenClaw loads the plugin it registers a
small set of agent tools that shell out to the user's installed `crabbox`
binary. The plugin embeds no CLI logic and duplicates no provider code: each
tool builds a typed `crabbox` argv, runs it as a subprocess, and returns the
captured output. The point is a bounded, allowlisted contract so an agent can
drive remote boxes without arbitrary shell access.

## Package And Manifest

`package.json` publishes the plugin as `@openclaw/crabbox-plugin`, type
`module`, with `index.js` as both the npm `main` and the OpenClaw extension
entrypoint:

```json
{
  "name": "@openclaw/crabbox-plugin",
  "type": "module",
  "main": "index.js",
  "openclaw": {
    "extensions": ["./index.js"],
    "compat": { "pluginApi": ">=2026.4.25" }
  }
}
```

`openclaw.plugin.json` declares the plugin id, the tools it owns, its
activation, and the config schema:

```json
{
  "id": "crabbox",
  "name": "Crabbox",
  "description": "Run Crabbox remote testbox checks from OpenClaw.",
  "activation": { "onStartup": true },
  "contracts": {
    "tools": [
      "crabbox_run",
      "crabbox_harness_validate",
      "crabbox_job_run_with_harness",
      "crabbox_warmup",
      "crabbox_status",
      "crabbox_list",
      "crabbox_stop"
    ]
  },
  "configSchema": { "...": "see Config below" }
}
```

The runtime entrypoint is `index.js`; its default export exposes
`register(api)`, which reads plugin config and registers the tools. Tests
in `index.test.js` lock the tool set, the provider enum, argv shapes, env
passing, and the disabled-tool guard, so a refactor cannot silently change the
agent-facing contract.

## Tools

The plugin registers a bounded tool surface:

```text
crabbox_run                   run a command on an existing lease after syncing the repo
crabbox_harness_validate      validate a harness Markdown file
crabbox_job_run_with_harness  run a configured job with an explicit harness
crabbox_warmup                provision or reuse a lease and wait until it is ready
crabbox_status                read the current state of a lease
crabbox_list                  list current machines for the owner/org
crabbox_stop                  stop a kept lease by ID or slug
```

Unlike a generic shell tool, each tool takes a **typed parameter object** (not
a raw argv array). The plugin maps those fields onto a fixed `crabbox`
subcommand and a curated subset of its flags, so an agent cannot inject
arbitrary arguments. Every schema sets `additionalProperties: false`.

### `crabbox_run`

Runs `crabbox run --id <id> [flags] -- <command...>`.

| Parameter | Type | Maps to |
|:----------|:-----|:--------|
| `id` (required) | string | `--id` (lease ID or slug) |
| `command` (required) | string array | positional command after `--` |
| `provider` | enum | `--provider` |
| `env` | object of strings | extra subprocess env (not a flag) |
| `noSync` | boolean | `--no-sync` |
| `syncOnly` | boolean | `--sync-only` |
| `forceSyncLarge` | boolean | `--force-sync-large` |
| `checksum` | boolean | `--checksum` |
| `debug` | boolean | `--debug` |
| `reclaim` | boolean | `--reclaim` |
| `junit` | string | `--junit` (comma-separated remote paths) |
| `timeoutSeconds` | number | per-call wrapper timeout |

### `crabbox_harness_validate`

Runs `crabbox harness validate [--json] <path>`. Parameters: `path` (required),
`json`, `timeoutSeconds`.

### `crabbox_job_run_with_harness`

Runs `crabbox job run [flags] --harness <path> <job>`.

| Parameter | Type | Maps to |
|:----------|:-----|:--------|
| `job` (required) | string | positional job name |
| `harness` (required) | string | `--harness` |
| `id` | string | `--id` |
| `index` | enum | `--index none\|light` |
| `noHydrate` | boolean | `--no-hydrate` |
| `githubRunner` | boolean | `--github-runner` |
| `stop` | enum | `--stop` |
| `dryRun` | boolean | `--dry-run` |
| `timeoutSeconds` | number | per-call wrapper timeout |

### `crabbox_warmup`

Runs `crabbox warmup [flags]`.

| Parameter | Type | Maps to |
|:----------|:-----|:--------|
| `provider` | enum | `--provider` |
| `profile` | string | `--profile` |
| `class` | string | `--class` |
| `type` | string | `--type` (provider server/instance type) |
| `ttl` | string | `--ttl` (e.g. `90m`) |
| `idleTimeout` | string | `--idle-timeout` (e.g. `30m`) |
| `keep` | boolean | `--keep` |
| `actionsRunner` | boolean | `--actions-runner` |
| `reclaim` | boolean | `--reclaim` |
| `timeoutSeconds` | number | per-call wrapper timeout |

### `crabbox_status`

Runs `crabbox status --id <id> [flags]`. Parameters: `id` (required),
`provider`, `wait` (`--wait`), `waitTimeout` (`--wait-timeout`, e.g. `10m`),
`json` (`--json`), `timeoutSeconds`.

### `crabbox_list`

Runs `crabbox list [flags]`. Parameters: `provider`, `json` (`--json`),
`refresh` (`--refresh`), `timeoutSeconds`.

### `crabbox_stop`

Runs `crabbox stop [--provider <p>] <id>`. Parameters: `id` (required),
`provider`, `timeoutSeconds`. The id is passed positionally, not as `--id`.

## Tool Gating

`crabbox_run`, the harness tools, `crabbox_warmup`, and `crabbox_stop` can be
disabled per install by setting `allowRun`, `allowHarness`, `allowWarmup`, or
`allowStop` to `false` in plugin config. A disabled tool is still registered,
but its `execute` throws
(`"... is disabled by plugin config"`) before the binary is invoked.
`crabbox_status` and `crabbox_list` are read-only and always allowed.

## Config

The plugin accepts seven optional config keys; the schema sets
`additionalProperties: false`, so unknown keys are rejected:

| Key | Type | Default | Effect |
|:----|:-----|:--------|:-------|
| `binary` | string | `crabbox` | Path to the Crabbox executable. Set when it is not on PATH. |
| `maxOutputBytes` | number | `60000` | Cap on captured stdout/stderr returned to the model per stream. |
| `timeoutSeconds` | number | `1800` | Default wrapper timeout for a Crabbox CLI invocation. |
| `allowRun` | boolean | `true` | Gate `crabbox_run`. |
| `allowHarness` | boolean | `true` | Gate harness validation and harness job tools. |
| `allowWarmup` | boolean | `true` | Gate `crabbox_warmup`. |
| `allowStop` | boolean | `true` | Gate `crabbox_stop`. |

`maxOutputBytes` and `timeoutSeconds` are coerced to positive integers; any
non-positive or non-finite value falls back to the default. A per-call
`timeoutSeconds` parameter overrides the configured default for a single
invocation.

Crabbox's own config (broker URL, provider, token, profile, class) lives in the
user/repo config files, not here. The plugin does not duplicate those keys; the
subprocess inherits the agent's environment (`process.env` plus any per-call
`env`), so it resolves the same config a bare `crabbox` would in the working
directory.

## Output Handling

The plugin captures stdout and stderr separately. Each stream is appended
incrementally and capped at `maxOutputBytes`; once a stream exceeds the cap it
is sliced to the limit and a `\n[truncated]\n` marker is appended, so an agent
can tell it did not receive the full transcript.

The tool result text is assembled from the invocation summary and any non-empty
output:

```text
$ crabbox run --id blue-lobster -- go test ./...

exit=1

stdout:
ok  	example.com/pkg	0.42s

stderr:
FAIL	example.com/other	[build failed]
```

The result also carries a structured `details` object with `ok`, `code`,
`signal`, `timedOut`, `stdout`, `stderr`, and the reconstructed `command`
string, so callers can branch on the exit code rather than parse the text.

When the wrapper timeout fires (or the host aborts the call), the plugin sends
`SIGTERM` to the subprocess. A timed-out run reports `timedOut: true` and adds a
`timed out` line to the summary; the exit code reflects the wrapper outcome, not
the inner remote command.

## What Belongs In The CLI Instead

History, log inspection, attach, results, usage, pond orchestration, and admin
operations are intentionally **not** plugin tools. Run them from a
shell-capable agent:

```sh
crabbox history --lease cbx_...
crabbox events run_... --after 0 --limit 50
crabbox attach run_...
crabbox logs run_...
crabbox results run_...
crabbox usage --scope user
crabbox pond peers --pond my-pond
crabbox pond connect my-pond --export
crabbox admin leases --state active
crabbox cleanup --dry-run
```

Why they stay out of the plugin:

- they often produce far more output than `maxOutputBytes` can usefully carry;
- agents usually want raw logs they can grep, not trimmed model output;
- `pond` is transport-aware orchestration that can update operator Tailscale
  policy or start SSH tunnels, so it stays CLI-led;
- admin tools are easier to gate at the shell level (env, allowlists) than
  through plugin config;
- `crabbox attach` is interactive by design.

## Provider Allowlist

The `provider` parameter is constrained to an enum of provider ids and aliases.
Adding a provider that an agent should be able to target requires extending this
enum in `index.js` and the matching fixture in `index.test.js`; the enum is the
agent-facing contract, and an unlisted value is rejected by schema validation
before it reaches the binary.

```text
aws | azure | azure-dynamic-sessions | gcp | google | google-cloud |
hetzner | proxmox | ssh | static | static-ssh | blacksmith-testbox |
blacksmith | namespace-devbox | namespace | namespace-devboxes |
semaphore | sem | sprites | daytona | islo | e2b | modal | tensorlake |
tl | tensorlake-sbx | cloudflare | cf
```

This is a deliberate subset of the provider adapters Crabbox ships. Providers
absent from the enum (for example local-container, parallels, railway, runpod,
upstash-box, wandb) are still usable from the CLI; they are simply not offered
as plugin-selectable targets. When `provider` is omitted, the agent's
configured default provider applies.

## When To Update

Edit the plugin when you:

- add or remove a provider that agents should target (update the enum);
- add a new agent-safe tool (read-only or bounded, owner-scoped output);
- change the curated flag set a tool maps onto;
- change default timeouts or output budgets.

Run the tests after every change:

```sh
node --test index.test.js
```

The tests exercise the tool set, provider enum, argv handling, env passing, and
the disabled-tool guard end to end.

Related docs:

- [docs/README.md](../README.md) — top-level overview, includes the plugin.
- [Source map](../source-map.md) — `package.json`, `openclaw.plugin.json`, `index.js`, `index.test.js`.
- [run command](../commands/run.md) — what `crabbox_run` ultimately invokes.
- [warmup command](../commands/warmup.md) — what `crabbox_warmup` invokes.
- [stop command](../commands/stop.md) — what `crabbox_stop` invokes.
