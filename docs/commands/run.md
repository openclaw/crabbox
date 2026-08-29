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
crabbox run --pool example/app/main/linux --pool-compatibility-key linux-16-vcpu -- pnpm test
crabbox run --pool example/app/main/linux --pool-identity-file pool-identity.json -- pnpm test
```

The trailing command after `--` is sent to the box verbatim as argv. Use
`--shell` to run it through the remote shell instead, for multi-statement
snippets, pipes, or shell expansion.

On POSIX and WSL2 SSH targets, private command staging does not change the
remote caller's umask for user work. Commands keep the target shell's creation
policy; Crabbox's staged scripts, input, and workspace-owner state remain private.

## Remote workspace root

Use `CRABBOX_WORK_ROOT` to change the portable base root for one run without
selecting a provider-specific flag:

```sh
CRABBOX_WORK_ROOT=/srv/crabbox \
  crabbox run --provider "$PROVIDER" -- pnpm test
```

This changes the base root, not the command's exact working directory. A normal
SSH-backed run derives `<root>/<lease>/<repository>`, syncs the checkout there,
and uses that derived repository workspace as the command PWD. There is no
provider-neutral exact-chdir flag; provider adapters and hydration own their
workspace semantics.

An explicitly configured provider-specific work root or workdir takes
precedence over the generic root. If the lease has a valid GitHub Actions
hydration marker, its canonical `WORKSPACE` takes precedence over both roots
for sync and command execution. See [Configuration](../features/configuration.md#work-roots)
for configuration precedence and [Sync](../features/sync.md#remote-workspace-path)
for path resolution.

## Leasing model

If `--id` is omitted, Crabbox creates a fresh, non-kept lease and releases it
when the command exits. With `--id` it reuses an existing lease; `--id` accepts
either the stable `cbx_...` ID or the active friendly slug (see
[identifiers](../features/identifiers.md)). When `--provider` is omitted, a
provider-bearing local claim selects the existing lease's provider before that
provider is configured. Exact lease IDs take precedence. Slug matches may span
multiple scopes of one canonical provider, which that provider resolves; claims
from different providers require a canonical ID or explicit provider. An
explicit `--provider` remains authoritative.

With `--pool <key>`, Crabbox borrows one hydrated broker ready-pool lease,
uses the pool-recorded SSH endpoint, keeps the borrow deadline alive while it
runs the command and return-time scrub, and then returns the lease.
Before a reusable return, Crabbox resets the checkout to the pool's recorded
branch, fetches its latest remote commit, removes run-local state, and verifies
the normal Git worktree is clean. The default `--pool-return auto` scrubs and
returns successful runs; command, lifecycle, or scrub failures drain the lease
so a bad or de-hydrated machine is not reused. Ignored task state is removed.
Successful runs may retain explicitly ignored dependency install trees.
Actions-hydrated leases drain if
their recorded hydration commit no longer matches the prepared branch commit.
Submodule worktrees drain instead of being reused. Reuse
requires a canonical HTTPS origin that succeeds
through a credential-free fetch preflight before the lease is borrowed. SSH,
local/file, query/fragment-bearing, and private credential-backed origins are rejected; use a forced
`drain` or `release` return policy for those repositories. Use
`--pool-return ready|drain|release` to override the
lifecycle policy for one run; `ready` still requires a successful scrub. See
[Broker ready pools](../spec/broker.md).
Pooled runs reject `--full-resync`/`--fresh-sync`. Reusable pooled runs require
a branch ref. With `--no-sync`, pooled borrows also require an exact commit
match. Use a forced `drain` or `release` policy for exact SHA or tag refs.
Reusable pooled runs also reject `--fresh-pr`; use a forced drain or release
for one-shot PR work.
Pooled runs also reject `--keep` and
`--keep-on-failure`; use `--pool-return ready|drain|release` for lifecycle.

Use `--pool-identity-file` to explicitly opt into a provider-scoped,
image-pinned typed pool. Create the file with `crabbox pool identity <key> --id
<lease-id> --cache-compatibility <value>`. The repository seed, immutable AWS
AMI and region, canonical architecture, and operator-declared cache value must
match exactly; unexpected identity or lease evidence drains the entry. Older
coordinators fail explicitly instead of borrowing from a legacy pool. Existing
`--pool` calls without this flag retain their provider-neutral legacy behavior.

On coordinator-backed one-shot runs, if SSH becomes unavailable after a
successful sync but before the command starts, Crabbox stops that stale lease,
creates one replacement lease, and retries sync once. It does not replace
explicit `--id`, kept, `--keep-on-failure`, `--no-sync`, `--sync-only`, or
custom-slug runs.

If the local SSH multiplexing socket is temporarily full before a command
starts, Crabbox retries that same session once, then disables multiplexing for
that invocation if the exact file-descriptor handoff failure recurs. The
existing lease and command are preserved; ordinary remote command failures do
not trigger this multiplexing recovery.

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
with `--keep` to write a small JSON lease handle for orchestrators on providers
that advertise `run-session`. Delegated providers return their own handle.
AWS and `local-container` opt into the same schema through the core SSH path: a
fresh run requires `--keep`, while a reused `--id` run requires the default or
`--stop-after never` policy so the lease cannot be released after the handle is
reported. Conflicting policies are rejected before acquisition. Core writes the
handle after recording the exact lease claim and before sync or command
execution, so later run failures leave the cleanup handle available. The handle
contains only the provider, exact lease ID, optional slug, reused/kept state,
optional run ID, and the exact `crabbox stop` cleanup command. On brokered AWS,
the run ID is the coordinator run that can later resolve to a signed terminal
receipt; direct AWS run IDs are local correlation identifiers only.

## Delegated providers

Most providers connect over SSH and Crabbox owns sync and command transport.
Delegated providers (for example Blacksmith Testbox, Blaxel, Daytona, Islo,
Azure Dynamic Sessions, Cloudflare Dynamic Workers, Cloudflare Sandbox, E2B,
Nomad, Superserve, OpenSandbox, and Vercel Sandbox) own command transport
themselves: Crabbox sends either checkout content or module source through the
provider's APIs, runs through the provider, and prints `sync=delegated` in the
final timing summary where a sync phase exists. These
providers reject the SSH-run-only features `--capture-stdout`,
`--capture-stderr`, `--capture-on-fail`, `--script`, `--script-stdin`, and
`--fresh-pr` unless a delegated adapter advertises the matching capability.
Module-runtime delegated providers use `--script <file>` or `--script-stdin` as
source module input and reject trailing `-- <command>` argv because they do not
provide a Linux shell. Delegated artifact features such as `--artifact-glob`,
`--require-artifact`, and `--download` are accepted only by delegated adapters
that explicitly advertise the matching bounded artifact capability.
`--keep-on-failure` is supported for one-shot delegated runs. See the
per-provider docs under [providers](../providers/README.md) for how `--id`
resolves and any extra sync limitations.

Vercel Sandbox forwards non-auth command environment values through the SDK
bridge request body and strips Vercel provider auth variables from
`--allow-env` forwarding. Use Crabbox env forwarding for live secrets; raw
`sandbox --env key=value` places values on argv and is only suitable for manual
non-secret debugging.

Cloudflare Sandbox forwards non-auth command environment values through the
bridge request body and strips Cloudflare provider auth variables from
`--allow-env` forwarding. Its client accepts either server-sent exec output or a
buffered JSON exec result, and archive sync is proofed through fake bridge tests
rather than live Cloudflare writes in the default test suite.

`--azure-backend dynamic-sessions` keeps `--provider azure` as the family
selector while routing to the `azure-dynamic-sessions` delegated backend.

`--provider blaxel` creates or reuses a Crabbox-claimed Blaxel Linux sandbox,
uploads the checkout as an archive through the Blaxel file API, executes the
command through the Blaxel process API, and mirrors the remote exit code.
`--sync-only`, `--no-sync`, `--force-sync-large`, `--keep`, and
`--keep-on-failure` follow the delegated-run contract; SSH-only run features are
rejected.

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

`--provider coder` remains on the normal SSH-run path. Crabbox asks the local
`coder` CLI to create or start a Linux workspace, syncs into
`coder.workRoot`, runs the command over `coder ssh --stdio`, and then stops the
workspace by default for one-shot cleanup. Set `--coder-delete-on-release` only
for disposable workspaces that should be deleted instead of stopped.

## Sync

Sync builds a file manifest with `git ls-files --cached --others
--exclude-standard`, then feeds that manifest to rsync over SSH. Tracked files
plus non-ignored untracked files transfer; `.git`, ignored build output,
dependency folders, `.crabboxignore` patterns, `sync.exclude` patterns, and
common caches stay out. Default excludes also cover common generated churn such
as `.ignored`, `.vite`, `playwright-report`, `test-results`, and local
`.crabbox` log/capture directories.

Jujutsu workspaces are supported for sync only when `.jj` is colocated with
same-root `.git` metadata. Native Jujutsu revision mapping is not supported yet;
`run` fails before lease acquisition or ready-pool borrowing instead of risking
sync of an outer Git checkout's revision. Use a colocated Git workspace or pass
`--no-sync` with a provider that supports it to run without transferring local
files. See
[sync](../features/sync.md#jujutsu-workspaces) for safe initialization guidance.

Before the first rsync into a Git checkout, Crabbox seeds the remote worktree
from your `origin` remote so the first sync is a dirty-tree overlay instead of a
full source upload. Crabbox also records a local/remote sync fingerprint and
skips rsync when the tracked commit, manifest, and dirty metadata have not
changed.

Existing SSH leases use one remote, lease-scoped workspace owner before Crabbox
reads hydration state, Git metadata, or the sync fingerprint. The owner remains
held through sync or fresh checkout, Actions hydration, the command, result and
artifact collection, failure capture, and ready-pool scrub/return. Separate
clients and `watch` iterations therefore cannot mutate or execute the same
reused workspace concurrently. A contending client waits for a bounded interval
and prints periodic progress. Newly acquired one-shot leases are already
exclusive and bypass this owner.

Ownership is fenced with a random token and renewed while the lifecycle is
active. If the local client disappears, Crabbox recovers an expired owner only
after verifying that its witnessed remote child is no longer alive. Ambiguous
renewal, release, token, or child state fails closed instead of risking a
concurrent checkout. POSIX, WSL2, and native Windows targets implement the same
protocol; the small sync-finalization lock remains nested inside it.

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

- `--no-sync` skips local file transfer and `--sync-only` syncs and exits on
  supported providers. Blacksmith Testbox rejects both: its native command owns
  sync even with `--id`, so reusing a Testbox does not provide a sync bypass.
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

Standalone `crabbox actions hydrate --id ...` acquires the same remote workspace
owner as ordinary reused runs, so hydration cannot overlap a sync, command,
collection, or cleanup from another client.

For an adopted Actions workspace, `--full-resync` invalidates the readiness
marker before resetting the remote tree. A command-bearing run continues only
when automatic local hydration can rebuild the canonical lease workspace; it
otherwise fails before reset. Omit `--no-hydrate` and use the canonical
workspace for the command path, or use `--sync-only` to reset and sync without
rehydration or a user command. The full invalidation, reset, sync, hydration,
and command sequence remains under the reusable workspace owner.

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

Use `--script <file>` or `--script-stdin` for multi-line remote commands. On
POSIX SSH leases, Crabbox uploads a standalone, content-hashed copy into
`.crabbox/scripts/` under the remote workdir and executes that copy with the
workdir as its process PWD. `$0` identifies the generated upload path, so
`dirname "$0"` resolves to `.crabbox/scripts/`, not the script's original local
directory. That directory component is not preserved in the uploaded copy and
cannot be recovered from `$0`. Standalone uploaded scripts should resolve
synced project assets from `$PWD`.

If a Git-managed script needs its synced repository path or adjacent assets,
invoke it as trailing argv so the project copy runs in place:

```sh
crabbox run -- ./scripts/check.sh
```

Crabbox includes the uploaded script directory in failure bundles. A shebang is
honored on POSIX targets; scripts without one run through `bash`. Native Windows
targets run uploaded scripts through Windows PowerShell, and
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
`pnpm`, `yarn`, `bun`, and `docker`. Additional opt-in built-ins are `go`,
`cargo`, `cmake`, `uv`, `python`, and `python3` on POSIX, WSL2, and native
Windows targets, plus `make` on POSIX and WSL2. Linux and WSL2 also support the
opt-in `raw_socket` capability probe. POSIX/Linux/WSL probes include `sudo`,
`apt`, and `bubblewrap`; native Windows probes include `powershell`,
`execution_policy`, `longpaths`, `temp`, and `pwsh`.

Use `--preflight-tools` to replace the default tool list for one run:

```sh
crabbox run --preflight --preflight-tools node,bun,docker -- bun test
crabbox run --preflight --preflight-tools default,uv -- node --test
crabbox run --preflight --preflight-tools default,cmake -- cmake --build build
crabbox run --preflight --preflight-tools python,python3 -- python3 -m pytest
crabbox run --preflight --preflight-tools raw_socket -- ./packet-tests
crabbox run --preflight --preflight-tools none -- ./smoke.sh
```

`default` expands to the default probe list; `none` keeps only the workspace
summary. Unknown tool names fail before leasing so typos do not hide missing
diagnostics. Unsupported OS-specific probes are skipped for the current target.
The CMake probe invokes the literal `cmake --version` command on POSIX, WSL2,
and native Windows targets. It prints only the first output line when CMake is
present or `cmake=missing` when it is unavailable; either result is diagnostic
only and does not block the workload. There is no `cmake3` alias. The Python
probes likewise invoke the literal requested command with `--version`, including
`python` and `python3` on native Windows; Crabbox does not map either name to
`py`. An unavailable literal command prints `<name>=missing` and the run
continues.

`raw_socket` uses `python3`, then `python`, to open and immediately close
`socket(AF_INET, SOCK_RAW, IPPROTO_RAW)` without binding, connecting, sending,
or receiving. Its stable result is `direct`, `sudo`, `unavailable`, or
`probe_missing`. `sudo` means only that the same bounded probe succeeded through
non-interactive `sudo -n`; Crabbox never elevates the workload, grants
capabilities, installs software, or changes an image or container. The probe is
separate from Python, Scapy, tcpdump, libpcap, and packet-capture availability.
Unsupported targets skip it through normal preflight target filtering.

Configure the default per repo:

```yaml
run:
  preflightTools:
    - raw_socket
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
parser-sensitive PR wording stays project-owned. Default headings are
context-neutral; put patch- or fix-specific claims in repository-owned template
fields only when the run actually proves them.

Use `--attest <path>` to write a signed receipt after a command completes,
including non-zero exits. SSH terminal receipts use schema v2 and bind the final
run outcome, raw command digest, timing, retained-log digest, and full observed
stream digest. Delegated providers retain schema v1 when they report a
definitive command exit. Check local receipts with [`crabbox verify`](verify.md).

Brokered runs submit a schema v2 terminal receipt with the finish request even
when `--attest` is omitted. The CLI verifies that the coordinator returns the
exact persisted receipt before treating the finish as recorded, so a
coordinator that predates receipt storage fails closed instead of silently
discarding the evidence. Deploy the coordinator before distributing a
receipt-bearing CLI. Retrieve and verify committed evidence later with
[`crabbox receipt <run-id>`](receipt.md). Signing uses a per-user Ed25519 key
minted on first use under the user config dir. Pass `--attest-key <path>` to
use an existing PKCS8 PEM Ed25519 key instead.

The coordinator creates the run record before remote command execution and the
CLI prints its ID. An orchestrator that must recover the ID after losing the
client output should also use `--lease-output <file>` on a supported retained
run. That file is written before SSH wait, sync, or command execution and
includes `runID`; its existing retention and stop-policy requirements still
apply.

## Artifacts and downloads

Use repeatable `--artifact-glob <glob>` to collect matching remote files after a
successful SSH-backed run. Globs resolve relative to the remote workdir and are
stored locally under `.crabbox/runs/<run-or-lease>/` as a tarball. Profile and
preset `artifactGlobs` are collected the same way. Delegated providers accept
artifact globs only when their adapter advertises bounded run artifact
retrieval; otherwise they reject the flag. Ordinary SSH-backed Linux, macOS,
and Windows WSL2 targets support artifact globs; native Windows still rejects
them. A macOS target needs stock `/bin/bash`, `find` with `-print0`, `tar`,
`base64`, and `/bin/rm`. Crabbox does not traverse directory symlinks while
matching. A matched leaf symlink is accepted only when it points to a regular
file.

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
them. When `--emit-proof` is also set, the proof includes the safely escaped
local capture path and final byte count for each redirected stream, but never
reads or embeds the captured bytes. Any other live console output remains in
the proof's redacted tail excerpt.

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

When the project capture destination is unwritable, automatic bundles fall
back to the Crabbox user state directory's `captures/` subdirectory. The
`failure-bundle local=...` line reports the actual path. Security-validation
and archive/read failures do not trigger fallback, and explicit stdout/stderr
capture paths never move. See [local capture storage](../observability.md#capturing-run-output-locally)
for the state path and retention details.

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

For newly created brokered leases, `run` also prints the exact selected image
ID/source and provider-side request, network-readiness, bootstrap, and total
startup timings when the provider reports them.

At the end of every command, `run` prints a one-line timing summary (lease,
bootstrap, sync, command, total, and end-to-end duration; whether sync was
skipped by fingerprint; and the remote exit code), followed by run details with
provider, lease ID, slug, run ID, machine type, repo path, remote workdir,
Actions URL when present, stop command, and idle timeout. Add `--label <text>`
to attach a short label to the run details, timing JSON, and coordinator run
record.

When a remote command exits non-zero, `run` prints a compact failure digest
after the timing summary: the failed phase when phase markers are known, a
likely area (provider auth, SSH/connectivity, sync, install/setup, user command,
model/tool/provider limit, or resource exhaustion), retryability when inferable, next commands
(`logs`, `events`, `doctor --from-run`, `ssh`, retrying with `--fresh-sync`, and
`stop`), and a short redacted stdout/stderr tail. It does not reconstruct secrets
or hidden local shell state. When an SSH backend supplies per-run memory
exhaustion evidence, the summary and digest use
`blocked_stage=resource_exhaustion resource_exhaustion=memory retry_likely=false`
and recommend increasing the memory limit or reducing workload concurrency.
Evidence read failures are warnings and do not replace the original command
failure.

Use `--timing-json` to emit a final JSON timing record with provider, lease ID,
slug, run ID, machine type, repo path, remote workdir, lease acquisition,
bootstrap, sync phases, command phases, command duration, command-path total,
end-to-end duration, exit code, normalized `runStatus`, optional `errorKind`,
stop command, artifacts, and Actions run URL when available. Failed runs also
include `blockedStage`, `resourceExhaustion`, and `retryLikely` when classifiable.
Commands can emit
phase markers on stdout or stderr as
`CRABBOX_PHASE:<name>`; Crabbox records those as `commandPhases` without removing
the marker line from output. In `blacksmith-testbox` mode, sync is reported as
delegated in the same schema.

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
--arch amd64|arm64           CPU architecture; cloud/Apple select capacity, while local-container asserts selected-daemon native architecture.
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
--image-min-os <version>
--image-sdk <name=version>     Repeatable.
--image-runtime <name=version> Repeatable.
--image-require-browser
--image-require-webview2
--image-require-desktop
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
per-provider docs under [providers](../providers/README.md).

Run-specific flags:

```text
--id <lease-id-or-slug>
--reclaim
--keep
--keep-on-failure
--stop-after success|always|failure|never
--lease-output <file>        Write a retained run-session handle when supported.
--no-sync                    Skip local file transfer; unsupported by Blacksmith Testbox.
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
--attest <path>
--attest-key <path>
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
