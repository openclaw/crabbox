# data

Run policy-scoped data commands from repo config.

`crabbox data` is a POC-level wrapper around the existing lease and run
primitives. It loads `dataRuns.<name>` from config, injects data-specific
environment variables, runs the configured command, and validates a JSON
manifest for execute runs. It does not yet add broker-native data history,
provider-enforced source/sink policy, or promotion.

For the product and architecture plan, see [Data Runs](../plan/data-runs.md).

## Subcommands

| Command | Description |
| --- | --- |
| `crabbox data list` | List configured data runs. |
| `crabbox data plan <name>` | Validate config and print the effective policy/run expansion. |
| `crabbox data run <name>` | Run in execute mode and require a valid manifest. |
| `crabbox data run --dry-run <name>` | Run in data dry-run mode. |
| `crabbox data promote <run-id>` | Reserved; not implemented in the POC. |
| `crabbox data manifest <run-id>` | Reserved; not implemented in the POC. |

## data list

```sh
crabbox data list
```

Prints each configured data run with provider, target, source, sink, and the
default execute manifest path.

## data plan

```sh
crabbox data plan normalize-events
crabbox data plan --mode dry-run normalize-events
```

`data plan` is read-only. It validates the data run config, prints source/sink
metadata, shows which policy fields are enforced versus declared-only, and
prints the expanded `crabbox run` command with the `CRABBOX_DATA_*` environment
that will be forwarded.

The POC enforces:

- lease TTL and idle timeout through the normal lease path;
- `policy.requireDryRun` by blocking execute mode when it is true;
- execute manifest presence through `run --require-artifact`;
- execute manifest JSON shape through a temporary post-run download.

Source/sink access, identity, egress, row/byte caps, and PII logging policy are
declared-only in the POC.

Execute mode with a required manifest needs run-file download support. Delegated
providers without that capability are rejected before lease creation.

## data run

```sh
crabbox data run normalize-events
crabbox data run --dry-run normalize-events
crabbox data run --id blue-lobster normalize-events
crabbox data run --stop never normalize-events
```

Execute mode requires the configured command to write a JSON manifest to
`$CRABBOX_DATA_MANIFEST`, unless `manifest.required: false` is set. The manifest
must include positive `schemaVersion`, matching `dataRun` and `mode`, plus
`source` and `sink` objects.

`--dry-run` sets `CRABBOX_DATA_MODE=dry-run` and does not require an execute
manifest. The command is still responsible for avoiding final-output writes.

### Flags

| Flag | Description |
| --- | --- |
| `--id <lease-or-slug>` | Run against an existing lease instead of creating one. |
| `--no-hydrate` | Skip configured Actions hydration. |
| `--github-runner` | Hydrate by registering a GitHub self-hosted runner. |
| `--stop <policy>` | Override stop policy: `auto`, `always`, `success`, `failure`, or `never`. |
| `--dry-run` | Run the data command in dry-run mode. |

## Environment

`data run` forwards these variables to the remote command:

```text
CRABBOX_DATA_RUN=1
CRABBOX_DATA_RUN_NAME=<name>
CRABBOX_DATA_MODE=execute|dry-run
CRABBOX_DATA_SOURCE_KIND=<kind>
CRABBOX_DATA_SOURCE_MODE=read
CRABBOX_DATA_SOURCE_URI=<uri>
CRABBOX_DATA_SOURCE_WATERMARK=<watermark>   # when configured
CRABBOX_DATA_SINK_KIND=<kind>
CRABBOX_DATA_SINK_MODE=write|write-staging
CRABBOX_DATA_SINK_URI=<uri>
CRABBOX_DATA_MANIFEST=<relative manifest path>
```

## Configuration

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

    policy:
      requireDryRun: true
      maxRows: 200000000
      piiLogging: forbid

    shell: true
    command: >
      python pipelines/normalize_events.py
        --source "$CRABBOX_DATA_SOURCE_URI"
        --sink "$CRABBOX_DATA_SINK_URI"
        --manifest "$CRABBOX_DATA_MANIFEST"
```

Supported POC fields:

- job/routing fields shared with `jobs`: `provider`, `target`, `windows.mode`,
  `profile`, `class`, `architecture`, `type`, `market`, `ttl`, `idleTimeout`,
  `hydrate`, `actions`, `shell`, `command`, `noSync`, `checksum`,
  `forceSyncLarge`, `label`, `downloads`, and `stop`;
- `source.kind`, `source.mode`, `source.uri`, `source.watermark`;
- `sink.kind`, `sink.mode`, `sink.uri`;
- `policy.requireDryRun`, `policy.maxBytes`, `policy.maxRows`,
  `policy.piiLogging`, `policy.egress.allow`;
- `manifest.path`, `manifest.required`.

`source.mode` must be `read`. `sink.mode` defaults to `write-staging` and may be
`write`.
