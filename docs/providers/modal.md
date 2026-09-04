# Modal Provider

Read this when you:

- choose `provider: modal`;
- configure the Modal app, sandbox image, working directory, or Python binary;
- change `internal/providers/modal`.

Modal is a **delegated-run** provider. Crabbox shells out to the local Modal
Python client to manage Sandbox lifecycle, upload files, query status, and run
commands. Modal owns the sandbox state and process transport; Crabbox owns local
config, repo claims, sync manifests and guardrails, slugs, timing summaries, and
the normalized `list`/`status` output. There is no SSH target.

## When to use

Use Modal when commands should run in Modal Sandboxes and you do not need a
Crabbox SSH box. Reach for a provisioned SSH provider (AWS, Hetzner, Azure, GCP,
static SSH) or another delegated provider instead when you need `crabbox ssh`,
VNC, code-server, or Actions hydration — none of those surfaces exist on Modal.

## Prerequisites

- Install Modal Python SDK 1.5.5 or newer: `pip install 'modal>=1.5.5'`.
- Authenticate Modal locally: `python3 -m modal setup`, or export
  `MODAL_TOKEN_ID` / `MODAL_TOKEN_SECRET`. The Python client runs with the
  current process environment, so any standard Modal auth state is picked up.

## Auth

Crabbox does not take Modal token flags and never passes tokens on the command
line. The local Python client reads normal Modal auth state — from
`python3 -m modal setup` or from the environment:

```sh
export MODAL_TOKEN_ID=...
export MODAL_TOKEN_SECRET=...
```

Rotate these values if they were ever pasted into a chat, shell history, issue,
or log.

## Commands

```sh
crabbox warmup --provider modal --modal-app my-app --modal-image python:3.13-slim
crabbox run --provider modal -- pnpm test
crabbox run --provider modal --id swift-crab --shell 'pnpm install && pnpm test'
crabbox status --provider modal --id swift-crab
crabbox stop --provider modal swift-crab
```

## Live Smoke

Run the shared smoke only after the Modal Python client is installed and
authenticated:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=modal CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

The smoke exits before any Modal `warmup`, `status`, `run`, `list`, or `stop`
call when the configured Python binary cannot import the Modal client. With a
working client and auth state, it creates one sandbox, waits for status, runs
one no-sync command, lists normalized inventory, and stops the lease.

## Config

```yaml
provider: modal
target: linux
modal:
  app: my-app
  image: python:3.13-slim
  workdir: /workspace/crabbox/work
  python: python3
  environment: my-app-dev
  secrets:
    - example
```

Provider flags:

```text
--modal-app
--modal-image
--modal-workdir
--modal-python
--modal-environment
--modal-secret NAME  # repeatable or comma-separated
```

Environment overrides:

```text
CRABBOX_MODAL_APP
CRABBOX_MODAL_IMAGE
CRABBOX_MODAL_WORKDIR
CRABBOX_MODAL_PYTHON
CRABBOX_MODAL_ENVIRONMENT
CRABBOX_MODAL_SECRETS     # comma-separated Modal Secret names
```

Defaults: app `crabbox`, image `python:3.13-slim`, workdir
`/workspace/crabbox`, Python binary `python3`.

`modal.secrets` contains Modal Secret names, not secret values. Crabbox resolves
those names inside Modal when creating the sandbox. Use `modal.environment` (or
`--modal-environment`) when the secrets belong to a named Modal environment.
Set these fields in the user config or pass the corresponding flags or environment
variables; repository-local config cannot select a Modal environment or Secret.

## Lifecycle

1. `warmup` / `run` without `--id` creates a Modal Sandbox in the configured
   `modal.app` from `modal.image`, with the sandbox timeout and Crabbox
   ownership tags, assigned atomically at creation. Crabbox durably stores a
   local claim binding the `cbx_...` lease ID and friendly slug to the exact
   sandbox ID, API endpoint list, authenticated workspace name, native
   environment ID/name, and native app ID/name. Scope metadata stays local;
   credentials are never stored in the claim.
2. The sandbox timeout is derived from `--ttl`: it defaults to 5 minutes when no
   TTL is set and is capped at 24 hours.
3. By default `run` archive-syncs the working tree: Git manifest → portable
   gzip archive → upload to `/tmp/crabbox-modal-sync-*.tgz` in the sandbox →
   in-sandbox `tar -xzf` into `modal.workdir`. For new sandboxes, archive
   preparation and size guardrails run before creation. With `sync.delete`,
   extraction is staged before replacing the workdir; the sync timeout bounds
   archiving and transfer.
4. The user command runs through `Sandbox.exec` in a `bash -lc` wrapper that
   first `cd`s into `modal.workdir`, streaming stdout/stderr back through
   Crabbox.
5. One-shot sandboxes are terminated after a `run` that did not pass `--keep`.
   `--keep` retains the sandbox; `--keep-on-failure` retains it after setup,
   sync, or command failure. Reused sandboxes remain available. Termination
   failure is a run failure and leaves the claim available for explicit stop.
   See the [shared sandbox lifecycle](../features/delegated-runner-contract.md#shared-sandbox-lifecycle)
   for exit-code precedence and timing semantics.

Stop, reuse, and one-shot cleanup require that exact local claim. Crabbox holds
the unchanged claim fence while verifying scope and performing each operation;
each Python child uses the same native client for its scope checks and action.
Running sandboxes must also appear under the bound native app ID with matching
ownership tags. Termination waits for a confirmed terminal state before removing
the claim, with a two-minute cleanup budget. A missing inventory entry, inaccessible sandbox, or failed status
check is not proof that termination succeeded, so uncertainty retains the claim.
An already-finished exact sandbox can have its claim removed after scope,
identity, ownership tags, and terminal state are verified.

Use the same Modal app/environment configuration and authority when stopping or
reusing a lease. Token rotation within that authority is supported; changing the
workspace, effective environment, app identity, or endpoint list is not silently
accepted. An empty `modal.environment` resolves through the SDK's configuration
or server default rather than assuming an environment named `main`.

### Older leases and lost local state

Older claims without an exact sandbox/scope binding cannot authorize stop or
reuse. `--reclaim` only changes repository ownership of an already exact claim;
it cannot adopt an unclaimed sandbox, fill in legacy ownership, or retarget a
claim to another resource or provider. `stop --reclaim` is not supported for
Modal. Read-only `list` and `status` remain available for discovery.

If local state is lost or predates this binding, inspect the exact sandbox in
Modal and clean it up explicitly through Modal's dashboard or SDK. For example,
after verifying the account and sandbox ID independently:

```python
import modal

sandbox = modal.Sandbox.from_id("sb-EXACT_SANDBOX_ID")
sandbox.terminate(wait=True)
assert sandbox.poll() is not None
```

Create a new Crabbox lease afterward. Do not reconstruct a claim from tags or
edit an existing claim to point at a different sandbox. Acquisition rollback is
limited to the exact sandbox just created and only while its local claim is
still absent; a concurrent or partially published claim retains the resource for
inspection.

Note: `warmup` always keeps the sandbox until an explicit `crabbox stop`. If you
pass `--keep=false` to `warmup`, Crabbox prints a warning and still keeps it.

## Capabilities

- SSH: no.
- Crabbox sync: yes — archive sync through Modal Sandbox upload + exec.
- Provider sync: no separate Modal sync step.
- Desktop / browser / code: no.
- Actions hydration: no.
- Coordinator (broker): no — Modal always runs direct from the CLI.
- URL bridge / pond: no advertised URL bridge. Modal does not expose a
  per-sandbox ingress URL through Crabbox today.

## Gotchas

- IDs can be a Crabbox slug, a `cbx_...` lease ID, or a raw Modal sandbox ID.
  Stop/reuse require a unique exact local claim; provider tags alone only allow
  read-only discovery.
- `--class` and `--type` are rejected. The configured Modal image owns the
  runtime contents and resources.
- `modal.workdir` must resolve to an absolute, dedicated directory. Broad roots
  such as `/`, `/tmp`, `/home`, and `/workspace` (and other system roots) are
  rejected before any sync or command runs.
- `--checksum` is rejected because Modal has no SSH/rsync target. Large-sync
  guardrails still apply; `--force-sync-large` is honored for intentional large
  archive syncs.
- Use `--sync-only` to pre-upload the archive into a kept sandbox before a later
  command.
- Delegated run/sync options that need an SSH target are rejected:
  `--script` / `--script-stdin`, `--fresh-pr`, `--full-resync`, `--env-helper`,
  `--capture-stdout` / `--capture-stderr`, `--capture-on-fail`, `--download`,
  `--artifact-glob`, `--require-artifact`, `--emit-proof`, and `--stop-after`.
- Forwarded environment values are written to a temporary shell profile,
  uploaded into `/tmp`, sourced (`set -a`) for the command, and removed
  best-effort afterward. Each operation has its own unpredictable profile path;
  the private local source is removed as soon as upload returns. Failed uploads
  retain cleanup responsibility, and remote cleanup uses a bounded, uncanceled
  context and the original ownership claim. The command does not run if sourcing
  its profile fails. They are never placed on the local Python process argv.
  Remote file permissions remain governed by the provider upload transport; this
  does not establish a new remote permission guarantee.

## Related docs

- [Provider backends](../provider-backends.md)
