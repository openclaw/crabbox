# CLI

This is the command-surface overview for the `crabbox` binary. It maps the whole
command tree, then documents the parts that are shared across commands: config
files, environment variables, exit codes, and output rules.

For per-command flags and examples, see [`commands/`](commands/README.md) — one
page per top-level command. For the concepts behind the commands (leases, sync,
checkpoints, ponds, and so on), see [`features/`](features/README.md).

## Name

`crabbox` — lease a remote box, sync your dirty checkout, run a command, stream
output, and clean up.

## Usage

```text
crabbox [global flags] <command> [args]
```

Global flags:

```text
-h, --help        show help; also: crabbox help <command>, crabbox <command> --help
-v, --version     print version
```

Primary results go to stdout. Progress, diagnostics, and errors go to stderr.
`--json` is available on most read commands and produces output stable enough to
script against. Progress lines are suppressed when stdout is not a TTY.

## Command map

Commands are grouped here for orientation. Each links to its detailed page under
[`commands/`](commands/README.md).

### Lease lifecycle

```text
crabbox warmup [lease flags]                 lease a box and wait until ready
crabbox run -- <command...>                  sync, run a remote command, stream output
crabbox run --pool <key> -- <command...>     borrow a hydrated ready-pool lease
crabbox status --id <id>                     show lease state (--wait to block)
crabbox inspect --id <id>                     print lease/provider details
crabbox list                                  list machines (alias: crabbox pool list)
crabbox share --id <id> [--user|--org]        grant access to a lease
crabbox unshare --id <id> [--user|--org|--all]
crabbox stop <id-or-slug>                     end a lease (alias: crabbox release)
crabbox cleanup [--dry-run]                   sweep expired direct-provider machines
crabbox pool ready [key]                      list hydrated broker ready-pool leases
```

See [warmup](commands/warmup.md), [run](commands/run.md),
[status](commands/status.md), [inspect](commands/inspect.md),
[list](commands/list.md), [share](commands/share.md),
[unshare](commands/unshare.md), [stop](commands/stop.md),
[cleanup](commands/cleanup.md), [pool](commands/pool.md).

### Run helpers and jobs

```text
crabbox sync-plan [--limit <n>]              preview local sync manifest size hotspots
crabbox job list                              list repo-local configured jobs
crabbox job run <name>                        run a configured job
```

See [sync-plan](commands/sync-plan.md), [job](commands/job.md).

### Observability

```text
crabbox history                               list recorded runs
crabbox logs <run-id>                         print run logs
crabbox events <run-id>                       print run events
crabbox attach <run-id>                       follow events for an active run
crabbox results <run-id>                      show test-result summaries
```

See [history](commands/history.md), [logs](commands/logs.md),
[events](commands/events.md), [attach](commands/attach.md),
[results](commands/results.md).

### Access and desktop

```text
crabbox ssh --id <id>                          print the SSH command
crabbox vnc --id <id> [--open]                 print/open SSH-tunneled VNC details
crabbox webvnc --id <id> [--open]              bridge a desktop lease into the web portal
crabbox code --id <id> [--open]                bridge a code lease into the web portal
crabbox egress start --id <id>                 bridge lease traffic through this machine
crabbox screenshot --id <id> [--output <png>]  capture a PNG from a desktop lease
crabbox desktop launch|terminal|record|proof|doctor|click|paste|type|key
```

See [ssh](commands/ssh.md), [vnc](commands/vnc.md),
[webvnc](commands/webvnc.md), [code](commands/code.md),
[egress](commands/egress.md), [screenshot](commands/screenshot.md),
[desktop](commands/desktop.md).

### Media and artifacts

```text
crabbox media preview --input <video> --output <preview.gif>
crabbox artifacts collect|video|gif|template|publish|list|pull
```

See [media](commands/media.md), [artifacts](commands/artifacts.md).

### Cache

```text
crabbox cache list|stats --id <id>            show remote cache usage
crabbox cache purge --id <id> --kind <kind>   remove cache content
crabbox cache warm --id <id> -- <command...>  run a cache-populating command
crabbox cache volumes [--json]                list configured provider cache volumes
```

See [cache](commands/cache.md).

### Checkpoints, capsules, images, Actions

```text
crabbox checkpoint create|list|inspect|restore|fork|delete|prune
crabbox capsule from-actions|replay|inspect|promote
crabbox image create|promote|fsr-status|delete
crabbox actions hydrate|register|dispatch
```

See [checkpoint](commands/checkpoint.md), [capsule](commands/capsule.md),
[image](commands/image.md), [actions](commands/actions.md).

### Pond (peer discovery / SSH-mesh)

```text
crabbox pond peers --pond <name>              list peer endpoints
crabbox pond connect <pond>                   open SSH -L forwards to members' exposed ports
crabbox pond disconnect <pond>                stop daemonized SSH-mesh forwards
crabbox pond release <pond>                   stop every lease in the pond
```

See [pond](commands/pond.md) and the [pond feature](features/pond.md).

### Providers and admin

```text
crabbox providers                             show provider capabilities
crabbox usage [--scope user|org|all]          cost and usage estimates
crabbox admin leases|lease-audit|providers|hosts|release|delete
crabbox admin aws-identity|aws-policy|mac-hosts
```

See [providers](commands/providers.md), [usage](commands/usage.md),
[admin](commands/admin.md).

### Config and auth

```text
crabbox login --url <url>                      GitHub login, store broker creds, verify
crabbox logout                                 remove the stored broker token
crabbox whoami                                 show broker identity
crabbox doctor                                 check local and broker/provider readiness
crabbox init                                   onboard the current repo
crabbox version                                print version
crabbox config path|show|set-broker
crabbox azure login                            detect Azure subscription, validate, store
```

See [login](commands/login.md), [logout](commands/logout.md),
[whoami](commands/whoami.md), [doctor](commands/doctor.md),
[init](commands/init.md), [config](commands/config.md),
[azure](commands/azure.md).

## `run`

`crabbox run` is the main command. See [run](commands/run.md) for the full flag
list. The behavior is, in order:

1. Load config.
2. When a coordinator (broker) is configured, create a durable `run_…` handle.
3. Acquire a lease unless `--id` is given.
4. Verify SSH readiness.
5. Use the GitHub Actions workspace when the lease carries a hydration marker.
6. Sync the current repo, unless a matching sync fingerprint lets Crabbox skip
   rsync entirely.
7. Seed remote Git from the configured origin/base ref before the first sync when
   possible, so rsync only ships diffs.
8. Run the command over SSH.
9. Stream remote output, append run events, and retain bounded command output in
   coordinator history.
10. Heartbeat brokered leases in the background.
11. Release the lease unless `--keep` is set.
12. Exit with the remote command's exit code.

Fresh, non-kept leases retry once with a new machine when bootstrap never reaches
SSH readiness. Existing leases and `--keep` runs are not retried automatically, so
a command is never duplicated on a machine you asked to keep.

Secrets are never accepted as flag values; environment forwarding is name-based
only (see [env forwarding](features/env-forwarding.md)). Crabbox stores local
lease claims under its state directory: `warmup` and first reuse claim the lease
for the current repo, and later `run`, `ssh`, `cache`, and `actions hydrate`
refuse a conflicting repo claim unless `--reclaim` is set (see
[identifiers](features/identifiers.md)).

### Delegated providers

Some providers do not lease an SSH box; Crabbox delegates sync and command
transport to the provider's own API or CLI. For these, SSH-specific options
(`ssh`, `desktop`, `vnc`, `code`, Actions hydration, `--checksum`, `--sync-only`)
are unavailable or partly restricted, and sync timing is reported as `delegated`.
Examples include `blacksmith-testbox`, `azure-dynamic-sessions`, `e2b`, `modal`,
`islo`, `cloudflare`, `upstash-box`, `tensorlake`, and `wandb`. See
[providers](features/providers.md) for the full adapter list and which surface
each one supports.

## Exit codes

`crabbox` returns `0` on success. Non-zero codes fall into two buckets:

```text
0      success
1–7    Crabbox itself failed before/around the command
       (usage/config, auth, capacity, provisioning, sync, SSH, lease readiness)
<code> the remote command's own exit code, passed through verbatim
```

When the remote command runs and exits non-zero, `crabbox run` returns that exact
code. Crabbox-internal failures (bad usage, missing auth, no capacity, sync or SSH
errors) are reported before the command runs and use the lower codes. There is no
fixed numeric enum for the internal categories; scripts should branch on `0`
versus non-zero and inspect stderr or `--json` for the reason.

## Config files

Config is YAML. Crabbox merges, in increasing precedence: user config, repo
config, environment variables, then flags.

Default paths:

```text
macOS:  ~/.config/crabbox/config.yaml (XDG) or ~/Library/Application Support/crabbox/config.yaml
Linux:  ~/.config/crabbox/config.yaml
repo:   crabbox.yaml or .crabbox.yaml in the repo root
```

`crabbox config path` prints the active user config path. `crabbox config show`
prints the merged config without secret values. See
[configuration](features/configuration.md) for the complete schema.

User config (machine-wide defaults and broker credentials):

```yaml
broker:
  url: https://broker.example.com
  mode: managed
  autoWebVNC: true
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
  hints: true
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

Repo config (project-specific choices, committed with the repo):

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
  # include (root-relative whitelist): when set, ONLY these paths are synced (after excludes).
  # Sync a few paths out of a large repo instead of blacklisting everything else.
  include:
    - src
    - scripts
    - package.json
    - pnpm-lock.yaml
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
  volumes:
    - name: pnpm-store
      key: my-app-linux-amd64-node24-pnpm10-lockhash
      path: /var/cache/crabbox/pnpm
```

### Targets

Managed provider targets are intentionally narrow:

- Hetzner managed provisioning supports Linux only.
- AWS and Azure support Linux, native Windows (`--target windows --windows-mode
  normal`) with managed desktop/VNC, and Windows WSL2 (`--target windows
  --windows-mode wsl2`) for POSIX sync, run, and Actions hydration. Use native
  Windows for desktop/VNC; use WSL2 for Linux tooling on a Windows host.
- AWS also supports EC2 Mac (`--target macos`) when an available Mac Dedicated
  Host exists in the selected region. Brokered mode can discover an available
  host; direct mode requires `CRABBOX_HOST_ID` or `hostId`. `CRABBOX_AWS_MAC_HOST_ID`
  and `aws.macHostId` remain AWS compatibility aliases. Azure has no managed
  macOS target.
- Existing macOS and Windows machines belong on `provider: ssh`.

Static macOS host:

```yaml
provider: ssh
target: macos
static:
  host: mac-studio.example.internal
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

Static Windows host:

```yaml
provider: ssh
target: windows
windows:
  mode: normal # normal or wsl2
static:
  host: win-dev.example.internal
  user: alice
  port: "22"
  workRoot: C:\crabbox
```

`windows.mode: normal` runs native PowerShell over OpenSSH and syncs with a tar
archive. `windows.mode: wsl2` runs commands through `wsl.exe --exec bash -lc` and
uses rsync inside WSL2, so `static.workRoot` should be a WSL path.

AWS EC2 Mac:

```yaml
provider: aws
target: macos
hostId: h-0123456789abcdef0
capacity:
  market: on-demand
```

`crabbox warmup --market spot|on-demand` and `crabbox run --market spot|on-demand`
override `capacity.market` for a single AWS lease, for temporary quota or capacity
shifts without rewriting repo config.

### Broker auth

Open GitHub browser login:

```sh
crabbox login --url https://broker.example.com
```

Trusted operators can set shared-token broker auth without putting the token in
shell history:

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

`crabbox config set-broker` edits config without verifying identity, for scripts.

## Environment variables

This is the canonical environment-variable reference. The most common ones:

```text
CRABBOX_COORDINATOR                broker URL (enables brokered mode for supported providers)
CRABBOX_COORDINATOR_TOKEN          broker user/shared token
CRABBOX_COORDINATOR_ADMIN_TOKEN    broker admin token
CRABBOX_COORDINATOR_MODE           managed|registered
CRABBOX_COORDINATOR_AUTO_WEBVNC    auto-start portal bridge for kept registered desktops
CRABBOX_ADMIN_TOKEN                alias for CRABBOX_COORDINATOR_ADMIN_TOKEN
CRABBOX_ACCESS_CLIENT_ID           Cloudflare Access service-token id
CRABBOX_ACCESS_CLIENT_SECRET       Cloudflare Access service-token secret
CRABBOX_ACCESS_TOKEN               Cloudflare Access token
CRABBOX_PROVIDER                   default provider
CRABBOX_TARGET                     default target (linux|macos|windows)
CRABBOX_TARGET_OS                  alias for CRABBOX_TARGET
CRABBOX_WINDOWS_MODE               normal|wsl2
CRABBOX_DESKTOP                    request the desktop capability
CRABBOX_BROWSER                    request the browser capability
CRABBOX_NETWORK                    auto|tailscale|public
CRABBOX_OWNER                      lease owner email
CRABBOX_ORG                        owning org
CRABBOX_PROFILE                    default profile
CRABBOX_CONFIG                     path to an explicit config file
CRABBOX_DEFAULT_CLASS              default machine class
CRABBOX_ARCH                       default CPU architecture (amd64|arm64)
CRABBOX_SERVER_TYPE                provider server/instance type override
CRABBOX_IDLE_TIMEOUT               idle expiry
CRABBOX_TTL                        max lease lifetime
CRABBOX_WORK_ROOT                  remote work root
```

Static / SSH host:

```text
CRABBOX_STATIC_ID
CRABBOX_STATIC_NAME
CRABBOX_STATIC_HOST
CRABBOX_STATIC_USER
CRABBOX_STATIC_PORT
CRABBOX_STATIC_WORK_ROOT
CRABBOX_SSH_KEY
CRABBOX_SSH_USER
CRABBOX_SSH_PORT
CRABBOX_SSH_FALLBACK_PORTS         comma-separated fallback ports, or none
```

Capacity and AWS:

```text
CRABBOX_AWS_REGION
CRABBOX_AWS_AMI
CRABBOX_AWS_SECURITY_GROUP_ID
CRABBOX_AWS_SUBNET_ID
CRABBOX_AWS_INSTANCE_PROFILE
CRABBOX_AWS_ROOT_GB
CRABBOX_AWS_SSH_CIDRS
CRABBOX_HOST_ID
CRABBOX_AWS_MAC_HOST_ID            legacy AWS alias
CRABBOX_CAPACITY_MARKET
CRABBOX_CAPACITY_STRATEGY
CRABBOX_CAPACITY_FALLBACK
CRABBOX_CAPACITY_REGIONS
CRABBOX_CAPACITY_AVAILABILITY_ZONES
CRABBOX_CAPACITY_HINTS
CRABBOX_CAPACITY_LARGE_CLASSES
```

Actions hydration:

```text
CRABBOX_ACTIONS_WORKFLOW
CRABBOX_ACTIONS_JOB
CRABBOX_ACTIONS_REF
CRABBOX_ACTIONS_REPO
CRABBOX_ACTIONS_RUNNER_VERSION
CRABBOX_ACTIONS_RUNNER_LABELS
CRABBOX_ACTIONS_EPHEMERAL
```

Sync, env, results, cache:

```text
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
CRABBOX_CACHE_PNPM / _NPM / _DOCKER / _GIT
CRABBOX_CACHE_MAX_GB
CRABBOX_CACHE_PURGE_ON_RELEASE
CRABBOX_CACHE_VOLUMES
```

Tailscale:

```text
CRABBOX_TAILSCALE
CRABBOX_TAILSCALE_TAGS
CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE
CRABBOX_TAILSCALE_AUTH_KEY_ENV
CRABBOX_TAILSCALE_AUTH_KEY                    direct-provider only, via auth-key env
CRABBOX_TAILSCALE_EXIT_NODE
CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS
```

Artifact publishing defaults (override `crabbox artifacts publish` flags):

```text
CRABBOX_ARTIFACTS_STORAGE
CRABBOX_ARTIFACTS_BUCKET
CRABBOX_ARTIFACTS_PREFIX
CRABBOX_ARTIFACTS_BASE_URL
CRABBOX_ARTIFACTS_AWS_REGION
CRABBOX_ARTIFACTS_AWS_PROFILE
CRABBOX_ARTIFACTS_ENDPOINT_URL
CRABBOX_ARTIFACTS_S3_ACL
CRABBOX_ARTIFACTS_PRESIGN
CRABBOX_ARTIFACTS_EXPIRES
```

Provider-specific (read by individual adapters; see each provider page under
[features/](features/README.md)):

```text
CRABBOX_BLACKSMITH_*               Blacksmith Testbox
CRABBOX_KUBEVIRT_*                 KubeVirt
CRABBOX_EXTERNAL_*                 External executable provider
CRABBOX_NAMESPACE_*                Namespace Devbox
CRABBOX_SEMAPHORE_* / SEMAPHORE_*  Semaphore
CRABBOX_E2B_API_KEY / E2B_*        E2B
CRABBOX_MODAL_* / MODAL_*          Modal
CRABBOX_AZURE_DYNAMIC_SESSIONS_*   Azure Dynamic Sessions
CRABBOX_WANDB_API_KEY / WANDB_*    Weights & Biases
HCLOUD_TOKEN / HETZNER_TOKEN       Hetzner
DAYTONA_API_KEY / DAYTONA_*        Daytona
RUNPOD_API_KEY                     RunPod
AWS_PROFILE / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
GITHUB_TOKEN                       GitHub API access for Actions/capsules
```

Worker/deploy variables (used to operate the broker itself, not for normal CLI
runs):

```text
CRABBOX_CLOUDFLARE_API_TOKEN
CRABBOX_CLOUDFLARE_ACCOUNT_ID
```

## Output rules

Human output streams progress to stderr and the command's own output to stdout:

```text
acquiring lease profile=project-check ttl=90m
leased cbx_abcdef123456 slug=swift-crab provider=aws server=i-0123 type=c7a.48xlarge ip=203.0.113.10 idle_timeout=30m0s expires=2026-05-01T17:30:00Z
syncing 184 files -> /work/crabbox/cbx_abcdef123456/my-app
running pnpm check:changed
...
released cbx_abcdef123456
```

JSON output (with `--json` where supported):

```json
{
  "leaseId": "cbx_abcdef123456",
  "machineId": "i-0123456789abcdef0",
  "state": "released",
  "exitCode": 0
}
```

No progress bars when stdout is not a TTY.

## See also

- [Commands](commands/README.md) — per-command flags and examples.
- [Configuration](features/configuration.md) — full config schema.
- [Getting started](getting-started.md) — first run walkthrough.
- [How it works](how-it-works.md) and [Architecture](architecture.md) — the
  CLI / broker / runner split.
- [Providers](features/providers.md) — adapter list and per-provider surface.
