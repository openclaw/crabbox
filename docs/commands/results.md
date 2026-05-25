# results

`crabbox results` prints structured test summaries attached to a recorded
run.

```sh
crabbox run --id cbx_abcdef123456 --junit junit.xml -- go test ./...
crabbox results run_abcdef123456
crabbox results run_abcdef123456 --failed-only
crabbox results run_abcdef123456 --json
```

## When Results Are Attached

Results are attached only when `crabbox run` is told where to find remote
JUnit XML. Use either:

```sh
crabbox run --junit junit.xml -- <command...>
crabbox run --junit junit.xml,reports/junit.xml -- <command...>
```

or repo config:

```yaml
results:
  junit:
    - junit.xml
    - reports/junit.xml
```

After the command exits, the CLI reads each remote file from the workdir,
parses JUnit, and sends only the summary to the coordinator. Raw XML is not
stored. Multiple JUnit files are merged into a single summary so a multi-
report test setup still produces one result record.

## Output

Human output shows totals and the names of failed test cases:

```text
run_abcdef123456 lease=cbx_abcdef123456 command="pnpm test"
totals: tests=412 failures=2 errors=0 skipped=4 time=42.318s
failures:
  src/auth.test.ts > login → returns user
  src/sync.test.ts > rsync → handles deletes
```

Use `--failed-only` when the totals are noise and you only need the failing
case list. With `--json`, `--failed-only` returns the stored failed-case array.

`--json` returns the stored structured summary:

```json
{
  "runId": "run_abcdef123456",
  "totals": { "tests": 412, "failures": 2, "errors": 0, "skipped": 4, "timeSeconds": 42.318 },
  "failures": [
    { "suite": "src/auth.test.ts", "name": "login → returns user" },
    { "suite": "src/sync.test.ts", "name": "rsync → handles deletes" }
  ],
  "files": [
    { "path": "junit.xml", "size": 12345 }
  ]
}
```

## Limits

The coordinator caps stored summaries:

- aggregate counters (tests, failures, errors, skipped) are kept verbatim;
- failed-case entries are capped to a bounded list;
- long strings (test names, suite names, message bodies) are truncated;
- file lists keep paths and sizes, never raw bytes.

This keeps the result record small enough for the lease detail page and
the run detail page to render without paging through gigabytes of XML.

## Flags

```text
--id <run-id>     run id (also accepted as a positional argument)
--failed-only     print only failed test cases
--json            print JSON
```

## When To Use Results vs Logs

- `results` is the structured summary - "did the suite pass, and which
  cases failed?";
- `logs` is the retained command output - "what did the command print?".

Use `results` for dashboards and quick triage. Use `logs` when you need to
read the actual stack trace.

## Future Formats

Today only JUnit XML is supported. Vitest JSON, Go `test2json`, and flaky-
test correlation across runs are tracked in
[Test results](../features/test-results.md).

Related docs:

- [run](run.md)
- [history](history.md)
- [logs](logs.md)
- [Test results](../features/test-results.md)
