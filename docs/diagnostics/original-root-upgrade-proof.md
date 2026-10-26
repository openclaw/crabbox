# Original-root upgrade and restart proof

This opt-in fixture checks the recorded **v0.79.1 source** against a candidate
commit, using one owned PostgreSQL database across real coordinator subprocess
restarts. It is not a provider qualification or a deployment command.

## Boundary

The fixture uses the real HTTP authentication/router, FleetCoordinator,
NodeCoordinatorRuntime, PostgreSQL storage and pg-boss startup/shutdown. Only AWS
operations are synthetic. All instance, account and volume identities in its
output are **synthetic**, not evidence about an existing cloud resource.

The HTTP listener is fixture-owned, not the packaged Node server frontend. The
proof does not cover the packaged updater, real AWS authentication, arbitrary
release upgrades, downgrade compatibility, or historical resource recovery. It
must not be pointed at production storage. No database state is seeded by SQL:
v0.79.1 creates its own lease records through authenticated HTTP requests.

## Setup

Use Node 24 (the repository's developer-tool installer pins 24.19.0), rootless
Podman and a clean task-owned checkout. Keep the evidence directory private: it
contains synthetic local authentication tokens and database backups. Do not copy
real credentials into it or approve dependency lifecycle scripts to make this
proof work.

The following commands assume execution from the repository root. Choose a new
owned evidence directory; do not reuse another developer's database or container.

```sh
umask 077
export PROOF_DIR="$PWD/.root-upgrade-proof"
mkdir "$PROOF_DIR"
export CANDIDATE=$(git rev-parse HEAD)
export OLD_SOURCE=$(node -p 'JSON.parse(require("fs").readFileSync("release/records/v0.79.1.json")).sourceCommit')
git cat-file -e "$OLD_SOURCE^{commit}"
mkdir "$PROOF_DIR/v0.79.1" "$PROOF_DIR/attempt-01"
git archive "$OLD_SOURCE" | tar -x -C "$PROOF_DIR/v0.79.1"
npm ci --prefix worker
npm ci --prefix "$PROOF_DIR/v0.79.1/worker"

podman info --format '{{.Host.Security.Rootless}}' # must be true
export PG_IMAGE='docker.io/library/postgres:17-bookworm@sha256:051f7b7b3abdd564d5d1bd1e8c4b9c1b6e77087d1dd22020ede611c096a272e0'
export PG_ID=$(podman run --detach --publish 127.0.0.1::5432 \
  --env POSTGRES_USER=crabbox_test \
  --env POSTGRES_PASSWORD=crabbox-test-only \
  --env POSTGRES_DB=crabbox_root_upgrade_test "$PG_IMAGE")
printf '%s\n' "$PG_ID" > "$PROOF_DIR/container-id.txt"
podman port "$PG_ID" 5432/tcp > "$PROOF_DIR/postgres-port.txt"
podman exec "$PG_ID" pg_isready -U crabbox_test -d crabbox_root_upgrade_test
```

Wait for that container's PostgreSQL readiness if it is still starting. Record
its inspect output and exact image identity. The password above belongs only to
the disposable loopback fixture. The scripts reject a non-loopback database URL
or a database name other than **crabbox_root_upgrade_test**.

Create a private configuration without displaying its tokens:

```sh
node --input-type=module <<'JS'
import fs from 'node:fs';
import crypto from 'node:crypto';
import path from 'node:path';
const directory = process.env.PROOF_DIR;
const port = fs.readFileSync(path.join(directory, 'postgres-port.txt'), 'utf8').trim().split(':').at(-1);
const config = {
  database: 'postgres://crabbox_test:crabbox-test-only@127.0.0.1:' + port + '/crabbox_root_upgrade_test',
  sharedToken: crypto.randomBytes(32).toString('hex'),
  adminToken: crypto.randomBytes(32).toString('hex'),
  candidate: process.env.CANDIDATE,
  oldSource: path.join(directory, 'v0.79.1'),
  evidence: path.join(directory, 'attempt-01'),
};
fs.writeFileSync(path.join(directory, 'fixture.json'), JSON.stringify(config), { mode: 0o600 });
JS
node worker/scripts/prove-root-upgrade.mjs "$PROOF_DIR/fixture.json"
```

The runner checks every bundled production source input and each dependency
lockfile against its committed revision. Both implementations can report package
version 0.79.1; use the recorded source commits and input hashes, not the package
version alone, to distinguish them. The old source is identified by the tracked
release record; this is not a separate tag-signature verification.

## Assertions and evidence

- An invalid HTTP bearer is rejected. v0.79.1 creates an ordinary lease and a
  retained lease with non-default keep/TTL/idle/work-root settings.
- The old subprocess shuts down and exits before the candidate opens the same
  database. Existing lease fields and HTTP readback survive unchanged.
- A legacy lease with existing lifetime identity remains readable. An ordinary
  legacy lease without authoritative root evidence fails closed rather than
  inventing an original volume.
- A new candidate acquisition publishes its exact synthetic root identity through
  the real guarded callback, without enrolling in lifetime policy. A later
  generic claim cannot erase it; a conflicting root publication is rejected.
- A second cold candidate process reads both old and new records. A synthetic
  replacement root is rejected without changing the stored original binding.

The private output contains build/source manifests, request responses, process
start/stop receipts, logs, before/after PostgreSQL rows and **PROOF-REPORT.json**.
Inspect the report **and** the process exit status; keep failed attempts as failed.
No completed create request should be replayed merely to get a cleaner report.

For interrupted fixture development only, **resumeOld** or **resumeReplacement**
may point to a retained failed attempt. These narrow continuations verify prior
source IDs, clean stop receipts and the exact database snapshot, preserve
completed creates, and record their predecessor. They are not generic benchmark
retries or a way to recover ambiguous remote operations. A report using a
continuation is a proof chain, not an uninterrupted single-pass success.

## Cleanup

Before stopping PostgreSQL, save an owned database backup and record its volume
names. Stop/remove only the container ID recorded by this invocation:

```sh
podman inspect "$PG_ID" > "$PROOF_DIR/postgres-inspect.json"
podman exec "$PG_ID" pg_dump -U crabbox_test -d crabbox_root_upgrade_test \
  --format=custom > "$PROOF_DIR/final-database.dump"
podman stop --time 10 "$PG_ID"
podman rm --volumes "$PG_ID"
```

Verify that the recorded container and anonymous volume no longer exist, the
recorded loopback port is closed, and all coordinator child processes have exited.
Keep the backup, receipts, failed attempts and a SHA-256 manifest in the private
evidence directory. Never remove a different container, reset a shared database,
or substitute expired leases for a successful cleanup receipt.
