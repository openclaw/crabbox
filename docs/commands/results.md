# results

`crabbox results` prints the structured test summary attached to a recorded
run. It answers "did the suite pass, and which cases failed?" without making
you page through the raw command log.

```sh
# Record a run, then read its results.
crabbox run --junit junit.xml -- go test ./...
crabbox results run_abcdef123456
crabbox results run_abcdef123456 --failed-only
crabbox results run_abcdef123456 --json
```

The run id is accepted as a positional argument or via `--id`. A coordinator
must be configured (`CRABBOX_COORDINATOR` or `config set-broker`), because
results are read back from the recorded run, not from the live box.

## How results get attached

Results are attached only when `crabbox run` knows where to find remote JUnit
XML after the command exits. There are three ways to point it at the files.

On the command line:

```sh
crabbox run --junit junit.xml -- <command...>
crabbox run --junit junit.xml,reports/junit.xml -- <command...>
crabbox run --junit junit.xml --fail-on-test-failures -- <command...>
```

In repo config:

```yaml
results:
  junit:
    - junit.xml
    - reports/junit.xml
  failOnFailures: true
```

Or let `run` scan common JUnit locations after the command:

```sh
crabbox run --results-auto -- <command...>
```

```yaml
results:
  auto: true
```

You can also set `CRABBOX_RESULTS_JUNIT` (comma-separated paths),
`CRABBOX_RESULTS_AUTO`, and `CRABBOX_RESULTS_FAIL_ON_FAILURES` in the
environment.

After the command finishes, the CLI reads each remote file from the workdir,
parses the JUnit XML, and sends only the parsed summary to the coordinator.
Raw XML is never stored. Multiple JUnit files are merged into one summary, so a
multi-report setup still produces a single result record. Bad files produce
named warnings without erasing valid summaries. Auto discovery accepts files
up to 16 MiB and 64 MiB total; it skips larger reports explicitly instead of
truncating them into invalid XML.

By default, result parsing does not replace the wrapped command's exit status.
With `--fail-on-test-failures` or `results.failOnFailures: true`, a command that
exits zero but produces parsed JUnit failures or errors becomes a failed run
with exit code 1. An existing command failure keeps its original exit code.

## Output

Human output starts with a totals line, then lists the failing cases (failures
and errors). Each failure line shows its location, kind, fully qualified name,
and the first line of the failure message:

```text
results format=junit files=1 suites=18 tests=412 failures=2 errors=0 skipped=4 time=42.318s
failed:
  src/auth.test.ts        failure  auth.login → returns user — expected 200, got 401
  src/sync.test.ts        failure  sync.rsync → handles deletes — timed out after 30s
```

`--failed-only` drops the totals line and prints just the failing cases. When
no failures are recorded it prints `no failed test cases recorded`. If the run
has no results at all, plain output is `no test results recorded for <run-id>`.
Human-readable failure fields visibly escape terminal controls and Unicode
formatting characters so stored JUnit text cannot affect the local terminal.

`--json` prints the stored summary. The `failed` array carries each failing
case; file entries are plain paths.

```json
{
  "format": "junit",
  "files": ["junit.xml", "reports/junit.xml"],
  "suites": 18,
  "tests": 412,
  "failures": 2,
  "errors": 0,
  "skipped": 4,
  "timeSeconds": 42.318,
  "failed": [
    {
      "suite": "src/auth.test.ts",
      "name": "login → returns user",
      "classname": "auth",
      "file": "src/auth.test.ts",
      "message": "expected 200, got 401",
      "type": "AssertionError",
      "kind": "failure"
    }
  ]
}
```

With `--json --failed-only`, only the `failed` array is printed (an empty array
when there are no failures or no results). JSON preserves the stored field
values unchanged; display escaping applies only to human-readable output.

## Storage limits

The coordinator keeps result records small enough for the run and lease detail
pages to render quickly:

- aggregate counters (suites, tests, failures, errors, skipped, time) are kept
  verbatim;
- the failed-case list is bounded;
- long strings (test, suite, and message text) are truncated;
- the file list keeps paths only, never raw bytes.

## results vs logs

- `results` is the structured summary: which suite passed and which cases
  failed.
- `logs` is the retained command output: what the command actually printed.

Use `results` for dashboards and quick triage. Use [`logs`](logs.md) when you
need the full stack trace.

## Flags

```text
--id <run-id>     run id (also accepted as the first positional argument)
--failed-only     print only failed test cases
--json            print JSON
```

## Supported formats

Today only JUnit XML is parsed. Other formats and cross-run flaky-test
correlation are tracked in [Test results](../features/test-results.md).

## Related docs

- [run](run.md)
- [history](history.md)
- [logs](logs.md)
- [Test results](../features/test-results.md)
