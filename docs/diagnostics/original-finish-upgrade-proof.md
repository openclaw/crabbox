# Original finish release-upgrade proof

This explicit local proof complements `worker/test/original-finish.postgres.test.ts`.
It does not rerun the authorization matrix or treat a synthetic receipt as historical
execution evidence. It checks a real v0.79.1-to-candidate storage upgrade and a separate
fresh installation. The old source is bound to `release/records/v0.79.1.json`, not a
hand-built approximation of its database schema.

## Isolation and prerequisites

Use the repository-pinned Node and Go toolchains, unchanged `worker/package-lock.json`,
and normal `npm ci` for both the candidate and a task-local `git archive` export of the
recorded release source. Keep integrity, lifecycle approval and network policy intact.
Build each CLI with `go build -trimpath -o <private-output> ./cmd/crabbox`. A source archive
without Git metadata needs `-buildvcs=false`; record the archive source and binary digest
explicitly rather than asserting embedded VCS provenance. No global installation is needed.

Use only an owned disposable rootless PostgreSQL fixture with the PostgreSQL 17 image
and digest pinned in `.github/workflows/ci.yml`. Inspect inventory first. Publish its
port on `127.0.0.1` only; do not change capabilities, security policy, system configuration,
or another task's containers. Put generated fixture credentials in a mode-0600 env file.
Create two empty databases, `crabbox_finish_upgrade_test` and `crabbox_finish_fresh_test`.
Never reset a used database after a failure or replay already completed creates.

Create a mode-0600 JSON configuration outside the checkout with:

- `database` and `freshDatabase`: corresponding loopback PostgreSQL URLs;
- `port`: an available loopback port above 1024, retained across coordinator restarts;
- `evidence`: a new private output directory (not the checkout);
- `oldCheckout`: authenticated release archive root;
- `candidate`: exact candidate commit whose production inputs are under test;
- `cli` and `oldCLI`: absolute paths to the respective built binaries;
- `sharedToken` and `adminToken`: newly generated fixture-only secrets.

Run with the pinned Node on PATH from the candidate root:

```sh
NO_PROXY=127.0.0.1,localhost node worker/scripts/prove-finish-upgrade.mjs /absolute/private/config.json
```

The runner verifies every bundled production source and lockfile against its commit,
records fixture/runner/bundle/binary hashes, and normally refuses databases that already
have coordinator storage. Loopback is excluded from an inherited HTTP proxy only for
this invocation; external proxy policy remains intact. Failed reports are preserved.

For an initial transport failure before **any** request, an optional `resumeEmpty` path
may identify the previous evidence directory. The runner requires its failed report,
matching source identities, no API/CLI requests, one cleanly stopped old process with
only ready/stopped events, and zero actual coordinator rows. Only the old database may
then be initialized already; the fresh database must still have no coordinator table.
Use a new evidence directory. This is not a general recovery or replay mode: if any
create completed or its effect is uncertain, stop and reconcile it separately. No
database is reset, and original private configs and failed reports stay intact.

## Assertions and boundaries

1. A separate old-release process starts its real `NodeCoordinatorRuntime`, including
   jobs and PostgreSQL initialization. Normal authenticated APIs create two leases,
   nondefault keep/TTL/idle/work-root settings, one completed signed run and one unfinished
   run. The old CLI verifies the completed receipt. The fixture signs and retains one
   original finish body for the unfinished run without submitting it. This is **not** a
   claim that the old release had an outbox or that missing historical bytes can be rebuilt.
2. The old process exits before the candidate starts on the same database and HTTP origin.
   Physical old lease/run/log/event rows must be unchanged before recovery. The candidate
   CLI reads the legacy receipt and imports the exact retained noncanonical body, with
   signer and finish-digest pins. Request-body hashes, receipt and persisted terminal state
   are checked. Unrelated legacy records and settings must remain byte-for-byte unchanged.
3. A new candidate process verifies durable receipt/log data and idempotently imports the
   same bytes. The relevant physical rows, including timestamps, must remain unchanged.
4. A distinct genuinely empty database exercises the candidate's native fresh initializer,
   a new synthetic run, CLI finish import and physical terminal readback. It is not the
   earlier manually created JSONB-only migration fixture.

`finish-upgrade-host.mjs` is a loopback-only fixture HTTP frontend, **not** the production
`node/server.ts` entrypoint. It calls the actual `routeCoordinatorRequest`, `FleetCoordinator`,
`NodeCoordinatorRuntime` and `PostgresCoordinatorStorage`. Authentication, organization
resolution, signature validation, run mutation and storage are not mocked. Only cloud
operations are synthetic; outbound fetch and provider release/deletion are forbidden.
The production entrypoint has no loopback-host or synthetic-provider injection switch.
This proof does not establish production deployment, arbitrary-version compatibility,
external identity-provider behavior, cloud provisioning, or all user configuration formats.
“Settings preserved” means the exact persisted lease settings identified above. Job wake
metadata is recorded but excluded from the lease/run/log/event equality assertion.

Preserve reports, exact original envelopes, physical snapshots and stopped-process receipts
before cleaning up. Export the two fixture databases if needed for inspection, then stop
and remove only the recorded owned container and its owned volume. Verify both are gone
and all coordinator processes/listeners have exited. Do not publish private fixture tokens
or raw database credentials in a PR.
