# run

`crabbox run` syncs the current dirty checkout to a box, runs a command, streams output, and returns the remote exit code.

```sh
crabbox run --id blue-lobster -- pnpm test:changed:max
crabbox run --class beast -- pnpm check
crabbox run --provider aws --class beast --market on-demand -- pnpm check
crabbox run --browser -- google-chrome --headless --version
crabbox run --desktop --browser --shell 'echo "$DISPLAY"; "$BROWSER" --version'
crabbox run --id blue-lobster --shell 'pnpm install --frozen-lockfile && pnpm test'
crabbox run --id cbx_abcdef123456 --junit junit.xml -- go test ./...
crabbox run --provider blacksmith-testbox --blacksmith-workflow .github/workflows/ci-check-testbox.yml --blacksmith-job test -- pnpm test
crabbox run --provider ssh --target macos --static-host mac-studio.local -- xcodebuild test
crabbox run --provider ssh --target windows --windows-mode normal --static-host win-dev.local -- dotnet test
crabbox run --provider ssh --target windows --windows-mode normal --static-host win-dev.local --shell 'Write-Output ("BROWSER=" + $env:BROWSER)'
crabbox run --provider ssh --target windows --windows-mode wsl2 --static-host win-dev.local -- pnpm test
```

If `--id` is omitted, Crabbox creates a fresh non-kept lease and releases it when the command exits. `--id` accepts the stable `cbx_...` ID or the active friendly slug.

With `--provider blacksmith-testbox`, `--id` accepts a Blacksmith `tbx_...` ID or a local Crabbox slug. Crabbox forwards the command to `blacksmith testbox run`, delegates sync to Blacksmith, and prints `sync=delegated` in the final timing summary.

When the lease has been hydrated by `crabbox actions hydrate`, `run` reads the remote marker under `$HOME/.crabbox/actions`, syncs into the workflow's `$GITHUB_WORKSPACE`, and sources the non-secret env file written by the workflow. That preserves the setup the workflow performed: checkout path, installed dependencies, service containers, caches, runner temp/toolcache paths, and any project-specific preparation. GitHub secrets and OIDC request tokens remain workflow-step scoped unless the project explicitly persists its own short-lived credentials.

If a configured Actions hydration workflow exists and a package-manager command such as `pnpm`, `npm`, `node`, or `corepack` is run before a hydration marker exists, Crabbox warns that the raw box may not have the project runtime installed. Hydrate first for CI-like setup, or include the runtime setup explicitly in the command.

`--browser` provisions or requires a known browser binary and injects
`CRABBOX_BROWSER=1`, `BROWSER`, and `CHROME_BIN` into the remote command. It
does not imply `--desktop`; use it alone for headless browser automation.
Browser login/profile state is not managed by Crabbox; use a scenario-owned
profile directory or app-specific auth artifact when tests need a logged-in
browser.

`--desktop` provisions or requires a visible Linux display and injects
`CRABBOX_DESKTOP=1` plus `DISPLAY=:99`. It does not imply a browser. Use
`--desktop --browser` for headed browser automation in the VNC-visible session.

Sync uses `git ls-files --cached --others --exclude-standard` to build a file manifest, then feeds that manifest to rsync over SSH. That means tracked files plus nonignored untracked files sync, while `.git`, ignored local build output, dependency folders, and common caches stay out of the transfer. Crabbox records a local/remote sync fingerprint and skips rsync when the tracked commit plus manifest and dirty metadata have not changed. Use `--checksum` when you need a paranoid checksum scan, and `--debug` to print sync timing, progress, and itemized rsync output.

For `provider=ssh`, `target=macos` and `target=windows windows.mode=wsl2`
use the same POSIX rsync flow. Native Windows mode uses PowerShell over OpenSSH
and sends the manifest as a tar archive into `static.workRoot`; cache purge and
GitHub Actions runner registration remain Linux-only.

On native Windows, plain argv is best for one executable such as `dotnet test`.
Use `--shell` for multi-statement PowerShell snippets, env inspection, or
commands that need PowerShell expression syntax.

Before rsync starts, Crabbox prints the candidate file count and byte estimate. Large syncs warn or fail according to `sync.warnFiles`, `sync.warnBytes`, `sync.failFiles`, and `sync.failBytes`; use `--force-sync-large` or `sync.allowLarge: true` only when the transfer size is intentional. Quiet rsync runs print a heartbeat, and `sync.timeout` kills stalled syncs.

At the end of every command, `run` prints a one-line summary with sync duration, command duration, total duration, whether sync was skipped by fingerprint, and the remote exit code.

Use `--timing-json` to emit a final JSON timing record with provider, lease ID, sync phases, command duration, total duration, exit code, and Actions run URL when available. In `blacksmith-testbox` mode, sync is reported as delegated in the same schema.

Before the first rsync into a Git checkout, Crabbox tries to seed the remote worktree from the local `origin` remote so the first sync is a dirty-tree overlay instead of a full source upload. Project-specific excludes, env forwarding, and base ref belong in `crabbox.yaml` or `.crabbox.yaml`.

After sync, Crabbox runs a remote sanity check. If the remote checkout reports at least 200 tracked deletions, Crabbox fails before running tests unless local `CRABBOX_ALLOW_MASS_DELETIONS=1` is set.

When a coordinator is configured, Crabbox records each remote command as a run history item. `crabbox history` lists those records and `crabbox logs <run-id>` prints retained remote output. Log retention is intentionally bounded so a noisy command cannot fill Durable Object storage.

Add `--junit <path>` or configure `results.junit` to attach JUnit XML summaries to the run record. `crabbox results <run-id>` then prints failed tests without reading the raw log.

Use `crabbox sync-plan` to inspect the same local manifest without leasing a box when a sync estimate looks unexpectedly large.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|ssh|blacksmith-testbox
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--profile <name>
--class <name>
--type <provider-type>
--market spot|on-demand
--ttl <duration>
--idle-timeout <duration>
--desktop
--browser
--keep
--no-sync
--sync-only
--force-sync-large
--shell
--checksum
--debug
--junit <comma-separated remote XML paths>
--reclaim
--timing-json
--blacksmith-org <org>
--blacksmith-workflow <file|name|id>
--blacksmith-job <job>
--blacksmith-ref <ref>
```

`--idle-timeout` controls inactivity expiry, default `30m`. `--ttl` remains the maximum wall-clock lifetime, default `90m`.
Crabbox records a local repo claim for each reused lease. If a lease is already claimed by another repo, use `--reclaim` to move the claim intentionally.

For AWS one-shot leases, `--market` overrides `capacity.market` for this run.
Explicit `--type` keeps exact-type semantics; Crabbox reports why that type
failed rather than falling back to a different size.

Blacksmith Testbox mode does not support `--sync-only`; Blacksmith owns its own sync behavior.
