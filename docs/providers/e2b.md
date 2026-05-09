# E2B Provider

Read when:

- choosing `provider: e2b`;
- configuring E2B templates or sandbox workdirs;
- changing `internal/providers/e2b`.

E2B is a delegated run provider. Crabbox uses E2B's public sandbox REST API for
sandbox lifecycle and the sandbox envd APIs for file upload and command
execution. E2B owns sandbox state and process transport; Crabbox owns local
config, repo claims, sync manifests and guardrails, slugs, timing summaries, and
normalized list/status rendering.

## When To Use

Use E2B when the remote Linux sandbox should be owned by E2B and commands can
run through the E2B sandbox APIs. Use AWS, Hetzner, Static SSH, or Daytona when
you need Crabbox SSH access.

## Commands

```sh
crabbox warmup --provider e2b --e2b-template base
crabbox run --provider e2b -- pnpm test
crabbox run --provider e2b --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider e2b --id blue-lobster
crabbox stop --provider e2b blue-lobster
```

## Auth

```sh
export E2B_API_KEY=e2b_...
```

`E2B_API_URL` or `e2b.apiUrl` can override the default
`https://api.e2b.app`. `E2B_DOMAIN` or `e2b.domain` can override the default
sandbox domain `e2b.app`.

## Config

```yaml
provider: e2b
target: linux
e2b:
  apiUrl: https://api.e2b.app
  domain: e2b.app
  template: base
  workdir: crabbox
  user: ""
```

Provider flags:

```text
--e2b-api-url
--e2b-domain
--e2b-template
--e2b-workdir
--e2b-user
```

## Lifecycle

1. Create or resolve a Crabbox-owned E2B sandbox from `e2b.template`.
2. Store Crabbox metadata and a local repo claim.
3. Build the Crabbox sync manifest, upload a gzipped archive into `/tmp`, and
   extract it into `/home/user/<e2b.workdir>` or an absolute configured workdir.
4. Execute commands through E2B's process stream in that workdir.
5. Delete the sandbox on release unless the lease is kept.

## Capabilities

- SSH: no.
- Crabbox sync: yes, archive sync through E2B file and command APIs.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- IDs can be Crabbox slugs, `cbx_...` lease IDs, or E2B sandbox IDs in raw or
  `e2b_<sandboxID>` form.
- Raw and synthetic E2B sandbox IDs are accepted only when the sandbox metadata
  marks it as Crabbox-owned.
- `--class` and `--type` are rejected because E2B template contents own sandbox
  resources.
- E2B workdirs must resolve to dedicated absolute directories. Broad roots such
  as `/`, `/home`, and `/tmp` are rejected before sync creates, deletes, or
  extracts files.
- `--checksum` is rejected because E2B does not expose a Crabbox SSH/rsync
  target. Large-sync guardrails still apply, and `--force-sync-large` is
  honored for intentional large archive syncs.

Related docs:

- [Feature: E2B](../features/e2b.md)
- [Provider backends](../provider-backends.md)
