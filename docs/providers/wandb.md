# W&B Sandboxes Provider

Read when:

- choosing `provider: wandb` (alias `weights-and-biases`);
- pointing Crabbox at a [Weights & Biases Sandbox](https://docs.wandb.ai/sandboxes);
- changing `internal/providers/wandb`.

W&B Sandboxes are managed, isolated Linux containers backed by
[CoreWeave Sandboxes](https://docs.coreweave.com/products/sandboxes). The
provider talks to the CoreWeave gateway over gRPC
(`coreweave.sandbox.v1beta2`) using the W&B API key as the
`x-wandb-api-key` metadata header, so a caller who has already run
`wandb login` does not need a new provider-specific credential.

This is a delegated-run provider: Crabbox does not provision an SSH box and
does not go through the broker coordinator. The CLI starts a sandbox, runs a
single command in it via the unary `Exec` RPC, and stops it again. There is
no workspace sync, so `crabbox run` requires `--no-sync`.

> **Experimental.** The upstream API is `coreweave.sandbox.v1beta2` and W&B
> Sandboxes are in preview. Expect field renames and breaking changes until
> upstream cuts a stable `v1`; the protos are pinned via `buf.lock` in
> `internal/providers/wandb/proto/`.

## When to use

Use W&B Sandboxes when you already have W&B credentials and want a managed
Linux sandbox without provisioning your own runner or creating a new account.
Pick a provider with SSH (AWS, Hetzner, Static SSH, Daytona) instead when you
need direct shell access or a persistent dev box — this provider is exec-only
by design.

W&B Sandboxes are Linux-only. Desktop, browser, code, SSH, Actions hydration,
and the broker coordinator are not available.

## Commands

```sh
crabbox run    --provider wandb --no-sync -- python eval.py
crabbox run    --provider wandb --no-sync --id swift-crab -- pnpm test
crabbox run    --provider wandb --no-sync --keep --lease-output session.json -- true
crabbox status --provider wandb --id swift-crab
crabbox stop   --provider wandb swift-crab
crabbox list   --provider wandb
crabbox doctor --provider wandb
```

Without `--id`, `run` acquires a new sandbox, runs the command, and stops the
sandbox afterwards. With `--id <sandbox-id>` it execs into the existing
sandbox and leaves it running. `warmup` is rejected — sandboxes are acquired
per run, with no separate provisioning phase.

`--lease-output <path>` writes a reusable session handle containing the stable
sandbox ID and an exact cleanup command. Pair it with `--keep` when another
process should reuse the newly acquired sandbox.

## Auth

Resolve the W&B API key in this precedence (first match wins):

1. `CRABBOX_WANDB_API_KEY` — explicit override, CI-friendly.
2. `wandb.apiKey` — config-file value.
3. `WANDB_API_KEY` — canonical env var written by `wandb login`.
4. `~/.netrc` — the `machine api.wandb.ai` (or `.com`) entry written by
   `wandb login`.

`WANDB_ENTITY_NAME` is also required; it is sent as the `x-entity-id` gRPC
header. `WANDB_PROJECT` is optional and, when set, is sent as
`x-project-name`.

```sh
export CRABBOX_WANDB_API_KEY=wandb_v1_...
export WANDB_ENTITY_NAME=my-team
```

The API key is never exposed as a CLI flag; do not pass secrets on the command
line.

## Config

```yaml
provider: wandb
target: linux
wandb:
  defaultImage: ubuntu:24.04
  maxLifetimeSeconds: 1800   # 30 min; W&B reclaims the sandbox at this limit
```

Defaults applied when unset: `defaultImage` is `ubuntu:24.04` and
`maxLifetimeSeconds` is `1800`.

Provider flags (each overrides the matching `wandb.*` config key):

```text
--wandb-image           # container image used when acquiring a new sandbox
--wandb-max-lifetime    # maximum sandbox lifetime in seconds
```

Environment overrides:

- `defaultImage` also reads `CRABBOX_WANDB_DEFAULT_IMAGE`, then
  `WANDB_DEFAULT_IMAGE`.
- `maxLifetimeSeconds` also reads `CRABBOX_WANDB_MAX_LIFETIME_SECONDS`, then
  `WANDB_MAX_LIFETIME_SECONDS`.
- `CWSANDBOX_BASE_URL` overrides the gateway endpoint (default
  `api.cwsandbox.com:443`); the `https://` / `http://` scheme is stripped if
  present.

## Lifecycle

`crabbox run`:

1. With `--id <sandbox-id>`, `Exec` against the existing sandbox and leave it
   running.
2. Otherwise, `Start` a sandbox with an idle keep-alive command, poll until it
   reaches `RUNNING` (200 ms backoff, ×1.5, capped at 2 s), `Exec` the
   command, then `Stop` it unless `--keep` is set.

`status` renders the sandbox state (for example `running`; a `COMPLETED`
sandbox is reported as `stopped` so `status --wait` treats it as terminal).
`list` paginates sandboxes tagged `crabbox`. `stop` issues a graceful stop
(15 s timeout). Acquire and exec apply a startup timeout that is the lesser of
five minutes and the sandbox lifetime.

## Capabilities

- SSH: no — gRPC `Exec` only.
- Crabbox sync: no — `--no-sync` is required.
- Desktop / browser / code: no.
- Actions hydration: no.
- Coordinator (broker): no — always direct from the CLI.

## Gotchas

- `maxLifetimeSeconds` is the main billing-safety limit. Crabbox defaults to
  30 minutes; raise it with `--wandb-max-lifetime` only when needed. A lease
  TTL shorter than the configured lifetime narrows it further.
- `Exec` is the unary RPC, so command output is buffered server-side and
  returned at completion rather than streamed; there is no interactive PTY.
- Environment variables are applied at `Start` time only. When you target an
  existing sandbox with `--id`, env vars cannot be forwarded onto it (the
  `Exec` RPC has no env field), so an `--id` run with `--allow-env` is
  rejected.
- `--reclaim`, `--shell`, `--sync-only`, `--checksum`, `--force-sync-large`,
  `--full-resync`, `--download`, `--artifact-glob`, and `--require-artifact`
  are rejected: W&B owns the sandbox lifecycle and there is no Crabbox
  SSH/rsync target.
- `--keep` retains a newly acquired sandbox after the run; `--keep-on-failure`
  retains it only when the command fails, so you can debug.
- gRPC failures map to sysexits-aligned exit codes:
  `77` (unauthenticated / permission denied), `4` (not found), `124`
  (deadline exceeded), `69` (unavailable / resource exhausted).

## Live smoke

Run a live smoke when changing W&B lifecycle, exec, status, or auth code. Keep
the API key in the environment; never pass it as an argument.

```sh
export CRABBOX_WANDB_API_KEY=wandb_v1_...
export WANDB_ENTITY_NAME=my-team
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox doctor --provider wandb
bin/crabbox run --provider wandb --no-sync --wandb-max-lifetime 60 -- echo crabbox-wandb-ok
```

Or the Go smoke test (acquire, exec, stop):

```sh
go test -tags smoke -run TestSmokeVersionAndExec -v ./internal/providers/wandb/
```

## Regenerating the proto stubs

```sh
cd internal/providers/wandb/proto
buf dep update          # refresh buf.lock
buf generate            # regenerate ../gen/
```

Bump the BSR commit pin in `buf.lock` only via a dedicated, auditable PR.

Related docs:

- [Provider backends](../provider-backends.md)
- [W&B Sandboxes docs](https://docs.wandb.ai/sandboxes)
- [CoreWeave Sandboxes docs](https://docs.coreweave.com/products/sandboxes)
</content>
</invoke>
