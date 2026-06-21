# Upstash Box Provider

Read this when you:

- choose `provider: upstash-box`;
- configure the Upstash Box runtime, size, base-URL endpoint, or working
  directory;
- change `internal/providers/upstashbox`.

[Upstash Box](https://upstash.com/docs/box/overall/quickstart) is a **delegated-run** provider. Crabbox calls the Upstash Box REST
API to manage Box lifecycle (create, get, list, delete), upload files, and run
commands over its exec / exec-stream endpoints. Upstash owns the sandbox state
and process transport; Crabbox owns local config, repo claims, archive sync and
guardrails, slugs, timing summaries, and the normalized `list`/`status` output.
There is no SSH target.

## When to use

Use Upstash Box when commands should run in Upstash-managed ephemeral Linux
sandboxes and you do not need a Crabbox SSH box. Reach for a provisioned SSH
provider (AWS, Hetzner, Azure, GCP, static SSH) or another SSH-capable delegated
provider instead when you need `crabbox ssh`, VNC, code-server, or Actions
hydration — none of those surfaces exist on Upstash Box.

## Prerequisites

- Create an Upstash Box API key.
- Export it as `UPSTASH_BOX_API_KEY` or `CRABBOX_UPSTASH_BOX_API_KEY`. Crabbox
  never accepts the key as a command-line flag.

## Auth

Crabbox sends the API key as the `X-Box-Api-Key` request header. Provide it
through the environment:

```sh
export UPSTASH_BOX_API_KEY=...
# or
export CRABBOX_UPSTASH_BOX_API_KEY=...
```

Crabbox redacts the configured API key from Upstash Box HTTP error bodies and
exec-stream error events before displaying diagnostics.

Rotate the key if it was ever pasted into a chat, shell history, issue, or log.

## Commands

```sh
crabbox warmup --provider upstash-box --upstash-box-runtime node --upstash-box-size small
crabbox run --provider upstash-box -- pnpm test
crabbox run --provider upstash-box --id swift-crab --shell 'pnpm install && pnpm test'
crabbox status --provider upstash-box --id swift-crab
crabbox stop --provider upstash-box swift-crab
```

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

Defaults: base URL `https://us-east-1.box.upstash.com`, runtime `node`, size
`small`, workdir `/workspace/home/crabbox`, `keepAlive` false.

Accepted values, validated before the API is called:

- `runtime`: `node`, `python`, `golang`, `ruby`, `rust`, or the Alpine variants
  `node-alpine`, `python-alpine`, `golang-alpine`, `ruby-alpine`,
  `rust-alpine`.
- `size`: `small`, `medium`, or `large`.

## Lifecycle

1. `warmup` / `run` without `--id` creates a Box named
   `crabbox-<slug>-<lease-hex>` with the configured runtime and size, then waits
   (up to 5 minutes) until the Box reports a ready status (`idle`, `running`,
   `ready`, or `paused`). Crabbox stores a local claim with a normal `cbx_...`
   lease ID and a friendly slug.
2. By default `run` archive-syncs the working tree: Git manifest → local
   `tar -czf` → upload into the Box workspace as
   `.crabbox-upstash-box-sync-*.tgz` → in-Box `tar -xzf` into the workdir
   (the temp archive is removed afterward).
3. The user command runs through the Box exec-stream endpoint wrapped in
   `sh -c`, with the workspace folder set as the working directory, streaming
   output back through Crabbox.
4. One-shot Boxes are deleted after a `run` that did not pass `--keep`. `--keep`
   and `--keep-on-failure` retain the Box until `crabbox stop`.

Note: `warmup` always keeps the Box until an explicit `crabbox stop`. If you
pass `--keep=false` to `warmup`, Crabbox prints a warning and still keeps it.

## Capabilities

- SSH: no.
- Crabbox sync: yes — archive sync through Box file upload + exec.
- Provider sync: no separate Upstash sync step.
- Desktop / browser / code: no.
- Actions hydration: no.
- Coordinator (broker): no — Upstash Box always runs direct from the CLI.

## Gotchas

- IDs can be a Crabbox slug, a `cbx_...` lease ID, or a raw Upstash Box ID (when
  that Box carries Crabbox naming, i.e. `crabbox-<slug>-<hex>`).
- `--class` and `--type` are rejected; use `--upstash-box-size` and
  `--upstash-box-runtime`.
- `upstashBox.workdir` must resolve to an absolute path **under**
  `/workspace/home/`. Broad or system roots — `/`, `/bin`, `/dev`, `/etc`,
  `/home`, `/lib`, `/lib64`, `/opt`, `/proc`, `/root`, `/sbin`, `/sys`, `/tmp`,
  `/usr`, `/var`, `/workspace`, and `/workspace/home` — are rejected before any
  sync or command runs.
- `--checksum` is rejected because Upstash Box has no SSH/rsync target.
  Large-sync guardrails still apply; `--force-sync-large` is honored for
  intentional large archive syncs.
- Use `--sync-only` to pre-upload the archive into a kept Box before a later
  command.
- Delegated run/sync options that need an SSH target or proof surface are
  rejected: `--script` / `--script-stdin`, `--fresh-pr`, `--full-resync`,
  `--env-helper`, `--capture-stdout` / `--capture-stderr`, `--capture-on-fail`,
  `--download`, `--artifact-glob`, `--require-artifact`, `--emit-proof`, and
  `--stop-after`.
- Forwarded environment values are written to a temporary shell profile in the
  Box workspace, sourced (`set -a`) for the command, and removed best-effort
  afterward. They are never placed on the local process argv.
- `upstashBox.keepAlive` maps to the Box create `keep_alive` option. Crabbox
  `--keep` independently controls whether a one-shot `run` deletes the Box
  afterward.

## Related docs

- [Crabbox setup guide](https://upstash.com/docs/box/guides/crabbox-setup) — Upstash's walkthrough for running Crabbox on Upstash Box.
- [Provider backends](../provider-backends.md)
