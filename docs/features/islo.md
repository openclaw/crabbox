# Islo

Read when:

- choosing `provider: islo`;
- configuring the Islo sandbox image, sizing, snapshot, or gateway profile;
- reviewing how Crabbox behaves on a delegated-run provider.

`provider: islo` is a delegated-run provider: Islo owns the sandbox and the
command transport, while Crabbox owns local config, repo claims, the sync
manifest and its guardrails, slugs, timing summaries, and normalized
`list`/`status` rendering. Crabbox uses the Islo Go SDK for auth and sandbox
lifecycle (create, list, status) and calls the HTTP API directly for stop (an
empty-body `DELETE`), archive upload, shares, and command output — the last via
a small SSE reader for the `POST /sandboxes/{name}/exec/stream` endpoint, since
the SDK's exec helper coalesces streamed output.

Sandboxes are Linux-only. There is no Crabbox-managed SSH lease; commands run
through Islo's streaming exec endpoint, not through `crabbox ssh`/rsync.

## Auth

```sh
export ISLO_API_KEY=ak_...
```

`ISLO_BASE_URL` (or `islo.baseUrl`) overrides the default `https://api.islo.dev`.
Both keys also accept the `CRABBOX_`-prefixed forms `CRABBOX_ISLO_API_KEY` and
`CRABBOX_ISLO_BASE_URL`, which take precedence.

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

Defaults: `baseUrl` `https://api.islo.dev`, `workdir` `crabbox`, `vcpus` `2`,
`memoryMB` `4096`, `diskGB` `20`. The image default comes from the resolved OS
target (the default OS `ubuntu:26.04` resolves to `docker.io/library/ubuntu:26.04`).

`islo.workdir` is a relative directory name under `/workspace`. Absolute paths
and `..` escapes are rejected before Crabbox prepares or syncs the workspace, so
the working set always lands in `/workspace/<islo.workdir>`.

Each config key has an equivalent flag and `CRABBOX_ISLO_*` environment
variable:

| Config key       | Flag                     | Env var                        |
| ---------------- | ------------------------ | ------------------------------ |
| `baseUrl`        | `--islo-base-url`        | `CRABBOX_ISLO_BASE_URL`        |
| `image`          | `--islo-image`           | `CRABBOX_ISLO_IMAGE`           |
| `workdir`        | `--islo-workdir`         | `CRABBOX_ISLO_WORKDIR`         |
| `gatewayProfile` | `--islo-gateway-profile` | `CRABBOX_ISLO_GATEWAY_PROFILE` |
| `snapshotName`   | `--islo-snapshot-name`   | `CRABBOX_ISLO_SNAPSHOT_NAME`   |
| `vcpus`          | `--islo-vcpus`           | `CRABBOX_ISLO_VCPUS`           |
| `memoryMB`       | `--islo-memory-mb`       | `CRABBOX_ISLO_MEMORY_MB`       |
| `diskGB`         | `--islo-disk-gb`         | `CRABBOX_ISLO_DISK_GB`         |

```sh
crabbox warmup --provider islo --islo-image docker.io/library/ubuntu:26.04
crabbox run --provider islo -- pnpm test
crabbox status --provider islo --id blue-lobster
crabbox stop --provider islo blue-lobster
```

## Behavior

- **warmup** creates a `crabbox-...` Islo sandbox and records a local lease ID of
  the form `isb_<sandbox-name>` plus a Crabbox slug.
- **run** creates or reuses a sandbox, validates `islo.workdir`, builds the
  Crabbox sync manifest, uploads it as a gzipped archive into
  `/workspace/<islo.workdir>`, streams stdout/stderr from Islo's SSE exec
  endpoint, and returns the remote exit code. A stream is only treated as
  successful once an exit event arrives.
- **list** and **status** go through the Islo SDK; **stop** issues a direct
  `DELETE`. All three act only on Crabbox-created sandboxes. Identifiers may be a
  Crabbox slug, an `isb_...` lease ID, or a Crabbox-created sandbox name;
  non-Crabbox sandboxes are rejected.
- The sandbox is deleted on release unless kept. `--keep-on-failure` keeps a
  newly created failed sandbox until an explicit `stop` or provider-side expiry.

## URL bridge (per-port shares)

Islo declares the `url-bridge` capability. Crabbox publishes a per-port public
HTTPS share for an exposed sandbox port via Islo's
`POST /sandboxes/{name}/shares` API and reuses an existing share for the same
port when one is present. This is how delegated providers surface a reachable
URL in place of an SSH-tunneled bridge.

## Rejected options

Because Islo owns command transport and there is no Crabbox-managed SSH/rsync
target, these `run` options are rejected:

- `--sync-only`, `--checksum`, `--force-sync-large`, `--full-resync` — no
  Crabbox rsync target to drive.
- `--script`, `--script-stdin`, `--fresh-pr`, local stdout/stderr captures,
  `--capture-on-fail`, `--download`, `--artifact-glob`, `--require-artifact`,
  `--env-helper`, `--stop-after` — these require Crabbox-owned transport or
  execution.

Large-sync guardrails still apply: the gzipped archive upload runs the same
size preflight as rsync providers, but because `--force-sync-large` is rejected
on Islo, an oversize sync cannot be forced through and fails the preflight
instead. `--shell` passes the raw shell string through to the remote shell.

## SSH access

Crabbox does not provision or route SSH to Islo sandboxes: `crabbox ssh`, `vnc`,
`code`, rsync, and Actions hydration are not available on `provider: islo`. When
you need a Crabbox-managed SSH box, use Hetzner, AWS, static SSH, or Daytona
instead.

## Related docs

- [Provider: Islo](../providers/islo.md)
- [Provider backends](../provider-backends.md)
