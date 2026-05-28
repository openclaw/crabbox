# OpenClaw Plugin

Read when:

- enabling Crabbox as a plugin inside OpenClaw;
- changing the plugin tools, schema, or wrapper behavior;
- understanding why some Crabbox surfaces are CLI-only and not plugin tools.

The Crabbox repository root is also a native OpenClaw plugin package. When
OpenClaw loads the plugin, it exposes a small set of agent tools that shell
out to the user's installed `crabbox` binary. The plugin does not embed the
CLI or duplicate any of its logic - it is a thin contract for safe, allowlisted
invocations.

## Plugin Manifest

`openclaw.plugin.json` declares the plugin id, the tools it owns, and the
config schema:

```json
{
  "id": "crabbox",
  "name": "Crabbox",
  "description": "Run Crabbox remote testbox checks from OpenClaw.",
  "activation": { "onStartup": true },
  "contracts": {
    "tools": [
      "crabbox_run",
      "crabbox_warmup",
      "crabbox_status",
      "crabbox_list",
      "crabbox_stop"
    ]
  },
  "configSchema": { ... }
}
```

The runtime entrypoint is `index.js`. Tests in `index.test.js` lock the tool
schemas, argv shapes, output trimming, and config validation so a future
refactor cannot silently change the agent-facing contract.

## Tools

```text
crabbox_run      run a command on a leased remote box
crabbox_warmup   acquire a warm box for repeated commands
crabbox_status   query a lease's state
crabbox_list     list visible leases for the current owner/org
crabbox_stop     stop a lease and release its provider resources
```

Each tool accepts an argv array of `string` plus an optional `env` object of
string values. The plugin enforces these as JSON schema before invoking the
binary, so an agent cannot pass arbitrary shell commands or non-string env
values.

`crabbox_run`, `crabbox_warmup`, and `crabbox_stop` can be disabled per
install by setting `allowRun`, `allowWarmup`, or `allowStop` to `false` in
plugin config. `crabbox_status` and `crabbox_list` are read-only and always
allowed.

## Config

The plugin accepts only four config keys, all optional:

```json
{
  "binary": "crabbox",
  "maxOutputBytes": 60000,
  "timeoutSeconds": 1800,
  "allowRun": true,
  "allowWarmup": true,
  "allowStop": true
}
```

| Key | Default | Effect |
|:----|:--------|:-------|
| `binary` | `crabbox` | Path to the Crabbox binary. Set when the binary is not on PATH. |
| `maxOutputBytes` | 60000 | Max captured stdout/stderr returned to the model per call. |
| `timeoutSeconds` | 1800 | Default wrapper timeout for a Crabbox CLI invocation. |
| `allowRun` | true | Gate `crabbox_run`. |
| `allowWarmup` | true | Gate `crabbox_warmup`. |
| `allowStop` | true | Gate `crabbox_stop`. |

Crabbox config (broker URL, provider, token, profile, class) lives in the
user/repo config files. The plugin does not duplicate those keys; it inherits
them from whatever `crabbox config show` would return for the agent's
working directory.

## Output Handling

The plugin captures stdout and stderr separately, trims each to
`maxOutputBytes`, and reports the exit code, the trimmed bytes, and a
truncation flag back to the model. Truncated output gets a tail marker so
agents know they did not get the full transcript:

```text
... [output truncated; 12345 of 87654 bytes shown]
```

Long-running tools still respect `timeoutSeconds`. When the wrapper times
out, the plugin sends SIGTERM, waits a short grace period, then escalates to
SIGKILL. The exit code in the response reflects the wrapper outcome, not the
inner remote command.

## What Belongs In The CLI Instead

History, log inspection, attach, results, usage, pond orchestration, and admin
operations are intentionally not plugin tools. They are best run from a
shell-capable agent:

```sh
crabbox history --lease cbx_...
crabbox events run_... --after 0 --limit 50
crabbox attach run_...
crabbox logs run_...
crabbox results run_...
crabbox usage --scope user
crabbox pond peers --pond alpha
crabbox pond connect alpha --export
crabbox admin leases --state active
crabbox cleanup --dry-run
```

Reasons for keeping these out of the plugin:

- they often produce more output than `maxOutputBytes` can usefully capture;
- agents tend to want raw logs they can grep, not trimmed model output;
- `pond` is transport-aware preview orchestration and may update operator
  Tailscale policy or start SSH tunnels, so it stays CLI-led for now;
- admin tools are easier to gate at the shell level (env, allowlists) than
  through plugin config;
- `crabbox attach` is interactive by design.

## Provider Allowlist

The plugin schema constrains the `provider` argument to the providers
Crabbox actually supports:

```text
aws | azure | gcp | google | google-cloud | hetzner | proxmox | ssh |
static | static-ssh | blacksmith-testbox | blacksmith | namespace-devbox |
namespace | namespace-devboxes | semaphore | sem | sprites | daytona | islo |
e2b | modal | tensorlake | tl | tensorlake-sbx | cloudflare | cf
```

Adding a provider to the CLI requires updating this list in `index.js` and
the test fixture in `index.test.js`. The schema is the agent-facing contract;
without the update, the new provider would be rejected by JSON validation
before reaching the binary.

## When To Update

Edit the plugin when you:

- add or remove a provider;
- add a new agent-safe tool (read-only, owner-scoped, bounded output);
- change argv conventions across all `crabbox` commands (rare);
- update default timeouts or output budgets.

Run `node --test index.test.js` after every change. The tests exercise the
schema, argv handling, and output trimming end-to-end.

Related docs:

- [docs/README.md](../README.md) - top-level overview includes the plugin.
- [Source map](../source-map.md) - `package.json`, `openclaw.plugin.json`,
  `index.js`, `index.test.js`.
- [run command](../commands/run.md) - what `crabbox_run` ultimately invokes.
- [warmup command](../commands/warmup.md) - what `crabbox_warmup` invokes.
- [stop command](../commands/stop.md) - what `crabbox_stop` invokes.
