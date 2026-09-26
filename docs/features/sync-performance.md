# Measuring sync performance

Use `scripts/benchmark-sync.py` with a disposable fixture. It creates committed
source files, deterministic binary assets and Git-ignored dependency trees.
The small, medium and large fixtures contain 1,001 / 20,002 / 100,004 candidate
files and approximately 4 / 298 / 981 MB, respectively. Large binary assets are
50, 100 and 200 MiB. `--generate-from` instead archives an existing repository's
HEAD into a separate fixture without modifying the source checkout.

```sh
bench=$(mktemp -d)
go test -c -o "$bench/sync.test" ./internal/cli
python3 scripts/benchmark-sync.py "$bench/medium" --generate medium
python3 scripts/benchmark-sync.py "$bench/medium" --changes 0 \
  --test-binary "$bench/sync.test" --phase '.*' --output "$bench/clean.json"
python3 scripts/benchmark-sync.py "$bench/medium" --changes 1 \
  --test-binary "$bench/sync.test" --phase '.*' --output "$bench/one.json"
python3 scripts/benchmark-sync.py "$bench/medium" --changes 100 \
  --test-binary "$bench/sync.test" --phase '.*' --output "$bench/hundred.json"
```

The benchmark separates manifest enumeration, ordinary dirty-path fingerprinting,
full content fingerprinting, snapshot copying, complete local-seed snapshot
preparation, and plan presentation. Each phase performs one complete operation.
`read-bytes/op` counts bytes returned to Crabbox's source-copy reader, including
filesystem-cache hits; `hashes/op` counts regular-file hash passes. These do not
include Git's own reads or rsync's reads. `git/op` counts child Git invocations,
including hermetic local-seed commands. `inblock/op` reports host `getrusage`
physical input operations; zero does not mean no logical I/O. This benchmark
is available on Linux and macOS.

Fixture generation, mutations and initial manifest setup are outside the timed
regions. Each scenario restores the first 100 fixture paths to their committed
contents, then makes zero, one, or 100 same-size edits. A fixture marker and lock
prevent accidental mutation of an ordinary checkout or simultaneous scenario
edits. Temporary snapshots are removed even after a bounded benchmark failure.
Run before/after binaries serially on the same fixture, and repeat pairs when
comparing wall times on a shared host. The local phase timeout is eight minutes.

## Full runs over SSH

Build `scripts/benchmark-sync.Dockerfile` for a local static SSH target. Besides
OpenSSH and rsync, it installs the GNU/util-linux tools used by the workspace
ownership and pruning scripts. BusyBox `flock` does not implement their timeout
option. Use an ephemeral SSH key and bind the published port only to loopback:

```sh
docker build -f scripts/benchmark-sync.Dockerfile -t sync-benchmark .
ssh-keygen -q -t ed25519 -N '' -f "$bench/key"
docker run -d --name sync-benchmark -p 127.0.0.1::22 \
  --mount "type=bind,src=$bench/key.pub,dst=/root/.ssh/authorized_keys,readonly" \
  sync-benchmark
docker port sync-benchmark 22/tcp
```

Write a private trusted config outside the fixture with `provider: ssh`,
`static.host: 127.0.0.1`, the printed `static.port`, `static.user: root`,
`static.workRoot: /work`, a unique `static.id`, and `ssh.key` pointing at the
ephemeral key. Use a distinct ID per fixture. Prime the workspace once, then:

```sh
python3 scripts/benchmark-sync.py "$bench/medium" --changes 1 \
  --crabbox /absolute/path/to/crabbox --config "$bench/ssh.yaml" \
  --run-arg=--provider=ssh --output "$bench/run-one.json"
```

This executes `run --no-hydrate --keep --timing-json -- true`, records wall time,
the complete timing report and counts Git, SSH and rsync subprocesses. SSH
process counts include configuration probes and control operations, so they
are not TCP connection counts. Add `--run-arg=--git-seed-source=local` to measure
offline local seeding; ordinary seeding must remain enabled in its configuration.
The local-seed hard object-size limit remains in force, so a large fixture can
be valid for ordinary sync while too large for local history transfer.

For WAN measurements, first acquire an explicitly sized disposable brokered
lease with a bounded TTL, then pass `--run-arg=--provider=aws` and
`--run-arg=--id=<lease>` instead of the static config. Use the same scenarios
and binaries. Stop the lease and verify its released state afterward. Static
SSH leases also need `crabbox stop` before removing their target.

```sh
docker rm -f sync-benchmark
docker image rm sync-benchmark
rm -rf "$bench" # only the disposable directory created above
```

See [sync behavior](sync.md) for scope, ownership, pruning, snapshot digest
caching and platform guarantees.
