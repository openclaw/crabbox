# Test Results

Read this when you are:

- adding or extending a result format;
- changing how failed cases are summarized or capped;
- debugging why [`crabbox results`](../commands/results.md) shows no data.

Crabbox can attach a parsed JUnit XML summary to a recorded run so a failed run
can answer "which tests failed?" without scraping a large raw log. The CLI reads
the remote JUnit files after your command exits, parses them locally, and sends
only the compact summary to the coordinator. Raw XML is never uploaded or
stored.

This page is the conceptual companion to the command reference; for invocation,
flags, and example output see [`results`](../commands/results.md).

## Pointing a run at result files

Results attach only when `crabbox run` knows where to find remote JUnit XML in
the workdir. There are three ways to tell it.

Per run, on the command line:

```sh
crabbox run --id cbx_... --junit junit.xml -- go test ./...
crabbox run --id cbx_... --junit junit.xml,reports/junit.xml -- go test ./...
crabbox run --id cbx_... --results-auto -- go test ./...
crabbox run --id cbx_... --junit junit.xml --fail-on-test-failures -- ./test-wrapper
```

Per repo, in any [config file](configuration.md):

```yaml
results:
  auto: true
  failOnFailures: true
  junit:
    - junit.xml
    - reports/junit.xml
```

Or through the environment: `CRABBOX_RESULTS_JUNIT` (comma-separated paths),
`CRABBOX_RESULTS_AUTO` (boolean), and `CRABBOX_RESULTS_FAIL_ON_FAILURES`
(boolean). The `--junit` path also flows through
[`crabbox job run`](jobs.md) (from a job's `junit:` list) and
[`crabbox capsule replay`](capsules.md).

## What happens after the command exits

`crabbox run` collects results only when `--results-auto` is set or at least one
explicit `--junit` path is configured.

For explicit `--junit` paths, the CLI resolves each listed file and reads it
only when its final target remains inside the workdir, then parses it. Relative
paths, absolute paths within the workdir, and symlinks that stay within the
workdir remain supported. Windows-native targets apply the same rule while
resolving directory junctions and symbolic links. Collection validates the
opened file and reads from the same descriptor or stream, so a background
process cannot swap a checked path before the read.

Auto discovery (`--results-auto`) is freshness-aware so it never reports stale
reports from an earlier run:

1. Before the command runs, the CLI writes a results-start marker. In a Git
   checkout it lives under the repo's Git dir (so the worktree stays clean);
   otherwise it falls back to `.crabbox/results-start` in the workdir.
2. After the command, it walks the workdir for `junit*.xml`, `TEST-*.xml`, and
   `results.xml`, pruning `node_modules` and `.git`.
3. Each candidate must be newer than the marker, must sniff as JUnit XML (the
   leading bytes contain `<testsuite`/`<testsuites`), and reports that contain
   failures or errors are prioritized over passing ones.
4. Collection considers at most 50 files, accepts reports up to 16 MiB each,
   and transfers at most 64 MiB total. Reports outside those bounds are skipped
   with a warning naming the file; accepted reports are never truncated.

Explicit `--junit` files and auto-discovered files are merged (de-duplicated by
normalized workdir-relative path), so a multi-report setup still produces one
result record. A malformed, partial, or oversized report emits a named warning
without discarding summaries parsed from other valid files. The CLI prints a
one-line summary to stderr and includes every valid parsed summary in the run's
`finish` payload.

Result collection warnings remain non-fatal. To make parsed test failures affect
the run status, opt in with `--fail-on-test-failures`,
`results.failOnFailures: true`, or `CRABBOX_RESULTS_FAIL_ON_FAILURES=true`.
When the wrapped command exits zero and a valid report contains failures or
errors, Crabbox records and exits with code 1 after collecting requested
artifacts. An existing non-zero command exit remains authoritative.

## The parsed summary

Parsing produces a `TestResultSummary` (`internal/cli/results_parse.go`): the
format (`junit`), the source file list, aggregate counters (suites, tests,
failures, errors, skipped, total time in seconds), and a `failed` list of
individual failing cases. Each `TestFailure` records its suite, test name,
optional classname/file, the first failure or error message, the JUnit `type`,
and a `kind` of `failure` or `error`. The parser accepts both `<testsuites>` and
a bare `<testsuite>` root and derives counts from case elements when suite
attributes are absent.

## Coordinator storage limits

The coordinator bounds the stored record so a huge report cannot blow past
Durable Object storage or slow the run and lease detail pages
(`worker/src/fleet.ts`, `boundedTestResults`):

- aggregate counters are kept verbatim;
- the failed-case list is capped at 100 entries;
- the file list is capped at 50 paths;
- every stored string (file, suite, test, message, type) is truncated to
  4096 bytes.

## Reading results back

```sh
crabbox history --lease cbx_...
crabbox results run_...
```

[`crabbox results`](../commands/results.md) reads the stored summary from the
recorded run, so a coordinator must be configured. It is distinct from
[`crabbox logs`](history-logs.md): `results` is the structured pass/fail
summary, `logs` is the retained command output.

Human result lines, shard failure summaries, and failure digests visibly escape
terminal controls and Unicode formatting characters in stored failure fields.
Machine-readable JSON preserves the stored values unchanged.

## Supported formats

- JUnit XML.

Possible future additions tracked for this feature: Vitest JSON, Go
`test2json`, flaky-test history across runs, and changed-file correlation.

## Source

- CLI command and output: `internal/cli/results.go`
- JUnit parsing: `internal/cli/results_parse.go`
- Remote collection, auto discovery, freshness marker: `internal/cli/results_remote.go`
- Run integration: `internal/cli/run.go`
- Config keys and env overrides: `internal/cli/config.go`
- Summary types: `internal/cli/coordinator.go`, `worker/src/types.ts`
- Storage bounds: `worker/src/fleet.ts`
