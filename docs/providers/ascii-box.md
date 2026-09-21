# Boat Provider (formerly ASCII Box)

Read when:

- choosing `provider: boat` (preferred) or the legacy `provider: ascii-box`;
- configuring the Boat / ASCII Box API endpoint or workdir;
- changing `internal/providers/asciibox`.

> **Rename note (September 2026):** ASCII renamed **Box** to **Boat**. Crabbox
> accepts `boat` as the current user-facing provider selector. The historical
> `ascii-box`, `ascii`, and `asciibox` names remain compatible so existing
> configuration and durable lease state continue to resolve safely.
>
> The canonical provider id stays `ascii-box` so durable lease and claim state
> keeps resolving. Crabbox reads both the renamed `sandbox`/`sandboxes` CLI JSON
> envelopes and the legacy `box`/`boxes` ones, and prefers the `boat` binary when
> the legacy `box` command is not installed, so one build works across the
> migration.

[Boat](https://boat.dev) (by ASCII) provides persistent Ubuntu sandbox VMs.
Crabbox currently uses the documented legacy `box --json` automation surface as
the control plane, lets `box ssh` prepare the CLI-managed SSH key, and then runs
normal Crabbox sync and commands over SSH. The provider does not depend on
private exec, upload, or command-stream REST endpoints.

## When To Use

Use Boat when commands should run in ASCII-managed Ubuntu sandboxes through the
provider's SSH endpoint. Use a delegated provider such as
[Upstash Box](https://upstash.com/docs/box/overall/quickstart), Modal, E2B, Islo,
or Cloudflare when the provider owns command execution instead of exposing SSH.

## Prerequisites

- Create a Boat/ASCII account. Current product home: <https://boat.dev>.
- Export the existing API key as `ASCII_BOX_API_KEY` or
  `CRABBOX_ASCII_BOX_API_KEY` while the Crabbox adapter remains on the legacy
  compatibility contract.
- Install the compatible `box` CLI. Crabbox discovers the platform-specific
  config path through `box status --json`, writes a private config from the API
  key under its state directory, and does not require a pre-existing `box login`.

The current Boat website advertises a new `boat` CLI. Migrating Crabbox's native
CLI invocation, environment names, API base URL, config keys, and persisted claim
identity should be done as a separately verified compatibility change rather than
silently changing all of those boundaries at once.

## Commands

```sh
crabbox warmup --provider boat
crabbox run --provider boat -- pnpm test
crabbox run --provider boat --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider boat --id blue-lobster
crabbox stop --provider boat blue-lobster
```

Legacy selectors remain accepted:

```text
ascii-box
ascii
asciibox
```

## Auth

```sh
export ASCII_BOX_API_KEY=...
```

`CRABBOX_ASCII_BOX_BASE_URL` or `asciiBox.baseUrl` can override the current
legacy adapter default `https://ascii.dev`. Custom endpoints require HTTPS,
except literal loopback hosts may use HTTP. Userinfo, queries, and fragments are
rejected before the API key is written to CLI configuration or passed to the
CLI.

`BOX_ORG` selects an organization by ID or name. Without it, Crabbox explicitly
uses `personal`; the native CLI's sticky organization selection is not inherited.
Keep the same endpoint and organization selector when reusing or stopping a lease.

## Config

Preferred provider selector with the current compatibility configuration block:

```yaml
provider: boat
target: linux
asciiBox:
  baseUrl: https://ascii.dev
  cliPath: box
  workdir: /home/user/crabbox
```

Provider flags (legacy compatibility names):

```text
--ascii-box-base-url
--ascii-box-cli
--ascii-box-workdir
```

Environment overrides (legacy compatibility names):

```text
CRABBOX_ASCII_BOX_API_KEY / ASCII_BOX_API_KEY
CRABBOX_ASCII_BOX_BASE_URL / ASCII_BOX_BASE_URL
CRABBOX_ASCII_BOX_CLI / BOX_CLI
CRABBOX_ASCII_BOX_HOME
CRABBOX_ASCII_BOX_WORKDIR
BOX_ORG
```

## Lifecycle

1. `crabbox warmup --provider boat` creates a sandbox through `box new --json`,
   verifies the original Box ID and creation timestamp through `box info`, stores
   an exact local ownership claim before preparing the SSH key with
   `box ssh <id> -- true`, waits for SSH, and keeps the sandbox until
   `crabbox stop`. The default SSH key lives in the private CLI home
   (`CRABBOX_ASCII_BOX_HOME`, otherwise Crabbox state).
2. `crabbox run --provider boat` provisions a sandbox for one run, or reuses an
   existing claimed lease/slug/Box ID, then uses the standard SSH sync and run
   path. Reuse requires a matching endpoint, organization, Box ID, and creation
   timestamp. `--reclaim` may transfer repository ownership of an already owned
   lease; it does not adopt an unclaimed sandbox.
3. `crabbox status` resolves the local lease claim or raw Box id and reads native
   state through `box info --json`.
4. `crabbox stop` requires the exact, unchanged local claim and freshly matching
   native identity before remote teardown or each native mutation. It releases
   the sandbox with `box stop --json`, requests deletion with
   `box delete --json --yes`, and validates the returned deletion operation's ID,
   kind, and exact Box target. Crabbox polls
   `box deletion status <operation-id>` while the operation is pending,
   processing, or blocked. It removes the claim only after that exact operation
   reports `completed` with a valid completion timestamp and complete
   `box list --all` inventory confirms absence. A successful native process exit
   alone does not prove deletion completion. The claim stays locked through
   teardown, deletion, retries, confirmation, and local removal. If the service
   temporarily refuses deletion until a recent snapshot exists, Crabbox shortens
   the sandbox TTL, waits for its managed stop transition, and retries deletion
   for up to two minutes, within a three-minute overall release budget that also
   honors caller cancellation, including deletion-operation polling. Missing,
   malformed, or changed operation evidence, failed operation lookups, and
   cancellation retain the claim without recording completion. The shared native
   CLI SSH key is retained.

Cleanup reports its current native-call phase and elapsed time at roughly
ten-second intervals, including while a native command is blocked. A remaining
budget is shown only when that command's context has a deadline; progress does
not extend it or impose a new whole-command timeout. Claim-lock waits and
best-effort remote teardown are outside this native-call progress reporter.
Deletion-wait failures retain the exact operation and its last validated status
in the error. A native lookup timeout remains recognizable as a timeout even
when the overall cleanup deadline has not expired; diagnostics stay redacted.
Native command capture is capped at 8 MiB per stream; oversized or
incomplete output is an error, never evidence of completed deletion.

SSH host trust is separate from the shared native authentication key. Readiness,
reuse, and guarded teardown use a protected `known_hosts` file for the exact
Crabbox lease, so a new Box may reuse an IP or gateway endpoint without inheriting
another lease's host key. A changed host key within the same lease is still
rejected. Existing leases from before lease-scoped trust enroll on their first
connection with the new client; Crabbox does not copy or remove pins from the
old provider-wide file.

If this release observes a valid native deletion acceptance but cannot finish
waiting because of a timeout, cancellation, or operation lookup failure, it
durably records the exact operation ID and its claim binding before returning
the error. The binding covers the original provider scope, Box ID, creation
timestamp, and repository owner. This records acceptance, not completion. A later
`crabbox stop` first checks the exact Box identity. If the Box remains observable,
Crabbox validates any recorded operation before proceeding. Pending, processing,
or blocked operations retain the claim without SSH teardown or another deletion
request. A matching operation that reports `completed` also retains the claim
while the Box remains observable because those native results are inconsistent.
If `box info` instead returns a recognized native 404 and complete
`box list --all` inventory also omits the exact Box ID, Crabbox reconciles the
unchanged local claim without reading a stale operation or repeating native
deletion. Crabbox repeats these checks inside the actual release fence before
removing the claim; an earlier lookup during lease resolution does not authorize
finalization.

If Crabbox finishes waiting for native deletion but final inventory confirmation
fails or is canceled, it durably records that completed deletion in the
still-locked claim before returning the error. The record is bound to the
original claim, including its provider scope, Box ID, creation timestamp, and
repository owner. A later `crabbox stop` can finish cleanup without repeating
native deletion only when that record still matches, `box info` reports
not-found, and complete native inventory confirms absence. Failed lookups,
changed claims, or an observable sandbox retain the claim.

Completion records from earlier unreleased builds that proved only native
request acceptance are rejected, not upgraded into operation-completion evidence.

Exact native 404 plus complete inventory absence independently proves absence;
it authorizes only removal of the unchanged local claim and never another native
mutation. Failed or partial inventory, an observable matching ID, a replacement
identity, cancellation, and missing or malformed native responses retain the
claim. Command startup, output-capture, and transport failures do not count as
native 404 evidence, even when their text mentions `404` or `not found`.
Missing, malformed, changed, or incomplete operation-record bindings are
rejected before native lookups. There is no automatic adoption
of an external deletion receipt, replacement of a recorded operation, or
conversion of an old claim into completed-deletion authority.

Raw IDs, provider aliases, and legacy claims without the full ownership binding
remain inspectable but cannot authorize reuse or deletion. Missing or changed
identity, failed lookups, incomplete inventory, and uncertain deletion preserve
the local claim. There is no implicit adoption or legacy upgrade; inspect such
resources with the native provider tools and manage them explicitly there after
verifying ownership. Setup rollback likewise targets only the original confirmed
creation attempt. If that attempt already published an exact claim, rollback
preserves accepted-operation references and completed-deletion records in that
same claim for a later safe `crabbox stop` retry. Unpublished attempts keep their
separate expected-absence guard; rollback never adopts a replacement claim.
Resources are retained when ownership or completion cannot be proven.

## Rename Compatibility

The adapter intentionally does **not** rewrite historical claim provider names or
ownership labels as part of the branding change. Those values participate in
fail-closed lifecycle checks. `boat` is routed to the same registered provider,
so users can adopt the current name without invalidating pre-rename local state.
A future native-CLI/config migration should preserve or explicitly migrate those
bindings with regression and live lifecycle proof.

## Limitations

- `--class`, `--type`, image, size, and keep-alive options are not exposed
  because the currently integrated CLI lifecycle surface does not document them.
- Desktop/VNC/code features are not advertised through Crabbox for this provider.
  Use the official Boat/ASCII tools directly for interactive sessions.
