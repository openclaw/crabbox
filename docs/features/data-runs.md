# Data Runs

Data Runs are a constrained pattern for running caller-owned data commands in
Crabbox and collecting bounded evidence. They build on `crabbox run` rather than
creating an ETL platform inside Crabbox.

Use this when a repo script should:

- run inside a short-lived Crabbox box or supported SSH lease;
- emit a bounded manifest, QA report, or summary artifact;
- keep raw datasets, connector state, schedules, and warehouse semantics outside
  Crabbox;
- report provider policy claims without pretending unsupported enforcement
  happened.

## Shape

`crabbox data run <name>` reads a `dataRuns.<name>` config entry and expands it
into existing run primitives:

- lease routing and capacity fields use the same flags as `crabbox run`;
- the configured command remains caller-owned code;
- the manifest path becomes a required artifact;
- the manifest is downloaded locally and validated after a successful run;
- a bounded summary JSON is written under `.crabbox/data-runs/<name>/`;
- when the underlying run has a coordinator run id, the bounded summary is stored
  on the run record as `dataSummary`.

This keeps the execution path inspectable: `--dry-run` prints the underlying
`crabbox run` command plus the manifest validation command.

## Manifest Boundary

The manifest schema is intentionally small. It records:

- schema version and success status;
- input and output names, identities, and bounded counts;
- scalar summary metrics;
- bounded artifact paths;
- declared identity, egress, and promotion policy claims.

The CLI rejects unsafe proof fields such as raw rows, raw data, credentials,
tokens, private keys, signed URLs, and row samples. This is a guardrail, not a
data-loss-prevention product. Callers still own redaction and data governance.

## Provider Boundary

For SSH-backed runs, Data Runs rely on the generic run evidence primitive:
`--require-artifact`. Delegated providers continue to fail closed for required
artifacts, artifact globs, and downloads unless an adapter grows an explicit
bounded retrieval capability. Islo supports safe relative single-file manifest
retrieval capped at 64 KiB.

Policy fields are neutral declarations:

- `declared-only`: Crabbox records the caller's intended policy but does not
  enforce it;
- `unsupported`: the provider cannot represent the policy.

Use `declared-only` by default. Providers can participate through the
provider-neutral data-run policy hook, but `enforced` is reserved for adapters
that can prove enforcement. Provider-specific identity, egress, labels, and
promotion behavior belongs behind provider adapters, not in core branching.

## Non-goals

Data Runs do not provide:

- connector catalogs;
- schedulers or DAG engines;
- retries or long-lived ETL state;
- raw data movement through Crabbox;
- source mutation or warehouse/lakehouse ownership;
- automatic production promotion.

Promotion should be a caller-owned, explicit, auditable command or follow-up
step.
