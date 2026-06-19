---
name: crabbox
description: "Crabbox remote validation and lease workflows."
---

# Crabbox

Use Crabbox when a project needs remote proof, larger cloud capacity, a fresh
PR checkout, a reusable warmed box, GitHub Actions-style setup, durable run
logs/results, UI proof artifacts, or sync from a dirty local checkout.

## Source Of Truth

- Run Crabbox from the repository root; sync mirrors the current checkout.
- Treat repo-local `crabbox.yaml` or `.crabbox.yaml` as executable project
  automation. Review it before remote runs, especially `provider`, `actions`,
  `jobs`, `profiles`, `env.allow`, artifacts, and cleanup policy.
- Verify the installed binary before relying on examples:
  `command -v crabbox && crabbox --version && crabbox --help | sed -n '1,120p'`.
- Use `crabbox providers` or `crabbox providers --json` for the current
  provider/capability matrix; provider docs can lag the compiled binary.
- Use `crabbox doctor` for live readiness checks and `crabbox config show` to
  inspect merged config without printing secrets.
- Prefer local targeted tests for tight edit loops. Move to Crabbox for broad
  suites, package-heavy checks, Docker/E2E/live-provider proof, cross-OS proof,
  UI proof, or commands that bog down the local machine.

## Auth And Config

Brokered operation needs a coordinator URL and token. First login usually needs
an explicit broker URL:

```sh
crabbox login --url <broker-url>
crabbox whoami
crabbox doctor
```

After `broker.url` is configured, `crabbox login` can reuse it. Trusted operator
automation can store a shared token without putting it on argv:

```sh
printf '%s' "$CRABBOX_COORDINATOR_TOKEN" |
  crabbox login --url <broker-url> --provider aws --token-stdin
```

Config precedence is `flags > env > repo config > user config > defaults`.
Default user config is `~/Library/Application Support/crabbox/config.yaml` on
macOS, `~/.config/crabbox/config.yaml` on Linux, or
`$XDG_CONFIG_HOME/crabbox/config.yaml` when set. `crabbox config path` prints
the active user config path.

Keep provider and broker tokens out of repo config and command arguments. Use
environment variables, a credential store, coordinator-managed secrets, or a
short-lived token command.

## Choose The Remote Surface

- `crabbox run -- <command>`: one command on a fresh or reused box.
- `crabbox warmup`: create a reusable lease and run commands later with `--id`.
- `crabbox prewarm`: warm a reusable lease and hydrate it from configured
  GitHub Actions.
- `crabbox job run <name>`: use a repo-local named flow that expands to
  warmup, optional hydration, run, and stop.
- `crabbox run --pool <key>`: borrow a hydrated broker ready-pool lease, run,
  then return/drain/release it according to `--pool-return`.
- `crabbox run --fresh-pr ...`: ignore local sync and check out a GitHub PR on
  the remote; add `--apply-local-patch` to test local uncommitted changes on
  top of that PR.
- `crabbox run --provider ssh`: use an existing macOS, Linux, or Windows host.
- `crabbox warmup --desktop --browser`: provision a visible desktop/browser for
  UI testing, WebVNC, screenshots, and artifacts.

If remote proof is blocked, name the missing capability precisely: auth,
coordinator, capacity, provider support, target OS, hydration, secret access,
artifact storage, desktop support, or a delegated-provider limitation.

## Common Remote Proof

One-shot command:

```sh
crabbox run --preflight --timing-json -- pnpm test
```

Warm and reuse a lease:

```sh
crabbox warmup --class beast --idle-timeout 90m
crabbox status --id <cbx_id-or-slug> --wait
crabbox run --id <cbx_id-or-slug> -- pnpm test:changed
crabbox run --id <cbx_id-or-slug> --full-resync -- pnpm test:changed
crabbox stop <cbx_id-or-slug>
```

Use a repo-local job when configured:

```sh
crabbox job list
crabbox job run --dry-run <job-name>
crabbox job run <job-name>
crabbox job run --id <cbx_id-or-slug> <job-name>
```

Use a ready-pool lease when the coordinator has hydrated pool capacity:

```sh
crabbox pool ready
crabbox run --pool <pool-key> -- pnpm test
crabbox run --pool <pool-key> --pool-return drain -- pnpm test:flaky
```

Use GitHub Actions hydration when the repository already owns setup in CI:

```sh
crabbox warmup --idle-timeout 90m
crabbox actions hydrate --id <cbx_id-or-slug>
crabbox run --id <cbx_id-or-slug> -- pnpm test
```

Use `--github-runner` only when the workflow needs full GitHub Actions
semantics such as repository secrets, OIDC, service containers, job containers,
or unsupported `uses:` steps:

```sh
crabbox actions hydrate --github-runner --id <cbx_id-or-slug>
```

## Sync And Fresh Checkouts

Normal sync transfers tracked files plus non-ignored untracked files, excludes
ignored dependency/build/cache output, honors `.crabboxignore` and
`sync.exclude`, seeds the remote checkout from `origin` when possible, and skips
rsync when the sync fingerprint matches.

Use `crabbox sync-plan` before large runs. Unexpected counts usually mean
local generated churn; update `.crabboxignore` or `sync.exclude` instead of
forcing huge uploads.

```sh
crabbox sync-plan
crabbox run --debug --timing-json -- pnpm test
crabbox run --full-resync -- pnpm test
```

Use fresh PR checkout when local dependency churn or dirty sync would confuse
the result:

```sh
crabbox run --fresh-pr example-org/my-app#123 --script ./scripts/e2e-smoke.sh
crabbox run --fresh-pr 123 --apply-local-patch -- pnpm test
```

`--fresh-pr` accepts `owner/repo#number`, GitHub PR URLs, or a numeric PR from
the current GitHub origin. Non-GitHub hosts are rejected. Fresh PR checkout is
an SSH-run sync feature; delegated providers reject it. Native Windows SSH
targets are supported.

When a warm lease smells stale, prefer `--full-resync` (alias `--fresh-sync`) to
reset the remote workdir, skip the sync fingerprint fast path, reseed Git when
possible, and upload the checkout from scratch.

## Scripts, Shells, And Windows Targets

Use plain argv after `--` for one executable. Use `--shell` for multi-statement
shell snippets, pipes, or shell expansion:

```sh
crabbox run --id <lease> -- go test ./...
crabbox run --id <lease> --shell 'corepack enable && pnpm install --frozen-lockfile && pnpm test'
```

Prefer uploaded scripts for multi-line commands. Scripts are included in failure
bundles and avoid brittle quoted shell strings:

```sh
crabbox run --script ./scripts/e2e-smoke.sh --timing-json
printf '%s\n' 'echo CRABBOX_PHASE:test' 'pnpm test' | crabbox run --script-stdin
```

Native Windows targets use PowerShell and tar-based manifest sync. Prefer plain
argv for one executable such as `dotnet test`; use `--shell` for multi-statement
PowerShell and `--script <file.ps1>` for longer scripts.

## Secrets And Environment Forwarding

Crabbox does not forward the whole local environment. Forwarding is name-based:
only allowlisted names that are actually set locally or in an allowed profile
cross the boundary. Avoid allowlisting secret-shaped names unless the run is an
explicit live-secret smoke.

```sh
crabbox run --allow-env CI,NODE_OPTIONS -- pnpm test
crabbox run \
  --env-from-profile ~/.project-live.profile \
  --allow-env API_TOKEN \
  --preflight \
  --script ./scripts/live-smoke.sh
```

`--env-from-profile` parses simple `export NAME=value` and `NAME=value` lines
without executing the profile. Crabbox prints redacted presence/length metadata,
not values. POSIX SSH leases can persist a helper for later commands on a lease
you control:

```sh
crabbox run \
  --id <lease> \
  --env-from-profile ~/.project-live.profile \
  --allow-env API_TOKEN \
  --env-helper live \
  -- true
crabbox run --id <lease> -- ./.crabbox/env/live ./scripts/live-smoke.sh
```

The generated helper and matching secret profile remain in the remote workdir
until cleanup, lease reset, or `--full-resync`; do not persist helpers on shared
or untrusted leases.

## Profiles, Presets, Proof, And Results

Repo config can define profiles, presets, doctor requirements, artifact globs,
required artifacts, and proof templates. Use them for stable validation lanes
instead of encoding project knowledge in agent prompts.

```sh
crabbox run \
  --profile live-qa \
  --preset qa-live \
  --scenario login-regression \
  --emit-proof /tmp/proof.md \
  --stop-after success
```

Use `--preflight` for a target capability snapshot before the command, not as
an installer. Use `--preflight-tools` to tune probes:

```sh
crabbox run --preflight --preflight-tools node,bun,docker -- bun test
crabbox run --preflight --preflight-tools default,uv -- node --test
```

Attach structured results and proof artifacts when the command emits them:

```sh
crabbox run --junit reports/junit.xml -- ./scripts/test-with-junit.sh
crabbox run --artifact-glob 'reports/**' --require-artifact reports/summary.json -- pnpm test:e2e
crabbox run --download reports/summary.json=.crabbox/logs/summary.json -- pnpm test:e2e
```

`--require-artifact` fails the run if the remote command exits 0 but the proof
file is missing. Keep required artifacts bounded and scrubbed; do not collect
raw datasets, secrets, credentials, signed URLs, or unredacted customer rows.

## Run Handles And Observability

Coordinator-backed runs print a durable `run_...` handle before leasing starts.
Keep that run ID in status updates and PR notes.

```sh
crabbox history --limit 20
crabbox history --lease <cbx_id-or-slug> --limit 20
crabbox attach <run_id>
crabbox attach <run_id> --after <seq>
crabbox events <run_id> --after <seq> --limit 100
crabbox events <run_id> --json
crabbox logs <run_id>
crabbox results <run_id>
```

Use `--timing-json` on `run`, `warmup`, and `actions hydrate` when a stable
machine-readable timing record is needed. Commands can mark subphases by
printing markers on stdout or stderr:

```sh
echo CRABBOX_PHASE:install
pnpm install --frozen-lockfile
echo CRABBOX_PHASE:test
pnpm test
```

Output events are capped previews. Use `logs` for retained output tails and
`results` for parsed test summaries.

## Desktop, WebVNC, And UI Proof

Create desktop/browser leases for visual QA, headed browser automation, or UI
proof:

```sh
crabbox warmup --desktop --browser
crabbox warmup --provider aws --os ubuntu:26.04 --desktop --browser --desktop-env wayland
crabbox warmup --provider aws --os ubuntu:26.04 --desktop --browser --desktop-env gnome
```

`ubuntu:26.04` is the default portable Linux OS selector where the provider
catalog supports it. Use `--os ubuntu:24.04` only when a test must stay on the
previous LTS. Explicit provider image flags still win over `--os`.

For human demos, prefer WebVNC over native VNC because `crabbox webvnc --open`
preloads the lease password in the browser fragment:

```sh
crabbox webvnc --id <lease> --open --take-control
crabbox webvnc status --id <lease>
crabbox webvnc reset --id <lease> --open --take-control
crabbox vnc --id <lease> --open
```

For input automation, use first-class helpers instead of hand-written
`xdotool`:

```sh
crabbox desktop doctor --id <lease>
crabbox desktop launch --id <lease> --browser --url https://example.com --webvnc --open --take-control
crabbox desktop click --id <lease> --x 640 --y 420
crabbox desktop paste --id <lease> --text "user@example.com"
printf 'user@example.com' | crabbox desktop paste --id <lease>
crabbox desktop type --id <lease> --text "user+qa@example.com"
crabbox desktop key --id <lease> ctrl+l
crabbox screenshot --id <lease> --output desktop.png
```

When desktop/WebVNC hangs, trust the inline rescue output first: `problem:` and
`rescue:` lines usually name exact next commands such as `webvnc status/reset`,
`desktop doctor`, or native `vnc --open`.

Use artifacts for UI QA proof instead of committing screenshots or videos to a
product repo branch:

```sh
crabbox artifacts collect --id <lease> --all --output artifacts/<slug>
crabbox artifacts publish --dir artifacts/<slug> --pr <number>
crabbox artifacts list <artifact-manifest-url-or-dir>
crabbox artifacts pull <artifact-manifest-url-or-dir> --output /tmp/<slug>-proof
```

`artifacts publish` uses brokered storage when configured, or explicit S3/R2 /
Cloudflare/local hosting flags. Use `--dry-run` before public PR comments when
reviewing generated Markdown or storage commands.

## Provider Boundaries

SSH-lease providers support the full sync/run surface when the target supports
it: scripts, fresh PR checkouts, captures, downloads, Actions hydration, SSH,
ports, WebVNC, code-server, desktop, browser, cache volumes, and cleanup.

Delegated-run providers own command transport. Expect them to reject SSH-run
features such as `--capture-stdout`, `--capture-stderr`, `--capture-on-fail`,
`--script`, `--script-stdin`, `--fresh-pr`, local captures, `--download`,
`--full-resync`, and `--env-helper` unless `crabbox providers --json` and the
provider docs advertise the matching capability. `--keep-on-failure` is still
useful for one-shot delegated providers that Crabbox would otherwise stop after
a failed command.

Module-runtime delegated providers, such as Cloudflare Dynamic Workers, run
source modules rather than Linux shell commands. Use `--script <file>` or
`--script-stdin` for module source; trailing `-- <command>`, SSH, rsync, ports,
Actions hydration, desktop, browser, and code-server do not apply unless the
provider explicitly documents them.

Use `--market spot|on-demand` on AWS `warmup` or one-shot `run` when account
quota or capacity testing needs a temporary market override. An explicit
`--type` means exact type; Crabbox reports quota/capacity/policy failures
instead of silently falling back.

## Local And Static Targets

Use `local-container` for fast local proof when the host has Docker or Podman.
`warmup` creates a container but does not sync; for an interactive synced
container, use `run --keep --sync-only`:

```sh
crabbox run --provider local-container --keep --slug local-smoke --sync-only
eval "$(crabbox ssh --provider local-container --id local-smoke)"
```

Pass `--local-container-runtime docker` or `--local-container-runtime podman`
when the engine matters, and keep that flag on reused lease commands such as
`run --id`, `ssh`, `status`, and `stop`. `crabbox ssh` prints an SSH command;
use `eval "$(crabbox ssh ...)"` to connect. After login, `cd` into the workdir
printed by `run --sync-only`.

Use static SSH for existing machines:

```sh
crabbox run --provider ssh --target macos --static-host mac.example.com -- xcodebuild test
crabbox run --provider ssh --target windows --windows-mode normal --static-host win.example.com -- dotnet test
```

Static hosts are host-managed: Crabbox does not provision or delete them.

## Useful Commands

```sh
crabbox providers
crabbox providers --json
crabbox doctor
crabbox config path
crabbox config show
crabbox whoami
crabbox sync-plan
crabbox warmup --class beast
crabbox prewarm
crabbox status --id <lease> --wait
crabbox inspect --id <lease> --json
crabbox run --id <lease> --preflight --timing-json -- pnpm test
crabbox job list
crabbox job run --dry-run <job-name>
crabbox pool ready
crabbox history --lease <lease>
crabbox events <run_id> --json
crabbox attach <run_id>
crabbox logs <run_id>
crabbox results <run_id>
crabbox cache stats --id <lease>
crabbox cache volumes
crabbox ssh --id <lease>
crabbox connect <lease>
crabbox ports --id <lease> --publish 8080
crabbox cp --id <lease> ./coverage.xml SANDBOX:/tmp/coverage.xml
crabbox webvnc --id <lease> --open
crabbox code --id <lease> --open
crabbox egress start --id <lease> --profile discord --daemon
crabbox desktop doctor --id <lease>
crabbox desktop proof --id <lease> --output artifacts/<slug>-proof -- ./scripts/visual-smoke.sh
crabbox artifacts collect --id <lease> --all --output artifacts/<slug>
crabbox artifacts publish --dir artifacts/<slug> --pr <number> --dry-run
crabbox usage --scope org
crabbox pause <lease>
crabbox resume <lease>
crabbox stop <lease>
```

## Failure Triage

- Provider missing or old CLI: verify `crabbox --help` and `crabbox providers`
  list the provider, then rebuild or install a current binary.
- Bad local config: compare `crabbox config show`, pass `--provider ...`
  explicitly, and run `crabbox doctor`.
- Sync surprise: run `crabbox sync-plan`, add excludes, then retry with
  `--debug --timing-json` or `--full-resync`.
- Raw box missing Node/pnpm/Docker: use `--preflight`; hydrate first if the
  repo has an Actions workflow, or include setup in the command/script.
- Command failed: keep the `run_...` handle, inspect `results`, then rerun the
  focused failing shard/file before a full suite.
- Desktop unhealthy: run `desktop doctor`, then follow `problem:` / `rescue:`
  output from `webvnc status` or `webvnc reset`.
- Cleanup uncertain: use `crabbox list`, `crabbox inspect --json`, and only
  stop leases or provider resources you created.
- Broker/auth confusion: use `crabbox doctor`, `crabbox whoami`, and
  `crabbox config show` before asking for cloud credentials.

## Cleanup

Brokered leases have coordinator-owned idle expiry and local lease claims.
Default idle timeout is 30 minutes unless config or flags set a different
value. Still stop boxes you created when done:

```sh
crabbox stop <cbx_id-or-slug>
```

When `crabbox list` prints `orphan=no-active-lease`, treat it as an operator
review hint: verify the provider machine is not referenced by an active
coordinator lease before deleting anything, especially if `keep=true` is set.
