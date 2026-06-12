# Islo Provider

Read when:

- choosing `provider: islo`;
- configuring the Islo sandbox image, sizing, snapshot, or gateway profile;
- changing `internal/providers/islo`.

Islo is a delegated-run provider. Crabbox uses the Islo Go SDK for sandbox
lifecycle (create, list, status) and calls the HTTP API directly for delete (an
empty-body `DELETE`), archive upload, shares, and command output — reading a
Server-Sent Events stream from the `POST /sandboxes/{name}/exec/stream` endpoint. Islo
owns sandbox state and command transport; Crabbox owns local config, repo
claims, sync manifests and guardrails, slugs, timing summaries, and normalized
`list`/`status` rendering. There is no Crabbox SSH lease and no broker
coordinator — the CLI talks to Islo directly.

## When to use

Use Islo when the remote Linux sandbox should be owned by Islo and commands run
through Islo's API. Choose AWS, Hetzner, Static SSH, or Daytona instead when you
need Crabbox-managed SSH access to the box.

Islo is Linux-only. Desktop, browser, code, Actions hydration, and SSH-based run
options are not available.

## Commands

```sh
crabbox warmup --provider islo --islo-image docker.io/library/ubuntu:26.04
crabbox run --provider islo -- pnpm test
crabbox run --provider islo --id swift-crab --shell 'pnpm install && pnpm test'
crabbox status --provider islo --id swift-crab --wait
crabbox pause --provider islo swift-crab
crabbox resume --provider islo swift-crab
crabbox stop --provider islo swift-crab
crabbox list --provider islo --json
```

`warmup` keeps the sandbox until an explicit `stop`. The lease ID, slug, or
Crabbox-created sandbox name printed by `warmup`/`run` can be passed to later
commands via `--id`.

## Auth

```sh
export ISLO_API_KEY=ak_...
```

`CRABBOX_ISLO_API_KEY` is also accepted and takes precedence over
`ISLO_API_KEY`. Do not pass the key as a command-line argument.

`ISLO_BASE_URL` (or `CRABBOX_ISLO_BASE_URL`, or `islo.baseUrl`) overrides the
default API base URL `https://api.islo.dev`.

## Config

```yaml
provider: islo
target: linux
islo:
  baseUrl: https://api.islo.dev
  image: docker.io/library/ubuntu:26.04
  workdir: crabbox
  gatewayProfile: ""
  snapshotName: ""
  vcpus: 2
  memoryMB: 4096
  diskGB: 20
```

Provider flags (each overrides the matching `islo.*` config key):

```text
--islo-base-url
--islo-image
--islo-workdir
--islo-gateway-profile
--islo-snapshot-name
--islo-vcpus
--islo-memory-mb
--islo-disk-gb
```

Every key also reads a `CRABBOX_ISLO_*` environment variable, which takes
precedence over the config file: `CRABBOX_ISLO_BASE_URL`, `CRABBOX_ISLO_IMAGE`,
`CRABBOX_ISLO_WORKDIR`, `CRABBOX_ISLO_GATEWAY_PROFILE`,
`CRABBOX_ISLO_SNAPSHOT_NAME`, `CRABBOX_ISLO_VCPUS`, `CRABBOX_ISLO_MEMORY_MB`,
and `CRABBOX_ISLO_DISK_GB`.

The default image follows the selected `--os` (default `ubuntu:26.04` resolves
to `docker.io/library/ubuntu:26.04`). `vcpus`, `memoryMB`, and `diskGB` are only
sent to Islo when greater than zero; otherwise the sandbox uses Islo's defaults.

### Workdir resolution

`--islo-workdir` / `islo.workdir` is a relative directory below `/workspace`
(default `crabbox`, so the workspace is `/workspace/crabbox`). Absolute paths and
`..` escapes are rejected before workspace preparation and sync.

## Lifecycle

1. Create or resolve a Crabbox-owned Islo sandbox. New sandboxes are named
   `crabbox-<repo>-<hex>`; the local lease ID is the sandbox name prefixed with
   `isb_`, paired with a friendly slug.
2. Write a local repo claim binding the lease to the current checkout.
3. Validate the workdir, build the Crabbox sync manifest, and upload a gzipped
   archive into `/workspace/<workdir>` through Islo's files-archive API. If that
   upload fails, Crabbox falls back to a base64 chunked upload over the exec
   endpoint.
4. Execute the command through Islo's streaming exec endpoint in that workdir.
5. Require an `exit` event before treating a stream as successful.
6. Delete the sandbox on release unless the lease is kept.

## Capabilities

- SSH: yes for direct login to existing Crabbox-created sandboxes. `crabbox ssh
  --provider islo --id <slug>` renders `ssh islo@<sandbox>.islo` on port 22
  by default. Crabbox still does not use SSH for Islo `run` or sync.
- Crabbox sync: yes, archive sync through the Islo files-archive API, with a
  base64 exec-upload fallback.
- URL bridge: yes. Exposed ports become public HTTPS shares through Islo's
  `/sandboxes/{name}/shares` API, surfaced by `--expose` and the pond bridge
  plane. Share creation is idempotent per port.
- Pause / resume: yes. `crabbox pause` snapshots the sandbox to disk and frees
  its CPU/memory via Islo's pause API; `crabbox resume` restores it. The lease
  claim is preserved across a pause.
- Desktop / browser / code: no.
- Actions hydration: no.
- Coordinator (broker): no — always direct from the CLI.

## Gotchas

- Direct SSH requires the authenticated Islo CLI and a one-time `islo ssh
  --setup`. The Islo CLI reads `islo login` state or `ISLO_API_KEY`;
  `CRABBOX_ISLO_API_KEY` alone only authenticates Crabbox.
- `--sync-only` and `--checksum` are rejected because `run` still uses Islo's
  delegated archive/exec transport, not Crabbox-managed rsync.
- `--full-resync`, `--force-sync-large`, `--script`, `--script-stdin`,
  `--fresh-pr`, `--env-helper`, local stdout/stderr captures,
  `--capture-on-fail`, `--download`, `--artifact-glob`, `--require-artifact`,
  `--emit-proof`, and `--stop-after` are rejected because Islo owns sync and
  command transport in delegated-run mode.
- `--keep-on-failure` keeps a newly created failed sandbox until an explicit
  `stop` or provider-side expiry.
- Large-sync guardrails still apply. Because `--force-sync-large` is rejected,
  trim the checkout (more `.gitignore`/sync excludes) when a sync trips the
  large-archive guardrail.
- `--shell` passes the raw shell string to `bash -lc` in the workdir.
- `--id` accepts a Crabbox slug, an `isb_<name>` lease ID, or a Crabbox-created
  sandbox name (one starting with `crabbox-`). Sandboxes not created by Crabbox
  are rejected.

Related docs:

- [Feature: Islo](../features/islo.md)
- [Provider backends](../provider-backends.md)
