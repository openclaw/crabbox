# Managed-host validation

The managed `ssh-host` provider is distinct from direct `ssh`. Validate against
an isolated coordinator and disposable target before enrolling shared capacity.
Never point the smoke harness at an existing broker or host.

## Local Linux borrower proof

Requirements: Docker with a Linux engine, Go from `go.mod`, Node from
`.node-version`, and installed coordinator dependencies. Docker runs the target
privileged **inside the disposable engine VM** to allow nested user/PID/mount
namespaces; this is a test-fixture requirement, not a production container recipe.
The target uses an init process to reap children during account teardown.

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
# Use amd64 instead when the Docker engine is x86_64.
GOOS=linux GOARCH=arm64 go build -trimpath -o bin/crabbox-host-linux ./cmd/crabbox
npm run build:node --prefix worker
python3 scripts/live-managed-ssh-host-smoke.py --evidence /tmp/managed-host-proof
# Optionally replace a built stable coordinator with the current one on the same DB:
# add --upgrade-worker /path/to/stable-checkout/worker
```

The script creates its own loopback-only coordinator, PostgreSQL database,
network, Linux target, synthetic credentials, and isolated CLI home/config. It
uses an anonymous public Git fixture for the existing ready-pool scrub protocol;
no repository credentials are forwarded. The resulting `result.json` records
completed checks and failure diagnostics, and resources are removed in `finally`.
An interrupted process may need manual removal of its exact `cbx-host-proof-*`
resources; do not remove unrelated containers.

Coverage includes borrower sync/exec, shipped-CLI interactive PTY access,
authenticated foreign-borrower relay rejection, retained-shell termination after
revocation, rejection of new revoked connections, and cleanup-before-reuse,
two portable-pool generations with fresh home directories and rotated endpoint
keys, private-group cleanup with `USERGROUPS_ENAB=no`, concurrent allocation, idempotent admission,
coordinator restart, failed cleanup quarantine, expiry, host-agent restart,
both graceful and crash recovery, and draining/removal without stopping the physical host. A failed run is not
proof of checks that follow its failure. Linux namespace restrictions and lack
of network isolation are described in [managed SSH hosts](../providers/ssh-host.md).

## Windows qualification boundary

Windows amd64/arm64 compilation checks API and build compatibility only. The
MXC adapter requires separate interactive-desktop hardware qualification of
provision/start, the nested SSH endpoint, borrower sync/exec, key rotation,
stop/deprovision, crash recovery, and the requested hardware capabilities.
Windows PTY allocation is not implemented; non-PTY shell/exec is available.
No successful Linux result or standalone MXC probe establishes those claims.
Keep Windows deployment experimental until that complete scenario is proven.

## Review fixes

Independent review identified three P2 lifecycle defects, addressed before RFR:

- Startup now fails every retained assignment, including graceful shutdowns that
  already removed the runtime, before admitting another generation.
- Linux tracks the private group GID and removes only the same empty group before
  confirming cleanup; it does not depend on `USERGROUPS_ENAB` defaults.
- MXC retries exact-ID deprovision directly rather than replaying non-idempotent
  stop. Its [pinned backend contract](https://github.com/microsoft/mxc/blob/v0.8.0/docs/isolation-session/state-aware-rust.md)
  specifies that account deprovision terminates its session and returns typed
  `stale_id` after removal. Only that deprovision receipt confirms prior absence.

The local borrower test also caught relay control teardown after pool revocation.
The CLI now closes its pinned control connections before returning pool access;
a regression test checks ordering and refusal to return after a close failure.

## Recorded local qualification — 2026-10-26

Runtime implementation: `15ea299ce9aa10d7ea3b73d0a23ef2188f9dbbe4`.
Native macOS client, Linux arm64 Docker target, Node 24.22.1 coordinator,
PostgreSQL 17, synthetic operator/borrower identities. All ten scenario checks
passed twice; the final run required both `crabbox stop` calls to succeed without
an outer retry. Disposable resources were removed after each run.

| Scenario | Result |
| --- | --- |
| Isolated coordinator health and unauthorized access rejection | Passed |
| Borrower CLI sync and execution through outbound relay | Passed |
| Account/process/workspace cleanup before reallocation | Passed |
| Two portable-pool generations, fresh home, rotated endpoint keys | Passed |
| Eight concurrent borrowers, one winner, idempotent retry | Passed |
| Coordinator restart preserves exclusive custody | Passed |
| Foreign-account mismatch quarantines; repair permits cleanup | Passed |
| Unattended expiry removes account/workspace before reuse | Passed |
| Agent crash and graceful restart fail interrupted leases safely | Passed |
| Drain/removal deny placement and leave the physical host running | Passed |

Additional checks: 4,686 coordinator tests passed (15 existing skips), focused
Go race tests for agent/relay/pool lifecycle fixes passed, both coordinator
typechecks and lint passed, Worker/Node builds passed, Windows amd64/arm64
cross-builds passed, and documentation generation/checks passed. The public PR
records the broader Go/CI results separately. No Windows hardware execution,
real cloud allocation, production deployment, or release is claimed.


## Review follow-up qualification — 2026-10-27

Runtime source: `88d03564a2411a89ea0ce6d05b34de155f0af5fb`.
The [sanitized result artifact](../diagnostics/managed-host-local-proof.json)
records executable SHA-256 digests, authorization receipts, and completed checks.
No credentials, host addresses, or private endpoint URLs are published.

The expanded disposable-host proof passed all 13 checks; a second run with
latest-stable upgrade coverage passed all 14. In addition to the
previous coverage, the shipped `crabbox connect` opened an isolated PTY shell;
an authenticated non-admin foreign borrower received HTTP 403 on relay admission;
revocation disconnected an already-open shell and refused new relay admission
with HTTP 403. No post-revocation output was observed. Pool generations still
received fresh homes and rotated endpoint identities. This is local Linux proof,
not Windows hardware qualification or a claim of network containment.

New regression tests fail on `b8ba9492` (shell and PTY requests rejected;
Windows pool identity generation returns HTTP 409) and pass on the fixed source.
The full coordinator suite passed 4,688 tests with 15 existing skips. Focused Go
race checks, lint, formatting, both coordinator typechecks, Worker/Node builds,
Windows amd64/arm64 builds, and docs checks passed.

The earlier broad native Go race run timed out; it is **not** a passing result.
The remaining checkpoint fork timeout was reproduced on unchanged PR base
`075d454a` with a standard temporary directory and `umask 022`: the fixture waits
for its synthetic Machine0 target on closed localhost port 22. The new CLI help
flags required a baseline checksum update; the built-binary contract now passes.
Fresh CI results are tracked on the PR rather than inferred from these local runs.

The second run first wrote a direct-SSH registration using the exact `v0.79.3`
coordinator and locked dependencies, replaced only the server with the new build
on the same PostgreSQL database, then read and refreshed that registration.
Owner, organization, provider, host, work root, and creation identity were
preserved. This tests stored-registration compatibility, not every historical
record type or a production deployment. All disposable resources were removed.
