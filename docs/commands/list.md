# list

`crabbox list` shows the current Crabbox machines (leases) for a provider. It is
read-only and never provisions or releases anything.

To inspect unverified claim files across every locally recorded provider without
loading a backend or requiring provider credentials, use
[`crabbox claims list`](claims.md). The meanings of bare `list`, `--provider`,
and `--all` remain provider-scoped.

```sh
crabbox list
crabbox list --provider aws
crabbox list --provider ssh --target macos --static-host mac-studio.local
crabbox list --provider cloudflare --refresh
crabbox list --provider hostinger --all
crabbox list --pond alpha
crabbox list --retained
crabbox list --json
crabbox list --jsonl
```

`crabbox pool list` is a compatibility alias for the same command.

## What it lists

The shape of the output depends on the selected `--provider`:

- **Coordinator-backed providers** (`hetzner`, `aws`, `azure`, `daytona`, `gcp` with a broker
  configured) list visible active and provisioning leases by default. Add
  `--retained` to include kept terminal leases whose provider cleanup is not
  confirmed. This applies to text, `--json`, and `--jsonl`. Add `--all` to request
  admin-wide provider inventory when an admin token is configured: machines
  backing live leases first, then every other provider machine (including
  orphans with no live lease) under `other provider inventory:`.
- **Direct cloud / hypervisor / static providers** list the machines the provider
  itself reports. In `provider=ssh` mode this prints the single configured static
  target.
- **Delegated and sandbox providers** (`exe-dev`, `namespace-devbox`, `semaphore`,
  `sprites`, `daytona`, `blaxel`, `islo`, `e2b`, and similar) render through the core lease
  view, so both human output and `--json` use the normalized Crabbox lease shape.

Providers that do not implement listing exit with an error.

Coordinator text output prints its header immediately, then streams rows as
summary pages arrive (at most 100 stored rows per page, including empty filtered
pages). Active leases appear first, then provisioning leases. With `--retained`,
kept terminal leases follow under `retained (cleanup not confirmed):`. These queries run concurrently
with a maximum of four in flight; output keeps that section order and deduplicates
lease IDs. Rows within each section follow the coordinator's keyset page order,
not a global newest-first sort. When stderr is a terminal, a one-line progress
hint appears while more pages are pending.

The default listing stops after the active and provisioning sections without
requesting retained-record pages. When stdout is a terminal, text output prints
one dim hint on stderr after the live sections:
`retained leases hidden; use --retained to include them`. It makes no extra
requests to count hidden records. For `--all`, live machines appear as indexed
admin lease pages arrive, followed by `other provider inventory:` (the pool
inventory is already loaded, so this costs no extra requests). Admin lease lookups follow every page without a 1,000-row cap.
Direct and delegated providers retain their provider-native inventory behavior
and accept but ignore `--retained`.

`--json` keeps its buffered, single-array contract and sorts the ordinary
coordinator lease view newest first, including with `--retained`. `--jsonl` streams
one object per line in the same live-first order as text, without headers or
separators. The two JSON flags
are mutually exclusive and both support `--retained` and `--pond`. Neither JSON
format prints the hidden-record hint. On error, text and
JSONL output may already contain rows; check the command's exit status before
treating the listing as complete. Text and both JSON formats omit
`creationEvents`, `telemetryHistory`, and `provisioningAttempts`; use
`crabbox inspect --provider aws --id <lease-id> --json` for full diagnostics.
Paging is a live view, so leases created or changed during a listing may require
a refresh. Coordinator lease rows retain `lifetimePolicy` in JSON and append
the lifetime summary in text: mode, canary marker, current status, effective
compute-stop deadline, confirmed-stop disk deletion deadline, retention, and
warnings. A missing deletion deadline means compute has not yet been confirmed
stopped. `--all` provider inventory is a resource view; use `admin leases` for
the full administrator lease policy view.

With an older coordinator, the CLI falls back to its newest-first response and
filters to live leases locally, including kept terminal leases only with
`--retained`. At that server's 500-row response cap it warns
that older kept leases may be omitted; update the coordinator or inspect a known
lease by exact ID. Older CLI versions retain their existing list API behavior.

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
Crabbox-owned lease view. Coordinator-backed listings show machines backing live
admin leases first, then the remaining provider inventory, which is where
orphaned machines appear. This is useful for direct providers such as Hostinger,
where an account can contain manually created VPSs alongside Crabbox-created
leases. Providers that do not expose a broader inventory may return the same
lease view they normally print.

## Filtering by pond

`--pond <name>` keeps only leases tagged with the requested pond. Pond names are
normalized like slugs (lowercased, non-alphanumeric runs collapsed to single
dashes), so `--pond "Alpha Pond"` matches a lease created with `--pond alpha-pond`.
See [pond](pond.md) and [docs/features/pond.md](../features/pond.md).

In either JSON mode the pond filter is applied to the same payload the backend emits;
backends whose entries carry no labels return an empty list rather than an
unfiltered one.

## Blacksmith external runners

In `blacksmith-testbox` mode, `list` reads `blacksmith testbox list` and renders the
same Crabbox list shape as other providers. With `--json` it preserves the
compatibility fields parsed from the Blacksmith table — id, status, repo, workflow,
job, ref, and created time — when the upstream table exposes those columns.

When a coordinator is configured, the same command also refreshes external runner
rows from the all-status `blacksmith testbox list --all` inventory (declared as
`inventoryScope: "org"` only when `blacksmith.org` is configured explicitly, so a
login's default org can never stale another org's runners), and best-effort infers the matching GitHub Actions run/workflow
from each row's repo, workflow, ref, and created time. The portal then shows that
Actions status, tags long-queued or long-running workflow owners as `stuck`,
exposes a copyable local stop command, and links each row to a read-only runner
detail page. Runners missing from a later org-scoped sync are marked stale across
owners within that provider/org. The portal deduplicates visible copies by the
newest observation and hides stale rows, including those last seen more than two
hours ago. Sync remains opportunistic when commands run, not a periodic heartbeat;
runners remain visibility-only records rather than Crabbox leases.

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
--retained               include kept terminal coordinator leases; ignored by direct/delegated providers
--json                   print one buffered JSON array
--jsonl                  stream one JSON object per line
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
