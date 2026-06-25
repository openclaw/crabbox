# Replicate Provider

Read this when:

- choosing `provider: replicate`;
- configuring the Replicate deployment or model version, workdir, polling, or
  archive limit;
- changing `internal/providers/replicate`.

Replicate is a delegated-run provider for Linux command execution on a
compatible Replicate runner deployment or model version. Crabbox does not
provision an SSH machine, does not use the coordinator broker, and does not
run rsync. Instead, Crabbox prepares a small workspace archive, sends it to the
Replicate prediction as an input, and expects the runner to execute the
requested command inside the configured workdir.

Current implementation status: the provider has a delegated-run lifecycle
backend. `warmup` and `doctor` validate local configuration and API token
presence without creating a prediction or consuming billing. `run` creates a
new prediction, sends the archive-backed runner input, polls until the
prediction reaches a terminal state, maps the runner's command exit code, and
stores a local claim for later lookup. `status` and `stop` operate on a
prediction ID, local Crabbox claim, or local claim slug. `list` shows local
Replicate claims for the configured API endpoint; it is not an account-wide
Replicate prediction inventory.

## When To Use

Use Replicate when you have built or selected a Replicate runner that accepts
Crabbox's runner input schema and can run Linux commands in its prediction
environment. It fits small proof runs where the provider owns command
execution and the workspace can be represented as a bounded data URL archive.

Use an SSH-lease provider such as AWS, Hetzner, Azure, Google Cloud, Static
SSH, or Local Container when you need `crabbox ssh`, VNC, code-server, normal
rsync, long-running warm boxes, Actions hydration, or desktop/browser/code
capability flags.

Replicate is Linux-only in Crabbox. SSH, desktop, browser, code-server,
Tailscale, coordinator routing, and Crabbox-managed VM lifecycle are not part
of the provider contract.

## Auth

Keep the Replicate API token in the environment. Crabbox intentionally has no
`--replicate-api-token` flag and no YAML token field.

```sh
export CRABBOX_REPLICATE_API_TOKEN="$(python3 -c 'import getpass; print(getpass.getpass("Replicate API token: "))')"
```

Token precedence:

1. `CRABBOX_REPLICATE_API_TOKEN`
2. `REPLICATE_API_TOKEN`

The token is sent only as Replicate API authentication by the provider client.
Crabbox strips `CRABBOX_REPLICATE_API_TOKEN` and `REPLICATE_API_TOKEN` from the
runner environment even if they are selected by `CRABBOX_ENV_ALLOW`,
`--allow-env`, or a forwarding profile. If the runner command needs secrets,
use separate non-provider-auth variable names. Do not place the Replicate token
in `crabbox.yaml`, shell history, issue bodies, PR descriptions, logs, or
screenshots.

## Config

Choose exactly one runner target for `run`, `warmup`, and `doctor`:
`replicate.deployment` or `replicate.version`. Setting both is rejected;
setting neither is rejected for commands that create or validate a runner
target. Existing-prediction commands such as `status`, `list`, and `stop` only
require the API URL, API token, and prediction or claim identifier.

```yaml
provider: replicate
target: linux
replicate:
  deployment: example-org/crabbox-runner
  # version: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  workdir: /workspace/crabbox
  waitSecs: 0
  pollIntervalSecs: 2
  execTimeoutSecs: 3600
  cancelAfterSecs: 0
  maxArchiveBytes: 10485760
```

Defaults:

- `apiURL`: `https://api.replicate.com/v1`
- `workdir`: `/workspace/crabbox`
- `waitSecs`: `0` (provider default wait behavior)
- `pollIntervalSecs`: `2`
- `execTimeoutSecs`: `3600`
- `cancelAfterSecs`: `0` (disabled)
- `maxArchiveBytes`: `10485760` (10 MiB)

Provider flags, each overriding the matching config key:

```text
--replicate-api-url
--replicate-deployment
--replicate-version
--replicate-workdir
--replicate-wait-secs
--replicate-poll-interval-secs
--replicate-exec-timeout-secs
--replicate-cancel-after-secs
--replicate-max-archive-bytes
```

Environment overrides:

```text
CRABBOX_REPLICATE_API_URL
REPLICATE_API_URL
CRABBOX_REPLICATE_DEPLOYMENT
CRABBOX_REPLICATE_VERSION
CRABBOX_REPLICATE_WORKDIR
CRABBOX_REPLICATE_WAIT_SECS
CRABBOX_REPLICATE_POLL_INTERVAL_SECS
CRABBOX_REPLICATE_EXEC_TIMEOUT_SECS
CRABBOX_REPLICATE_CANCEL_AFTER_SECS
CRABBOX_REPLICATE_MAX_ARCHIVE_BYTES
```

`--class` and `--type` are rejected for `provider=replicate`; Replicate
capacity and hardware selection belong to the selected deployment or model
version rather than Crabbox's VM class map.

## Runner Contract

The runner input is JSON-shaped and includes the command, working directory,
optional archive data URL, forwarded environment, timeout settings, metadata,
and output schema hint. The runner must extract the archive into the configured
workdir when one is present, run the command there, and return JSON output.

Command success is based on the runner output's command exit code, not merely
on Replicate reporting the prediction as `succeeded`. The output must include
an integer `exit_code` field. `exitCode` is accepted as a compatibility alias.

Expected output shape:

```json
{
  "exit_code": 0,
  "stdout": "test output\n",
  "stderr": ""
}
```

If the prediction succeeds but the runner omits `exit_code`, or returns it as a
string or float, Crabbox treats that as a runner contract failure.

## Commands

Discovery and non-billing readiness checks:

```sh
crabbox providers --json
crabbox warmup --provider replicate
crabbox doctor --provider replicate --json
```

Run lifecycle:

```sh
crabbox run --provider replicate -- pnpm test
crabbox status --provider replicate --id pred_abc123 --json
crabbox list --provider replicate --json
crabbox stop --provider replicate rbx_pred_abc123
```

Each `run` creates a new one-shot prediction. `crabbox run --provider replicate
--id ...` is rejected; use `status` or `stop` with an existing prediction ID,
local Crabbox claim ID, or local claim slug instead.

## Capabilities

- SSH: no.
- Crabbox sync: delegated archive sync through a small data URL archive.
- Provider sync: no separate provider-native source checkout is assumed.
- Desktop / browser / code / VNC: no.
- Actions hydration: no.
- Coordinator broker: no, Replicate runs direct from the CLI.
- Run sessions: yes, backed by local Crabbox claims for the created prediction.
  Claims support `status`, `list`, and `stop`; there is no Crabbox-managed VM
  cleanup resource beyond canceling the Replicate prediction.

## Archive Limits

Replicate v1 support is intentionally bounded to small data URL archives. The
default `replicate.maxArchiveBytes` is 10 MiB. Oversized workspaces should fail
before a prediction is created, so a local size mistake does not unexpectedly
consume provider billing.

Large-workspace upload through object storage or another staged data plane is a
future extension and is not documented as available here.

## Live Smoke

Normal verification must not create live Replicate predictions or consume
billing. A live smoke should only run when all of these are true:

- a Replicate token is exported through `CRABBOX_REPLICATE_API_TOKEN` or
  `REPLICATE_API_TOKEN`;
- exactly one deployment or version is configured;
- the runner is known to implement Crabbox's JSON input/output contract;
- an explicit arming variable such as `CRABBOX_REPLICATE_LIVE_SMOKE=1` is set.

Until a guarded `scripts/live-replicate-smoke.sh` exists, classify live smoke
script proof as deferred rather than substituting docs checks for real provider
behavior. Non-mutating local proof can still cover `providers --json`,
configuration validation, missing-auth `doctor`, and token-redaction behavior.
Any future smoke must report one of: success, `skipped`,
`environment_blocked`, `quota_blocked`, `validation_failed`, `cleanup_failed`,
or `diagnostic_only`. If it creates a prediction, it must preserve the
prediction ID long enough to attempt `crabbox stop --provider replicate`
cleanup and report the result.

Related docs:

- [Provider backends](../provider-backends.md)
- [Provider live smoke](../features/provider-live-smoke.md)
