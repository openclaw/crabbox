# bench

`crabbox bench` records and reports local benchmark timing observations. It is a
local evidence workflow, not a provider leaderboard.

Benchmark rows are append-only JSONL records. They wrap the existing
`TimingReport` payload with local comparison context such as command
fingerprint, command display text, provider family/kind, repo fingerprint, and
cold/warm state when known. Crabbox only writes this ledger when explicitly
requested.

Default store:

```text
<CrabboxStateDir()>/timings.jsonl
```

`CrabboxStateDir()` uses `$XDG_STATE_HOME/crabbox` when set; otherwise it uses
the user config directory's `crabbox/state` directory.

## Record from a run

Use `run --timing-record` to append the final timing report from a real run:

```sh
crabbox run --timing-record=default -- pnpm test
crabbox run --timing-record ./bench/timings.jsonl --provider aws -- pnpm test
```

Plain `crabbox run` remains non-recording by default.

## Run and record a benchmark

`bench run` runs the same command for each selected provider and repeat, then
records observations in the benchmark store:

```sh
crabbox bench run --providers aws,hetzner --repeats 3 -- pnpm test
crabbox bench run --provider aws --cold -- go test ./...
```

The command uses the same execution path as `crabbox run`, so provider setup,
sync, command streaming, timing, and normal cleanup behavior stay centralized.
If any provider or repeat fails, Crabbox continues the remaining attempts and
exits non-zero after printing the number of recorded observations. Use
`--store <path>` to write a specific JSONL store; the default is the local state
store.

## Record an existing timing JSON payload

`bench record` ingests one saved `TimingReport` JSON object from a file or
stdin:

```sh
crabbox bench record --timing-json timing.json --command "pnpm test" --cold
```

`--command` or command args after `--` set the command display text and command
fingerprint used for grouping:

```sh
crabbox bench record --timing-json timing.json -- pnpm test
```

## Report local observations

`bench report` reads the local store and groups observations by provider,
provider family/kind, machine type, command fingerprint, and cold/warm bucket.

```sh
crabbox bench report
crabbox bench report --since 7d --providers aws,hetzner
crabbox bench report --command-fingerprint sha256:... --json
```

Human output includes successful sample count (`n`), median total duration, p95
total duration when enough samples exist, median sync and command duration,
failure count, and an evidence marker. `bench report` marks groups as
`insufficient_successful_samples` until the group has at least `--min-samples`
successful observations. The default is `2`.

JSON output uses the same grouped data:

```json
{
  "schemaVersion": 1,
  "storePath": ".../timings.jsonl",
  "filters": {
    "since": "7d",
    "providers": ["aws"],
    "minSamples": 2
  },
  "groups": [
    {
      "provider": "aws",
      "providerFamily": "aws",
      "providerKind": "ssh-lease",
      "machineType": "c7a.large",
      "commandFingerprint": "sha256:...",
      "n": 2,
      "medianTotalMs": 64000,
      "medianSyncMs": 12000,
      "medianCommandMs": 45000,
      "failureCount": 0,
      "insufficientEvidence": false,
      "evidence": "sufficient_local_samples"
    }
  ]
}
```

## Privacy and interpretation

Timing records can include repo paths, workdirs, command display text, labels,
artifact paths, and lease metadata because they preserve the existing
`TimingReport` payload. Treat the store as local private state. Delete it when
you no longer want the observations.

Reports say what happened locally for matching workloads. They do not claim that
one provider is fastest globally, do not publish measurements, and do not infer
cost unless a future record includes a defensible cost basis.

## Flags

```text
bench run:
--store default|path        JSONL store destination (default: default)
--provider <name>           run one provider
--providers a,b             run comma-separated providers
--repeats <n>               repeat count per provider (default: 1)
--cold                      mark observations as cold runs
--warm                      mark observations as warm/reused runs

bench record:
--store default|path        JSONL store destination (default: default)
--timing-json path|-        TimingReport JSON input, or stdin with -
--source <label>            record source label
--command <text>            command display text for grouping
--cold                      mark observation as a cold run
--warm                      mark observation as a warm/reused run
--repeat-index <n>          one-based repeat index when known

bench report:
--store default|path        JSONL store to read (default: default)
--provider <name>           include one provider
--providers a,b             include comma-separated providers
--command-fingerprint <id>  include one command fingerprint
--since <duration>          include records since 7d, 24h, etc.
--min-samples <n>           successful samples required for sufficient evidence
--json                      print machine-readable report JSON
```
