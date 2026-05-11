# Islo

Read when:

- choosing `provider: islo`;
- configuring Islo sandbox image, sizing, or gateway profile;
- reviewing delegated provider behavior.

`provider: islo` delegates sandbox setup and command execution to Islo. Crabbox
uses the Islo Go SDK for auth, sandbox lifecycle, list, status, and stop. It
builds the normal Crabbox sync manifest and uploads it as a gzipped archive into
the sandbox workdir before executing the command. The SDK's current exec stream
helper coalesces output, so Crabbox keeps a small SSE reader for
`POST /sandboxes/{name}/exec/stream` while still using the SDK auth provider.

## Auth

```sh
export ISLO_API_KEY=ak_...
```

`ISLO_BASE_URL` or `islo.baseUrl` can override the default
`https://api.islo.dev`.

## Config

```yaml
provider: islo
target: linux
islo:
  image: docker.io/library/ubuntu:24.04
  workdir: crabbox
  gatewayProfile: ""
  snapshotName: ""
  vcpus: 2
  memoryMB: 4096
  diskGB: 20
```

`islo.workdir` must be a relative directory name under `/workspace`. Absolute
paths and `..` escapes are rejected before Crabbox prepares or syncs the
sandbox workspace.

Equivalent flags:

```sh
crabbox warmup --provider islo --islo-image docker.io/library/ubuntu:24.04
crabbox run --provider islo -- pnpm test
crabbox status --provider islo --id <slug>
crabbox stop --provider islo <slug>
```

## Behavior

- `warmup` creates a `crabbox-...` Islo sandbox and stores a local lease ID of
  the form `isb_<crabbox-sandbox-name>` plus a Crabbox slug.
- `run` creates or reuses a sandbox, validates `islo.workdir` as a relative
  directory under `/workspace`, syncs the local Git-managed working set into
  `/workspace/<islo.workdir>`, streams stdout/stderr from Islo's SSE exec
  endpoint, and returns the remote exit code.
- `--sync-only` and `--checksum` are rejected because the Crabbox `provider:
  islo` backend does not yet drive a Crabbox-managed SSH/rsync target. Large-sync
  guardrails still apply, and `--force-sync-large` is honored for intentional
  large archive syncs.
- `list`, `status`, and `stop` use the Islo SDK and return core-rendered
  Crabbox views for Crabbox-created sandboxes only.

Islo sandboxes are reachable interactively over SSH using Islo's own host alias
(`ssh <sandbox-name>.islo` after `islo ssh --setup`); see
[Provider: Islo → SSH access](../providers/islo.md#ssh-access) for the recipe.
Crabbox itself is not yet wired to drive `crabbox ssh`, `vnc`, `code`, or
Actions runner hydration through that path — for those, use Hetzner, AWS,
static SSH, or Daytona.
