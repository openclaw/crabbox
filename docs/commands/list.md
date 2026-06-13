# list

`crabbox list` shows the current Crabbox machines (leases) for a provider. It is
read-only and never provisions or releases anything.

```sh
crabbox list
crabbox list --provider aws
crabbox list --provider ssh --target macos --static-host mac-studio.local
crabbox list --provider cloudflare --refresh
crabbox list --provider hostinger --all
crabbox list --pond alpha
crabbox list --json
```

`crabbox pool list` is a compatibility alias for the same command.

## What it lists

The shape of the output depends on the selected `--provider`:

- **Coordinator-backed providers** (`hetzner`, `aws`, `azure`, `gcp` with a broker
  configured) list the leases the broker tracks for you.
- **Direct cloud / hypervisor / static providers** list the machines the provider
  itself reports. In `provider=ssh` mode this prints the single configured static
  target.
- **Delegated and sandbox providers** (`exe-dev`, `namespace-devbox`, `semaphore`,
  `sprites`, `daytona`, `islo`, `e2b`, and similar) render through the core lease
  view, so both human output and `--json` use the normalized Crabbox lease shape.

Providers that do not implement listing exit with an error.

## Refreshing provider state

`--refresh` asks providers that keep local claims to check live runner state before
printing. Without it, those providers report only their local claims and stay
credential-free. For example, `crabbox list --provider cloudflare` reports local
claims by default; add `--refresh` to query live container state.
`crabbox list --provider cloudflare-dynamic-workers` follows the same local-claim
model for Dynamic Workers run metadata; `--refresh` asks the loader about each
claimed run ID and reports missing metadata as a stale local claim.

## Including full provider inventory

`--all` asks providers that support it to include inventory outside the
Crabbox-owned lease view. This is useful for direct providers such as Hostinger,
where an account can contain manually created VPSs alongside Crabbox-created
leases. Providers that do not expose a broader inventory may return the same
lease view they normally print.

## Filtering by pond

`--pond <name>` keeps only leases tagged with the requested pond. Pond names are
normalized like slugs (lowercased, non-alphanumeric runs collapsed to single
dashes), so `--pond "Alpha Pond"` matches a lease created with `--pond alpha-pond`.
See [pond](pond.md) and [docs/features/pond.md](../features/pond.md).

In `--json` mode the pond filter is applied to the same payload the backend emits;
backends whose entries carry no labels return an empty list rather than an
unfiltered one.

## Blacksmith external runners

In `blacksmith-testbox` mode, `list` reads `blacksmith testbox list` and renders the
same Crabbox list shape as other providers. With `--json` it preserves the
compatibility fields parsed from the Blacksmith table — id, status, repo, workflow,
job, ref, and created time — when the upstream table exposes those columns.

When a coordinator is configured, the same command also refreshes owner-scoped
external runner rows in the portal lease table from the current all-status
Blacksmith list, and best-effort infers the matching GitHub Actions run/workflow
from each row's repo, workflow, ref, and created time. The portal then shows that
Actions status, tags long-queued or long-running workflow owners as `stuck`,
exposes a copyable local stop command, and links each row to a read-only runner
detail page. Runners missing from a later sync are marked stale rather than treated
as Crabbox leases.

## Flags

```text
--provider <name>        provider to list (see crabbox providers for the full set)
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>     provider=ssh: host of the static target
--static-user <user>     provider=ssh: SSH user
--static-port <port>     provider=ssh: SSH port
--static-work-root <path> provider=ssh: remote work root
--all                    include provider inventory outside Crabbox-owned leases where supported
--refresh                query live provider state where supported
--pond <name>            only list leases tagged with this pond
--json                   print JSON
```

Provider-specific connection overrides are also accepted when listing that
provider, including:

```text
--exe-dev-control-host <host>
--sprites-api-url <url>
--azure-dynamic-sessions-endpoint <url>
--azure-dynamic-sessions-api-version <version>
--e2b-api-url <url>
--e2b-domain <domain>
```

The `--provider` value accepts any registered provider name or alias; run
[`crabbox providers`](providers.md) to see the full set.
