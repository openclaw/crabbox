# Observability

Read this when you need to:

- debug a failed or slow run;
- check who used capacity this month;
- find a remote machine to SSH into for live inspection;
- correlate Actions hydration with the remote workspace.

Crabbox surfaces operational visibility through CLI commands, coordinator usage
summaries, retained run history and logs, provider labels, GitHub Actions run
links, and Worker logs. The single most useful habit is to keep the **lease ID**
(`cbx_...`) and the **run ID** (`run_...`) together — most debugging paths start
from one of those two identifiers. See [Identifiers](features/identifiers.md) for
the full ID scheme.

## Lease state

Inspect a lease with `status`, `list`, and `inspect`:

```sh
crabbox status --id swift-crab
crabbox list --json
crabbox inspect --id swift-crab --json
```

Fields worth checking:

- lease ID and slug;
- owner and org;
- provider and server type;
- state (`active`, `released`, `expired`, `failed`);
- `createdAt`, `lastTouchedAt`, `idleTimeoutSeconds`, `ttlSeconds`, `expiresAt`;
- public address;
- SSH user and port;
- keep/delete behavior.

`status` accepts `--wait` (with `--wait-timeout`, default 5m) to block until the
lease reaches a stable state. Provider machines are tagged with Crabbox metadata,
so a cloud console can be correlated back to the lease that owns the instance.

## Usage and cost

`usage` reports monthly cost and capacity summaries from the coordinator:

```sh
crabbox usage
crabbox usage --scope user --user alice@example.com
crabbox usage --scope org --org example-org
crabbox usage --scope all --json
```

Scope is `user` (default), `org`, or `all`; `--month YYYY-MM` selects the
reporting month (defaults to the current UTC month). Reports include lease count,
active lease count, elapsed runtime, estimated elapsed cost, reserved worst-case
cost, and breakdowns by owner, org, provider, and server type. `usage` requires a
configured coordinator. See [Cost & usage](features/cost-usage.md) for how the
Worker computes rates and enforces budget caps.

## Run history and logs

Coordinator-backed `crabbox run` creates a durable run record before leasing
begins, appends lifecycle events as the CLI progresses, and finishes the run with
exit code, timing, and retained command output. Inspect those records with:

```sh
crabbox history
crabbox history --lease cbx_...
crabbox history --owner alice@example.com --json
crabbox logs run_...
crabbox events run_...
crabbox attach run_...
crabbox results run_...
```

- **`history`** lists recorded runs. Filter with `--lease`, `--owner`, `--org`,
  `--state`, and `--limit` (default 50). It is intended for command debugging,
  not unbounded log archival.
- **`logs <run-id>`** prints the retained remote stdout/stderr capture for a run.
  Use `--tail N` for the last N lines. Logs are stored in 64 KiB chunks with an
  8 MiB cap, so very long output is truncated.
- **`events <run-id>`** prints the ordered phase and output events for a run.
  Filter with `--type`, `--phase`, `--after N`, and `--limit` (default 500).
- **`attach <run-id>`** follows events for a still-active run, polling every
  `--poll` interval (default 1s), so you can watch a run another CLI started.
- **`results <run-id>`** prints structured test-result summaries. See
  [Test results](features/test-results.md).

The run ID is `run_<hex>`; the lease ID is `cbx_<12 hex>`. `history`, `logs`,
`events`, `attach`, and `results` all accept the run ID as a positional argument
or via `--id`.

### Capturing run output locally

By default, coordinator logs and events go to the broker and live progress goes
to your terminal. To redirect streams into local files:

- `run --capture-stdout <path>` writes remote stdout to the file and leaves
  coordinator logs/events plus lifecycle progress on stderr.
- `run --capture-stderr <path>` does the same for remote stderr.

Failed runs write a local failure bundle to `.crabbox/captures/*.tar.gz` by
default. SSH-backed runs bundle the uploaded script, redacted env/config
summaries, timing JSON, command stdout/stderr, common test/report/log paths, and
a generic gateway log tail when present. Blacksmith delegated runs bundle
stdout/stderr plus timing and redacted env/config metadata. The stdout/stderr
files captured inside automatic failure bundles are size-capped — pass
`--capture-stdout` / `--capture-stderr` when you need a complete local stream
file. Remote archive entries are confined to the bundle subtree; unsafe links
and special files are omitted. `--capture-on-fail` is still accepted as a compatibility alias; failure
bundles are saved automatically on non-zero exit regardless.

Crabbox does **not** redact captured files. Treat every bundle and capture file
as secret-bearing until you have reviewed it. On Unix-like hosts, Crabbox
creates local captures, downloads, proofs, and failure bundles with owner-only
file permissions (`0600`); new output directories use `0700`.

To pull successful-run artifacts back without routing file bytes through the
coordinator log, use `--download remote=local` (repeatable):

```sh
crabbox run --id swift-crab \
  --download dist/report.xml=./report.xml \
  -- pnpm test
```

Test results are stored as structured summaries when `--junit`,
`--results-auto`, `results.junit`, or `results.auto` is configured.

Successful Blacksmith runs additionally support `--emit-proof <path>`: when
requested, Crabbox writes bounded stdout/stderr, timing, metadata, and the
generated proof block as local run artifacts.

### Timing and phases

`--timing-json` prints final timing as JSON, including sync phases and command
phases. Failed runs add `blockedStage` and `retryLikely` when Crabbox can
classify the likely blocker; the human-readable run summary prints the same
values as `blocked_stage` and `retry_likely`.

Commands can define their own phases by printing marker lines to stdout or
stderr:

```sh
echo CRABBOX_PHASE:install
pnpm install --frozen-lockfile
echo CRABBOX_PHASE:build
pnpm build
echo CRABBOX_PHASE:test
pnpm test
```

### Local benchmark ledger

`--timing-record=default` and `--timing-record <path>` append the final
`TimingReport` to a local JSONL benchmark store. The default store is
`<CrabboxStateDir()>/timings.jsonl`, which uses `$XDG_STATE_HOME/crabbox` when
available and otherwise falls back to the user config directory's
`crabbox/state` directory.

The ledger is explicit local state. Plain `crabbox run` does not write it.
Recorded rows can include repo paths, remote workdirs, command display text,
labels, artifact paths, and lease metadata because they preserve the timing
payload. Use [`crabbox bench report`](commands/bench.md) to aggregate local
observations, and treat insufficient sample counts as a prompt to collect more
local evidence rather than as a provider ranking.

### Forwarding live secrets

When local `CRABBOX_ENV_ALLOW` is set, `run` prints the variable names selected
for forwarding plus safe metadata (whether secret-looking names are set and their
value length). Values are never printed. Delegated Testbox providers report that
forwarding is unsupported, because secrets belong in the provider workflow.

For one-off live secrets, avoid hand-written `source` boilerplate:

```sh
crabbox run \
  --env-from-profile ~/.project-live.profile \
  --allow-env API_TOKEN \
  --preflight \
  -- ./scripts/live-smoke.sh
```

Crabbox parses simple `export NAME=value` and `NAME=value` profile lines without
executing the profile. Only names selected by `--allow-env`, `env.allow`, or
`CRABBOX_ENV_ALLOW` are forwarded, and summaries show presence/length metadata
for secret-looking names rather than values. On native Windows, the profile
handoff file is uploaded as UTF-8 and imported with PowerShell UTF-8 decoding, so
non-ASCII token material and paths do not depend on the system code page. See
[Env forwarding](features/env-forwarding.md).

### Running scripts instead of argv

Use `--script <file>` or `--script-stdin` when the remote command is more than a
small argv:

```sh
crabbox run \
  --script ./scripts/provider-smoke.sh \
  --env-from-profile ~/.project-live.profile \
  --allow-env API_TOKEN \
  --timing-json
```

The script is uploaded under `.crabbox/scripts/` in the remote workdir and is
included in failure bundles. POSIX SSH providers support this path; delegated
providers reject it before reading stdin because they own command transport.
Native Windows targets upload scripts too and run them through Windows
PowerShell — use `--shell` for short snippets and `--script <file.ps1>` for
longer runs. Crabbox writes Windows scripts as UTF-8 with a byte-order mark when
the input has none, which keeps Windows PowerShell 5.1 from mojibaking non-ASCII
script source:

```sh
crabbox run \
  --provider ssh \
  --target windows \
  --windows-mode normal \
  --static-host win-dev.local \
  --preflight \
  --script ./scripts/windows-smoke.ps1 \
  -- -Mode smoke
```

For PR debugging that should not inherit local dependency churn, run against a
fresh remote checkout:

```sh
crabbox run \
  --fresh-pr example-org/my-app#123 \
  --script ./scripts/e2e-smoke.sh
```

Add `--apply-local-patch` only when the local diff should be applied on top of
the PR checkout. PR URLs must be on `github.com`; non-GitHub and GitHub
Enterprise PR URLs are rejected rather than rewritten to a public clone. The
numeric shorthand uses the current repository's GitHub origin.

## Remote debugging

SSH in for live process and filesystem inspection:

```sh
crabbox ssh --id swift-crab
crabbox inspect --id swift-crab --json
```

Useful remote checks:

```sh
crabbox-ready
test -f /var/lib/crabbox/bootstrapped
df -h
free -h
ps aux --sort=-%cpu | head
```

A lease created with `--keep` stays reachable over SSH until `crabbox stop`, idle
expiry, or the TTL cap removes it. For one-shot E2E debugging, add
`--keep-on-failure`: Crabbox releases successful runs normally, but on failure it
prints the inspect, SSH, and stop commands for the exact failed lease and leaves
idle/TTL expiry to clean it up later.

### Preflight capability snapshot

For a concise pre-command capability snapshot, add `--preflight`:

```sh
crabbox run --id swift-crab --preflight -- pnpm test:changed
```

Preflight prints a target-specific capability snapshot from the same workdir the
command will run in. It sources the Actions handoff env file when present and
marks the workspace as raw or Actions-hydrated. A raw workspace that has Actions
hydration configured prints the exact hydrate command suggestion and whether the
selected provider/target supports hydration.

Preflight is a probe layer, not an installer. Missing tools print
`tool=missing`; Crabbox does not run `apt install`, `corepack prepare`,
`bun install`, or any other setup. Install toolchains through Actions hydration,
a prebaked image, a devcontainer/Nix/mise/asdf setup, or the uploaded
script/command itself.

The built-in probes cover common toolchains — `git`, `tar`, `node`, `npm`,
`corepack`, `pnpm`, `yarn`, `bun`, `docker`, `uv` — plus target-specific probes
such as `sudo`, `apt`, `bubblewrap`, `powershell`, `execution_policy`,
`longpaths`, `temp`, and `pwsh`. Override the probe list per run:

```sh
crabbox run --preflight --preflight-tools node,bun,docker -- bun test
```

Or per repository:

```yaml
run:
  preflightTools:
    - node
    - bun
    - docker
```

## Actions hydration

`crabbox actions hydrate` populates a lease's workspace by driving the repo's
configured workflow setup over SSH by default, waiting for a ready marker. The
marker path is the key local correlation point; `--github-runner` instead
registers the box as a self-hosted runner and reports the workflow run URL.

```sh
crabbox actions hydrate --id swift-crab --workflow .github/workflows/hydrate.yml
crabbox inspect --id swift-crab --json
```

A hydrated run writes non-secret handoff data for later
`crabbox run --id swift-crab` commands. Secrets and OIDC tokens stay
workflow-step scoped unless the workflow intentionally writes its own short-lived
handoff. See [Actions hydration](features/actions-hydration.md).

## Live provider debugging

For live provider or end-to-end test runs, prefer an Actions-hydrated lease when
tests need Node, pnpm, Docker services, repository secrets, or GitHub OIDC:

```sh
crabbox warmup --provider aws --class beast --keep
crabbox actions hydrate --id swift-crab --workflow .github/workflows/hydrate.yml
mkdir -p .crabbox/logs
CRABBOX_ENV_ALLOW=OPENAI_API_KEY,OPENAI_BASE_URL \
  crabbox run --id swift-crab \
  --preflight \
  --timing-json \
  --capture-stdout .crabbox/logs/live-provider.stdout.log \
  --capture-stderr .crabbox/logs/live-provider.stderr.log \
  --keep-on-failure \
  --shell 'echo CRABBOX_PHASE:install; pnpm install --frozen-lockfile; echo CRABBOX_PHASE:test; pnpm test:live'
```

For Blacksmith Testbox comparison runs, keep secrets in the Testbox workflow
environment. Crabbox reports that `CRABBOX_ENV_ALLOW` forwarding is unsupported,
because Blacksmith owns command execution:

```sh
CRABBOX_ENV_ALLOW=OPENAI_API_KEY \
  crabbox run --provider blacksmith-testbox \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --preflight \
  -- pnpm test:live:providers
```

## Worker logs

When the coordinator path fails before SSH is established, check Worker logs and
Durable Object errors. The symptoms usually fall into a few groups:

- auth failure;
- cost limit rejection;
- provider quota or capacity rejection;
- provider API failure;
- Durable Object alarm or state-transition bug.

Keep the lease ID, owner, org, provider, class, and request time on hand when
comparing CLI output to Worker logs. See [How it works](how-it-works.md) and the
[coordinator](features/coordinator.md) reference for the request flow.

## Gaps

Crabbox observability is sufficient for maintainer operations but is not yet a
full analytics product. Notably missing:

- alerting on budget or failure-rate thresholds;
- a dashboard UI.
