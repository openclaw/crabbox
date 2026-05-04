# CLI

## Name

`crabbox`

One-liner: lease shared remote test boxes, sync local work, run commands, and clean up.

## Usage

```text
crabbox [global flags] <command> [args]
```

Global flags:

```text
-h, --help
--version
```

Primary output goes to stdout. Progress, diagnostics, and errors go to stderr. JSON output is stable enough for scripts.

## Commands

```text
crabbox doctor
crabbox login [--url <url>] [--provider hetzner|aws] [--no-browser]
crabbox login --url <url> --token-stdin [--provider hetzner|aws]
crabbox logout
crabbox whoami [--json]
crabbox init [--force]
crabbox config show [--json]
crabbox config path
crabbox config set-broker --url <url> --token-stdin [--provider hetzner|aws]
crabbox warmup [--provider hetzner|aws|ssh|blacksmith-testbox] [--target linux|macos|windows] [--desktop] [--browser] [--tailscale] [--network auto|tailscale|public] [--profile <name>] [--idle-timeout <duration>] [--timing-json]
crabbox run [--id <lease-id-or-slug>] [--provider hetzner|aws|ssh|blacksmith-testbox] [--target linux|macos|windows] [--windows-mode normal|wsl2] [--desktop] [--browser] [--tailscale] [--network auto|tailscale|public] [--shell] [--checksum] [--debug] [--force-sync-large] [--timing-json] [--blacksmith-workflow <workflow>] -- <command...>
crabbox desktop launch --id <lease-id-or-slug> [--browser] [--url <url>] [-- <command...>]
crabbox screenshot --id <lease-id-or-slug> [--output <path>]
crabbox sync-plan [--limit <n>]
crabbox history [--lease <lease-id>] [--owner <email>] [--org <name>] [--limit <n>] [--json]
crabbox logs <run-id> [--json]
crabbox events <run-id> [--after <seq>] [--limit <n>] [--json]
crabbox attach <run-id> [--after <seq>] [--poll <duration>]
crabbox results <run-id> [--json]
crabbox cache stats --id <lease-id-or-slug> [--json]
crabbox cache purge --id <lease-id-or-slug> --kind pnpm|npm|docker|git|all --force
crabbox cache warm --id <lease-id-or-slug> -- <command...>
crabbox actions hydrate --id <lease-id-or-slug> [--workflow <file|name|id>] [--wait-timeout <duration>] [--timing-json]
crabbox actions register --id <lease-id-or-slug> [--repo owner/name]
crabbox actions dispatch [--workflow <file|name|id>] [-f key=value]
crabbox status --id <lease-id-or-slug> [--network auto|tailscale|public] [--wait]
crabbox list [--json]
crabbox usage [--scope user|org|all] [--user <email>] [--org <name>] [--month YYYY-MM] [--json]
crabbox admin leases [--state active|released|expired|failed] [--owner <email>] [--org <name>] [--json]
crabbox admin release <lease-id-or-slug> [--delete]
crabbox admin delete <lease-id-or-slug> --force
crabbox ssh --id <lease-id-or-slug> [--network auto|tailscale|public]
crabbox vnc --id <lease-id-or-slug> [--network auto|tailscale|public] [--open]
crabbox webvnc --id <lease-id-or-slug> [--network auto|tailscale|public] [--open]
crabbox inspect --id <lease-id-or-slug> [--network auto|tailscale|public] [--json]
crabbox stop <lease-id-or-slug>
crabbox cleanup [--dry-run]
```

## Common Flows

One-shot run:

```sh
crabbox run --profile project-check -- pnpm check:changed
```

AWS EC2 Spot run:

```sh
crabbox run --class beast -- pnpm check:changed
```

Warm a box, then reuse it:

```sh
crabbox warmup --profile project-check
crabbox warmup --tailscale
crabbox warmup --desktop --browser
crabbox run --id blue-lobster -- pnpm test:changed
crabbox vnc --id blue-lobster --open
crabbox webvnc --id blue-lobster --open
crabbox desktop launch --id blue-lobster --browser --url https://example.com
crabbox screenshot --id blue-lobster --output desktop.png
crabbox run --id blue-lobster --shell 'pnpm install --frozen-lockfile && pnpm test'
crabbox stop blue-lobster
```

Hydrate through GitHub Actions, then run local dirty work in the hydrated workspace:

```sh
crabbox warmup
crabbox actions hydrate --id blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox stop blue-lobster
```

Use Blacksmith Testboxes through the same Crabbox surface:

```sh
blacksmith auth login
crabbox warmup --provider blacksmith-testbox --blacksmith-workflow .github/workflows/ci-check-testbox.yml --blacksmith-job test
crabbox run --provider blacksmith-testbox --id blue-lobster -- pnpm test:changed
crabbox run --provider blacksmith-testbox --blacksmith-workflow .github/workflows/ci-check-testbox.yml --blacksmith-job test -- pnpm test
crabbox stop --provider blacksmith-testbox blue-lobster
```

Use an existing macOS or Windows SSH host:

```sh
crabbox run --provider ssh --target macos --static-host mac-studio.local -- xcodebuild test
crabbox run --provider ssh --target windows --windows-mode normal --static-host win-dev.local -- dotnet test
crabbox run --provider ssh --target windows --windows-mode wsl2 --static-host win-dev.local -- pnpm test
```

Create managed AWS desktop boxes:

```sh
crabbox warmup --provider aws --target windows --desktop --market on-demand
CRABBOX_AWS_MAC_HOST_ID=h-... crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id blue-lobster
crabbox screenshot --id blue-lobster --output desktop.png
```

Managed provider targets are intentionally narrow:

- Hetzner managed provisioning supports Linux only.
- AWS supports Linux, native Windows (`--target windows --windows-mode normal`),
  and EC2 Mac (`--target macos`) when the Mac Dedicated Host is provided.
- Existing macOS and Windows machines belong on `provider=ssh`.

Use Tailscale as an optional network plane:

```sh
crabbox warmup --tailscale
crabbox ssh --id blue-lobster --network tailscale
crabbox vnc --id blue-lobster --network tailscale --open
```

Inspect pool:

```sh
crabbox list
crabbox list --json
```

Inspect local sync size:

```sh
crabbox sync-plan
crabbox sync-plan --limit 10
```

Inspect usage and estimated cost:

```sh
crabbox usage
crabbox usage --scope org --org openclaw
crabbox usage --scope all --json
```

Cleanup direct-provider leftovers:

```sh
crabbox cleanup --dry-run
crabbox cleanup
```

Cleanup is intentionally conservative: it skips kept machines, deletes expired ready/leased/active direct machines, and gives running/provisioning direct machines an extra stale safety window. When a coordinator is configured, brokered cleanup is owned by the Durable Object alarm instead of provider-side sweeping.

Debug config:

```sh
crabbox doctor
crabbox whoami
crabbox config show
crabbox config show --json
```

Inspect recorded runs:

```sh
crabbox run --id blue-lobster --junit junit.xml -- go test ./...
crabbox history --lease cbx_abcdef123456
crabbox logs run_123
crabbox events run_123
crabbox attach run_123
crabbox results run_123
```

Inspect or warm caches on a kept box:

```sh
crabbox cache stats --id blue-lobster
crabbox cache warm --id blue-lobster -- pnpm install --frozen-lockfile
crabbox cache purge --id blue-lobster --kind pnpm --force
```

Trusted operator lease controls:

```sh
crabbox admin leases --state active
crabbox admin release blue-lobster
crabbox admin delete cbx_abcdef123456 --force
```

Trusted operator image controls:

```sh
crabbox image create --id cbx_abcdef123456 --name openclaw-crabbox-20260501-1246 --wait
crabbox image promote ami-1234567890abcdef0
```

## `run`

`crabbox run` is the main command.

Behavior:

1. Load config.
2. Create a durable `run_...` handle when a coordinator is configured.
3. Acquire a lease unless `--id` is provided.
4. Verify SSH readiness.
5. Use the GitHub Actions workspace when the lease has a hydration marker.
6. Sync current repo, unless a matching sync fingerprint lets Crabbox skip rsync.
7. Seed remote Git from the configured origin/base ref before first sync when possible.
8. Run command over SSH.
9. Stream remote output, append run events, and retain bounded command output in coordinator history.
10. Heartbeat coordinator leases in the background.
11. Release lease unless `--keep` is set.
12. Exit with the remote command exit code.

Fresh non-kept leases retry once with a new machine when bootstrap never reaches SSH readiness. Existing leases and `--keep` runs are not retried automatically, so commands are not duplicated on a machine the user asked to keep. Runner bootstrap retries apt and installs only Crabbox plumbing before `crabbox-ready` is allowed to pass.

Flags:

```text
--id <lease-id-or-slug>  reuse an existing lease
--provider <name>        hetzner, aws, ssh, or blacksmith-testbox
--target <name>          linux, macos, or windows
--windows-mode <mode>    normal or wsl2
--static-host <host>     existing SSH host for provider=ssh
--static-user <user>     static SSH user override
--static-port <port>     static SSH port override
--static-work-root <path> static target work root
--profile <name>        profile to run on
--class <name>          machine class override
--type <name>           provider server or instance type override
--ttl <duration>        maximum lease lifetime, default 90m
--idle-timeout <duration> idle expiry, default 30m
--no-sync               run without syncing
--sync-only             sync and exit
--force-sync-large      allow a sync candidate above configured fail thresholds
--keep                  keep lease after command exits
--shell                 run the command string through bash -lc
--checksum              use checksum rsync instead of size/time
--debug                 print sync timing and itemized rsync output
--junit <paths>         comma-separated remote JUnit XML paths to attach to run history
--open                 open local VNC client for `crabbox vnc`
--output <path>        local PNG path for `crabbox screenshot`
--reclaim              claim an existing lease for the current repo
--timing-json          print a final JSON timing record
--blacksmith-org <org>  Blacksmith organization
--blacksmith-workflow <file|name|id> Blacksmith Testbox workflow
--blacksmith-job <job>  Blacksmith Testbox workflow job
--blacksmith-ref <ref>  Blacksmith Testbox git ref
```

Secrets must not be accepted as flag values. Env forwarding is name-based only.

Crabbox stores local lease claims under its state directory. `warmup` and first reuse claim the lease for the current repo; later `run`, `ssh`, `cache`, and `actions hydrate/register` refuse a conflicting repo claim unless `--reclaim` is set.

With `provider: blacksmith-testbox`, Crabbox delegates machine setup, sync, and command transport to the Blacksmith CLI. `--sync-only` is unsupported, sync timing is reported as `sync=delegated`, and Blacksmith auth is handled by `blacksmith auth login`, not `crabbox login`.

## Exit Codes

```text
0   success
1   generic Crabbox failure
2   invalid usage or config
3   auth failure
4   no capacity
5   provisioning failure
6   sync failure
7   SSH failure
8   lease expired
10+ remote command exit code when available
```

If the remote command exits with a code, `crabbox run` returns that code unless Crabbox itself failed first.

## Config Files

The implemented config format is YAML. The default path is:

```text
macOS: ~/.config/crabbox/config.yaml through XDG, or ~/Library/Application Support/crabbox/config.yaml
Linux: ~/.config/crabbox/config.yaml
repo:  crabbox.yaml or .crabbox.yaml
```

User config:

```yaml
broker:
  url: https://crabbox.openclaw.ai
  provider: aws
  token: ...
  access:
    clientId: ...
    clientSecret: ...
profile: project-check
class: beast
lease:
  idleTimeout: 30m
  ttl: 90m
capacity:
  market: spot
  strategy: most-available
  fallback: on-demand-after-120s
aws:
  region: eu-west-1
  rootGB: 400
ssh:
  key: ~/.ssh/id_ed25519
  user: crabbox
  port: "2222"
  # Ordered fallback ports tried after ssh.port; use [] to disable fallback.
  fallbackPorts:
    - "22"
```

Static macOS target:

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: steipete
  port: "22"
  workRoot: /Users/steipete/crabbox
```

Static Windows target:

```yaml
provider: ssh
target: windows
windows:
  mode: normal # normal or wsl2
static:
  host: win-dev.local
  user: Peter
  port: "22"
  workRoot: C:\crabbox
```

AWS EC2 Mac target:

```yaml
provider: aws
target: macos
aws:
  macHostId: h-0123456789abcdef0
capacity:
  market: on-demand
```

`windows.mode: normal` runs native PowerShell over OpenSSH and syncs with a tar
archive. `windows.mode: wsl2` runs commands through `wsl.exe --exec bash -lc`
and uses rsync inside WSL2, so `static.workRoot` should be a WSL path.

`crabbox warmup --market spot|on-demand` and `crabbox run --market spot|on-demand`
override `capacity.market` for a single AWS lease. Use this for temporary quota
or capacity shifts without rewriting repo config.

Open GitHub browser login:

```sh
crabbox login
```

Trusted operators can still set shared-token broker auth without putting the token in shell history:

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://crabbox.openclaw.ai \
  --provider aws \
  --token-stdin
```

`crabbox config set-broker` remains available for scripts that only want to edit config without verifying identity.

Repo-local config is YAML and should hold project-specific choices:

```yaml
profile: project-check
class: beast
actions:
  workflow: .github/workflows/crabbox.yml
  ref: main
  fields:
    - crabbox_docker_cache=true
  runnerLabels:
    - crabbox
sync:
  delete: true
  checksum: false
  gitSeed: true
  fingerprint: true
  baseRef: main
  timeout: 15m
  warnFiles: 50000
  warnBytes: 5368709120
  failFiles: 150000
  failBytes: 21474836480
  allowLarge: false
  exclude:
    - node_modules
    - .turbo
    - dist
env:
  allow:
    - CI
    - NODE_OPTIONS
    - PROJECT_*
results:
  junit:
    - junit.xml
cache:
  pnpm: true
  npm: true
  docker: true
  git: true
  maxGB: 80
  purgeOnRelease: false
```

Blacksmith Testbox config:

```yaml
provider: blacksmith-testbox
blacksmith:
  org: openclaw
  workflow: .github/workflows/ci-check-testbox.yml
  job: test
  ref: main
  idleTimeout: 90m
  debug: false
```

## Environment Variables

```text
CRABBOX_COORDINATOR
CRABBOX_COORDINATOR_TOKEN
CRABBOX_ACCESS_CLIENT_ID
CRABBOX_ACCESS_CLIENT_SECRET
CRABBOX_ACCESS_TOKEN
CRABBOX_PROVIDER
CRABBOX_TARGET
CRABBOX_WINDOWS_MODE
CRABBOX_STATIC_ID
CRABBOX_STATIC_NAME
CRABBOX_STATIC_HOST
CRABBOX_STATIC_USER
CRABBOX_STATIC_PORT
CRABBOX_STATIC_WORK_ROOT
CRABBOX_PROFILE
CRABBOX_CONFIG
CRABBOX_DEFAULT_CLASS
CRABBOX_SERVER_TYPE
CRABBOX_IDLE_TIMEOUT
CRABBOX_TTL
CRABBOX_SSH_KEY
CRABBOX_SSH_USER
CRABBOX_SSH_PORT
CRABBOX_SSH_FALLBACK_PORTS       comma-separated fallback ports, or none
CRABBOX_WORK_ROOT
CRABBOX_ACTIONS_WORKFLOW
CRABBOX_ACTIONS_JOB
CRABBOX_ACTIONS_REF
CRABBOX_ACTIONS_REPO
CRABBOX_ACTIONS_RUNNER_VERSION
CRABBOX_ACTIONS_RUNNER_LABELS
CRABBOX_ACTIONS_EPHEMERAL
CRABBOX_BLACKSMITH_ORG
CRABBOX_BLACKSMITH_WORKFLOW
CRABBOX_BLACKSMITH_JOB
CRABBOX_BLACKSMITH_REF
CRABBOX_BLACKSMITH_IDLE_TIMEOUT
CRABBOX_BLACKSMITH_DEBUG
CRABBOX_RESULTS_JUNIT
CRABBOX_SYNC_CHECKSUM
CRABBOX_SYNC_DELETE
CRABBOX_SYNC_GIT_SEED
CRABBOX_SYNC_FINGERPRINT
CRABBOX_SYNC_BASE_REF
CRABBOX_SYNC_TIMEOUT
CRABBOX_SYNC_WARN_FILES
CRABBOX_SYNC_WARN_BYTES
CRABBOX_SYNC_FAIL_FILES
CRABBOX_SYNC_FAIL_BYTES
CRABBOX_SYNC_ALLOW_LARGE
CRABBOX_ENV_ALLOW
CRABBOX_CACHE_PNPM/NPM/DOCKER/GIT
CRABBOX_CACHE_MAX_GB
CRABBOX_CACHE_PURGE_ON_RELEASE
```

Provider/deploy variables live outside normal CLI operation:

```text
CRABBOX_CLOUDFLARE_API_TOKEN
CRABBOX_CLOUDFLARE_ACCOUNT_ID
CRABBOX_CLOUDFLARE_ZONE_ID
HCLOUD_TOKEN
AWS_PROFILE/AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY
GITHUB_TOKEN
```

## Output Rules

Human output:

```text
acquiring lease profile=project-check ttl=90m
leased cbx_abcdef123456 slug=blue-lobster provider=aws server=i-0123 type=c7a.48xlarge ip=203.0.113.10 idle_timeout=30m0s expires=2026-05-01T17:30:00Z
syncing 184 files -> /work/crabbox/cbx_abcdef123456/openclaw
running pnpm check:changed
...
released cbx_abcdef123456
```

JSON output:

```json
{
  "leaseId": "cbx_abcdef123456",
  "machineId": "hz-ccx33-01",
  "state": "released",
  "exitCode": 0
}
```

No progress bars when stdout is not a TTY.
