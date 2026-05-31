# harness

Validate harness files used by proof-aware Crabbox runs.

```sh
crabbox harness validate HARNESS.md
crabbox harness validate HARNESS.md --json
```

`crabbox harness validate` parses the Markdown file, validates YAML
frontmatter, verifies `plan_file` when present, and prints the harness and plan
hashes. It does not lease a box or run commands.

Harness files can be attached to runs with:

```sh
crabbox run --harness HARNESS.md -- pnpm test
crabbox job run full-ci --harness HARNESS.md
```

See [Harnesses](../features/harness.md) for the file format, evidence outputs,
and compliance behavior.
