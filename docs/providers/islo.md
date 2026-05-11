# Islo Provider

Read when:

- choosing `provider: islo`;
- configuring Islo sandbox image, size, snapshot, or gateway profile;
- changing `internal/providers/islo`.

Islo is a delegated run provider. Crabbox uses the Islo SDK for sandbox
lifecycle and a streaming exec endpoint for command output. Islo owns sandbox
state and command transport; Crabbox owns local config, repo claims, sync
manifests and guardrails, slugs, timing summaries, and normalized list/status
rendering.

## When To Use

Use Islo when the remote sandbox should be owned by Islo and command execution
should happen through Islo's API. Use AWS, Hetzner, Static SSH, or Daytona when
you need Crabbox SSH access.

## Commands

```sh
crabbox warmup --provider islo --islo-image docker.io/library/ubuntu:24.04
crabbox run --provider islo -- pnpm test
crabbox run --provider islo --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider islo --id blue-lobster
crabbox stop --provider islo blue-lobster
```

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
  baseUrl: https://api.islo.dev
  image: docker.io/library/ubuntu:24.04
  workdir: crabbox
  gatewayProfile: ""
  snapshotName: ""
  vcpus: 2
  memoryMB: 4096
  diskGB: 20
```

Provider flags:

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

`--islo-workdir` / `islo.workdir` is interpreted as a relative directory below
`/workspace`. Crabbox rejects absolute paths and `..` escapes before workspace
preparation and sync.

## Lifecycle

1. Create or resolve a Crabbox-owned Islo sandbox.
2. Store a local lease ID with the `isb_` prefix and a friendly slug.
3. Validate the Islo workdir, build the Crabbox sync manifest, and upload a
   gzipped archive into `/workspace/<islo.workdir>`.
4. Execute commands through Islo's streaming exec endpoint in that workdir.
5. Require an exit event before treating a stream as successful.
6. Delete the sandbox on release unless kept.

## Capabilities

- SSH: not driven by Crabbox. Islo sandboxes are reachable from the host with
  the OS `ssh` client via the `<sandbox-name>.islo` host alias after a one-time
  `islo ssh --setup` (see [SSH access](#ssh-access) below). Crabbox itself does
  not yet route `crabbox ssh`, sync, or run through that path.
- Crabbox sync: yes, archive sync through the Islo API or chunked exec fallback.
- Provider sync: no separate Islo CLI sync.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Actions hydration: no.
- Coordinator: no.

## SSH access

Islo provisions a per-sandbox SSH endpoint and configures `~/.ssh/config` for
you. The sandbox name (the Crabbox-created `crabbox-...` name, or any other
slug shown by `islo ls`) is the SSH host:

```sh
islo ssh --setup            # one-time, idempotent; edits ~/.ssh/config
ssh <sandbox-name>.islo     # interactive shell on the sandbox
ssh <sandbox-name>.islo pnpm test       # one-shot remote command
```

This is useful for ad-hoc inspection of a Crabbox-created Islo sandbox while
`provider: islo` still uses the streaming exec endpoint for `crabbox run`.
Certificates are minted automatically and cached by the Islo CLI; no key files
need to be plumbed into Crabbox.

## Gotchas

- `--sync-only` and `--checksum` are rejected because the `provider: islo`
  backend does not yet expose a Crabbox-managed SSH/rsync target, even though
  the sandbox is independently reachable with `ssh <sandbox-name>.islo`.
- Large-sync guardrails still apply. Use `--force-sync-large` when a large Islo
  archive sync is intentional.
- `--shell` passes the raw shell string to the remote shell path.
- IDs can be Crabbox slugs, `isb_...` lease IDs, or Crabbox-created sandbox
  names. Non-Crabbox Islo sandboxes are rejected.

Related docs:

- [Feature: Islo](../features/islo.md)
- [Provider backends](../provider-backends.md)
