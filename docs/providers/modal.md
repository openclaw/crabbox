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

- Install the Modal Python client: `pip install modal`.
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

## Config

```yaml
provider: modal
target: linux
modal:
  app: my-app
  image: python:3.13-slim
  workdir: /workspace/crabbox/work
  python: python3
```

Provider flags:

```text
--modal-app
--modal-image
--modal-workdir
--modal-python
```

Environment overrides:

```text
CRABBOX_MODAL_APP
CRABBOX_MODAL_IMAGE
CRABBOX_MODAL_WORKDIR
CRABBOX_MODAL_PYTHON
```

Defaults: app `crabbox`, image `python:3.13-slim`, workdir
`/workspace/crabbox`, Python binary `python3`.

## Lifecycle

1. `warmup` / `run` without `--id` creates a Modal Sandbox in the configured
   `modal.app` from `modal.image`, with the sandbox timeout and Crabbox
   ownership tags. Crabbox stores a local claim with a normal `cbx_...` lease ID
   and a friendly slug.
2. The sandbox timeout is derived from `--ttl`: it defaults to 5 minutes when no
   TTL is set and is capped at 24 hours.
3. By default `run` archive-syncs the working tree: Git manifest → local
   `tar -czf` → upload to `/tmp/crabbox-modal-sync-*.tgz` in the sandbox →
   in-sandbox `tar -xzf` into `modal.workdir`.
4. The user command runs through `Sandbox.exec` in a `bash -lc` wrapper that
   first `cd`s into `modal.workdir`, streaming stdout/stderr back through
   Crabbox.
5. One-shot sandboxes are terminated after a `run` that did not pass `--keep`.
   `--keep` and `--keep-on-failure` retain the sandbox until `crabbox stop`.

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

- IDs can be a Crabbox slug, a `cbx_...` lease ID, or a raw Modal sandbox ID
  (when that sandbox carries Crabbox tags).
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
  best-effort afterward. They are never placed on the local Python process argv.

## Related docs

- [Provider backends](../provider-backends.md)
