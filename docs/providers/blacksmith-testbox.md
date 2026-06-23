# Blacksmith Testbox Provider

Read when:

- choosing `provider: blacksmith-testbox`;
- wrapping an existing Blacksmith Testbox workflow with Crabbox;
- changing `internal/providers/blacksmith`.

Blacksmith Testbox is a delegated-run provider. Crabbox does not provision,
bootstrap, rsync, or expose VNC for the remote machine. It shells out to the
authenticated `blacksmith` CLI and adds Crabbox ergonomics on top: stable lease
IDs and slugs, repo claims, timing summaries, proof artifacts, and normalized
`list`/`status` output. Target OS is Linux only.

Configured [`cache.volumes`](../features/cache-volumes.md) are forwarded
as Blacksmith sticky disks during Testbox warmup. Use them for package-manager
stores and other rebuildable dependency caches; keep secrets, checkout state,
and proof artifacts out of sticky disks.

## When to use

Use Blacksmith when the repository already has a Testbox workflow and the remote
workspace should be owned and synced by Blacksmith. Choose AWS, Hetzner, Static
SSH, or Daytona instead when Crabbox needs to own SSH sync, interactive access,
or VNC/code surfaces.

## Commands

One-shot run:

```sh
crabbox run \
  --provider blacksmith-testbox \
  --blacksmith-org example-org \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --blacksmith-ref main \
  --timing-json \
  -- pnpm test
```

Reuse an existing Testbox by ID or slug:

```sh
crabbox run --provider blacksmith-testbox --id tbx_123 -- pnpm test
crabbox status --provider blacksmith-testbox --id tbx_123
crabbox stop --provider blacksmith-testbox tbx_123
```

Keep a Testbox between runs via a JSON session handle:

```sh
crabbox run --provider blacksmith-testbox --keep --lease-output /tmp/session.json -- npm test
lease_id="$(node -e 'console.log(require("/tmp/session.json").leaseId)')"
crabbox run --provider blacksmith-testbox --id "$lease_id" -- npm run smoke
crabbox stop --provider blacksmith-testbox "$lease_id"
```

Warm a fresh Testbox:

```sh
crabbox warmup \
  --provider blacksmith-testbox \
  --blacksmith-org example-org \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --blacksmith-ref main
```

`blacksmith` is accepted as an alias, but docs and scripts should prefer
`blacksmith-testbox`.

## Live Smoke

Run the shared smoke only when the selected workflow is a real Testbox workflow:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blacksmith-testbox CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

The smoke exits before any Blacksmith `list` or `run` call when it cannot derive
an org, when the configured workflow file is missing, or when that workflow file
does not contain a `useblacksmith/testbox`, `useblacksmith/begin-testbox`, or
`useblacksmith/run-testbox` step. With a valid org and workflow, it lists the
current inventory and runs one delegated `echo blacksmith-crabbox-ok && pwd`
command through the configured workflow/job/ref.

## Auth

Authentication lives entirely in the `blacksmith` CLI. Log in once before using
the provider:

```sh
blacksmith auth login
```

Crabbox never handles Blacksmith credentials directly; it invokes the
already-authenticated `blacksmith` binary on your PATH.

## Config

```yaml
provider: blacksmith-testbox
blacksmith:
  org: example-org
  workflow: .github/workflows/ci-check-testbox.yml
  job: test
  ref: main
  idleTimeout: 90m
  debug: false
```

Provider flags (override config):

```text
--blacksmith-org
--blacksmith-workflow
--blacksmith-job
--blacksmith-ref
```

Environment variables supply the same defaults:

```text
CRABBOX_BLACKSMITH_ORG
CRABBOX_BLACKSMITH_WORKFLOW
CRABBOX_BLACKSMITH_JOB
CRABBOX_BLACKSMITH_REF
CRABBOX_BLACKSMITH_IDLE_TIMEOUT
CRABBOX_BLACKSMITH_DEBUG
```

`blacksmith.workflow` (or `actions.workflow`, when it is not a generic
`hydrate`/`crabbox` workflow name) is required only to create a new Testbox.
Reusing an existing ID or slug does not need it. `idleTimeout` falls back to the
global `idleTimeout` when unset, and `debug` passes `--debug` through to the
Blacksmith CLI.

### Environment forwarding is unsupported

`--env-from-profile`, `--allow-env`, and `CRABBOX_ENV_ALLOW` help SSH-backed
providers but cannot inject CLI-side environment values into a delegated Testbox
command. When any of those knobs are present, Crabbox prints an
`env forwarding ... unsupported` summary and exits before warmup. Put live
secrets in the Blacksmith workflow instead. Repo-level env allowlists are
ignored for this provider so they can still cover SSH-backed providers.

## Lifecycle

Crabbox forwards to the Blacksmith CLI:

```sh
blacksmith testbox warmup <workflow> ...
blacksmith testbox run --id <tbx-id> ...
blacksmith testbox list
blacksmith testbox list --all
blacksmith testbox stop --id <tbx-id>
```

On warmup, Crabbox generates a per-Testbox SSH key locally, passes the public
key to `blacksmith testbox warmup --ssh-public-key`, parses the returned `tbx_`
ID, claims the Testbox for the current repo, and assigns a friendly slug. Reusing
a lease across repos needs `--reclaim`.

One-shot runs stop the Testbox and remove the local claim and key after the
command completes, unless `--keep` is set. `--keep-on-failure` keeps a failed
one-shot Testbox alive for debugging; successful runs still stop normally. A
failed Testbox otherwise remains available until idle timeout or an explicit
`crabbox stop`.

If `list`/`status` work but new warmups sit `queued` with no IP, Blacksmith is
accepting requests but not assigning capacity. Stop any queued IDs you created
and fall back to AWS, Hetzner, Static SSH, or Daytona until Blacksmith service,
billing, or org limits recover. A failed warmup triggers a best-effort cleanup
sweep of newly created Testboxes that match your configured workflow/job/ref.

### Failure bundles and proof

Failed runs write a local failure bundle (stdout, stderr, timing, redacted
env/config metadata) even though remote file capture is delegated to Blacksmith.
Captured streams are size-capped so a verbose successful run does not fill local
temp storage.

`--emit-proof <path>` works for successful Blacksmith runs. Crabbox renders the
same proof block used by SSH-backed runs from the delegated stdout/stderr
transcript, command timing, the Testbox ID, and any GitHub Actions run URL found
in the stream. When proof is requested, Crabbox also writes bounded transcript
artifacts under `.crabbox/runs/<testbox-id>/`:

```text
blacksmith.stdout.log
blacksmith.stderr.log
timing.json
metadata.json
```

### Sync stall guard

Crabbox terminates a local `blacksmith` invocation that stays in the sync phase
for five minutes without printing a sync-completion marker. Set
`CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS=0` to disable the guard, or a larger
millisecond value for intentionally huge local diffs. (`OPENCLAW_TESTBOX_SYNC_TIMEOUT_MS`
is also honored for legacy compatibility.)

### Portal visibility

When coordinator auth is configured, `crabbox list --provider blacksmith-testbox`
syncs visibility-only Testbox rows into the portal lease table. If Crabbox can
infer the owning GitHub Actions run, the portal links the row to the run and
workflow, shows the Actions status/conclusion, flags long-queued or long-running
rows as `stuck`, exposes a copyable local stop command, and provides a
visibility-only detail page.

## Capabilities

- SSH: no Crabbox SSH lease.
- Crabbox sync: no.
- Provider sync: yes, Blacksmith-owned.
- Desktop/browser/code: no Crabbox VNC/code surface.
- Proof: yes, from the delegated stream, timing, and metadata.
- Actions hydration: Blacksmith owns workflow setup; not Crabbox SSH hydration.
- Coordinator: no (always direct from the CLI).

## Gotchas

- `--sync-only`, `--checksum`, and `--force-sync-large` do not apply because
  Blacksmith owns sync.
- `--script`, `--script-stdin`, `--fresh-pr`, local stdout/stderr captures, and
  `--download` are rejected because Blacksmith owns command transport and remote
  file transport. Use `--emit-proof` for PR-ready transcript proof.
- `--artifact-glob` and `--require-artifact` run through the Blacksmith adapter:
  after command success, Crabbox asks the same Testbox to validate required
  globs and stream one bounded local tarball under `.crabbox/runs/<lease>/`.
- `--actions-runner` is rejected; Blacksmith owns runner hydration.
- `--tailscale`, desktop helpers, screenshots, VNC, and `artifacts collect` are
  rejected because Blacksmith owns machine connectivity.
- `list` and `status` are core-rendered from parsed Blacksmith CLI output.

Related docs:

- [Feature: Blacksmith Testbox](../features/blacksmith-testbox.md)
- [Provider backends](../provider-backends.md)
