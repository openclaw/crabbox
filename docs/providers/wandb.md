# W&B Sandboxes Provider

Read when:

- choosing `provider: wandb` (alias `weights-and-biases`);
- pointing Crabbox at a [Weights & Biases Sandbox](https://wandb.ai/);
- changing `internal/providers/wandb`.

W&B Sandboxes are Firecracker-style execution environments operated by Weights
& Biases. The upstream `cwsandbox` CLI 0.23.0 only exposes `exec/logs/ls/sh` —
sandbox creation (`Sandbox.run`) and `Sandbox.stop` live only in the Python
SDK. To keep crabbox's delegated-run shape (single subprocess per call, JSON
on stdout, exit code passthrough) the provider ships an embedded Python shim
(`internal/providers/wandb/shim/wandb_sandbox.py`) that wraps the SDK and
exposes the subcommands crabbox needs.

## Mapping And Semantic Differences

W&B Sandboxes are stateful boxes, not stateless deployments. `crabbox run`
maps onto the following lifecycle:

1. If `--id <sandbox-id>` is supplied, crabbox runs `exec` against that
   existing sandbox.
2. Otherwise crabbox calls the shim's `acquire` subcommand, which issues
   `cwsandbox.Sandbox.run(container_image=..., max_lifetime_seconds=..., tags=["crabbox"])`
   and waits for the sandbox to reach `RUNNING`.
3. The user command is then dispatched through `cwsandbox.Sandbox.exec` and
   stdout/stderr stream through to the terminal.
4. The sandbox is stopped (`Sandbox.stop(graceful_shutdown_seconds=10, missing_ok=True)`)
   unless `--keep` was passed.

`Status` returns the SDK's normalized status (e.g. `running`, `completed`,
`failed`). `List` flattens `Sandbox.list(tags=["crabbox"])` into one row per
sandbox. `Stop` issues `Sandbox.stop` against the supplied id. `Warmup` is
rejected — sandboxes are acquired per run; there is no separate provisioning
phase for now.

## When To Use

Use W&B Sandboxes when the workload already runs on Weights & Biases or when
you want a delegated Firecracker sandbox without operating your own VM
inventory. Pick a different provider for SSH-shaped sessions; W&B Sandboxes
are exec-only.

## Commands

```sh
crabbox run --provider wandb --no-sync -- echo hello
crabbox run --provider wandb --no-sync --id $WANDB_SANDBOX_ID -- pnpm test
crabbox status --provider wandb --id $WANDB_SANDBOX_ID
crabbox stop   --provider wandb $WANDB_SANDBOX_ID
crabbox list   --provider wandb
crabbox doctor --provider wandb
```

`warmup` is rejected — sandboxes are acquired per `run`.

## Auth

```sh
export WANDB_API_KEY=...   # required; W&B account API key
```

`CRABBOX_WANDB_API_KEY` is also accepted and wins over `WANDB_API_KEY`,
matching the precedence used by other delegated providers
(`CRABBOX_E2B_API_KEY` over `E2B_API_KEY`, `CRABBOX_RAILWAY_API_TOKEN` over
`RAILWAY_API_TOKEN`). The API key is read from the environment only; the
provider does not register a CLI flag for it. Do not pass it on the command
line.

## Setup

Install the SDK once per machine:

```sh
pip install 'wandb[sandbox]'
export WANDB_API_KEY=...
```

`wandb[sandbox]` (not just bare `cwsandbox`) is required: the shim imports
`wandb.sandbox`, whose side-effect registers a W&B-specific auth mode against
`cwsandbox` (via `cwsandbox.set_auth_mode`). That auth mode injects an
`x-wandb-api-key` metadata header sourced from `WANDB_API_KEY`. The cwsandbox
gateway rejects W&B-format keys when sent as a bare Bearer token, so without
`wandb[sandbox]` installed the shim would 401.

Run `crabbox doctor --provider wandb` to verify the shim is runnable and the
installed `cwsandbox` version is `>= 0.20.0`.

## Config

```yaml
provider: wandb
target: linux
wandb:
  python: python3
  defaultImage: ubuntu:24.04
  maxLifetimeSeconds: 1800
```

Provider flags:

```text
--wandb-python
--wandb-image
--wandb-max-lifetime
```

Environment overrides:

```text
CRABBOX_WANDB_API_KEY              (or WANDB_API_KEY)
CRABBOX_WANDB_PYTHON               (or WANDB_PYTHON)
CRABBOX_WANDB_DEFAULT_IMAGE        (or WANDB_DEFAULT_IMAGE)
CRABBOX_WANDB_MAX_LIFETIME_SECONDS (or WANDB_MAX_LIFETIME_SECONDS)
```

## Capabilities

- SSH: no.
- Crabbox sync: no. `--no-sync` is required.
- Provider sync: no.
- Desktop/browser/code: no.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- The provider relies on a Python shim because the cwsandbox CLI 0.23.0 lacks
  `run` and `stop`. The shim is extracted to `${TMPDIR:-/tmp}/crabbox-wandb-shim-<sha>.py`
  idempotently on first invocation.
- `pip install 'wandb[sandbox]'` is required on the host that runs crabbox.
- `--reclaim`, `--shell`, `--class`, `--type` are rejected — W&B owns sandbox
  lifecycle and shape.
- Non-zero shim exits surface as `wandbAPIError`, mirroring the error shape
  used by other delegated providers.

Related docs:

- [Provider backends](../provider-backends.md)
