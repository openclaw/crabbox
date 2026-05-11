# Blacksmith Testbox Provider

Read when:

- choosing `provider: blacksmith-testbox`;
- wrapping an existing Blacksmith Testbox workflow with Crabbox;
- changing `internal/providers/blacksmith`.

Blacksmith Testbox is a delegated run provider. Crabbox does not provision,
bootstrap, rsync, or expose VNC for the remote machine. It shells out to the
authenticated Blacksmith CLI and keeps Crabbox ergonomics around IDs, slugs,
repo claims, timing, and normalized output.

## When To Use

Use Blacksmith when the repo already has a Testbox workflow and the remote
workspace should be owned by Blacksmith. Use AWS, Hetzner, Static SSH, or Daytona
when Crabbox must own SSH sync and interactive access.

## Commands

One-shot run:

```sh
crabbox run \
  --provider blacksmith-testbox \
  --blacksmith-org openclaw \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --blacksmith-ref main \
  --timing-json \
  -- pnpm test
```

Reuse an existing Testbox:

```sh
crabbox run --provider blacksmith-testbox --id tbx_123 -- pnpm test
crabbox status --provider blacksmith-testbox --id tbx_123
crabbox stop --provider blacksmith-testbox tbx_123
```

Warm a fresh Testbox:

```sh
crabbox warmup \
  --provider blacksmith-testbox \
  --blacksmith-org openclaw \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --blacksmith-ref main
```

`blacksmith` is accepted as an alias, but docs and scripts should prefer
`blacksmith-testbox`.

## Config

```yaml
provider: blacksmith-testbox
blacksmith:
  org: openclaw
  workflow: .github/workflows/ci-check-testbox.yml
  job: test
  ref: main
  idleTimeout: 90m
```

Environment variables can provide the same defaults:

```text
CRABBOX_BLACKSMITH_ORG
CRABBOX_BLACKSMITH_WORKFLOW
CRABBOX_BLACKSMITH_JOB
CRABBOX_BLACKSMITH_REF
```

Blacksmith authentication stays in the Blacksmith CLI. Run
`blacksmith auth login` before using this provider.

## Lifecycle

Crabbox forwards:

```sh
blacksmith testbox warmup ...
blacksmith testbox run ...
blacksmith testbox list
blacksmith testbox list --all
blacksmith testbox stop ...
```

If list/status calls work but new warmups sit `queued` with no IP, the
Blacksmith service or organization is accepting requests but not assigning
capacity. Stop queued IDs you created and use AWS, Hetzner, Static SSH, or
Daytona until Blacksmith service, billing, or org limits are healthy again.

Crabbox stores a per-Testbox SSH key locally, claims the Testbox for the current
repo, maps IDs to friendly slugs, and prints a normal Crabbox timing summary.
One-shot runs clean up the local claim/key and stop the Testbox after command
completion unless `--keep` is set.

Crabbox terminates a local Blacksmith CLI invocation that remains in the sync
phase for five minutes without post-sync output. Set
`CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS=0` to disable the guard, or set a larger
millisecond value for intentionally huge local diffs.

When coordinator auth is configured, `crabbox list --provider blacksmith-testbox`
also syncs visibility-only Testbox rows into the portal lease table. If Crabbox
can infer the owning GitHub Actions run, the portal links the row to the run and
workflow, shows the Actions status/conclusion, flags long-queued or long-running
rows as `stuck`, exposes a copyable local stop command, and provides a
visibility-only detail page for the row.

## Capabilities

- SSH: no Crabbox SSH lease.
- Crabbox sync: no.
- Provider sync: yes, Blacksmith-owned.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Actions hydration: Blacksmith-owned workflow setup, not Crabbox SSH hydration.
- Coordinator: no.

## Gotchas

- `--sync-only`, `--checksum`, and `--force-sync-large` do not apply because
  Blacksmith owns sync.
- `list` and `status` are core-rendered from parsed Blacksmith CLI output.
- `blacksmith.workflow` is required only when Crabbox needs to create a Testbox.
  Reusing an existing ID or slug does not need workflow config.

Related docs:

- [Feature: Blacksmith Testbox](../features/blacksmith-testbox.md)
- [Provider backends](../provider-backends.md)
