# Modal Provider

Read when:

- choosing `provider: modal`;
- configuring Modal apps, images, or sandbox workdirs;
- changing `internal/providers/modal`.

Modal is a delegated run provider. Crabbox shells out to the local Modal Python
client for Sandbox lifecycle, file upload, status/list, and command execution.
Modal owns sandbox state and process transport; Crabbox owns local config, repo
claims, sync manifests and guardrails, slugs, timing summaries, and normalized
list/status rendering.

## When To Use

Use Modal when commands should run in Modal Sandboxes and you do not need a
Crabbox SSH target. Use AWS, Hetzner, Static SSH, Namespace Devbox, Semaphore,
Sprites, or Daytona when you need `crabbox ssh`, VNC, code-server, or Actions
hydration.

## Prerequisites

- Install the Modal Python client: `pip install modal`.
- Authenticate with Modal: `python3 -m modal setup` or set
  `MODAL_TOKEN_ID` / `MODAL_TOKEN_SECRET` in the environment. Do not pass Modal
  tokens as command-line flags.

## Commands

```sh
crabbox warmup --provider modal --modal-app crabbox --modal-image python:3.13-slim
crabbox run --provider modal -- pnpm test
crabbox run --provider modal --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider modal --id blue-lobster
crabbox stop --provider modal blue-lobster
```

## Auth

Crabbox does not accept Modal token flags. The local Python client reads normal
Modal auth state from `python3 -m modal setup` or from:

```sh
export MODAL_TOKEN_ID=...
export MODAL_TOKEN_SECRET=...
```

Rotate these values if they were pasted into a chat, shell history, issue, or
log.

## Config

```yaml
provider: modal
target: linux
modal:
  app: crabbox
  image: python:3.13-slim
  workdir: /workspace/crabbox
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

## Lifecycle

1. `warmup` / `run` without `--id` creates a Modal Sandbox in `modal.app` with
   `modal.image`, the configured timeout, and Crabbox ownership tags.
2. Crabbox stores a local claim with a normal `cbx_...` lease ID and friendly
   slug.
3. By default `run` archive-syncs the working tree: Git manifest → local
   `tar -czf` → Modal process-stdin upload to `/tmp/crabbox-modal-sync-*.tgz`
   → in-sandbox `tar -xzf` into `modal.workdir`.
4. The user command runs through `Sandbox.exec` in a `bash -lc` wrapper that
   changes into `modal.workdir`, streaming stdout/stderr back through Crabbox.
5. Non-kept one-shot sandboxes are terminated after the run. `--keep` and
   `--keep-on-failure` retain the sandbox until `crabbox stop`.

## Capabilities

- SSH: no.
- Crabbox sync: yes, archive sync through Modal Sandbox exec/upload.
- Provider sync: no separate Modal sync command.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- IDs can be Crabbox slugs, `cbx_...` lease IDs, or Modal sandbox IDs when the
  sandbox carries Crabbox tags.
- `--class` and `--type` are rejected; the configured Modal image owns runtime
  contents and resources for this first implementation.
- `modal.workdir` must be an absolute, dedicated directory. Broad roots such as
  `/`, `/tmp`, `/home`, and `/workspace` are rejected before sync or command
  execution.
- `--checksum` is rejected because Modal does not expose Crabbox's SSH/rsync
  target. Large-sync guardrails still apply, and `--force-sync-large` is
  honored for intentional large archive syncs.
- Use `--sync-only` to pre-upload the archive into a kept sandbox before a
  later command.
- `--script`, `--script-stdin`, `--fresh-pr`, local stdout/stderr captures,
  `--capture-on-fail`, and `--download` are rejected because Modal owns command
  transport and Crabbox has no SSH target for those paths.
- Forwarded environment values are written to a temporary shell profile, uploaded
  into `/tmp`, sourced for the command, and removed best-effort. They are not
  placed on the local Python process argv.

Related docs:

- [Provider backends](../provider-backends.md)
