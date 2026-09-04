# ASCII Box Provider

Read when:

- choosing `provider: ascii-box`;
- configuring the ASCII Box API endpoint or workdir;
- changing `internal/providers/asciibox`.

[ASCII Box](https://box.ascii.dev) provides Ubuntu sandbox VMs. Crabbox uses the
documented `box --json` CLI as the control plane, lets `box ssh` prepare the
CLI-managed SSH key, and then runs normal Crabbox sync and commands over SSH.
The provider does not depend on private exec, upload, or command-stream REST
endpoints.

## When To Use

Use ASCII Box when commands should run in ASCII-managed Ubuntu sandboxes through
the `box` CLI's SSH endpoint. Use a delegated provider such as [Upstash Box](https://upstash.com/docs/box/overall/quickstart),
Modal, E2B, Islo, or Cloudflare when the provider owns command execution instead
of exposing SSH.

## Prerequisites

- Create an ASCII Box account at <https://box.ascii.dev>.
- Export the API key as `ASCII_BOX_API_KEY` or `CRABBOX_ASCII_BOX_API_KEY`.
- Install the official `box` CLI. Crabbox discovers the platform-specific config
  path through `box status --json`, writes a private config from the API key
  under its state directory, and does not require a pre-existing `box login`.

## Commands

```sh
crabbox warmup --provider ascii-box
crabbox run --provider ascii-box -- pnpm test
crabbox run --provider ascii-box --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider ascii-box --id blue-lobster
crabbox stop --provider ascii-box blue-lobster
```

## Auth

```sh
export ASCII_BOX_API_KEY=...
```

`CRABBOX_ASCII_BOX_BASE_URL` or `asciiBox.baseUrl` can override the default
`https://ascii.dev`. Custom endpoints require HTTPS, except literal loopback
hosts may use HTTP. Userinfo, queries, and fragments are rejected before the
API key is written to Box CLI configuration or passed to the CLI.

`BOX_ORG` selects an organization by ID or name. Without it, Crabbox explicitly
uses `personal`; the native CLI's sticky organization selection is not inherited.
Keep the same endpoint and organization selector when reusing or stopping a lease.

## Config

```yaml
provider: ascii-box
target: linux
asciiBox:
  baseUrl: https://ascii.dev
  cliPath: box
  workdir: /home/user/crabbox
```

Provider flags:

```text
--ascii-box-base-url
--ascii-box-cli
--ascii-box-workdir
```

Environment overrides:

```text
CRABBOX_ASCII_BOX_API_KEY / ASCII_BOX_API_KEY
CRABBOX_ASCII_BOX_BASE_URL / ASCII_BOX_BASE_URL
CRABBOX_ASCII_BOX_CLI / BOX_CLI
CRABBOX_ASCII_BOX_HOME
CRABBOX_ASCII_BOX_WORKDIR
BOX_ORG
```

## Lifecycle

1. `crabbox warmup --provider ascii-box` creates a Box through `box new --json`,
   verifies the original Box ID and creation timestamp through `box info`, stores
   an exact local ownership claim before preparing the SSH key with
   `box ssh <id> -- true`, waits for SSH, and keeps the Box until
   `crabbox stop`. The default SSH key lives in the private Box CLI home
   (`CRABBOX_ASCII_BOX_HOME`, otherwise Crabbox state).
2. `crabbox run --provider ascii-box` provisions a Box for one run, or reuses an
   existing claimed lease/slug/Box ID, then uses the standard SSH sync and run path.
   Reuse requires a matching endpoint, organization, Box ID, and creation
   timestamp. `--reclaim` may transfer repository ownership of an already owned
   lease; it does not adopt an unclaimed Box.
3. `crabbox status` resolves the local lease claim or raw Box id and reads Box
   state through `box info --json`.
4. `crabbox stop` requires the exact, unchanged local claim and freshly matching
   native identity before remote teardown or each native mutation. It releases
   the Box with `box stop --json`, requests deletion with `box delete --json --yes`,
   and validates the returned deletion operation's ID, kind, and exact Box target.
   Crabbox polls `box deletion status <operation-id>` while the operation is
   pending, processing, or blocked. It removes the claim only after that exact
   operation reports `completed` with a valid completion timestamp and complete
   `box list --all` inventory confirms absence. A successful native process exit
   alone does not prove deletion completion. The claim stays locked through
   teardown, deletion, retries, confirmation, and local removal. If the
   service temporarily refuses deletion until a recent snapshot exists,
   Crabbox shortens the Box TTL, waits for its managed stop transition, and
   retries deletion for up to two minutes, within a three-minute overall release
   budget that also honors caller cancellation, including deletion-operation
   polling. Missing, malformed, or changed operation evidence, failed operation
   lookups, and cancellation retain the claim without recording completion. The
   shared native CLI SSH key is retained.

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
`crabbox stop` first reads that same operation. Pending, processing, or blocked
operations retain the claim without normal Box lookups, SSH teardown, or another
deletion request. Only a matching operation that explicitly reports `completed`
with a valid completion timestamp can proceed to `box info` not-found and
complete inventory absence checks. Crabbox repeats these operation and absence
checks inside the actual release fence before removing the claim; an earlier
lookup during lease resolution does not authorize finalization.

If Crabbox finishes waiting for native deletion but final inventory confirmation
fails or is canceled, it durably records that completed deletion in the
still-locked claim before returning the error. The record is bound to the original
claim, including its provider scope, Box ID, creation timestamp, and repository owner.
A later `crabbox stop` can finish cleanup without repeating native deletion only
when that record still matches, `box info` reports not-found, and complete native
inventory confirms absence. Failed lookups, changed claims, or an observable Box
retain the claim.

Completion records from earlier unreleased builds that proved only native
request acceptance are rejected, not upgraded into operation-completion evidence.

Not-found and empty inventory alone are not deletion-completion evidence: ASCII
can hide a Box while its deletion operation is still pending or blocked. Claims
without either Crabbox's completion record or its bound accepted-operation
reference stay retained, including external deletions, native commands interrupted
before valid acceptance was observed, and a process termination before the record
was durably written. Missing, malformed, changed, or incomplete record bindings
are rejected. There is no automatic adoption of an external deletion receipt,
replacement of a recorded operation, or conversion of an old claim into
completed-deletion authority.

Raw IDs, provider names, and legacy claims without the full ownership binding
remain inspectable but cannot authorize reuse or deletion. Missing or changed
identity, failed lookups, incomplete inventory, and uncertain deletion preserve
the local claim. There is no implicit adoption or legacy upgrade; inspect such
resources with the native CLI and manage them explicitly there after verifying
ownership. Setup rollback likewise targets only the original confirmed creation
attempt. If that attempt already published an exact claim, rollback preserves
accepted-operation references and completed-deletion records in that same claim
for a later safe `crabbox stop` retry. Unpublished attempts keep their separate
expected-absence guard; rollback never adopts a replacement claim. Resources are
retained when ownership or completion cannot be proven.

## Limitations

- `--class`, `--type`, image, size, and keep-alive Box options are not exposed
  because the public CLI lifecycle surface does not document them.
- Desktop/VNC/code features are not advertised through Crabbox for this
  provider. Use the official Box tools directly for interactive sessions.
