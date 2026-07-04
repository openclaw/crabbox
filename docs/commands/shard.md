# shard

`crabbox shard` forks a [checkpoint](checkpoint.md) into `--count` leases
concurrently, runs the command on every fork in parallel through the normal
[`crabbox run`](run.md) pipeline, and merges the collected JUnit results into
one suite verdict. It is the parallel counterpart of
[`checkpoint fork --count`](checkpoint.md): the same fork flow and
`{{index}}`/`{{total}}`/`{{lease}}`/`{{slug}}` placeholders, but shards run at
the same time and their [test results](../features/test-results.md) fold into
a single answer with one aggregate exit code.

```sh
crabbox shard --count 8 --from chk_abc123 --results-auto -- pnpm test -- --shard '{{index}}/{{total}}'
crabbox shard --count 4 --from chk_deps --junit junit.xml --fail-on-test-failures -- go test ./...
crabbox shard --count 3 --from chk_abc123 --fail-fast --quiet -- pnpm vitest --shard '{{index}}/{{total}}'
```

The command template owns the sharding: crabbox never splits tests itself.
`--count` is always explicit, the plan is printed before any lease is
provisioned, and a repo config cap (`shard.maxCount`) bounds what a checkout
may request. Requires an SSH lease provider that supports checkpoint fork;
delegated providers are rejected. Provider-native snapshot fan-out stays on
[`checkpoint fork --snapshot`](checkpoint.md).

## Lease lifecycle

Every shard forks its own lease from the checkpoint and runs with an injected
`--id`, so sync, history, logs, artifacts, and results behave exactly as for
`run`. Forks exist only for the duration of their shard: each lease is
released when its shard finishes, on failure, and on Ctrl-C, always with a
fresh context so cleanup survives cancellation. A shard that fails to
provision does not orphan its siblings; the others run to completion and
release normally. Pass `--keep` to retain all forked leases instead.

## Streaming

All shards stream live. Every line is prefixed with a stable shard marker so
interleaved output stays readable:

```text
[3/8] running on cbx_a1b2c3 ...
[3/8] ✓ auth.test.ts (14 tests)
[1/8] ✓ cart.test.ts (9 tests)
```

`--quiet` suppresses the passthrough streams and keeps only the per-shard
lifecycle lines (`shard 3/8 provisioned ...`, `shard 3/8 done exit=0 ...`),
which are printed to stderr in both modes.

## Merged results and exit codes

A failing shard never aborts the others: the default is a complete verdict
over the whole suite. `--fail-fast` opts into canceling the remaining shards
after the first failure.

With results wiring configured (`--junit`, `--results-auto`, or the repo
`results` config), each shard's JUnit XML is parsed through the existing
results pipeline and the per-shard summaries merge into one verdict: a
per-shard table, merged totals with suite time versus wall clock, and every
failed case with its shard index and run id. `--json` emits the same report as
JSON.

```text
shard results
SHARD  LEASE          RUN         EXIT  TESTS  FAIL  ERR  SKIP  TIME
1/3    cbx_a1b2c3     run_x9y8z7  0     120    0     0    2     41.2s
2/3    cbx_d4e5f6     run_w6v5u4  0     118    1     0    0     43.8s
3/3    cbx_g7h8i9     run_t3s2r1  0     121    0     0    1     40.9s
shard verdict shards=3 failed_shards=0 tests=359 failures=1 errors=0 skipped=3 suite_time=125.9s wall=52.4s
failed:
  [2/3]  src/cart.test.ts failure  CartSuite.applies coupon — expected 3 got 2
```

The exit code aggregates across shards: if any shard's command exits non-zero,
shard exits with that code (or `1` when failing shards disagree); shards that
fail to provision or run exit `7`; and `--fail-on-test-failures` keeps its
documented meaning evaluated against the merged summary, exiting `1` when the
merged results contain failures or errors even though every command exited
zero. Inside each shard the flag is forced off so the policy is applied once,
to the whole suite. Interrupting the command cancels the in-flight shards:
they are reported as `canceled`, not failed, and a pure interrupt exits `130`.
Under `--fail-fast` only the shard that actually failed counts toward the
verdict; the canceled siblings are tallied separately.

## Flags

```text
--count <n>              Number of parallel shards to run; required, no default.
--from <checkpoint-id>   Checkpoint to fork each shard from; required.
--fail-fast              Cancel remaining shards after the first failing shard.
--keep                   Keep forked leases after their shard finishes.
--quiet                  Suppress shard output streams; keep per-shard status lines.
--json                   Print the merged verdict as JSON.
--dry-run                Show the shard plan without provisioning.
--fail-on-test-failures  Exit non-zero when the merged JUnit results contain
                         failures or errors.
```

Lease-creation flags (`--provider`, `--class`, `--ttl`, `--slug`, ...) apply
to every fork; `--slug` gets stable numeric suffixes exactly as
`checkpoint fork --count` does. Restore flags (`--workdir`, `--clear`,
`--reclaim`) match `checkpoint fork`. Most other `run` flags, such as
`--junit`, `--results-auto`, `--shell`, `--allow-env`, or `--preset`, pass
through to every shard's run. Flags that conflict with parallel shard runs are
rejected: `--id`, `--pool`, `--pool-return`, `--sync-only`, `--no-sync`,
`--apply-local-patch`, `--fresh-pr`, `--script`, `--script-stdin`,
`--stop-after`, `--lease-output`, `--keep-on-failure`, `--capture-stdout`,
`--capture-stderr`, `--download`, `--emit-proof`, `--proof-template`,
`--timing-json`, and `--timing-record`. `--label` is reserved: shard labels
each run `shard N/M` so runs stay readable in [`history`](history.md).

## Limits

- No automatic test splitting: the command template distributes work via
  `{{index}}`/`{{total}}`, exactly as the runner's own sharding flag expects.
- `shard.maxCount` in repo config bounds `--count`:

  ```yaml
  shard:
    maxCount: 8
  ```

- The merged verdict is printed locally; each shard's run record keeps its own
  JUnit summary on the coordinator, readable via [`results`](results.md).

## Related docs

- [checkpoint](checkpoint.md) — create checkpoints and serial fork fan-out.
- [run](run.md) — the pipeline each shard executes.
- [results](results.md) — per-run recorded test results.
- [Test results](../features/test-results.md) — JUnit collection and storage.
