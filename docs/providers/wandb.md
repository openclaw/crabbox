# W&B Sandboxes Provider

Read when:

- choosing `provider: wandb` (alias `weights-and-biases`);
- pointing crabbox at a [Weights & Biases Serverless Sandbox](https://docs.wandb.ai/sandboxes);
- changing `internal/providers/wandb`.

## What it is

**[wandb.ai](https://wandb.ai/) is the brand the AI/ML research community
already trusts.** Used in production by OpenAI, Anthropic, Meta AI, Mistral
(launch customer of Sandboxes), and every major academic ML lab — for
experiment tracking, sweep orchestration, model registry, and now compute.

W&B Serverless Sandboxes are the Weights & Biases-branded surface on
[CoreWeave Sandboxes](https://docs.coreweave.com/products/sandboxes), since
CoreWeave's [2025 acquisition](https://www.coreweave.com/news/coreweave-completes-acquisition-of-weights-biases)
of W&B. Sandboxes run as managed, isolated containers on CoreWeave's GPU
fleet via CKS (CoreWeave Kubernetes Service). The Python `wandb.sandbox`
package is a thin auth-wrapper over CoreWeave's open-source
[`cwsandbox`](https://github.com/coreweave/cwsandbox-client) gRPC client;
the protos are published at [buf.build/coreweave/sandbox](https://buf.build/coreweave/sandbox).

> **Experimental.** Upstream API is `coreweave.sandbox.v1beta2` and
> Serverless Sandboxes is in public preview. Expect field renames and
> breaking changes until upstream cuts `v1`. Pin via `buf.lock` in
> `internal/providers/wandb/proto/`.

## Why this provider exists

Every other crabbox provider asks the user for a new provider-specific
credential. **This one doesn't.** Every AI/ML practitioner who has ever run
`wandb login` — researchers, sweep operators, eval harness builders — already
has the W&B API key CoreWeave Sandboxes accepts as the `x-wandb-api-key` gRPC
metadata header. The key is sitting in `~/.netrc` from a year ago; the caller
must also set `WANDB_ENTITY_NAME` so the sandbox gateway knows the W&B entity
scope.

That is the wedge. **`crabbox run --provider wandb -- python train.py`
warms a CoreWeave GPU sandbox using the W&B identity the researcher
already has** — zero new accounts, no provider-specific token, zero billing
setup. For the AI research persona, this is the lowest-friction provider in
the tree.

## Lifecycle

`crabbox run`:

1. With `--id <sandbox-id>` → `Exec` against the existing sandbox.
2. Otherwise: `Start` (with idle keep-alive command), poll `Get` with
   200 ms → 1.5× → 2 s backoff until `SANDBOX_STATUS_RUNNING`, then `Exec`
   the user command, then `Stop` unless `--keep`.

`Status` calls `Get` and renders the proto status enum (`SANDBOX_STATUS_RUNNING`,
`…_COMPLETED`, `…_FAILED`, …). `List` paginates `List` filtered by the
`crabbox` tag. `Stop` issues a graceful `Stop` (10 s default). `Warmup` is
rejected — sandboxes are acquired per run, there is no separate provisioning
phase.

## When to use

- You already use W&B and want zero-credential onboarding.
- You want to run on CoreWeave GPUs without provisioning your own runner.
- You want sandbox activity to show up under the same auth identity as your
  W&B runs.

Pick a different provider for SSH sessions or persistent dev boxes — this
provider is exec-only by design.

## Commands

```sh
crabbox run    --provider wandb --no-sync -- python eval.py
crabbox run    --provider wandb --no-sync --id $WANDB_SANDBOX_ID -- pnpm test
crabbox status --provider wandb --id $WANDB_SANDBOX_ID
crabbox stop   --provider wandb $WANDB_SANDBOX_ID
crabbox list   --provider wandb
crabbox doctor --provider wandb
```

## Live Smoke

Keep the API key in `CRABBOX_WANDB_API_KEY` or `WANDB_API_KEY`; do not pass it
on the command line. Set `WANDB_ENTITY_NAME` to the W&B entity/team that owns
the sandbox runs.

```sh
export CRABBOX_WANDB_API_KEY=wandb_v1_...
export WANDB_ENTITY_NAME=my-team
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox doctor --provider wandb
bin/crabbox run --provider wandb --no-sync --wandb-max-lifetime 60 -- echo crabbox-wandb-ok
```

Or the standalone script (no coordinator required):

```sh
CRABBOX_LIVE=1 CRABBOX_WANDB_API_KEY=wandb_v1_... WANDB_ENTITY_NAME=my-team scripts/wandb-smoke.sh
```

Or the Go smoke test (Acquire → Exec → Stop):

```sh
go test -tags smoke -run TestSmokeVersionAndExec -v ./internal/providers/wandb/
```

## Auth

Credential precedence (first match wins):

1. `CRABBOX_WANDB_API_KEY` — explicit override, CI-friendly.
2. `cfg.wandb.apiKey` — config file value.
3. `WANDB_API_KEY` — canonical env var written by `wandb login`.
4. `~/.netrc` — machine `api.wandb.ai` entry, written by `wandb login`.

Required W&B scoping:

- `WANDB_ENTITY_NAME` → sent as `x-entity-id`.

Optional W&B scoping:

- `WANDB_PROJECT`     → sent as `x-project-name`.

The API key is never exposed as a CLI flag — secrets do not belong on
`argv`.

## Config

```yaml
provider: wandb
target: linux
wandb:
  defaultImage: ubuntu:24.04
  maxLifetimeSeconds: 1800   # 30 min — billing safety, server enforces
```

Provider flags:

```text
--wandb-image
--wandb-max-lifetime
```

Endpoint override (matches the upstream cwsandbox SDK behaviour):

```text
CWSANDBOX_BASE_URL    # default: atc.cw-sandbox.com:443
```

## Capabilities

- SSH: no (gRPC `Exec` only; PTY via `StreamExec` deferred to v2).
- crabbox sync: no. `--no-sync` is required.
- Desktop / browser / code: no.
- Coordinator: no.

## Gotchas

- `max_lifetime_seconds` is the **single most important billing-safety knob**.
  crabbox defaults to 30 min; raise via `--wandb-max-lifetime` only when you
  know you need it.
- `Exec` is the unary RPC, not the bidi `StreamExec`. Output is buffered
  server-side and returned at completion. Streaming PTY is a follow-up.
- `AddFile` (unary file upload) caps near 100 MB. For large source trees use
  the upstream's `S3Mount` at `Start` — supported by the proto but not yet
  exposed as a crabbox flag.
- `--reclaim`, `--shell`, `--class`, `--type` are rejected — CoreWeave owns
  sandbox lifecycle and shape.
- gRPC errors are surfaced as `*wandbAPIError` with sysexits.h-aligned exit
  codes (`77 EX_NOPERM`, `69 EX_UNAVAILABLE`, `124` GNU timeout).

## Regenerating the proto stubs

```sh
cd internal/providers/wandb/proto
buf dep update          # refresh buf.lock
buf generate            # regenerate ../gen/
```

Bump the BSR commit pin in `buf.lock` only via a dedicated PR titled
`proto: bump cwsandbox to <sha>` so the diff is auditable.

Related docs:

- [Provider backends](../provider-backends.md)
- [W&B Serverless Sandboxes docs](https://docs.wandb.ai/sandboxes)
- [CoreWeave Sandboxes docs](https://docs.coreweave.com/products/sandboxes)
