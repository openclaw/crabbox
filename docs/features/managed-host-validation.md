# Managed-host validation

The managed `ssh-host` provider is distinct from direct `ssh`. Validate against
an isolated coordinator and disposable target before enrolling shared capacity.
Never point the Linux smoke harness at an existing broker or host. The Windows
proof snapshot requires an explicitly designated native proof target and still
uses its own disposable coordinator.

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
Qualify the intended Windows build and hardware before deployment; a passing
ARM64 execution/authority proof does not qualify GPU access, other Windows
builds, or native amd64 execution. The recorded native run below qualifies the
managed execution lifecycle on its stated target.

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

## Native Windows authority qualification — 2026-10-27

CLI/agent source: `f39ff31885878ae7d8c391e81fbb9a8bab0c814d`.
Coordinator pool-refresh source: `2aee0af76`; the prior authority controls are
unchanged by this placement-only correction.
The [sanitized Windows receipt](../diagnostics/managed-host-windows-proof.json)
records final built-binary SHA-256 digests and observed authority/cleanup results.
The [proof harness snapshot](../diagnostics/managed-host-windows-harness/README.md)
contains the driver source and reproduction instructions.

The macOS ARM64 CLI used a disposable loopback Node 24.22.1/PostgreSQL 17
coordinator and a real Windows ARM64 build 28000.3231 target. The pinned MXC
0.8.0 executor ran from a task-specific Interactive desktop agent; normal SSH
was only the operator transport. No production broker or existing host-access
configuration was changed.

The native run found and fixed three defects that cross-builds missed:

- MXC's `cmd.exe /c` wrapper broke quoted executable/argument paths. The
  endpoint now uses the existing encoded PowerShell command and literal quoting.
- The framed-input helper assumed OpenSSH's overlapped stdin handles, while
  the managed endpoint provides synchronous pipes. It now selects a compatible
  stream before reading any bytes, preserving the async OpenSSH path and exact
  framing without replay.
- A rotated pool session received a fresh workspace, but its pool entry retained
  the previous account path. Provider installation now returns the replacement
  work root, published with the revalidated active grant rather than before it.

Both new native regression tests fail with only their production fix reverted
and pass with the fix. Existing async tests also pass: partial/pending reads,
binary payloads, frame boundaries, EOF, short input, write errors, cancellation,
and preserving process-owned stdin. Both regressions are included in native
Windows CI. The coordinator placement regression fails before its fix and
passes afterward, including an idempotent acknowledgment replay; all 4,689
coordinator tests pass (15 existing skips).

The shipped CLI syncs and executes under the MXC borrower identity, distinct
from the operator desktop session. A foreign relay identity and authenticated
non-admin foreign borrower receive HTTP 403. A wrong key at the actual nested
SSH endpoint is denied, and its proposed effect file is absent.

An open non-PTY shell and a detached borrower writer remain alive after the
exact MXC exec launcher is killed. Lease revocation then disconnects that shell,
rejects new relay admission, and removes the exact account, workspace, endpoint,
and every process in its session before the host is available again. A subsequent
owner receives a new workspace and endpoint host key. An agent crash/restart
settles persistent-session custody and fails the interrupted lease before reuse.

Two Windows portable-pool generations passed identity generation, registration,
borrow, sync/exec, scrub/return, and exact-session cleanup with fresh workspaces
and rotated endpoint keys. Unattended expiry reached the persisted expired state
and removed its exact session before reuse. All nine Windows checks passed;
the task root, Interactive task, tunnel, containers/network, and ephemeral keys
were removed, and existing SSH access was verified afterward.

This is execution, authorization, and lifecycle qualification on the recorded
native target—not Windows PTY support, GPU qualification, or network isolation.
Windows networking remains explicitly allowed. Earlier Linux/upgrade evidence
and the historical broad-local timeout remain separately scoped above.


The subsequent CI build-only correction moved the byte-identical Windows
launcher helper into the Windows-tagged source file. Linux-targeted deadcode
v0.45.0 then returned no findings; the native Windows CMD regression passed
again (0.36 seconds), and its temporary test root was removed. The qualified
launcher command and lifecycle behavior did not change.
