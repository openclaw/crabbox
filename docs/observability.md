# Observability

Read when:

- debugging a failed or slow run;
- checking who used capacity this month;
- finding a remote machine for SSH inspection;
- correlating Actions hydration with the remote workspace.

Crabbox exposes operational visibility through CLI commands, coordinator usage summaries, retained run history/logs, provider labels, GitHub Actions run links, and Worker logs. The reliable path is to keep the lease ID and run ID together.

## Lease State

Use `status`, `list`, and `inspect`:

```sh
bin/crabbox status --id blue-lobster
bin/crabbox list --json
bin/crabbox inspect --id blue-lobster --json
```

Important fields:

- lease ID and slug;
- owner and org;
- provider and server type;
- state;
- `createdAt`, `lastTouchedAt`, `idleTimeoutSeconds`, `ttlSeconds`, and `expiresAt`;
- public address;
- SSH user and port;
- keep/delete behavior.

Provider machines are labeled with Crabbox metadata so cloud consoles can be correlated back to the lease.

## Usage And Cost

Use `usage` for monthly summaries:

```sh
bin/crabbox usage
bin/crabbox usage --scope user --user alice@example.com
bin/crabbox usage --scope org --org example-org
bin/crabbox usage --scope all --json
```

Reports include lease count, active lease count, elapsed runtime, estimated elapsed cost, reserved worst-case cost, and breakdowns by owner, org, provider, and server type.

## Run History And Logs

Coordinator-backed `crabbox run` creates a durable run record before leasing
starts, appends lifecycle events while the CLI progresses, and finishes the run
with exit code, timing, and retained command output.

Use:

```sh
bin/crabbox history
bin/crabbox history --lease cbx_...
bin/crabbox history --owner alice@example.com --json
bin/crabbox events run_...
bin/crabbox attach run_...
bin/crabbox logs run_...
bin/crabbox results run_...
```

History is for command debugging, not unlimited log archival. Events are ordered
phase and output chunks for reconnect/inspection, and `attach` can follow those
events while the original CLI is still alive. Logs are bounded retained remote
stdout/stderr captures. `run --capture-stdout <path>` stores stdout only in the
local file and leaves coordinator logs/events to stderr plus lifecycle events.
`run --capture-stderr <path>` does the same for remote stderr. Failed runs write
a local `.crabbox/captures/*.tar.gz` bundle by default. SSH-backed runs include
the uploaded script, redacted env/config summaries, timing JSON, command
stdout/stderr, common test/report/log paths, and a generic gateway log tail when
present. Blacksmith delegated runs include stdout/stderr plus timing and
redacted env/config metadata. Successful Blacksmith runs also support
`--emit-proof`; when requested, Crabbox writes bounded stdout/stderr, timing,
metadata, and the generated proof block as local run artifacts. Implicit
stdout/stderr files inside automatic failure bundles are capped; use
`--capture-stdout` / `--capture-stderr` when a full local stream file is
required. `--capture-on-fail` remains accepted for older scripts. Crabbox does
not redact captured files, so treat them as secret-bearing until reviewed.
`run --download remote=local` copies successful-run artifacts back to the local
machine without adding file bytes to coordinator logs.
Test results are stored as structured summaries when `--junit` or
`results.junit` is configured.

`--timing-json` includes sync phases and command phases. Failed runs add
`blockedStage` and `retryLikely` when Crabbox can classify the likely blocker;
the human run summary prints the same values as `blocked_stage` and
`retry_likely`. Commands can add user-defined phases by printing marker lines
to stdout or stderr:

```sh
echo CRABBOX_PHASE:install
pnpm install --frozen-lockfile
echo CRABBOX_PHASE:build
pnpm build
echo CRABBOX_PHASE:test
pnpm test
```

When local `CRABBOX_ENV_ALLOW` is set, `run` prints the variable names selected
for forwarding plus safe metadata such as whether secret-looking names are set
and their value length. Values are never printed. Delegated Testbox providers
print that this forwarding is unsupported and that secrets belong in the
provider workflow.

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
handoff file is uploaded as UTF-8 and imported with PowerShell UTF-8 decoding,
so non-ASCII token material and paths do not depend on the system code page.

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
PowerShell; use `--shell` for short snippets and `--script <file.ps1>` for
longer runs. Crabbox writes Windows scripts as UTF-8 with a byte-order mark when
the input has no BOM, which keeps Windows PowerShell 5.1 from mojibaking
non-ASCII script source.

Native Windows example:

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

For PR debugging that should not inherit local dependency churn, use a fresh
remote checkout:

```sh
crabbox run \
  --fresh-pr acme/app#123 \
  --script ./scripts/e2e-smoke.sh
```

Add `--apply-local-patch` only when the local diff should be applied on top of
the PR checkout. PR URLs must be on `github.com`; non-GitHub and GitHub
Enterprise PR URLs are rejected instead of being rewritten to a public clone.
Numeric shorthand uses the current repository's GitHub origin.

## Remote Debugging

Use SSH for live process and filesystem inspection:

```sh
bin/crabbox ssh --id blue-lobster
bin/crabbox inspect --id blue-lobster --json
```

Useful remote checks:

```sh
crabbox-ready
test -f /var/lib/crabbox/bootstrapped
df -h
free -h
ps aux --sort=-%cpu | head
```

If a lease was created with `--keep`, SSH remains available until `crabbox stop`, idle expiry, or the TTL cap removes it. For one-shot E2E debugging, add `--keep-on-failure`; Crabbox releases successful runs normally, but on failure it prints inspect, SSH, and stop commands for the exact failed lease and lets idle/TTL expiry clean it up later.

For a concise pre-command capability snapshot, add `--preflight`:

```sh
bin/crabbox run --id blue-lobster --preflight -- pnpm test:changed
```

The preflight prints a target-specific capability snapshot from the same
command workdir. It sources the Actions handoff env file when present, and
marks the workspace as raw or Actions-hydrated. Raw workspaces with Actions
hydration configured print the exact hydrate command suggestion and whether the
selected provider/target supports hydration.

Preflight is a probe layer, not an installer. Missing tools print
`tool=missing`; Crabbox does not run `apt install`, `corepack prepare`,
`bun install`, or any other setup. Install toolchains through Actions
hydration, a prebaked image, devcontainer/Nix/mise/asdf setup, or the uploaded
script/command itself.

The default built-in probes cover common toolchains: `git`, `tar`, `node`,
`npm`, `corepack`, `pnpm`, `yarn`, `bun`, `docker`, plus target-specific probes
such as `sudo`, `apt`, `bubblewrap`, `powershell`, `execution_policy`,
`longpaths`, `temp`, and `pwsh`. `uv` is available as an additional built-in.
Override the list per run:

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

## Actions Hydration

`crabbox actions hydrate` runs the configured workflow setup locally over SSH by default and waits for a ready marker. The marker path is the key local correlation point; `--github-runner` also reports the workflow run URL when Crabbox uses the GitHub runner fallback.

Use:

```sh
bin/crabbox actions hydrate --id blue-lobster
bin/crabbox inspect --id blue-lobster --json
```

The hydrated run writes non-secret handoff data for later `crabbox run --id blue-lobster` commands. Secrets and OIDC tokens remain workflow-step scoped unless the workflow intentionally writes its own short-lived handoff.

## Live Provider Debugging

For live provider or end-to-end test runs, prefer an Actions-hydrated lease
when tests need Node, pnpm, Docker services, repository secrets, or GitHub OIDC:

```sh
crabbox warmup --provider aws --class beast --keep
crabbox actions hydrate --id blue-lobster --workflow .github/workflows/hydrate.yml
mkdir -p .crabbox/logs
CRABBOX_ENV_ALLOW=OPENAI_API_KEY,OPENAI_BASE_URL \
  crabbox run --id blue-lobster \
  --preflight \
  --timing-json \
  --capture-stdout .crabbox/logs/live-provider.stdout.log \
  --capture-stderr .crabbox/logs/live-provider.stderr.log \
  --keep-on-failure \
  --shell 'echo CRABBOX_PHASE:install; pnpm install --frozen-lockfile; echo CRABBOX_PHASE:test; pnpm test:live'
```

For Blacksmith Testbox comparison runs, keep secrets in the Testbox workflow
environment. Crabbox will show that `CRABBOX_ENV_ALLOW` forwarding is
unsupported because Blacksmith owns command execution:

```sh
CRABBOX_ENV_ALLOW=OPENAI_API_KEY \
  crabbox run --provider blacksmith-testbox \
  --blacksmith-workflow .github/workflows/ci-check-testbox.yml \
  --blacksmith-job test \
  --preflight \
  -- pnpm test:live:providers
```

## Worker Logs

When the coordinator path fails before SSH, check Worker logs and Durable Object errors. The symptoms usually group into:

- auth failure;
- cost limit rejection;
- provider quota or capacity rejection;
- provider API failure;
- Durable Object alarm or state transition bug.

Keep the lease ID, owner, org, provider, class, and request time when comparing CLI output to Worker logs.

## Gaps

Current Crabbox observability is enough for maintainer operations, but not yet a full analytics product. Missing pieces:

- alerting on budget or failure-rate thresholds;
- dashboard UI.
