# E2B Provider

Read when:

- choosing `provider: e2b`;
- configuring E2B templates, sandbox users, or workdirs;
- changing `internal/providers/e2b`.

E2B is a delegated-run provider. Crabbox uses E2B's public sandbox REST API for
sandbox lifecycle and the per-sandbox `envd` APIs for file upload and command
execution. E2B owns sandbox state and process transport; Crabbox owns local
config, repo claims, sync manifests and guardrails, slugs, timing summaries, and
normalized `list`/`status` rendering. There is no Crabbox SSH lease and no broker
coordinator — the CLI talks to E2B directly.

## When to use

Use E2B when the remote Linux sandbox should be owned by E2B and commands run
through the E2B sandbox APIs. Choose AWS, Hetzner, Static SSH, or Daytona instead
when you need direct SSH access to the box.

E2B is Linux-only. Desktop, browser, code, Actions hydration, and SSH-based run
options are not available.

## Commands

```sh
crabbox warmup --provider e2b --e2b-template base
crabbox run --provider e2b -- pnpm test
crabbox run --provider e2b --id swift-crab --shell 'pnpm install && pnpm test'
crabbox status --provider e2b --id swift-crab --wait
crabbox stop --provider e2b swift-crab
crabbox list --provider e2b --json
```

`warmup` always keeps the sandbox until an explicit `stop`. The lease ID, slug,
or E2B sandbox ID printed by `warmup`/`run` can be passed to later commands via
`--id`.

## Auth

```sh
export E2B_API_KEY=e2b_...
```

`CRABBOX_E2B_API_KEY` is also accepted and takes precedence over `E2B_API_KEY`.
Do not pass the key as a command-line argument.

Endpoint overrides:

- `CRABBOX_E2B_API_URL` / `E2B_API_URL` or `e2b.apiUrl` override the default API
  URL `https://api.e2b.app`. Overrides must use HTTPS; plain HTTP is accepted
  only for localhost or loopback development endpoints.
- `CRABBOX_E2B_DOMAIN` / `E2B_DOMAIN` or `e2b.domain` override the default sandbox
  domain `e2b.app`.

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

Provider flags (each overrides the matching `e2b.*` config key):

```text
--e2b-api-url
--e2b-domain
--e2b-template
--e2b-workdir
--e2b-user
```

`template` also reads `CRABBOX_E2B_TEMPLATE`, `workdir` reads
`CRABBOX_E2B_WORKDIR`, and `user` reads `CRABBOX_E2B_USER`.

### Workdir and user resolution

A relative `e2b.workdir` resolves inside the selected E2B user's home: the
default user home is `/home/user`, `user: ubuntu` resolves under `/home/ubuntu`,
and `user: root` resolves under `/root`. An absolute `workdir` is used as-is.

`e2b.user` must be a login name, not a path. Values containing `/`, `\`, `.`,
`..`, or a null byte (for example `../tmp` or `team/dev`) are rejected before any
sandbox or process call.

The resolved workdir must be a dedicated subdirectory. Broad system roots such as
`/`, `/home`, `/tmp`, `/root`, `/usr`, `/var`, `/etc`, and similar are rejected
before sandbox creation and before any sync that creates, deletes, or extracts
files.

## Lifecycle

1. Create or resolve a Crabbox-owned E2B sandbox from `e2b.template` (default
   `base`), with internet access enabled.
2. Store Crabbox metadata on the sandbox and write a local repo claim bound to
   the exact API endpoint and sandbox ID.
3. Build the Crabbox sync manifest, upload a gzipped archive into `/tmp`, and
   extract it into the resolved workdir.
4. Execute the command through the E2B process stream in that workdir.
5. Delete the sandbox on release unless the lease is kept.

E2B caps sandbox timeouts at one hour. Crabbox clamps a longer local lease TTL to
that limit when creating or connecting to a sandbox, and a TTL of zero falls back
to five minutes.

## Capabilities

- SSH: no.
- Crabbox sync: yes, archive sync through the E2B file and process APIs.
- Desktop / browser / code: no.
- Actions hydration: no.
- Coordinator (broker): no — always direct from the CLI.

## Live smoke

Run a live smoke when changing E2B lifecycle, sync, status, or process-stream
code. Keep the API key in the environment; never pass it as an argument.

```sh
export E2B_API_KEY=e2b_...
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=e2b \
CRABBOX_LIVE_REPO=/path/to/my-app \
scripts/live-smoke.sh
```

The shared harness exits before any E2B `warmup`, `status`, `run`, `list`, or
`stop` command when no API key is exported. With the key configured, it creates
one E2B sandbox from the selected template, waits for readiness, runs one
no-sync command, lists normalized E2B inventory, and stops the lease.

For manual debugging, run the same lifecycle directly:

```sh
export E2B_API_KEY=e2b_...
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox warmup --provider e2b --e2b-template base --timing-json
lease=<slug-or-cbx_id-from-warmup-output>

bin/crabbox status --provider e2b --id "$lease" --wait
bin/crabbox run --provider e2b --id "$lease" --no-sync -- echo crabbox-e2b-ok
bin/crabbox stop --provider e2b "$lease"
bin/crabbox list --provider e2b --json
```

Expected results:

- `warmup` prints `provider=e2b`, the Crabbox lease ID, slug, and E2B sandbox ID.
- `status --wait` reports the sandbox as ready.
- The no-sync run prints `crabbox-e2b-ok`.
- `stop` deletes the sandbox and removes the local lease claim.
- The final `list` shows no Crabbox-owned sandbox remaining.

## Gotchas

- `--id` accepts a Crabbox slug, a `cbx_...` lease ID, or an E2B sandbox ID in
  raw or `e2b_<sandboxID>` form. Destructive cleanup requires an exact local
  endpoint-and-sandbox claim. To recover a Crabbox-owned sandbox whose claim is
  missing or legacy, inspect the exact sandbox ID and pass `stop --reclaim`;
  Crabbox revalidates its canonical lease metadata before adopting and deleting
  it.
- `--class` and `--type` are rejected; the E2B template owns sandbox resources.
- `--checksum` is rejected because there is no Crabbox SSH/rsync target. Large-
  sync preflight guardrails still apply.
- Because E2B does not declare archive-sync as a generic feature, the shared
  delegated guards reject `--sync-only` and `--force-sync-large`, along with
  `--full-resync`, `--script`/`--script-stdin`, `--fresh-pr`, `--env-helper`,
  local stdout/stderr captures (`--capture-stdout`, `--capture-stderr`,
  `--capture-on-fail`), `--download`, `--artifact-glob`, `--require-artifact`,
  `--emit-proof`, and `--stop-after`. Crabbox owns command transport for E2B and
  has no SSH target for those paths.
- `--keep-on-failure` keeps a newly created sandbox after a failed command
  instead of deleting it, subject to the one-hour sandbox timeout.
- To prove cleanup in a smoke, run `crabbox list --provider e2b --json` after a
  `stop` or a non-kept run and confirm no Crabbox-owned sandbox remains.

Related docs:

- [Feature: E2B](../features/e2b.md)
- [Provider backends](../provider-backends.md)
