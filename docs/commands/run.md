# run

`crabbox run` syncs the current dirty checkout to a box, runs a command, streams output, and returns the remote exit code.

```sh
crabbox run --id blue-lobster -- pnpm test:changed:max
crabbox run --class beast -- pnpm check
crabbox run --provider aws --class beast --market on-demand -- pnpm check
crabbox run --provider azure --class beast -- pnpm check
crabbox run --tailscale -- pnpm check
crabbox run --id blue-lobster --network tailscale -- pnpm test
crabbox run --browser -- google-chrome --headless --version
crabbox run --desktop --browser --shell 'echo "$DISPLAY"; "$BROWSER" --version'
crabbox run --id blue-lobster --shell 'pnpm install --frozen-lockfile && pnpm test'
crabbox run --id blue-lobster --script ./scripts/live-smoke.sh
crabbox run --env-from-profile ~/.project-live.profile --allow-env API_TOKEN --script ./scripts/live-smoke.sh
crabbox run --id blue-lobster --full-resync -- pnpm check:changed
crabbox run --label "update flow smoke" -- pnpm test:changed
crabbox run --slug update-flow-smoke -- pnpm test:changed
crabbox run --id blue-lobster --env-from-profile ~/.project-live.profile --allow-env OPENAI_API_KEY --env-helper live -- ./.crabbox/env/live pnpm test:live
crabbox run --fresh-pr acme/app#123 --script ./scripts/e2e-smoke.sh
crabbox run --id cbx_abcdef123456 --junit junit.xml -- go test ./...
crabbox run --provider blacksmith-testbox --blacksmith-workflow .github/workflows/ci-check-testbox.yml --blacksmith-job test -- pnpm test
crabbox run --provider namespace-devbox --namespace-image builtin:base -- pnpm test
crabbox run --provider semaphore --semaphore-project my-app -- pnpm test
crabbox run --provider sprites -- pnpm test
crabbox run --provider daytona --daytona-snapshot crabbox-ready -- pnpm test
crabbox run --provider islo --islo-image docker.io/library/ubuntu:24.04 -- pnpm test
crabbox run --provider e2b --e2b-template base -- pnpm test
crabbox run --provider ssh --target macos --static-host mac-studio.local -- xcodebuild test
crabbox run --provider ssh --target windows --windows-mode normal --static-host win-dev.local -- dotnet test
crabbox run --provider ssh --target windows --windows-mode normal --static-host win-dev.local --shell 'Write-Output ("BROWSER=" + $env:BROWSER)'
crabbox run --provider ssh --target windows --windows-mode normal --static-host win-dev.local --script ./scripts/windows-smoke.ps1 -- -Mode smoke
crabbox run --provider ssh --target windows --windows-mode wsl2 --static-host win-dev.local -- pnpm test
crabbox run --profile live-qa --preset qa-live --scenario login-regression --emit-proof /tmp/proof.md --stop-after success
```

If `--id` is omitted, Crabbox creates a fresh non-kept lease and releases it when the command exits. `--id` accepts the stable `cbx_...` ID or the active friendly slug.

With `--provider blacksmith-testbox`, `--id` accepts a Blacksmith `tbx_...` ID or a local Crabbox slug. Crabbox forwards the command to `blacksmith testbox run`, delegates sync to Blacksmith, and prints `sync=delegated` in the final timing summary.

With `--provider namespace-devbox`, `--id` accepts a Crabbox `cbx_...` ID,
local slug, or existing Devbox name. Namespace owns create, SSH config, and list
through the `devbox` CLI; Crabbox syncs over SSH and runs the command through the
standard SSH executor.

With `--provider semaphore`, `--id` accepts a Semaphore-backed Crabbox
`cbx_...` ID or local slug. Semaphore owns the CI job and debug SSH endpoint;
Crabbox syncs over SSH and runs the command through the standard SSH executor.

With `--provider sprites`, `--id` accepts a Sprites-backed Crabbox `cbx_...` ID,
local slug, `spr_<sprite-name>` ID, or raw sprite name with `--reclaim`.
Sprites owns the microVM lifecycle and `sprite proxy`; Crabbox bootstraps SSH,
syncs over SSH, and runs the command through the standard SSH executor.

With `--provider daytona`, `--id` accepts a Daytona-backed Crabbox `cbx_...` ID
or local slug. Crabbox uploads the sync archive through Daytona toolbox file
APIs, extracts it in the sandbox, and runs the command through Daytona toolbox
process APIs. The final timing summary reports `sync=delegated`.

With `--provider islo`, `--id` accepts an `isb_<crabbox-sandbox-name>` lease ID,
a Crabbox-created sandbox name, or a local Crabbox slug. Islo owns sandbox
workspace setup and command execution, so sync is delegated and the final timing
summary reports `sync=delegated`.

With `--provider e2b`, `--id` accepts a Crabbox `cbx_...` lease ID, local slug,
or Crabbox-owned E2B sandbox ID in raw or `e2b_<sandboxID>` form. Crabbox uploads
the sync archive through E2B file APIs, extracts it in the sandbox, and runs the
command through E2B process APIs. The final timing summary reports
`sync=delegated`.

When the lease has been hydrated by `crabbox actions hydrate`, `run` reads the remote marker under `$HOME/.crabbox/actions`, syncs into the workflow's `$GITHUB_WORKSPACE`, and sources the non-secret env file written by the workflow. If no marker exists and `actions.workflow` is configured, `run` now performs local Actions hydration automatically after sync unless `--no-hydrate` or `--no-sync` is set. That preserves the setup the workflow performed: checkout path, installed dependencies, caches, runner temp/toolcache paths, and any project-specific preparation.

If a JavaScript package-manager command such as `pnpm`, `npm`, `node`, or
`corepack` is run on a raw SSH workspace before a hydration marker exists,
Crabbox probes the remote tool first unless automatic hydration is available.
Missing tools fail before source sync with guidance to hydrate first, include
runtime setup in the command, or choose a provider/image with the JavaScript
toolchain.

`--browser` provisions or requires a known browser binary and injects
`CRABBOX_BROWSER=1`, `BROWSER`, and `CHROME_BIN` into the remote command. It
does not imply `--desktop`; use it alone for headless browser automation.
Browser login/profile state is not managed by Crabbox; use a scenario-owned
profile directory or app-specific auth artifact when tests need a logged-in
browser.

`--desktop` provisions or requires a visible desktop/VNC session and injects
`CRABBOX_DESKTOP=1`; POSIX desktop targets also use `DISPLAY=:99`. It does not
imply a browser. Use `--desktop --browser` for headed browser automation in the
VNC-visible session.

`--tailscale` asks new managed Linux leases to join the configured tailnet.
`--network` selects how Crabbox resolves SSH for reused leases and for the final
connection after a new lease becomes ready. `auto` prefers Tailscale when
metadata exists and SSH is reachable, `tailscale` fails if the tailnet path is
not available, and `public` forces the provider host. See
[Tailscale](../features/tailscale.md).

Sync uses `git ls-files --cached --others --exclude-standard` to build a file manifest, then feeds that manifest to rsync over SSH. That means tracked files plus nonignored untracked files sync, while `.git`, ignored local build output, dependency folders, `.crabboxignore` patterns, `sync.exclude` patterns, and common caches stay out of the transfer. Default excludes also cover common generated churn such as `.ignored`, `.vite`, `playwright-report`, `test-results`, and local `.crabbox` log/capture directories. Crabbox records a local/remote sync fingerprint and skips rsync when the tracked commit plus manifest and dirty metadata have not changed. Use `--full-resync` (alias `--fresh-sync`) when a warm lease smells stale: Crabbox deletes the remote workdir, skips the fingerprint fast path, reseeds Git when possible, and uploads the checkout from scratch. Use `--checksum` when you need a paranoid checksum scan, and `--debug` to print sync timing, progress, and itemized rsync output.

Use `--script <file>` or `--script-stdin` for multi-line remote commands.
Crabbox uploads the script into `.crabbox/scripts/` under the remote workdir,
runs it as a file, and includes that script directory in failure bundles. A
shebang is honored on POSIX targets; scripts without a shebang run through
`bash`. Native Windows targets run uploaded scripts through Windows PowerShell,
and `--script-stdin` is treated as a PowerShell script. When a native Windows
script path does not already end in `.ps1`, Crabbox adds that extension before
uploading so `powershell.exe -File` can execute it normally.
Trailing command arguments after `--` are passed to the script. This is a
direct SSH-run feature; delegated providers reject it before reading stdin.

Use `--env-from-profile <file>` with `--allow-env <name>` for live secrets.
Crabbox parses simple profile lines without executing the profile, forwards
only allowed names, and prints redacted presence/length metadata instead of
values. `--allow-env` is repeatable and also accepts comma-separated names.
Native Windows env-profile handoff files are uploaded as UTF-8 and imported
with PowerShell UTF-8 decoding, so non-ASCII profile values are preserved.
POSIX SSH targets also probe the uploaded profile from inside the remote
workdir and print redacted remote presence metadata before the command runs.
Add `--env-helper <name>` to persist a reusable helper at
`.crabbox/env/<name>` for that lease; the helper sources the matching profile
and then execs the command you pass to it. Persist helpers only on boxes you
control, because the profile remains on the remote workdir until you delete it
or reset the lease.

Use `--fresh-pr <owner/repo#number>` to skip local dirty sync and create a
fresh remote GitHub PR checkout. `--fresh-pr <number>` uses the current
repository's GitHub origin, including common SSH and credentialed HTTPS origin
forms. GitHub PR URLs are accepted only for `github.com`; GitHub Enterprise or
other hosts are rejected so Crabbox does not clone the wrong public repository.
Add `--apply-local-patch` only when the local `git diff --binary HEAD` should
be applied on top of the PR checkout. `--fresh-pr` needs the SSH-run sync path;
delegated providers and native Windows targets reject it.

For `provider=ssh`, `target=macos` and `target=windows windows.mode=wsl2`
use the same POSIX rsync flow. Native Windows mode uses PowerShell over OpenSSH
and sends the manifest as a tar archive into `static.workRoot`; cache purge and
GitHub Actions runner registration remain Linux-only.

On native Windows, plain argv is best for one executable such as `dotnet test`.
Use `--shell` for multi-statement PowerShell snippets, env inspection, or
commands that need PowerShell expression syntax. Use `--script <file.ps1>` for
longer PowerShell runs that should be uploaded and recorded with the run.
Crabbox writes uploaded Windows scripts as UTF-8 with a byte-order mark when the
input has no BOM, which keeps Windows PowerShell 5.1 from treating non-ASCII
source as the system ANSI code page.

Before rsync starts, Crabbox prints the candidate file count and byte estimate. Large syncs warn or fail according to `sync.warnFiles`, `sync.warnBytes`, `sync.failFiles`, and `sync.failBytes`; use `--force-sync-large` or `sync.allowLarge: true` only when the transfer size is intentional. Quiet rsync runs print a heartbeat; after several minutes without visible progress the heartbeat includes a concrete retry hint. `sync.timeout` kills stalled syncs and suggests `--full-resync` or a fresh lease.
Large sync warnings also print the top source directories by file count plus a hint to update `.crabboxignore` or `sync.exclude`.

Before sync, `run` prints a compact context block with run ID, portal/log URLs,
lease ID, slug, provider, SSH target, remote workdir, and whether the workspace
is raw or Actions-hydrated. Add `--preflight` to print a remote capability
snapshot before the command runs. POSIX targets report the remote user, current
directory, sudo/apt availability, Node, pnpm, Docker, and bubblewrap. Native
Windows targets report the user, current directory, Windows PowerShell version,
execution policy, git, tar, Node, pnpm, global `core.longpaths`, temp directory,
and whether `pwsh` is available. The probe runs from the command workdir and
sources the Actions handoff env file when present. Raw workspaces with Actions
hydration configured also print the exact `crabbox actions hydrate ...`
suggestion and whether the selected provider/target supports it.

Configured profiles can define reusable presets. `--preset <name>` expands the
profile command before execution, applies profile and preset environment
defaults, and prints the expanded command for auditability. `--scenario <value>`
sets the common `{{scenario}}` variable; use repeatable `--preset-var name=value`
for other placeholders. Profile doctor checks run before the remote command
when the selected profile enables them, so missing Node, pnpm, Docker, Compose,
or disk prerequisites fail before the expensive lane starts.

Use repeatable `--artifact-glob <glob>` to collect matching remote files after a
successful SSH-backed run. Globs are resolved relative to the remote workdir and
stored locally under `.crabbox/runs/<run-or-lease>/` as a tarball. Profile and
preset `artifactGlobs` are collected the same way. Delegated providers reject
artifact globs because they own command transport. Native Windows and macOS
targets reject artifact globs; use Linux or Windows WSL2 for this collector.

Use `--emit-proof <path>` to render a Markdown `## Real behavior proof` block
after a successful run. The proof is a derived artifact: it uses run metadata,
the expanded command, selected live console output, collected artifact paths,
and the selected `--proof-template` or preset template. Keep proof templates in
repo config so parser-sensitive PR wording stays project-owned.

Use `--stop-after success|always|failure|never` to make lease cleanup explicit.
Without it, a newly acquired one-shot lease is released after the command and an
existing `--id` lease is left alone. The final run details print the exact
`crabbox stop ...` command either way.

At the end of every command, `run` prints a one-line timing summary with sync
duration, command duration, total duration, whether sync was skipped by
fingerprint, and the remote exit code. It also prints run details with provider,
lease ID, slug, run ID, machine type, repo path, remote workdir, Actions URL
when present, stop command, and idle timeout.
Use `--label <text>` to attach a short human-readable label to the run details,
timing JSON, and coordinator run record when available.
Use `--slug <slug>` only when creating a fresh lease. Crabbox normalizes the
requested slug and may add a suffix when an active lease already uses it.

Use `--capture-stdout <path>` when stdout is binary or terminal-hostile. Crabbox
writes the remote stdout bytes directly to the local file, leaves stderr on the
terminal, and skips stdout run-log/event capture. This is useful for Windows
native probes that emit images, Sixel frames, ZIPs, or other byte streams.
Delegated providers reject local capture flags because they own command
transport.

Use `--capture-stderr <path>` the same way for remote stderr. Crabbox diagnostics
still print to the terminal; only the remote command's stderr stream is mirrored
to the local file and omitted from retained run-log/event capture.

When the remote command exits non-zero, Crabbox writes a local-only
`.crabbox/captures/*.tar.gz` failure bundle by default. SSH-backed bundles
include the uploaded script directory, redacted env/config summaries, timing
JSON, command stdout/stderr, common debug paths such as `test-results`,
`playwright-report`, `coverage`, JUnit XML files, nearby `*.log` files, and a
generic gateway log tail when a known gateway log path exists. Blacksmith
delegated bundles include stdout/stderr plus timing and redacted env/config
metadata. Implicit stdout/stderr entries are capped to keep automatic bundles
bounded; explicit `--capture-stdout` / `--capture-stderr` files are included as
caller-created local files. `--capture-on-fail` remains accepted as a
compatibility alias. Crabbox does not redact captured files; the caller owns
redaction before sharing them.

Use `--keep-on-failure` for interactive debugging of a newly acquired lease. On
non-zero exit, Crabbox skips the normal one-shot release, prints inspect/SSH/stop
commands for the exact failed box, and leaves it alive until explicit stop or
the configured idle/TTL expiry. Existing `--id` leases already remain alive, but
the flag still makes the intent visible in the command line.

Use repeatable `--download remote=local` when the command writes proof files on
the box. Downloads run only after a successful remote command, paths are
resolved relative to the remote workdir unless absolute, and Windows paths use
`=` instead of `:` so drive letters remain unambiguous.
Crabbox rejects local output path collisions between stdout capture, stderr
capture, and downloads before command execution.

Use `--timing-json` to emit a final JSON timing record with provider, lease ID,
slug, run ID, machine type, repo path, remote workdir, sync phases, command
phases, command duration, total duration, exit code, stop command, artifacts,
and Actions run URL when available. Failed runs also include `blockedStage` and
`retryLikely` when Crabbox can classify the likely blocker. Commands can emit
phase markers on stdout or stderr as `CRABBOX_PHASE:<name>`; Crabbox records
those as `commandPhases` without removing the marker line from output. In
`blacksmith-testbox` mode, sync is reported as delegated in the same schema.

Before the first rsync into a Git checkout, Crabbox tries to seed the remote worktree from the local `origin` remote so the first sync is a dirty-tree overlay instead of a full source upload. Project-specific excludes can live in `.crabboxignore` or `sync.exclude` in `crabbox.yaml` / `.crabbox.yaml`; env forwarding and base ref belong in config.

After sync, Crabbox runs a remote sanity check. If the remote checkout reports at least 200 tracked deletions, Crabbox fails before running tests unless local `CRABBOX_ALLOW_MASS_DELETIONS=1` is set.

When a coordinator is configured, Crabbox records each remote command as a run history item. `crabbox history` lists those records and `crabbox logs <run-id>` prints retained remote output. Log retention is intentionally bounded so a noisy command cannot fill Durable Object storage.

Add `--junit <path>` or configure `results.junit` to attach JUnit XML summaries to the run record. `crabbox results <run-id>` then prints failed tests without reading the raw log.

Use `crabbox sync-plan` to inspect the same local manifest without leasing a box when a sync estimate looks unexpectedly large.

## Preflight

`--preflight` prints a target-specific capability snapshot after sync and
before the remote command. It is diagnostic only: Crabbox does not install
tools, change the machine, or fail the run just because a tool is missing.
Install logic belongs in Actions hydration, a prebaked image, a devcontainer,
Nix/mise/asdf, or the command/script you run.

By default, Crabbox checks common language and infrastructure tools plus
OS-specific basics. Default generic probes are `git`, `tar`, `node`, `npm`,
`corepack`, `pnpm`, `yarn`, `bun`, and `docker`; `uv` is available as an
additional built-in. Linux/WSL-style POSIX probes also include `sudo`, `apt`,
and `bubblewrap`; native Windows probes include `powershell`,
`execution_policy`, `longpaths`, `temp`, and `pwsh`.

Use `--preflight-tools` to replace the default tool list for one run:

```sh
crabbox run --preflight --preflight-tools node,bun,docker -- bun test
crabbox run --preflight --preflight-tools default,uv -- node --test
crabbox run --preflight --preflight-tools none -- ./smoke.sh
```

`default` expands to Crabbox's default built-ins; `none` keeps only the
workspace summary. Unknown tool names fail before leasing so typos do not hide
missing diagnostics. Unsupported OS-specific probes are skipped for the current
target.

Configure the default per repo:

```yaml
run:
  preflightTools:
    - node
    - bun
    - docker
```

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|azure|gcp|proxmox|ssh|exe-dev|blacksmith-testbox|namespace-devbox|semaphore|sprites|daytona|islo|e2b
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--profile <name>
--class <name>
--type <provider-type>
--azure-os-disk managed|ephemeral|auto
--market spot|on-demand
--slug <slug>
--ttl <duration>
--idle-timeout <duration>
--desktop
--browser
--code
--tailscale
--tailscale-tags <comma-separated tags>
--tailscale-hostname-template <template>
--tailscale-auth-key-env <env-var>
--tailscale-exit-node <name-or-100.x>
--tailscale-exit-node-allow-lan-access
--network auto|tailscale|public
--keep
--keep-on-failure
--no-sync
--no-hydrate
--sync-only
--force-sync-large
--shell
--script <file>
--script-stdin
--fresh-pr <owner/repo#number|url|number>
--apply-local-patch
--allow-env <name>
--env-from-profile <file>
--checksum
--debug
--junit <comma-separated remote XML paths>
--preflight
--preflight-tools <comma-separated tool names>
--capture-stdout <local path>
--capture-stderr <local path>
--capture-on-fail
--download <remote=local>
--label <text>
--reclaim
--timing-json
--blacksmith-org <org>
--blacksmith-workflow <file|name|id>
--blacksmith-job <job>
--blacksmith-ref <ref>
--exe-dev-control-host <host>
--exe-dev-image <image>
--exe-dev-cpus <n>
--exe-dev-memory <size>
--exe-dev-disk <size>
--exe-dev-command <command>
--exe-dev-user <user>
--exe-dev-work-root <path>
--exe-dev-no-email
--namespace-image <image>
--namespace-size <S|M|L|XL>
--namespace-repository <repo>
--namespace-site <site>
--namespace-volume-size-gb <gb>
--namespace-auto-stop-idle-timeout <duration>
--namespace-work-root <path>
--namespace-delete-on-release
--semaphore-host <host>
--semaphore-project <project>
--semaphore-machine <type>
--semaphore-os-image <image>
--semaphore-idle-timeout <duration>
--sprites-api-url <url>
--sprites-work-root <path>
--e2b-api-url <url>
--e2b-domain <domain>
--e2b-template <template-id>
--e2b-workdir <path>
--e2b-user <user>
```

`--idle-timeout` controls inactivity expiry, default `30m`. `--ttl` remains the maximum wall-clock lifetime, default `90m`.
Crabbox records a local repo claim for each reused lease. If a lease is already claimed by another repo, use `--reclaim` to move the claim intentionally.

`--code` provisions or requires a Linux lease with code-server installed. Use
`crabbox code --id <lease>` to expose the editor through the authenticated
portal.

For AWS one-shot leases, `--market` overrides `capacity.market` for this run.
Explicit `--type` keeps exact-type semantics; Crabbox reports why that type
failed rather than falling back to a different size.

Azure one-shot leases use managed `StandardSSD_LRS` OS disks by default so they
can become native checkpoint sources. Use `--azure-os-disk ephemeral` only for
stateless leases where native Azure checkpoint/fork support is not needed.
`--azure-os-disk auto` is accepted for compatibility and resolves to managed.

Delegated providers such as Blacksmith Testbox, Daytona `run`, Islo, and E2B
own command transport. They reject SSH-run-only features including
`--capture-stdout`, `--capture-stderr`, `--capture-on-fail`, `--download`,
`--script`, `--script-stdin`, and `--fresh-pr`. Provider-specific docs note any
extra sync limitations. `--keep-on-failure` is supported for one-shot delegated
runs that Crabbox would otherwise stop after a failed command.
