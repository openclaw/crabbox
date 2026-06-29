# Crabbox Data Runs

Status: POC CLI wrapper shipped. Current behavior is documented in
[data](../commands/data.md). Broker-native data history, provider-enforced
source/sink policy, promotion, and manifest lookup remain proposed here.

Read when:

- evaluating ETL-shaped work for Crabbox;
- designing runs with scoped production or customer-data access;
- connecting Crabbox to a scheduler, ingestion tool, or data workflow;
- reviewing the data boundary for disposable boxes.

## Decision

Build Data Runs as a run class, not an ETL platform.

A data run is a normal Crabbox lease with a data policy attached:

```text
trigger -> lease box -> sync code -> attach scoped access
        -> run command -> validate manifest -> record proof -> release
```

Crabbox owns the execution envelope: lease lifecycle, identity attachment,
policy metadata, proof collection, audit trail, cost controls, and cleanup. The
caller owns scheduling, retries, pipeline logic, and warehouse semantics.

Product sentence:

```text
A short-lived box for every data run.
```

## Boundary

Data Runs add data-specific proof to the existing loop:

```text
lease -> sync -> run -> record proof -> release
```

Do not build a connector catalog, visual pipeline editor, DAG scheduler,
warehouse, lakehouse, central project secret store, or source-of-truth data
system.

The broker remains a control plane. Raw data stays on the runner-to-source and
runner-to-sink path; it must not traverse the broker.

Safe defaults:

```text
source: read
scratch: read/write, lease-local, deleted with lease
sink: write to staging
promote: explicit, auditable, optional
```

In-place source mutation is not a default behavior. It needs an explicit promote
or commit step and a policy that permits it.

## Config Shape

Repository config gets a `dataRuns` section. A data run is a command plus a
source/sink contract:

```yaml
dataRuns:
  normalize-events:
    provider: aws
    target: linux
    ttl: 90m

    source:
      kind: s3
      mode: read
      uri: s3://example-raw/events/

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
      piiLogging: forbid

    shell: true
    command: >
      python pipelines/normalize_events.py
        --source "$CRABBOX_DATA_SOURCE_URI"
        --sink "$CRABBOX_DATA_SINK_URI"
        --manifest "$CRABBOX_DATA_MANIFEST"
```

Command surface:

```sh
crabbox data list
crabbox data plan normalize-events
crabbox data run normalize-events --dry-run
crabbox data run normalize-events
crabbox data promote <run-id>
crabbox data manifest <run-id>
```

The POC implements `list`, `plan`, and `run`. `promote` and `manifest` are
reserved command names that fail clearly until those phases exist.

Later, the same primitive can sit under `run`:

```sh
crabbox run --data normalize-events --dry-run
crabbox run --data normalize-events
```

## Modes

| Mode | Writes | Purpose |
| --- | --- | --- |
| `plan` | none | Validate config, identity, reachability, schema, and enforcement tier. |
| `dry-run` | scratch only | Run on a bounded sample or staging prefix and emit a quality report. |
| `execute` | approved sink or staging | Run the deterministic command with policy and manifest checks. |
| `promote` | final sink only | Commit approved staging output to the final target. |

Reasoning or AI assistance belongs in `plan` and `dry-run`. Production writes
should use pinned code, a pinned image, or a reviewed command.

## Manifest

Every successful `execute` run must emit a manifest unless the config explicitly
sets `manifest.required: false`. Crabbox stores a bounded summary in run
metadata and keeps the full file as a normal artifact.

Suggested path:

```text
crabbox-data/<run-id>/data-manifest.json
```

Minimal shape:

```json
{
  "schemaVersion": 1,
  "dataRun": "normalize-events",
  "runID": "run_abc123",
  "mode": "execute",
  "source": {
    "kind": "s3",
    "uri": "s3://example-raw/events/",
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
    "status": "passed"
  }
}
```

The manifest is proof, not a data payload. It must not include rows, samples,
credentials, signed URLs, or connection strings.

## Policy Tiers

Each policy field is either declared or enforced.

Declared policy is visible in plan output, logs, portal views, and automation.
It makes the contract reviewable even when Crabbox cannot enforce the field.

Enforced policy is applied by Crabbox or a provider adapter. Examples:

- lease TTL and idle timeout;
- provider identity attachment, such as an instance profile or service account;
- environment forwarding allowlists;
- brokered run metadata and audit records;
- artifact upload size limits;
- provider or bridge egress allowlists where available.

Plan output should be explicit:

```text
policy ttl=90m enforced=lease
policy requireDryRun=true enforced=execute-gate
policy egress.allow declared=only
policy maxBytes=500GiB declared=only
```

Core must stay provider-neutral. If policy enforcement needs cloud-specific
behavior, add a provider hook instead of adding `provider == ...` logic to core.

## Implementation

Phase 1 adds `crabbox data` as a CLI wrapper over today's lease/run primitives.
No Worker schema change is required. The POC slice is shipped.

Phase 1 includes:

- config parsing for `dataRuns`;
- `data list`, `data plan`, `data run --dry-run`, and `data run`;
- reserved env injection for run name, mode, source URI, sink URI, and manifest
  path;
- execute manifest validation after command success;
- artifact collection for the manifest and optional quality report;
- tests for config validation, command expansion, and manifest failure.

Future delegated-provider support can use a bounded stdout manifest summary
when the provider cannot download files after a run:

```text
CRABBOX_DATA_MANIFEST_BEGIN
{ "...": "bounded manifest summary" }
CRABBOX_DATA_MANIFEST_END
```

The shipped POC validates downloaded file manifests. It rejects delegated
providers that cannot download run files before lease creation.

Phase 2 makes data runs first-class in brokered run history: run kind, data run
name, mode, bounded manifest summary, portal/history rendering, and cost/usage
grouping. Raw data still stays out of the broker.

Phase 3 adds provider hooks for identity, egress, labeling, policy enforcement
reporting, and bounded delegated artifact retrieval. The broker rejects a policy
that requires enforcement when the selected provider cannot enforce it.

Phase 4 adds `crabbox data promote <run-id>`. Promotion runs in a separate
short-lived data box or provider-native operation with its own manifest. It is
never a hidden side effect of `execute`.

## Security Rules

- Prefer provider-native short-lived or attached identity over forwarded
  secrets.
- Never pass secrets as CLI flags.
- Do not forward broad cloud env such as `AWS_*` by default.
- Do not print rows, samples, credentials, signed URLs, or connection strings.
- Treat manifests and artifacts as potentially sensitive.
- Make source mutation opt-in and separately reviewable.
- Require dry-run for high-risk policies.
- Keep untrusted tenants on separate brokers and cloud accounts.

## First User

The best first user is a software or data team that already has pipeline code,
an external trigger, managed cloud credentials, and a need for isolated,
audited, high-compute transforms.

That lets Crabbox win with its strengths: disposable compute, policy metadata,
remote execution, artifacts, and cleanup.

## Acceptance Criteria

- A dry-run cannot write final outputs.
- A successful execute run without a required manifest fails.
- The broker never receives raw source data.
- Plan output distinguishes enforced policy from declared-only policy.
- Provider-specific enforcement lives behind provider adapters.
- Promotion is explicit, audited, and separately reversible.
