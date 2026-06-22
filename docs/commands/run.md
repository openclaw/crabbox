# run

`crabbox run` syncs the current dirty checkout to a box, runs a command there,
streams the output back, and exits with the remote command's exit code. It is
the core verb: lease (or reuse) a machine, ship your code, run something, get
the result.

```sh
crabbox run --id swift-crab -- pnpm test:changed
crabbox run --class beast -- pnpm check
crabbox run --provider aws --class beast --market on-demand -- pnpm check
crabbox run --provider azure --class beast -- pnpm check
crabbox run --provider azure --arch arm64 --class fast -- go test ./...
crabbox run --tailscale -- pnpm check
crabbox run --id swift-crab --network tailscale -- pnpm test
crabbox run --browser -- google-chrome --headless --version
crabbox run --desktop --browser --shell 'echo "$DISPLAY"; "$BROWSER" --version'
crabbox run --id swift-crab --shell 'pnpm install --frozen-lockfile && pnpm test'
crabbox run --id swift-crab --script ./scripts/live-smoke.sh
crabbox run --id swift-crab --full-resync -- pnpm check:changed
crabbox run --label "update flow smoke" -- pnpm test:changed
crabbox run --slug update-flow-smoke -- pnpm test:changed
crabbox run --pond alpha --slug web -- pnpm test:integration
crabbox run --fresh-pr example-org/my-app#123 --script ./scripts/e2e-smoke.sh
crabbox run --id cbx_abcdef123456 --junit junit.xml -- go test ./...
crabbox run --provider ssh --target macos --static-host mac-studio.local -- xcodebuild test
crabbox run --provider ssh --target windows --windows-mode normal --static-host win-dev.local -- dotnet test
crabbox run --profile live-qa --preset qa-live --scenario login-regression --emit-proof /tmp/proof.md --stop-after success
crabbox run --pool example/app/main/aws/linux/c6i.2xlarge -- pnpm test
```

The trailing command after `--` is sent to the box verbatim as argv. Use
`--shell` to run it through the remote shell instead, for multi-statement
snippets, pipes, or shell expansion.

## Leasing model

If `--id` is omitted, Crabbox creates a fresh, non-kept lease and releases it
when the command exits. With `--id` it reuses an existing lease; `--id` accepts
either the stable `cbx_...` ID or the active friendly slug (see
[identifiers](../features/identifiers.md)).

With `--pool <key>`, Crabbox borrows one hydrated broker ready-pool lease,
uses the pool-recorded SSH endpoint, runs the command, and returns the lease.
The default `--pool-return auto` returns successful runs to the pool and drains
failed runs so a bad machine is not reused. Use
`--pool-return ready|drain|release` to override that policy for one run. See
[Broker ready pools](../spec/broker.md).
Pooled runs reject `--full-resync`/`--fresh-sync`. With `--no-sync`, pooled
borrows require an exact commit match. Pooled runs also reject `--keep` and
`--keep-on-failure`; use `--pool-return ready|drain|release` for lifecycle.

On coordinator-backed one-shot runs, if SSH becomes unavailable after a
successful sync but before the command starts, Crabbox stops that stale lease,
creates one replacement lease, and retries sync once. It does not replace
explicit `--id`, kept, `--keep-on-failure`, `--no-sync`, `--sync-only`, or
custom-slug runs.

Crabbox records a local repo claim for each reused lease. If a lease is already
claimed by another repo, pass `--reclaim` to move the claim intentionally.

`--idle-timeout` controls inactivity expiry (default `30m`); `--ttl` is the
maximum wall-clock lifetime (default `90m`). Use `--stop-after
success|always|failure|never` to make lease cleanup explicit. Without it, a
newly acquired one-shot lease is released after the command and an existing
`--id` lease is left alone. The run details always print the exact `crabbox
stop ...` command. Use `--keep-on-failure` to keep a newly acquired lease alive
for debugging when the remote command exits non-zero; Crabbox then prints
inspect/SSH/stop commands for the exact failed box. Add `--lease-output <file>`
with `--keep` to write a small JSON lease handle for orchestrators.

## Delegated providers

Most providers connect over SSH and Crabbox owns sync and command transport.
Delegated providers (for example Blacksmith Testbox, Daytona, Islo, Azure
Dynamic Sessions, Cloudflare Dynamic Workers, E2B, Superserve, OpenSandbox, and
Vercel Sandbox) own command transport themselves: Crabbox sends either checkout
content or module source through the provider's APIs, runs through the provider,
and prints `sync=delegated` in the final timing summary where a sync phase exists. These
providers reject the SSH-run-only features `--capture-stdout`,
`--capture-stderr`, `--capture-on-fail`, `--script`, `--script-stdin`, and
`--fresh-pr` unless a delegated adapter advertises the matching capability.
Module-runtime delegated providers use `--script <file>` or `--script-stdin` as
source module input and reject trailing `-- <command>` argv because they do not
provide a Linux shell. Delegated artifact features such as `--artifact-glob`,
`--require-artifact`, and `--download` are accepted only by delegated adapters
that explicitly advertise the matching bounded artifact capability.
`--keep-on-failure` is supported for one-shot delegated runs. See the
per-provider docs under [providers](../features/providers.md) for how `--id`
resolves and any extra sync limitations.

Vercel Sandbox forwards non-auth command environment values through the SDK
bridge request body and strips Vercel provider auth variables from
`--allow-env` forwarding. Use Crabbox env forwarding for live secrets; raw
`sandbox --env key=value` places values on argv and is only suitable for manual
non-secret debugging.

`--azure-backend dynamic-sessions` keeps `--provider azure` as the family
selector while routing to the `azure-dynamic-sessions` delegated backend.

`--provider cloudflare-dynamic-workers` is a module-runtime provider. It accepts
Worker module source through `--script` or `--script-stdin`, supports cache and
egress controls through `--cloudflare-dynamic-workers-*` flags, and rejects
Linux shell semantics such as trailing command argv, SSH, sync-only, ports,
Actions hydration, browser, desktop, code-server, `--class`, and `--type`.

`--provider docker-sandbox --docker-sandbox-clone` has one provider-local
exception to the default one-shot cleanup rule: if Crabbox creates a fresh
clone-mode sandbox for the run and the command succeeds, Crabbox keeps that
sandbox even without `--keep`. This preserves unfetched in-sandbox commits.
Crabbox prints the exact `crabbox stop --provider docker-sandbox <slug>`
command for manual cleanup. Reused `--id` Docker Sandbox runs keep their
existing lifecycle behavior.

## Sync

Sync builds a file manifest with `git ls-files --cached --others
--exclude-standard`, then feeds that manifest to rsync over SSH. Tracked files
plus non-ignored untracked files transfer; `.git`, ignored build output,
dependency folders, `.crabboxignore` patterns, `sync.exclude` patterns, and
common caches stay out. Default excludes also cover common generated churn such
as `.ignored`, `.vite`, `playwright-report`, `test-results`, and local
`.crabbox` log/capture directories.

Before the first rsync into a Git checkout, Crabbox seeds the remote worktree
from your `origin` remote so the first sync is a dirty-tree overlay instead of a
full source upload. Crabbox also records a local/remote sync fingerprint and
skips rsync when the tracked commit, manifest, and dirty metadata have not
changed.

Use `--full-resync` (alias `--fresh-sync`) when a warm lease smells stale:
Crabbox deletes the remote workdir, skips the fingerprint fast path, reseeds Git
when possible, and uploads the checkout from scratch. Use `--checksum` for a
paranoid checksum scan instead of size/time comparison, and `--debug` to print
sync timing, progress, and itemized rsync output.

After sync, Crabbox runs a remote sanity check. If the remote checkout reports
at least 200 tracked deletions, the run fails before the command unless local
`CRABBOX_ALLOW_MASS_DELETIONS=1` is set.

Project-specific excludes live in `.crabboxignore` or `sync.exclude` in
`crabbox.yaml` / `.crabbox.yaml`. See [sync](../features/sync.md). Use
[`crabbox sync-plan`](sync-plan.md) to inspect the same manifest without leasing
a box.

### Large sync guardrails

Before rsync starts, Crabbox prints the candidate file count and byte estimate.
Large syncs warn or fail according to `sync.warnFiles`, `sync.warnBytes`,
`sync.failFiles`, and `sync.failBytes`; use `--force-sync-large` or
`sync.allowLarge: true` only when the size is intentional. Large-sync warnings
list the top source directories by file count plus a hint to update
`.crabboxignore` or `sync.exclude`. Quiet rsync runs print a heartbeat; after
several minutes without visible progress the heartbeat includes a concrete retry
hint, and `sync.timeout` kills stalled syncs.

### Sync alternatives

- `--no-sync` skips rsync entirely and `--sync-only` syncs and exits.
- `--fresh-pr <owner/repo#number|url|number>` skips local dirty sync and creates
  a fresh remote checkout of a GitHub PR. A bare `<number>` uses the current
  repository's GitHub origin. Only `github.com` PR URLs are accepted; other
  hosts are rejected. Add `--apply-local-patch` to apply the local `git diff
  --binary HEAD` on top of the PR checkout. `--fresh-pr` needs the SSH-run sync
  path; delegated providers reject it. Native Windows SSH targets are
  supported.

## Actions hydration

When the lease was hydrated by [`crabbox actions hydrate`](actions.md), `run`
reads the remote marker under `$HOME/.crabbox/actions`, syncs into the
workflow's `$GITHUB_WORKSPACE`, and sources the non-secret env file written by
the workflow. If no marker exists and `actions.workflow` is configured, `run`
performs local Actions hydration automatically after sync unless `--no-hydrate`
or `--no-sync` is set. This preserves the setup the workflow performed:
checkout path, installed dependencies, caches, runner temp/toolcache paths, and
any project-specific preparation. See
[Actions hydration](../features/actions-hydration.md).

If a JavaScript package-manager command (`pnpm`, `npm`, `node`, `corepack`)
runs on a raw SSH workspace before a hydration marker exists and no automatic
hydration is available, Crabbox probes the remote tool first and fails before
sync with guidance to hydrate, include runtime setup in the command, or choose a
provider/image with the JavaScript toolchain.

## Capabilities

`--browser` provisions or requires a known browser binary and injects
`CRABBOX_BROWSER=1`, `BROWSER`, and `CHROME_BIN` into the remote command. It
does not imply `--desktop`; use it alone for headless browser automation.
Browser login/profile state is not managed by Crabbox.

`--desktop` provisions or requires a visible desktop/VNC session and injects
`CRABBOX_DESKTOP=1`. Linux defaults to XFCE on `DISPLAY=:99`; leases created
with `--desktop-env wayland` expose `XDG_RUNTIME_DIR` and `WAYLAND_DISPLAY`
from `/var/lib/crabbox/desktop.env` instead. Use `--desktop --browser` for
headed browser automation in the VNC-visible session.

`--code` provisions or requires a Linux lease with code-server. Use [`crabbox
code --id <lease>`](code.md) to expose the editor through the authenticated
portal.

Reusing a lease requires matching capability labels. See
[capabilities](../features/capabilities.md).

## Network

`--tailscale` asks new managed Linux leases to join the configured tailnet.
`--network` selects how Crabbox resolves SSH for reused leases and for the final
connection after a new lease becomes ready: `auto` prefers Tailscale when
metadata exists and SSH is reachable, `tailscale` fails if the tailnet path is
not available, and `public` forces the provider host. See
[Tailscale](../features/tailscale.md).

## Targets

For `--provider ssh` with `--target macos` and `--target windows
--windows-mode wsl2`, sync uses the same POSIX rsync flow. Native Windows mode
(`--windows-mode normal`) uses PowerShell over OpenSSH and sends the manifest as
a tar archive into `static.workRoot`; cache purge and GitHub Actions runner
registration remain Linux-only.

On native Windows, plain argv is best for a single executable such as `dotnet
test`. Use `--shell` for multi-statement PowerShell snippets, env inspection, or
PowerShell expression syntax, and `--script <file.ps1>` for longer runs. Crabbox
writes uploaded Windows scripts as UTF-8 with a BOM when the input has none, so
Windows PowerShell 5.1 does not treat non-ASCII source as the system ANSI code
page.

## Scripts

Use `--script <file>` or `--script-stdin` for multi-line remote commands.
Crabbox uploads the script into `.crabbox/scripts/` under the remote workdir,
runs it as a file, and includes that script directory in failure bundles. A
shebang is honored on POSIX targets; scripts without one run through `bash`.
Native Windows targets run uploaded scripts through Windows PowerShell, and
`--script-stdin` is treated as a PowerShell script; a non-`.ps1` script path
gets a `.ps1` extension added before upload. Trailing arguments after `--` are
passed to the script. This is an SSH-run feature for OS-backed providers.
Delegated module-runtime providers that advertise `module-run` accept the same
script flags as source module input, but they reject trailing command argv and
`--shell`; they do not imply shell, SSH, rsync, or POSIX filesystem behavior.
Use `--script <file>` when the runtime needs a filename extension to identify
the module language. `--script-stdin` is JavaScript module source.

For Cloudflare Dynamic Workers, the script body must be Worker module source,
for example:

```js
export default { fetch() { return new Response("ok") } };
```

## Live secrets and env forwarding

Use `--env-from-profile <file>` with `--allow-env <name>` for live secrets.
Crabbox parses simple profile lines without executing the profile, forwards only
allowed names, and prints redacted presence/length metadata instead of values.
`--allow-env` and `--env-from-profile` are repeatable, and `--allow-env` also
accepts comma-separated names. Native Windows profile files are uploaded as
UTF-8 and imported with PowerShell UTF-8 decoding so non-ASCII values survive.
POSIX SSH targets also probe the uploaded profile from inside the remote workdir
and print redacted remote presence metadata before the command runs.

Add `--env-helper <name>` to persist a reusable helper at `.crabbox/env/<name>`
for that lease; the helper sources the matching profile and execs the command
you pass it. Persist helpers only on boxes you control, because the profile
stays on the remote workdir until you delete it or reset the lease. See
[env forwarding](../features/env-forwarding.md).

## Preflight

`--preflight` prints a target-specific capability snapshot after sync and before
the remote command. It is diagnostic only: Crabbox does not install tools,
change the machine, or fail just because a tool is missing. Install logic
belongs in Actions hydration, a prebaked image, a devcontainer, Nix/mise/asdf,
or the command/script you run.

By default it probes common language and infrastructure tools plus OS-specific
basics. Default generic probes are `git`, `tar`, `node`, `npm`, `corepack`,
`pnpm`, `yarn`, `bun`, and `docker`; `uv` is available as an additional built-in.
POSIX/Linux/WSL probes also include `sudo`, `apt`, and `bubblewrap`; native
Windows probes include `powershell`, `execution_policy`, `longpaths`, `temp`,
and `pwsh`.

Use `--preflight-tools` to replace the default tool list for one run:

```sh
crabbox run --preflight --preflight-tools node,bun,docker -- bun test
crabbox run --preflight --preflight-tools default,uv -- node --test
crabbox run --preflight --preflight-tools none -- ./smoke.sh
```

`default` expands to the built-ins; `none` keeps only the workspace summary.
Unknown tool names fail before leasing so typos do not hide missing
diagnostics. Unsupported OS-specific probes are skipped for the current target.

Configure the default per repo:

```yaml
run:
  preflightTools:
    - node
    - bun
    - docker
```

## Profiles, presets, and proof

Configured profiles can define reusable presets. `--preset <name>` expands the
profile command before execution, applies profile and preset environment
defaults, and prints the expanded command for auditability. `--scenario <value>`
sets the common `{{scenario}}` variable; use repeatable `--preset-var
name=value` for other placeholders. Profile doctor checks run before the remote
command when the selected profile enables them, so missing Node, pnpm, Docker,
Compose, or disk prerequisites fail before the expensive lane starts.

Use `--emit-proof <path>` to render a Markdown `## Real behavior proof` block
after a successful run, derived from run metadata, the expanded command,
selected live console output, collected artifact paths, and the
`--proof-template` or preset template. Keep proof templates in repo config so
parser-sensitive PR wording stays project-owned.

## Artifacts and downloads

Use repeatable `--artifact-glob <glob>` to collect matching remote files after a
successful SSH-backed run. Globs resolve relative to the remote workdir and are
stored locally under `.crabbox/runs/<run-or-lease>/` as a tarball. Profile and
preset `artifactGlobs` are collected the same way. Delegated providers accept
artifact globs only when their adapter advertises bounded run artifact
retrieval; otherwise they reject the flag. Native Windows and macOS targets
reject artifact globs; use Linux or Windows WSL2.

Use repeatable `--require-artifact <glob>` when a successful command must emit a
proof file, manifest, report, or other evidence artifact. Required artifact globs
are checked after the remote command exits 0 and before `--download` files are
written locally. They are also collected into the run artifact tarball. If any
required glob matches nothing, the run fails even though the command itself
succeeded. Matches must resolve to regular files, so dangling symlinks and
symlinks to directories do not satisfy the proof gate. The same SSH-run target
limits as `--artifact-glob` apply. Delegated providers that support bounded run
artifact retrieval enforce provider-owned file and byte limits before returning
local artifacts.

Use repeatable `--download remote=local` when the command writes proof files on
the box. Downloads run only after a successful remote command, paths resolve
relative to the remote workdir unless absolute, and Windows paths use `=`
instead of `:` so drive letters stay unambiguous. Crabbox rejects local output
path collisions between stdout capture, stderr capture, and downloads before
command execution. On Unix-like hosts, Crabbox-created download, capture, proof,
and failure-bundle files use owner-only permissions (`0600`), and newly created
output directories use `0700`.

See [artifacts](artifacts.md) for the richer collection and publishing workflow.

## Output capture

Use `--capture-stdout <path>` when stdout is binary or terminal-hostile. Crabbox
writes the remote stdout bytes directly to the local file, leaves stderr on the
terminal, and skips stdout run-log/event capture. `--capture-stderr <path>`
works the same way for stderr. Both are SSH-run-only; delegated providers reject
them.

When the remote command exits non-zero, Crabbox writes a local-only
`.crabbox/captures/*.tar.gz` failure bundle by default. SSH-backed bundles
include the uploaded script directory, redacted env/config summaries, timing
JSON, command stdout/stderr, common debug paths such as `test-results`,
`playwright-report`, `coverage`, JUnit XML files, nearby `*.log` files, and a
gateway log tail when a known gateway log path exists. Implicit stdout/stderr
entries are capped to keep bundles bounded; explicit `--capture-stdout` /
`--capture-stderr` files are included as caller-created local files.
Remote archive entries are confined to the bundle subtree; unsafe links and
special files are omitted.
`--capture-on-fail` remains accepted as a compatibility alias. Crabbox does not
redact captured files; the caller owns redaction before sharing them.

## Test results

Add `--junit <path>` (comma-separated) or configure `results.junit` to attach
JUnit XML summaries to the run record. Use `--results-auto` or `results.auto:
true` to scan common remote JUnit XML paths written by the command; auto
discovery skips dependency and Git directories and bounds remote file reads
before parsing. Malformed or over-limit reports produce named warnings while
valid reports remain attached. Add `--fail-on-test-failures` (or configure
`results.failOnFailures: true`) to exit 1 when a successful command writes
JUnit failures or errors. [`crabbox results <run-id>`](results.md) then prints failed
tests without reading the raw log. See [test results](../features/test-results.md).

## Output and observability

Before sync, `run` prints a compact context block with run ID, portal/log URLs,
lease ID, slug, provider, SSH target, remote workdir, and whether the workspace
is raw or Actions-hydrated.

At the end of every command, `run` prints a one-line timing summary (sync
duration, command duration, total duration, whether sync was skipped by
fingerprint, and the remote exit code), followed by run details with provider,
lease ID, slug, run ID, machine type, repo path, remote workdir, Actions URL
when present, stop command, and idle timeout. Add `--label <text>` to attach a
short label to the run details, timing JSON, and coordinator run record.

When a remote command exits non-zero, `run` prints a compact failure digest
after the timing summary: the failed phase when phase markers are known, a
likely area (provider auth, SSH/connectivity, sync, install/setup, user command,
or model/tool/provider limit), retryability when inferable, next commands
(`logs`, `events`, `doctor --from-run`, `ssh`, retrying with `--fresh-sync`, and
`stop`), and a short redacted stdout/stderr tail. It does not reconstruct secrets
or hidden local shell state.

Use `--timing-json` to emit a final JSON timing record with provider, lease ID,
slug, run ID, machine type, repo path, remote workdir, sync phases, command
phases, command duration, total duration, exit code, stop command, artifacts,
and Actions run URL when available. Failed runs also include `blockedStage` and
`retryLikely` when classifiable. Commands can emit phase markers on stdout or
stderr as `CRABBOX_PHASE:<name>`; Crabbox records those as `commandPhases`
without removing the marker line from output. In `blacksmith-testbox` mode, sync
is reported as delegated in the same schema.

Use `--timing-record=default` or `--timing-record <path>` to append the final
timing payload to a local benchmark JSONL store. This is opt-in; ordinary
`crabbox run` invocations do not persist timing rows. The persisted row wraps the
same `TimingReport` payload with local benchmark context such as command
fingerprint, repo fingerprint, provider family/kind, and cold/warm state when
known. See [`crabbox bench`](bench.md) for reporting and privacy guidance.

When a coordinator is configured, Crabbox records each remote command as a run
history item. [`crabbox history`](history.md) lists those records and [`crabbox
logs <run-id>`](logs.md) prints retained remote output (retention is bounded so
a noisy command cannot fill storage). See
[history and logs](../features/history-logs.md).

## Pond

Use `--pond <name>` to tag a new lease into a named pond. Pond is a reserved
provider label that groups peers, and [`crabbox list --pond <name>`](list.md)
selects them as a set. With `--tailscale` on a Tailscale-capable provider the
CLI also advertises a `tag:cbx-pond-<owner>-<name>` ACL tag, and cloud-init keeps
`/etc/hosts.cbx` plus a managed `/etc/hosts` block in sync every 30 seconds so
Tailscale peers in the same pond reach each other as `<slug>.cbx`. See
[pond](pond.md).

## Provider notes

For AWS one-shot leases, `--market` overrides `capacity.market` for this run.
Explicit `--type` keeps exact-type semantics; Crabbox reports why that type
failed rather than falling back to a different size.

XCP-ng one-shot leases use the SSH-run path on Linux only. The provider clones
the configured template with config-drive cloud-init, so `--script`,
`--env-from-profile`, `--capture-stdout`, `--download`, and the other SSH-run
features work after the VM is ready. Run `crabbox doctor --provider xcp-ng
--json` before live runs, and keep `CRABBOX_XCP_NG_PASSWORD` in env or private
config rather than argv.

Azure one-shot leases use managed `StandardSSD_LRS` OS disks by default so they
can become native checkpoint sources. Use `--azure-os-disk ephemeral` only for
stateless leases that do not need native Azure checkpoint/fork support;
`--azure-os-disk ephemeral-preview` opts into Azure's public-preview
full-caching ephemeral OS disk mode. `--azure-os-disk auto` is accepted for
compatibility and resolves to managed.

## Flags

Lease-create flags (shared with `warmup`, `actions hydrate`, and other
lease-acting commands):

```text
--provider <name>            See `crabbox providers` for the full list.
--profile <name>
--class <name>
--arch amd64|arm64           CPU architecture; arm64 supports Linux on AWS/Azure and native Windows on Azure.
--os <selector>              Portable Linux OS image, e.g. ubuntu:26.04
--type <provider-type>
--market spot|on-demand
--slug <slug>                Only when creating a fresh lease.
--pond <name>
--expose <port>              Repeatable; SSH-mesh-reachable TCP port.
--cache-volume [name=]key:path
                             Require a provider cache volume.
--ttl <duration>             Default 90m.
--idle-timeout <duration>    Default 30m.
--desktop
--desktop-env xfce|wayland|gnome
--browser
--code
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>         provider=ssh
--static-user <user>
--static-port <port>
--static-work-root <path>
--network auto|tailscale|public
--tailscale
--tailscale-tags <comma-separated tags>
--tailscale-hostname-template <template>
--tailscale-auth-key-env <env-var>
--tailscale-exit-node <name-or-100.x>
--tailscale-exit-node-allow-lan-access
```

Provider-specific flags are registered by each adapter and only apply to that
provider (for example `--azure-backend`, `--azure-os-disk`, the
`--blacksmith-*`, `--exe-dev-*`, `--namespace-*`, `--semaphore-*`,
`--sprites-*`, `--e2b-*`, and `--azure-dynamic-sessions-*` families). See the
per-provider docs under [providers](../features/providers.md).

Run-specific flags:

```text
--id <lease-id-or-slug>
--reclaim
--keep
--keep-on-failure
--stop-after success|always|failure|never
--lease-output <file>
--no-sync
--sync-only
--no-hydrate
--full-resync                Alias: --fresh-sync
--checksum
--force-sync-large
--debug
--shell
--script <file>
--script-stdin
--fresh-pr <owner/repo#number|url|number>
--apply-local-patch
--allow-env <name>           Repeatable or comma-separated.
--env-from-profile <file>    Repeatable.
--env-helper <name>
--preset <name>
--scenario <value>
--preset-var name=value      Repeatable or comma-separated.
--emit-proof <path>
--proof-template <name>
--preflight
--preflight-tools <comma-separated tool names>
--junit <comma-separated remote XML paths>
--results-auto
--fail-on-test-failures
--artifact-glob <glob>       Repeatable.
--require-artifact <glob>    Repeatable.
--download <remote=local>    Repeatable.
--capture-stdout <local path>
--capture-stderr <local path>
--capture-on-fail            Compatibility alias.
--label <text>
--timing-json
--timing-record default|off|path
```
