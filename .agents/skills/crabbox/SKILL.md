---
name: crabbox
description: "Use Crabbox for remote validation, warmed reusable boxes, Actions hydration, fresh PR checkouts, secret-safe env forwarding, scripts, timing, logs, results, captures, caches, and lease cleanup."
---

# Crabbox

Use Crabbox when a project needs remote proof, larger cloud capacity, warm
reusable runner state, Actions hydration, fresh PR checkouts, live-secret
smoke tests, durable logs/results, or fast sync from a dirty local checkout.

## Before Running

- Run from the repository root. Crabbox sync mirrors the current checkout.
- Prefer local targeted tests for tight edit loops.
- Check repo-local `crabbox.yaml` or `.crabbox.yaml` before adding flags.
- Sanity-check the selected binary before remote work:
  `command -v crabbox && crabbox --version && crabbox --help | sed -n '1,80p'`.
- Install from the repository release docs or use `bin/crabbox` after a local
  build.
- Auth is required for brokered operation. Normal users run `crabbox login`.
- Trusted operator automation can store the shared token with:
  `printf '%s' "$CRABBOX_COORDINATOR_TOKEN" | crabbox login --url <broker-url> --provider aws --token-stdin`.
- User config lives at `~/Library/Application Support/crabbox/config.yaml` on
  macOS or the platform user config dir elsewhere. It should contain:

```yaml
broker:
  url: <broker-url>
  token: <token>
provider: aws
```

## Pick The Proof

- Small/narrow tests, docs link checks, formatting, and focused unit tests:
  run locally first.
- Broad suites, package-heavy checks, Docker/E2E/live-provider proof, or
  cross-OS behavior: run on Crabbox.
- If a local command starts fanning out or bogs down the Mac, stop it and move
  the proof remote.
- Before shipping a user-visible behavior fix, prefer one remote command that
  starts from the user-facing entrypoint.
- If remote proof is blocked, say exactly which capability is missing:
  auth, capacity, provider support, target OS, hydration, or secret access.

## OpenClaw Provider Choice

For OpenClaw/OpenClawd validation, prefer Blacksmith Testbox first when the
workflow already gives the needed setup and the delegated command path is enough:

```sh
crabbox run --provider blacksmith-testbox --timing-json -- pnpm test
```

If Blacksmith is queued, unavailable, unauthenticated, or lacks the SSH-backed
feature needed for the proof, rerun the same command shape on AWS:

```sh
crabbox run --provider aws --timing-json -- pnpm test
```

Actions setup remains repo-owned in both paths. Blacksmith owns Testbox workflow
hydration and command transport. AWS uses Crabbox SSH sync/run and Actions
hydration for the persistent workspace. This is operator choice, not automatic
CLI fallback.

## Common Flow

One-shot command:

```sh
crabbox run --timing-json --preflight -- pnpm test
```

Warm a reusable box:

```sh
crabbox warmup --idle-timeout 90m
crabbox warmup --provider aws --class beast --market on-demand --idle-timeout 90m
```

Repos with `actions.workflow` hydrate automatically during `crabbox run`. Use
manual hydration when you want to prepare a reused lease before running commands:

```sh
crabbox actions hydrate --id <cbx_id-or-slug>
```

Use `crabbox actions hydrate --github-runner --id <cbx_id-or-slug>` when the
workflow needs full GitHub Actions semantics, repository secrets, OIDC, service
containers, or unsupported `uses:` steps.

Run commands:

```sh
crabbox run --id <cbx_id-or-slug> -- pnpm test:changed
crabbox run --id <cbx_id-or-slug> --full-resync -- pnpm test:changed
crabbox run --id <cbx_id-or-slug> --shell "corepack enable && pnpm install --frozen-lockfile && pnpm test"
```

For package-manager commands on raw AWS/Hetzner boxes, rely on configured
Actions hydration or include setup in the command; bootstrap only installs
Crabbox plumbing, not project runtimes. Add `--timing-json` when comparing
providers or sync phases.

Stop boxes you created before handoff:

```sh
crabbox stop <cbx_id-or-slug>
```

## Scripts, Secrets, And Fresh PRs

Prefer uploaded scripts for multi-line commands. This avoids giant quoted shell
strings and preserves the script in failure bundles:

```sh
crabbox run --script ./scripts/e2e-smoke.sh --timing-json
printf '%s\n' 'echo CRABBOX_PHASE:test' 'pnpm test' | crabbox run --script-stdin
```

For live-secret smoke tests, use explicit allowlists. Never print values:

```sh
crabbox run \
  --env-from-profile ~/.project-live.profile \
  --allow-env API_TOKEN \
  --preflight \
  --script ./scripts/live-smoke.sh
```

Crabbox parses simple `export NAME=value` and `NAME=value` profile lines
without executing the profile. It probes the uploaded profile remotely and
prints names plus redacted presence/length metadata only. On POSIX SSH leases,
add `--env-helper <name>` when follow-up commands should reuse a remote
`.crabbox/env/<name>` wrapper:

```sh
crabbox run \
  --env-from-profile ~/.project-live.profile \
  --allow-env API_TOKEN \
  --env-helper live \
  -- ./.crabbox/env/live ./scripts/live-smoke.sh
```

Persist helpers only on leases you control; the profile remains on the remote
workdir until cleanup, lease reset, or `--full-resync`.

Use fresh PR checkout when local dependency churn or dirty sync would confuse
the result:

```sh
crabbox run --fresh-pr owner/repo#123 --script ./scripts/e2e-smoke.sh
crabbox run --fresh-pr 123 --apply-local-patch -- pnpm test
```

`--fresh-pr` accepts `owner/repo#number`, `github.com` PR URLs, or a numeric PR
from the current GitHub origin. Non-GitHub hosts are rejected.

When sync guardrails look high because the checkout is noisy, prefer
`--fresh-pr ... --apply-local-patch`. Normal sync output prints both the full
candidate and the dirty delta; guardrails use the dirty delta when present.
When a warm lease smells stale, use `--full-resync` (alias `--fresh-sync`) to
reset the remote workdir, skip the sync fingerprint fast path, reseed Git when
possible, and upload the checkout from scratch.

## Observability And Captures

Add `--preflight` for remote user/cwd/runtime capability checks before the
command. Add `--timing-json` when comparing providers, sync behavior, or flaky
latency.

Commands can create subphases by printing markers:

```sh
echo CRABBOX_PHASE:install
pnpm install --frozen-lockfile
echo CRABBOX_PHASE:test
pnpm test
```

For terminal-hostile or large output:

```sh
mkdir -p .crabbox/logs
crabbox run \
  --capture-stdout .crabbox/logs/run.stdout.log \
  --capture-stderr .crabbox/logs/run.stderr.log \
  --keep-on-failure \
  --download test-results/report.json=.crabbox/logs/report.json \
  -- pnpm test:e2e
```

Failed SSH runs save `.crabbox/captures/*.tar.gz` automatically. Add
`--keep-on-failure` for live debugging when you want the exact failed lease left
alive for SSH inspection until idle/TTL expiry. Captured files and failure
bundles are local-only and not redacted by Crabbox; review before sharing.

Use `crabbox sync-plan` before large runs. Unexpected counts usually mean
generated churn; add project-specific excludes to `.crabboxignore` or
`sync.exclude`. If quiet rsync watchdogs or SSH timeouts print `next_action=`,
follow that hint: usually retry with `--full-resync`, then replace the lease if
the problem persists.

## Useful Commands

```sh
crabbox status --id <id-or-slug> --wait
crabbox inspect --id <id-or-slug> --json
crabbox webvnc --id <id-or-slug> --open
crabbox webvnc daemon start --id <id-or-slug> --open
crabbox webvnc daemon status --id <id-or-slug>
crabbox webvnc daemon stop --id <id-or-slug>
crabbox webvnc status --id <id-or-slug>
crabbox webvnc reset --id <id-or-slug> --open
crabbox desktop doctor --id <id-or-slug>
crabbox desktop click --id <id-or-slug> --x 640 --y 420
crabbox desktop paste --id <id-or-slug> --text "user@example.com"
crabbox desktop type --id <id-or-slug> --text "user+qa@example.com"
crabbox desktop key --id <id-or-slug> ctrl+l
crabbox artifacts collect --id <id-or-slug> --all --output artifacts/<slug>
crabbox artifacts publish --dir artifacts/<slug> --pr <number>
crabbox sync-plan
crabbox history --lease <id-or-slug>
crabbox events <run_id> --json
crabbox attach <run_id>
crabbox logs <run_id>
crabbox results <run_id>
crabbox cache stats --id <id-or-slug>
crabbox ssh --id <id-or-slug>
crabbox usage --scope org
```

For human desktop demos, prefer WebVNC over native VNC because
`crabbox webvnc --open` preloads the lease password in the browser fragment.
Use native `crabbox vnc --id <id-or-slug> --open` as the fallback printed by
`crabbox webvnc status` or `crabbox webvnc reset`. For input automation, use
`crabbox desktop click/paste/type/key` instead of hand-written `xdotool`;
`desktop type` switches to clipboard paste for symbol-heavy text such as emails
and passwords. `desktop key` accepts both `--id <lease> <keys>` and positional
`<lease> <keys>` forms for shortcuts.

When desktop/WebVNC hangs, trust the inline rescue output first: `problem: VNC
bridge disconnected`, `problem: browser not launched`, `problem: input stack
dead`, or similar will be followed by exact `rescue:` commands such as
`crabbox webvnc status/reset` or `crabbox desktop doctor`.

For UI QA proof, use `crabbox artifacts collect` instead of ad hoc screenshots
and shell recordings. It can bundle screenshots, MP4 recordings, trimmed GIFs,
desktop doctor output, WebVNC status, run logs, and metadata, then
`crabbox artifacts publish --pr <n>` can publish inline-ready Markdown through
the configured coordinator artifact backend. Use explicit `--storage s3`,
`--storage r2`, or `--storage local` only as a local fallback.

## Run Inspection Workflow

Use the CLI for durable run inspection.

Find recent runs:

```sh
crabbox history --limit 20
crabbox history --lease <id-or-slug> --limit 20
```

Follow an active run:

```sh
crabbox attach <run_id>
crabbox attach <run_id> --after <seq>
```

Page through recorded events:

```sh
crabbox events <run_id> --after <seq> --limit 100
crabbox events <run_id> --json
```

Inspect completed output and structured test summaries:

```sh
crabbox logs <run_id>
crabbox results <run_id>
```

Use `--debug` on `run` when measuring sync timing.
Use `--timing-json` on `warmup`, `actions hydrate`, and `run` when a stable
machine-readable timing record is needed.
Use `--market spot|on-demand` on AWS `warmup` or one-shot `run` when account
quota or capacity testing needs a temporary market override.

## Provider Boundaries

SSH-backed providers support core sync/run features such as scripts, fresh PR
checkouts, captures, downloads, Actions hydration, SSH, and WebVNC when the
target supports them.

Delegated run providers own command transport. Expect them to reject SSH-run
features such as `--script`, `--script-stdin`, `--fresh-pr`, local stdout/stderr
captures, `--capture-on-fail`, `--download`, `--full-resync`, and
`--env-helper`, unless that provider doc says otherwise. `--keep-on-failure` is
still useful for one-shot delegated providers that Crabbox would otherwise stop
after a failed command.

Native Windows targets use PowerShell and tar-based manifest sync. Prefer
plain argv for one executable and `--shell` for multi-statement PowerShell.
Scripts and fresh PR checkout are POSIX SSH-run features, not native Windows
features.

## Run Handles

Coordinator-backed `crabbox run` prints `recording run run_...` before leasing
starts. Keep that run ID in status updates. Use `crabbox events run_...` for
ordered lifecycle/output events, `crabbox attach run_...` to follow an active
run, and `crabbox logs run_...` or `crabbox results run_...` after completion.

Output events are a capped preview, not unlimited logs. Use `logs` for the
retained command output tail when debugging noisy runs.

## Hydration Boundary

Repository setup belongs in the repository hydration workflow. That workflow
owns checkout, runtime setup, dependencies, services, secret-backed preparation,
the ready marker, and keepalive.

Crabbox owns runner registration, workflow dispatch, SSH sync, command
execution, logs/results, local lease claims, and idle cleanup. Do not add
project-specific setup to the Crabbox binary.

## Failure Triage

- Provider missing or old CLI: verify `crabbox --help` lists the provider and
  rebuild or install a current binary before falling back.
- Bad local config: pass `--provider ...` explicitly and compare
  `crabbox config show`.
- Sync surprise: run `crabbox sync-plan`, then `crabbox run --debug
  --timing-json`.
- Raw box missing Node/pnpm/Docker: use `--preflight`; hydrate first if the repo
  has an Actions workflow.
- Command failed: rerun the focused failing shard/file before a full suite.
- Cleanup uncertain: `crabbox list`, `crabbox inspect --json`, then stop only
  leases or provider resources you created.
- Broker/auth confusion: use `crabbox doctor`, `crabbox whoami`, and
  `crabbox config show` before asking for cloud credentials.

## Cleanup

Brokered leases have coordinator-owned idle expiry and local lease claims, so
projects should not maintain their own lease ledger. Default idle timeout is 30
minutes unless config or flags set a different value. Still stop boxes you
created when done.

When `crabbox list` prints `orphan=no-active-lease`, treat it as an operator
review hint: verify the provider machine is not referenced by an active
coordinator lease before deleting anything, especially if `keep=true` is set.
