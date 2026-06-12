# data

`crabbox data` is a thin wrapper for caller-owned data workflows that already
fit `crabbox run`: lease or reuse a box, sync the checkout, run a command, and
prove the command emitted a bounded manifest. It does not add connectors,
schedules, retries, warehouse semantics, or raw data movement to Crabbox.

Use it for pipeline contract smokes, backfill or migration checks, dbt/SQLMesh
or Airbyte-style wrappers, and agent-driven debugging/repair loops. The data
work can run entirely inside one box, from one box into caller-owned services, or
as service-to-service coordination. In all cases, Crabbox only runs the command
and validates bounded proof; the caller owns source and destination access.

## Usage

```sh
crabbox data list
crabbox data run <name>
crabbox data validate-manifest <path>
```

Configure runs in repo config:

```yaml
dataRuns:
  nightly-import:
    provider: aws
    target: linux
    shell: true
    command: ./scripts/nightly-import.sh
    manifest: reports/data/manifest.json
    requiredArtifacts:
      - reports/data/quality.json
    artifactGlobs:
      - reports/data/**
    policy:
      sourceIdentity: service-account:data-reader
      sinkIdentity: service-account:data-writer
      egress: restricted
      promotion: manual
      enforcement: declared-only
```

`data run` expands the entry into `crabbox run` with:

- `--require-artifact <manifest>` so a command that exits 0 still fails if the
  manifest is missing;
- `--download <manifest>=.crabbox/data-runs/<name>/manifest.json` so the CLI can
  validate the manifest locally;
- any configured `requiredArtifacts`, `artifactGlobs`, `junit`, and `downloads`;
- a `data:<name>` run label.

Use `--dry-run` to inspect the exact `crabbox run` and validation commands
without leasing:

```sh
crabbox data run --dry-run nightly-import
```

For delegated providers that reject `--shell`, use `commandArgs` to preserve an
exact argv:

```yaml
dataRuns:
  wandb-import:
    provider: wandb
    noSync: true
    commandArgs:
      - sh
      - -lc
      - mkdir -p reports/data && python run.py
    manifest: reports/data/manifest.json
```

## Manifest Contract

The manifest must be JSON, at most 64 KiB, and use schema
`crabbox.data-run.v1`:

```json
{
  "schemaVersion": "crabbox.data-run.v1",
  "name": "nightly-import",
  "status": "success",
  "inputs": [
    { "name": "source.users", "identity": "service-account:data-reader" }
  ],
  "outputs": [
    { "name": "warehouse.users", "identity": "service-account:data-writer", "rows": 42 }
  ],
  "summary": {
    "topology": "box-to-services",
    "useCase": "pipeline-debugging-contract-smoke",
    "flow": "source.users -> normalize_users -> warehouse.users",
    "tables": 1,
    "changed": true
  },
  "artifacts": [
    { "path": "reports/data/manifest.json", "kind": "manifest", "bytes": 512 }
  ],
  "policy": {
    "sourceIdentity": "service-account:data-reader",
    "sinkIdentity": "service-account:data-writer",
    "egress": "restricted",
    "enforcement": "declared-only"
  },
  "promotion": {
    "mode": "manual",
    "target": "staging-to-prod",
    "enforcement": "declared-only"
  }
}
```

Successful data runs require `status: success` and at least one output. The CLI
writes a bounded local summary to
`.crabbox/data-runs/<name>/summary.json`. When a coordinator run record exists,
the same bounded summary is attached to that run record as `dataSummary`.

Crabbox rejects obvious unsafe proof fields such as credentials, tokens, signed
URLs, raw rows, raw data, and row samples. Keep raw data in caller-owned storage;
the manifest should contain identities, counts, bounded metrics, and artifact
paths only.

## Policy And Providers

`policy.enforcement` and `promotion.enforcement` can be `declared-only` or
`unsupported`. The current wrapper records and validates the claim; it does not
make provider adapters enforce identity, egress, or promotion. `enforced` is
reserved for a later provider-policy hook that can prove enforcement.

Delegated-run providers still reject `--artifact-glob`, and they reject
`--require-artifact` / `--download` until they implement a bounded artifact
retrieval capability. Islo supports bounded single-file retrieval; use safe
relative manifest paths and keep the downloaded proof under 64 KiB.
