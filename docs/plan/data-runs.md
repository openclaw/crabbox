# Data Runs: Design Plan

Read when:

- deciding whether Crabbox should grow a data-oriented execution surface;
- designing a run that needs scoped access to production or customer data;
- integrating Crabbox with an external scheduler, ingestion tool, or data-team
  workflow;
- reviewing security boundaries for data manipulation inside disposable boxes.

Status: proposal. This is not shipped behavior. The current authoritative
behavior remains the run, jobs, environment-forwarding, observability, and
artifacts documentation.

## Decision

Crabbox should pursue this shape, but only as a policy-scoped run pattern first.
A dedicated `crabbox data` command family should be treated as follow-on work,
not the first move.

Crabbox should provide the disposable execution boundary for data work:

```text
external trigger
  -> Crabbox creates a short-lived data box
  -> the box reads approved sources
  -> the box transforms data in isolated scratch space
  -> the box writes approved sink or staging outputs
  -> Crabbox records proof, lineage, metrics, and artifacts
  -> the box is released
```

Crabbox should not become an ETL platform, scheduler, connector catalog, data
warehouse, or data-plane broker. The caller owns pipeline logic. Crabbox owns
the safe execution envelope.

The product sentence is:

```text
A short-lived box for every data run.
```

That extends the existing run loop instead of creating a separate product:

```text
lease -> sync -> run -> record proof -> release
```

A normal Crabbox run says "run this command and tell me whether it passed." A
data run says "run this command with declared source/sink access and prove what
data it read, wrote, and validated."

The conservative first product move is to document and standardize the pattern
with today's `run`, `job`, env-forwarding, artifact, timing, and phase features.
Only add a new CLI namespace if repeated use proves that `jobs` and `run`
cannot express the contract cleanly enough.

## Stress Against The Promise

Crabbox's promise is not "we do data movement." Its promise is generic remote
software testing and execution with evidence. Data Runs match that promise only
when they make an execution boundary safer and more reviewable.

The idea passes the promise test because:

- a data pipeline is still caller-owned code running on a short-lived box;
- Crabbox already owns lease lifecycle, sync, run, logs, timing, artifacts, and
  cleanup;
- data-specific proof is another form of evidence, like JUnit, logs,
  screenshots, or timing records;
- policy declarations make remote execution safer without making Crabbox own
  the source or sink systems.

The idea fails the promise test if it asks Crabbox to own:

- connector implementation or catalog selection;
- schedule state, DAG retries, incremental cursors, or warehouse semantics;
- raw data transport through the broker, local CLI, or model context;
- source mutation without an explicit audited promotion step;
- provider-specific identity or egress behavior in core.

That means the correct first issue is not "build ETL in Crabbox." The correct
issue is "decide and document the bounded data-run contract, then prove it with
existing run primitives." Implementation should stop there unless real use shows
the generic primitives need a thin wrapper.

## Promise Fit Test

Every Data Runs slice must pass this test before it belongs in Crabbox:

- It runs caller-owned code or tooling inside a Crabbox lease or delegated
  sandbox.
- It improves the execution boundary, policy declaration, evidence capture, or
  cleanup story around that run.
- It keeps raw data out of the broker and out of model context.
- It treats sources, sinks, schemas, connector state, retries, scheduling, and
  warehouse semantics as caller-owned concerns unless the field is only a
  bounded proof summary.
- It routes provider-specific identity, network, labels, images, and cleanup
  through provider adapters rather than adding provider-name behavior to core.
- It remains useful as a command-line primitive that external schedulers,
  agents, CI jobs, or data platforms can call.

If a proposed slice fails this test, it belongs in a data tool, scheduler,
connector repo, or team-specific pipeline, not in Crabbox.

## Why It Fits

The current Crabbox promise is generic remote software testing and execution.
Data Runs fit that promise when they stay inside the same boundary:

- short-lived remote compute;
- provider abstraction;
- dirty-checkout sync or delegated archive sync;
- scoped environment forwarding;
- run history, logs, events, timing, and phase records;
- JUnit and artifact capture;
- cost guardrails and cleanup;
- SSH-lease and delegated sandbox providers.

The missing piece is a data-specific contract around a command, not a
data-specific execution engine. A data movement tool, warehouse client, dbt
project, custom script, or connector runtime can run inside the box. Crabbox
should make the run auditable, bounded, and disposable.

Current run primitives are already close:

- `--allow-env` and `--env-from-profile` provide explicit runtime inputs;
- `--artifact-glob` collects proof files from SSH-backed runs;
- `--junit` and results capture preserve structured validation;
- `--timing-json` reports stable timing;
- `CRABBOX_PHASE:<name>` markers let user commands name command phases;
- delegated providers can run sandbox proofs without exposing SSH.

Data Runs should compose those primitives into a reviewed workflow rather than
forking a parallel runner.

## Hard Boundary

Data Runs preserve Crabbox's architecture:

- The broker owns auth, lease state, policy metadata, usage, cost guardrails,
  cleanup, run records, and upload grants.
- The CLI and runner own command execution.
- Provider adapters own provider-specific identity, network, image, label,
  region, host, and cleanup behavior.
- The data path stays runner-to-source and runner-to-sink.
- Raw data must not traverse the broker.
- Raw data should not be copied into the model context.
- Source access is read-only by default.
- Sink access is write-only or staging-write by default.
- In-place source mutation requires an explicit promote/commit step and a
  policy that permits it.

The safe default is:

```text
source: read
scratch: read/write, lease-local, deleted with lease
sink: write to staging
promote: explicit, auditable, optional
```

## Non-Goals

Do not build these into the first product:

- a connector catalog;
- a visual pipeline editor;
- a long-running DAG scheduler;
- a central project secret store;
- a data warehouse or lakehouse;
- arbitrary multi-tenant untrusted execution;
- automatic source mutation;
- retry state that outlives the caller's scheduler or repository workflow;
- AI agents that directly write production data without dry-run and policy
  gates.

## User Experience

Phase 0 should use existing jobs and run flags. A repository can define a normal
Crabbox job that runs a data command, forwards only named variables, captures
reports, and requires the command to emit a manifest.

```yaml
jobs:
  normalize-events:
    provider: aws
    target: linux
    class: large
    ttl: 90m
    idleTimeout: 20m
    shell: true
    command: >
      CRABBOX_DATA_MODE=execute
      CRABBOX_DATA_SOURCE_URI=s3://example-raw/events/
      CRABBOX_DATA_SINK_URI=s3://example-clean/events-staging/
      CRABBOX_DATA_MANIFEST=reports/data/manifest.json
      python pipelines/normalize_events.py
        --source "$CRABBOX_DATA_SOURCE_URI"
        --sink "$CRABBOX_DATA_SINK_URI"
        --manifest "$CRABBOX_DATA_MANIFEST"
    downloads:
      - reports/data/manifest.json=reports/data/manifest.json
    junit:
      - reports/junit.xml
```

Run it with the existing surface:

```sh
crabbox job run normalize-events
crabbox run \
  --allow-env 'SOURCE_*' \
  --env-from-profile ~/.config/example-data.env \
  --require-artifact reports/data/manifest.json \
  --artifact-glob 'reports/data/**' \
  --junit reports/junit.xml \
  --shell 'python pipelines/normalize_events.py --manifest reports/data/manifest.json'
```

If that pattern repeats enough to justify first-class ergonomics, repository
config can grow a `dataRuns` section. A data run would look like a job, but with
a data access contract and manifest requirement.

```yaml
dataRuns:
  normalize-events:
    provider: aws
    target: linux
    class: large
    ttl: 90m
    idleTimeout: 20m

    source:
      kind: s3
      mode: read
      uri: s3://example-raw/events/
      watermark: updated_at

    sink:
      kind: s3
      mode: write-staging
      uri: s3://example-clean/events-staging/

    identity:
      aws:
        instanceProfile: crabbox-data-normalize-events

    policy:
      requireDryRun: true
      maxBytes: 500GiB
      maxRows: 200000000
      allowSchemaChange: false
      piiLogging: forbid
      egress:
        allow:
          - s3.amazonaws.com
          - "*.s3.amazonaws.com"

    command: >
      python pipelines/normalize_events.py
        --source "$CRABBOX_DATA_SOURCE_URI"
        --sink "$CRABBOX_DATA_SINK_URI"
        --manifest "$CRABBOX_DATA_MANIFEST"
```

Possible future command surface:

```sh
crabbox data list
crabbox data plan normalize-events
crabbox data run normalize-events --dry-run
crabbox data run normalize-events
crabbox data promote <run-id>
crabbox data manifest <run-id>
```

`crabbox data plan` would be read-only. It validates config, resolves provider
capabilities, prints the effective policy, and reports which parts can be
enforced by Crabbox versus only declared for the pipeline command.

`crabbox data run --dry-run` would execute against a bounded sample, scratch
prefix, or command-defined dry-run mode. It must not write final outputs.

`crabbox data run` would execute the transform and require the command to
produce a data manifest.

`crabbox data promote` would be the explicit step that turns staging output into
final output when the sink mode requires promotion.

The same concept can also be exposed through the existing `run` command once a
data run has expanded:

```sh
crabbox run --data normalize-events --dry-run
crabbox run --data normalize-events
```

That would keep the primitive close to `crabbox run`: users are still running
commands on boxes. The `crabbox data ...` namespace is the clearer workflow
surface; the `run --data` form is the composable primitive.

## Run Modes

Data Runs need four modes because data mutation has higher risk than test
execution.

| Mode | Writes | Purpose |
| --- | --- | --- |
| `plan` | none | Validate config, identity, source/sink reachability, schema, and enforcement tier. |
| `dry-run` | scratch only | Run on a bounded sample or staging prefix and emit a diff or quality report. |
| `execute` | approved sink/staging | Run the deterministic command with full policy and manifest checks. |
| `promote` | final sink only | Commit approved staging output to the final target. |

Reasoning or AI assistance belongs in `plan` and `dry-run` first. Production
writes should execute pinned code, a pinned image, or a reviewed command.

## Data Manifest

Every execute run must emit a manifest. Crabbox validates and stores a bounded
summary in run metadata; the full file remains a normal artifact.

Suggested path:

```text
reports/data/<run-id>/data-manifest.json
```

Suggested schema:

```json
{
  "schemaVersion": 1,
  "dataRun": "normalize-events",
  "runID": "run_abc123",
  "mode": "execute",
  "startedAt": "2026-06-04T10:00:00Z",
  "finishedAt": "2026-06-04T10:08:32Z",
  "source": {
    "kind": "s3",
    "uri": "s3://example-raw/events/",
    "watermark": "2026-06-04T09:55:00Z",
    "bytesRead": 1280000000,
    "rowsRead": 4200000
  },
  "sink": {
    "kind": "s3",
    "uri": "s3://example-clean/events-staging/run_abc123/",
    "bytesWritten": 620000000,
    "rowsWritten": 4199800
  },
  "transform": {
    "repo": "example-org/my-app",
    "commit": "abc1234",
    "commandHash": "sha256:..."
  },
  "quality": {
    "status": "passed",
    "warnings": [],
    "checks": [
      { "name": "schema_compatible", "status": "passed" },
      { "name": "null_rate", "status": "passed" }
    ]
  },
  "outputs": [
    {
      "kind": "manifest",
      "path": "reports/data/run_abc123/data-manifest.json",
      "sha256": "..."
    }
  ]
}
```

Crabbox should reject successful `execute` runs that omit the manifest unless
the data run explicitly sets `manifest.required: false`.

For delegated providers that cannot download files after a run, Phase 1 should
support a bounded manifest-on-stdout protocol:

```text
CRABBOX_DATA_MANIFEST_BEGIN
{ "...": "bounded manifest summary" }
CRABBOX_DATA_MANIFEST_END
```

The manifest file remains preferred for SSH providers. The stdout protocol lets
delegated sandboxes participate without pretending they expose the SSH artifact
surface.

## Policy Model

Data policy has two enforcement tiers.

**Declared policy** is visible to users, logs, portal, and automation. It makes
the contract reviewable even when Crabbox cannot enforce every part itself.

**Enforced policy** is actively applied by Crabbox or the provider:

- lease TTL and idle timeout;
- provider identity attachment, such as an AWS instance profile or GCP service
  account;
- environment forwarding allowlists;
- brokered run metadata and audit trail;
- artifact upload size limits;
- optional egress allowlists where a provider or bridge can enforce them.

The CLI should print the tier for each policy line:

```text
policy source.mode=read enforced=identity
policy sink.mode=write-staging enforced=identity
policy egress.allow declared=only
policy maxBytes=500GiB enforced=manifest-check
```

That keeps the system honest. A policy is still useful when declared only, but
the caller must know the difference.

## Implementation Plan

### Phase 0: Document The Existing Pattern

Before adding code, document how to run policy-scoped data work with existing
Crabbox primitives:

- a normal `job` or `run` command;
- explicit env forwarding through `--allow-env` and `--env-from-profile`;
- a repository-owned command that reads source/sink refs from env or config;
- `CRABBOX_PHASE:<name>` markers for lifecycle evidence;
- JUnit or equivalent validation output;
- `reports/data/**` proof files collected through `--artifact-glob` on direct
  `run` or specific `downloads` from jobs;
- required evidence checks through `--require-artifact`;
- a manifest and quality report that contain bounded summaries, not raw rows.

This phase should also define what Crabbox must not do with raw data. It is the
right first move because it proves whether the promise holds without adding a
domain-specific command surface.

### Phase 1: CLI Wrapper And Manifest

Add a `crabbox data` command family that expands a `dataRuns.<name>` config into
today's lease/run primitives only after Phase 0 proves the pattern is recurring
and awkward enough to need a wrapper. It must work with both SSH-lease providers
and delegated-run providers.

No Worker schema changes are required for the first slice.

Phase 1 should include:

- config parsing for `dataRuns`;
- `data list`;
- `data plan`;
- `data run --dry-run`;
- `data run`;
- reserved env injection:
  - `CRABBOX_DATA_RUN=1`
  - `CRABBOX_DATA_RUN_NAME`
  - `CRABBOX_DATA_MODE`
  - `CRABBOX_DATA_SOURCE_KIND`
  - `CRABBOX_DATA_SOURCE_URI`
  - `CRABBOX_DATA_SINK_KIND`
  - `CRABBOX_DATA_SINK_URI`
  - `CRABBOX_DATA_MANIFEST`
- manifest validation after command success;
- local artifact collection for the manifest and optional quality report;
- tests for config validation, command expansion, and manifest failure.

This is enough for trusted data teams that already have cloud IAM and network
controls.

### Phase 2: Broker Awareness

Make data runs first-class in brokered run history.

Add:

- run kind: `command | data`;
- data run name and mode in `RunRecord`;
- bounded manifest summary in run metadata;
- portal and `history` rendering for source, sink, row counts, byte counts, and
  quality status;
- cost/usage grouping by run kind and data run name.

Raw data still stays out of the broker.

### Phase 3: Provider Identity And Network Enforcement

Add provider adapter hooks for data policy support. Core should not branch on
provider names.

Provider-owned capabilities:

- attach or override cloud identities for data runs;
- apply provider egress restrictions where practical;
- label/tag data leases with data run name, mode, and policy hash;
- report which policy fields were enforced.

The broker should reject a policy that asks for enforced source/sink access when
the provider cannot enforce it.

### Phase 3a: Delegated Artifact Retrieval

Add an optional delegated-provider capability for fetching a bounded remote file
or artifact after command success.

This should be a generic provider hook, not provider-name logic in core. It
would let delegated sandbox providers support the same manifest/artifact
behavior as SSH leases when their APIs can retrieve files.

### Phase 4: Promotion

Add `crabbox data promote <run-id>` for staging-to-final commits.

Promotion should be implemented as a separate short-lived data box or a
provider-native operation with its own manifest. It should never be a silent
post-step of `execute`.

## Security Rules

- Prefer cloud-native short-lived or attached identity over forwarded secrets.
- Never pass secrets as CLI flags.
- Do not forward broad cloud env such as `AWS_*` by default.
- Do not print rows, samples, credentials, signed URLs, or connection strings in
  run logs.
- Treat manifests and artifacts as potentially sensitive.
- Make source mutation opt-in and separately reviewable.
- Require dry-run for policies marked high risk.
- Keep untrusted tenants on separate brokers and cloud accounts.

## Best First User

The best first user is not a generic no-code ETL buyer. It is a software or
data team that already has:

- pipeline code in a repo;
- an existing scheduler, webhook, ingestion trigger, or CI workflow;
- cloud object storage, warehouse, or database credentials managed outside
  Crabbox;
- a need for isolated, audited, high-compute transforms;
- a desire to avoid long-lived shared ETL workers.

That lets Crabbox win with its strengths: disposable compute, policy metadata,
remote execution, artifacts, and cleanup.

## Final Shape

Crabbox Data Runs are ephemeral, policy-scoped data execution runs: read from
approved sources, transform inside a disposable data box, write to approved
staging or sinks, emit lineage and quality proof, then disappear.

This is powerful without turning Crabbox into the wrong product.
