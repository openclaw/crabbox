# Measuring sync performance

Use `scripts/benchmark-sync.py` with a disposable fixture. It creates committed
source files, deterministic binary assets and Git-ignored dependency trees.
The small, medium and large fixtures contain 1,001 / 20,002 / 100,004 candidate
files and approximately 4 / 298 / 981 MB, respectively. Large binary assets are
50, 100 and 200 MiB. `--generate-from` instead archives an existing repository's
HEAD into a separate fixture without modifying the source checkout.

```sh
bench=$(mktemp -d)
export GOCACHE="$bench/go-cache"
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
Use the same benchmark source/instrumentation with each implementation. On a
shared host, always compare three interleaved pairs rather than single runs:

```sh
python3 scripts/benchmark-sync.py "$bench/medium" --changes 0 \
  --baseline-test-binary "$bench/baseline.test" --test-binary "$bench/sync.test" \
  --phase snapshot-local --timeout 240 --scenario-timeout 1500 \
  --output "$bench/paired-clean.json"
```

This runs A B A B A B, records each executable's digest, reports the median of
each implementation and all three paired deltas, and writes partial results
after every sample. Repeat for `--changes 1` and `--changes 100`. Each sample has
a hard process-group timeout (480 seconds by default), and the entire sequence
has a separate sample budget (1,800 seconds by default). Timeouts retain captured
output and are reported explicitly; incomplete sequences have no median summary.
Generation and scenario mutation are outside this budget. Snapshot staging is
private to each sample and removed afterward. Avoid other local builds, tests
and fixture generation during a measurement sequence.

## Full runs over SSH

To isolate remote manifest writing, run the production receiver against synthetic
1,000-, 10,000-, and 50,000-file trees. Tree creation is outside the timed region;
each iteration includes the receiver shell, framing, metadata setup, and length
checks, followed by untimed byte-for-byte verification of both manifests:

```sh
go test ./internal/cli -run '^$' -bench '^BenchmarkRemoteWriteSyncManifests$' -benchtime=1x -count=3
```

Build baseline and candidate test binaries with this same benchmark and run
three interleaved pairs on the same host. This measures receiver work, excluding
SSH latency and workspace-owner registration. Use a full SSH run below to include
those layers. The manifest payload scales with filename bytes, not file contents.

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
