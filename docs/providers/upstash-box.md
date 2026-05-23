# Upstash Box Provider

Read when:

- choosing `provider: upstash-box`;
- configuring Upstash Box runtime, size, region endpoint, or workdir;
- changing `internal/providers/upstashbox`.

Upstash Box is a delegated run provider. Crabbox calls the Upstash Box REST API
for Box lifecycle, file upload, status/list, delete, and command streaming.
Upstash owns sandbox state and process transport; Crabbox owns local config,
repo claims, archive sync, slugs, timing summaries, and normalized list/status
rendering.

## When To Use

Use Upstash Box when commands should run in Upstash-managed ephemeral Linux
sandboxes and you do not need a Crabbox SSH target. Use AWS, Hetzner, Static
SSH, Namespace Devbox, Semaphore, Sprites, or Daytona when you need `crabbox
ssh`, VNC, code-server, or Actions hydration.

## Prerequisites

- Create an Upstash Box API key.
- Export it as `UPSTASH_BOX_API_KEY` or `CRABBOX_UPSTASH_BOX_API_KEY`. Do not
  pass API keys as command-line flags.

## Commands

```sh
crabbox warmup --provider upstash-box --upstash-box-runtime node --upstash-box-size small
crabbox run --provider upstash-box -- pnpm test
crabbox run --provider upstash-box --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider upstash-box --id blue-lobster
crabbox stop --provider upstash-box blue-lobster
```

## Auth

```sh
export UPSTASH_BOX_API_KEY=...
```

`CRABBOX_UPSTASH_BOX_BASE_URL` or `upstashBox.baseUrl` can override the default
`https://us-east-1.box.upstash.com`.

## Config

```yaml
provider: upstash-box
target: linux
upstashBox:
  baseUrl: https://us-east-1.box.upstash.com
  runtime: node
  size: small
  workdir: /workspace/home/crabbox
  keepAlive: false
```

Provider flags:

```text
--upstash-box-base-url
--upstash-box-runtime
--upstash-box-size
--upstash-box-workdir
--upstash-box-keep-alive
```

Environment overrides:

```text
CRABBOX_UPSTASH_BOX_API_KEY / UPSTASH_BOX_API_KEY
CRABBOX_UPSTASH_BOX_BASE_URL / UPSTASH_BOX_BASE_URL
CRABBOX_UPSTASH_BOX_RUNTIME
CRABBOX_UPSTASH_BOX_SIZE
CRABBOX_UPSTASH_BOX_WORKDIR
CRABBOX_UPSTASH_BOX_KEEP_ALIVE
```

## Lifecycle

1. `warmup` / `run` without `--id` creates a Box named
   `crabbox-<slug>-<lease-hex>` with the configured runtime and size.
2. Crabbox stores a local claim with a normal `cbx_...` lease ID and friendly
   slug.
3. By default `run` archive-syncs the working tree: Git manifest → local
   `tar -czf` → Upstash Box file upload to `/tmp/crabbox-upstash-box-sync-*.tgz`
   → in-Box `tar -xzf` into `upstashBox.workdir`.
4. The user command runs through the Box exec-stream endpoint in an `sh -c`
   wrapper that changes into `upstashBox.workdir`, streaming output back through
   Crabbox.
5. Non-kept one-shot Boxes are deleted after the run. `--keep` and
   `--keep-on-failure` retain the Box until `crabbox stop`.

## Capabilities

- SSH: no.
- Crabbox sync: yes, archive sync through Box file upload and exec.
- Provider sync: no separate Upstash sync command.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- IDs can be Crabbox slugs, `cbx_...` lease IDs, or Upstash Box IDs when the Box
  uses Crabbox naming.
- `--class` and `--type` are rejected; use `--upstash-box-size` and
  `--upstash-box-runtime`.
- `upstashBox.workdir` must be an absolute, dedicated directory. Broad roots
  such as `/`, `/tmp`, `/workspace`, and `/workspace/home` are rejected before
  sync or command execution.
- `--checksum` is rejected because Upstash Box does not expose Crabbox's
  SSH/rsync target. Large-sync guardrails still apply, and `--force-sync-large`
  is honored for intentional large archive syncs.
- Use `--sync-only` to pre-upload the archive into a kept Box before a later
  command.
- `--script`, `--script-stdin`, `--fresh-pr`, local stdout/stderr captures,
  `--capture-on-fail`, and `--download` are rejected because Upstash owns
  command transport and Crabbox has no SSH target for those paths.
- Forwarded environment values are written to a temporary shell profile in
  `/tmp`, sourced for the command, and removed best-effort.
- `upstashBox.keepAlive` maps to the provider's Box create option. Crabbox
  `--keep` still controls whether a one-shot run deletes the Box afterward.

Related docs:

- [Provider backends](../provider-backends.md)
