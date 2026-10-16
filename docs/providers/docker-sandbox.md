# Docker Sandbox Provider

Read when:

- choosing `provider: docker-sandbox`;
- configuring Docker Sandboxes through the standalone `sbx` CLI;
- changing `internal/providers/dockersandbox`.

Docker Sandbox is a delegated-run provider. Crabbox shells out to the standalone
`sbx` CLI for sandbox lifecycle and command execution. Docker owns the sandbox
runtime and transport; Crabbox owns provider selection, local config, friendly
slugs, local ownership claims, timing summaries, and normalized list/status
rendering.

This provider is intentionally separate from
[Local Container](local-container.md). `docker-sandbox` has no aliases, and the
existing `docker`, `container`, and `local-docker` aliases continue to resolve
to `local-container`.

## When To Use

Use Docker Sandbox when you want a Docker-managed Linux sandbox and command
execution through `sbx exec`. Use Local Container when you want a local
Docker-compatible container with Crabbox-managed SSH, rsync, desktop/browser
surfaces, or local Docker socket pass-through.

## Prerequisites

- The standalone `sbx` CLI must be on `PATH`, or configured with
  `--docker-sandbox-cli` / `dockerSandbox.cliPath`.
- Run `sbx login` before Crabbox lifecycle commands when your Docker Sandbox
  account requires authentication.
- The host must satisfy Docker Sandbox virtualization requirements. Doctor
  surfaces KVM, hypervisor, or virtualization errors when the CLI reports them.

## Supported `sbx` Contract

Crabbox intentionally targets the standalone Docker Sandbox CLI surface, not an
internal Docker API.

Validated local proof for this PR used this `sbx` version pair:

- `sbx` client `v0.31.3`
- `sbx` server `v0.31.3`

This is the validated compatibility baseline, not a promise that older or newer
`sbx` releases have the same behavior. Crabbox treats other `sbx` versions as
best-effort compatible only when they preserve the command and JSON contract
below. When Docker changes this contract, update this document and the
provider-local tests in `internal/providers/dockersandbox` in the same change.
`crabbox doctor --provider docker-sandbox` reports `sbx_compatibility=ok` when
the installed `sbx` version matches this baseline and a warning when it is a
best-effort version.

Crabbox currently depends on these `sbx` commands and flags:

- `sbx version`
- `sbx ls --json`
- `sbx diagnose --output json` as optional doctor enrichment
- `sbx create --name <name> [--template ...] [--cpus ...] [--memory ...] [--clone] shell <repo-root> [extra-workspaces...]`
- `sbx exec [--workdir ...] [--env-file ...] <sandbox-name> <command...>`
- `sbx ports <sandbox-name> [--json] [--publish ...] [--unpublish ...]`
- `sbx cp [-L] <src> <dst>`
- `sbx rm --force <sandbox-name>`

These calls match the official Docker Sandboxes docs. The public starting
point is <https://docs.docker.com/ai/sandboxes/>; the matching local scrape is
`../docs/content/manuals/ai/sandboxes/_index.md`. The install and first-run
walkthrough lives at <https://docs.docker.com/ai/sandboxes/get-started/> and
`../docs/content/manuals/ai/sandboxes/get-started.md`. The sibling Docker docs
checkout also includes exact CLI references:
`../docs/data/sbx_cli/sbx_create_shell.yaml` documents that `sbx create shell
PATH` mounts the workspace path inside the sandbox, and
`../docs/data/sbx_cli/sbx_exec.yaml` documents that `sbx exec` receives a
sandbox name, command arguments, `--workdir`, and `--env-file`.
`../docs/data/sbx_cli/sbx_ports.yaml` documents the publish/list/unpublish port
surface, and `../docs/data/sbx_cli/sbx_cp.yaml` documents `sbx cp` with
`SANDBOX:PATH` syntax.

`sbx ls --json` compatibility currently accepts either a top-level array or an
object wrapper containing arrays under `sandboxes`, `items`, `data`, or
`results`. Per-item field parsing accepts common variants: `id`, `ID`,
`sandboxId`, and `sandbox_id`; `name`, `Name`, `sandboxName`, and
`sandbox_name`; `state`, `status`, and `Status`; `agent` and `Agent`; and
`workspace`, `workdir`, `workingDir`, and `working_dir`. Records without a name
or id are ignored.

## Commands

```sh
crabbox doctor --provider docker-sandbox
crabbox warmup --provider docker-sandbox --slug live-smoke
crabbox run --provider docker-sandbox -- echo ok
crabbox run --provider docker-sandbox --id live-smoke -- pwd
crabbox ports --provider docker-sandbox --id live-smoke --publish 3000
crabbox cp --provider docker-sandbox --id live-smoke SANDBOX:/tmp/output.log ./output.log
crabbox list --provider docker-sandbox --json
crabbox status --provider docker-sandbox --id live-smoke
crabbox stop --provider docker-sandbox live-smoke
```

## Config

```yaml
provider: docker-sandbox
target: linux
dockerSandbox:
  target: local     # local (default) or cloud
  cliPath: sbx
  agent: shell
  template: ""
  cpus: 0
  memory: ""
  clone: false
  workdir: ""        # empty means the current repo root path inside the sandbox
  extraWorkspaces: []
  mcp: []            # optional sbx MCP server names; repeated values pass through to sbx create --mcp
  kit: []
```

Provider flags:

```text
--docker-sandbox-target
--docker-sandbox-cli
--docker-sandbox-agent
--docker-sandbox-template
--docker-sandbox-cpus
--docker-sandbox-memory
--docker-sandbox-clone
--docker-sandbox-workdir
--docker-sandbox-extra-workspace
--docker-sandbox-mcp   # repeatable; forwarded to sbx create --mcp in order
--docker-sandbox-kit
```

Environment overrides:

```text
CRABBOX_DOCKER_SANDBOX_TARGET
CRABBOX_DOCKER_SANDBOX_CLI
CRABBOX_DOCKER_SANDBOX_AGENT
CRABBOX_DOCKER_SANDBOX_TEMPLATE
CRABBOX_DOCKER_SANDBOX_CPUS
CRABBOX_DOCKER_SANDBOX_MEMORY
CRABBOX_DOCKER_SANDBOX_CLONE
CRABBOX_DOCKER_SANDBOX_WORKDIR
CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES
CRABBOX_DOCKER_SANDBOX_MCP   # comma-separated MCP server names; forwarded to sbx create --mcp
CRABBOX_DOCKER_SANDBOX_KIT
```

`extraWorkspaces`, `mcp`, and `kit` environment values are comma-separated. Crabbox passes non-empty `mcp` entries through unchanged to repeatable `sbx create --mcp` flags.

## Lifecycle

For streamed workload execution, ordinary positive native CLI exits remain
command exits. Returned transport, cancellation, deadline, and I/O errors instead
produce exit 1 and matching timing/result status, even when the native runner also
reports a nonzero code. Later cancellation does not replace an already observed
ordinary exit. A normal CLI diagnostic exit cannot be distinguished from a remote
workload exit without stronger native protocol evidence.

Run finalization shares Crabbox's terminal error merger while keeping Docker's
clone retention and native-workspace telemetry provider-specific. A failed
automatic removal returns exit 1 after command success and leaves the session
marked kept with its claim available for recovery. After a command or setup
failure, later removal or timing errors remain visible without replacing the
primary exit code or cause. `--keep-on-failure` also retains an acquired sandbox
when command or environment preparation fails before execution.

Clone retention is decided from successful command execution, before timing
output: a later timing-write failure returns an error but does not discard
unfetched commits. Failed or unexecuted clone commands are still cleaned up
unless `--keep` or `--keep-on-failure` applies. Non-clone sandboxes already removed
before a timing-write failure remain removed; reporting cannot retain them
retroactively. Timing continues to mark sync skipped because the workspace is
provider-delegated, not because Crabbox silently applies `--no-sync`.

1. `warmup` or `run` without `--id` creates a Crabbox-owned sandbox name such as
   `crabbox-my-app-1a2b3c`.
2. Crabbox runs `sbx create --name <name> ... shell <repo-root>` and records a
   local claim as `dsbx_<sandbox-name>`.
3. `run` executes through `sbx exec --workdir <repo-root> <sandbox-name> <cmd>`
   by default, matching the workspace path passed to `sbx create shell`. Set
   `dockerSandbox.workdir` only when your template mounts the workspace
   somewhere else. Commands with shell operators, leading environment
   assignments, or `--shell` are wrapped as `sh -lc`.
   Crabbox forwards selected `--allow-env` / `--env-from-profile` values through
   a temporary `sbx exec --env-file` so values do not appear in local process
   arguments.
4. `list`, `status`, and `stop` intersect `sbx ls --json` with local Crabbox
   claims for `provider=docker-sandbox`. Sandboxes without a local Crabbox claim
   are not listed and cannot be stopped by Crabbox.
5. `crabbox ports` resolves a Crabbox-owned claim, then maps directly to
   `sbx ports` for listing, publishing, or unpublishing host port forwards.
6. `crabbox cp` resolves a Crabbox-owned claim, rewrites the special
   `SANDBOX:PATH` side to the real sandbox name, and then calls `sbx cp` for
   host-to-sandbox or sandbox-to-host copies.
7. When `dockerSandbox.clone` / `--docker-sandbox-clone` is enabled, Crabbox
   requires the current workspace to be the main checkout of a normal Git
   repository. Linked Git worktrees are rejected before `sbx create --clone`,
   matching Docker Sandbox upstream clone restrictions.
8. Fresh one-shot `run` calls in clone mode keep the newly created sandbox after
   a successful command, even without `--keep`, so unfetched in-sandbox commits
   are not discarded by automatic cleanup. Crabbox prints the matching manual
   cleanup command instead of calling `sbx rm --force` on success.
9. `crabbox stop --provider docker-sandbox <slug>` maps to
   `sbx rm --force <sandbox-name>` and removes the local claim. Crabbox does
   not map stop to `sbx stop`, because Crabbox stop means provider resource
   cleanup, while `sbx stop` retains sandbox state.

## Cloud target

Select Docker-managed compute explicitly with `dockerSandbox.target: cloud`,
`--docker-sandbox-target cloud`, or `CRABBOX_DOCKER_SANDBOX_TARGET=cloud`.
The default stays `local`; the `docker` alias still selects Local Container.
Cloud and local claims are separate. Supply the cloud selector on every cloud
command, including `status`, `cp`, `stop`, and `cleanup`.

### Setup and verified CLI surface

Install from Docker's official Homebrew tap on an Apple silicon Mac:

```sh
brew tap docker/tap
brew install --cask docker/tap/sbx
sbx login
sbx --cloud diagnose
crabbox doctor --provider docker-sandbox --docker-sandbox-target cloud
```

A Docker Agentic Platform subscription with Cloud Sandboxes access is required.
Crabbox checks `sbx version --json` and `sbx --cloud diagnose --json` before
scanning or archiving the checkout for allocation; CLI, version, authentication, cloud connectivity, and entitlement
checks must all pass. Doctor does not create a sandbox. Authentication remains
owned by `sbx`; Crabbox neither reads nor stores Docker credentials.

Cloud support requires **sbx v0.48.0 or newer**, the locally verified baseline.
Docker's general cloud guides start at v0.45.0; Crabbox's narrower baseline
covers the current structured diagnostics and `--static-mcp` interface.
The official `docker/tap/sbx` cask pins a SHA-256 for Docker's release artifact.
The v0.48.0 help was checked for `create`, `exec`, `cp`, `ls`, `diagnose`, `ttl`,
and `rm`, including their `--cloud` forms. The signed-out diagnostic was checked
with isolated empty user state. Coordinator verification with a real account
covered device-flow login and entitlement refusal: doctor and warmup returned
exit 2 without allocating or changing inventory because the account lacked a
Docker Agentic Platform subscription. The full live lifecycle remains unverified;
the coordinator must run the live recipe below with an entitled account before merge.

Sources: [installation](https://docs.docker.com/ai/sandboxes/install/),
[cloud setup](https://docs.docker.com/ai/sandboxes/cloud/),
[cloud usage](https://docs.docker.com/ai/sandboxes/cloud/usage/),
[local/cloud differences](https://docs.docker.com/ai/sandboxes/cloud/local-vs-cloud/),
and [official releases](https://github.com/docker/sbx-releases).

### Source, execution, and results

Cloud creation uses `sbx --cloud create --name <crabbox-name> --ttl <duration>
--on-timeout delete shell`, with no host path. `warmup` and each ordinary `run`
prepare the selected checkout through Crabbox's shared archive sync: tracked
files and eligible untracked edits, respecting ignore and sync rules. The archive
is copied with `sbx --cloud cp` and extracted in
`/home/agent/workspace/project`. `--no-sync` preserves an existing checkout;
`--sync-only` updates it without running a command. This is a point-in-time copy,
not a mount or live synchronization. Remote edits never flow back automatically.

Commands run through `sbx --cloud exec --workdir <workspace> <immutable-id>`.
Output is streamed as untrusted provider output and native command exit codes
are preserved. Transport failures, source transfer failures and cleanup failures
remain errors. Exact `--download REMOTE=LOCAL` and `--require-artifact REMOTE`
paths are supported through the shared result machinery (64 KiB per file), with
bounded `sbx exec` capture before any host file is written. Directories and
symlinks are rejected for these downloads.
Use `crabbox cp` for larger files or directories. Artifact globs are not supported.

The cloud target rejects local `--clone`, extra host workspaces and user environment
forwarding. Core-generated run/lease/slug metadata is passed as non-secret values.
Set `env.allow: []` when inherited forwarding defaults such as `CI` are present.
Configure cloud secrets with `sbx` if a workload needs them; local
sandbox secrets are separate. Crabbox does not create credential files or put
credentials in command arguments. Inherited SSH options do not configure this
transport. `--full-resync`, Actions hydration, desktop and SSH access remain
unsupported by this provider.

Cloud sizing accepts 1 CPU/2g, 2/4g, 4/8g, 8/16g or 16/32g; setting one resource
lets `sbx` select the matching other resource. Templates must already exist in
the cloud registry and supply their own sizing. MCP attachments use cloud
`--static-mcp` names, not the local MCP store. Cloud account network policy is
also separate. Publishing a port exposes a **public HTTPS URL**, not a localhost
socket. Only sandbox port numbers (optionally `/tcp`) are accepted; authenticate
your service and remove exposure when finished.

### Ownership and lifetime

Like the local target, cloud sandboxes receive a `crabbox-` name and a local
ownership claim. Cloud claims use a distinct `dsbxc_` lease namespace and cloud
scope, recording the immutable `sbx_` ID and the original name. Every mutation
requires that exact claim; a name prefix alone never grants ownership. Reuse,
copy, status and deletion verify the immutable ID and recorded name against
`sbx --cloud ls --json` for the currently authenticated account. An account
switch, missing ID or changed name fails closed. Local claims cannot authorize
cloud operations and cloud claims cannot authorize local operations.

Crabbox maps `--ttl` to cloud `--ttl` with an explicit `--on-timeout delete`.
Crabbox's effective TTL is 30 minutes by default and is passed explicitly,
overriding Docker's one-hour native default. The provider maximum is 24 hours,
subject to Crabbox's configured lifetime policy.
The requested creation deadline is recorded in the claim and shown in labels.
Retaining a lease does not disable expiration. Cloud compute is billable, and a
laptop sleeping or closing Crabbox does not stop it. Native expiration is the
independent safety bound. Crabbox does not extend or renew native TTL on reuse.

There is no native idle timeout in the verified CLI. Crabbox records activity
and applies idle expiry when `crabbox cleanup` runs; it is not a background idle
watchdog. Cleanup also recognizes the recorded creation TTL. A missing resource
is omitted from live lists and cannot be reused. Its local claim is kept because
absence could also mean a different Docker account; check the account before
using the separate local claim-forgetting workflow.

`crabbox stop` means **remove**, using `sbx --cloud rm --force <immutable-id>`.
Crabbox checks inventory afterward because native `rm` may return success while
deletion is pending. An unconfirmed deletion preserves the claim and fails with
retry guidance. An accepted deletion is durably marked `deleting`, preventing
reuse; a later stop or cleanup can retire that claim once the ID disappears.
Mere absence without an accepted deletion never retires a claim automatically.
An ambiguous create response reports the generated name for
manual inspection instead of guessing ownership or removing by name.
Cloud acquisition respects `directLeases: deny`; a trusted
`directLeaseAllow: [docker-sandbox]` exception permits it. Local use remains exempt.

### Live proof recipe (coordinator only)

Start in this branch's worktree with `bin/crabbox` built. The recipe creates a
small synthetic checkout, a separate config and one isolated Crabbox state root.
`sbx` continues using the coordinator's own logged-in Docker account.
Do not change accounts during this proof. Native shell creation needs no AI
provider API key.

```sh
brew tap docker/tap
brew install --cask docker/tap/sbx  # or brew upgrade --cask docker/tap/sbx
sbx login
sbx --cloud diagnose
CRABBOX_CLOUD_BINARY="$PWD/bin/crabbox"
CRABBOX_CLOUD_PROOF_DIR="$(mktemp -d -t crabbox-cloud-proof)"
mkdir -p "$CRABBOX_CLOUD_PROOF_DIR/repo/bin" "$CRABBOX_CLOUD_PROOF_DIR/state"
cp "$CRABBOX_CLOUD_BINARY" "$CRABBOX_CLOUD_PROOF_DIR/repo/bin/crabbox"
printf 'provider: docker-sandbox\nenv:\n  allow: []\ndockerSandbox:\n  target: cloud\n' > "$CRABBOX_CLOUD_PROOF_DIR/config.yaml"
chmod 600 "$CRABBOX_CLOUD_PROOF_DIR/config.yaml"
export CRABBOX_CONFIG="$CRABBOX_CLOUD_PROOF_DIR/config.yaml"
export XDG_STATE_HOME="$CRABBOX_CLOUD_PROOF_DIR/state"
export CRABBOX_DOCKER_SANDBOX_TARGET=cloud
cd "$CRABBOX_CLOUD_PROOF_DIR/repo"
git init -q
printf 'bin/\ncloud-*.txt\n' > .gitignore
printf 'Synthetic cloud proof checkout\n' > README.md
git add .gitignore README.md

bin/crabbox doctor --provider docker-sandbox
bin/crabbox warmup --provider docker-sandbox --slug cloud-proof --ttl 30m
bin/crabbox status --provider docker-sandbox --id cloud-proof
bin/crabbox run --provider docker-sandbox --id cloud-proof --shell \
  'test -f README.md && mkdir -p proof && printf "cloud-source-ok\n" > proof/result.txt && cat proof/result.txt'
bin/crabbox cp --provider docker-sandbox --id cloud-proof \
  SANDBOX:/home/agent/workspace/project/proof/result.txt ./cloud-result.txt
cat ./cloud-result.txt
bin/crabbox run --provider docker-sandbox --id cloud-proof --no-sync \
  --download proof/result.txt=./cloud-downloaded.txt -- cat proof/result.txt
cmp ./cloud-result.txt ./cloud-downloaded.txt
bin/crabbox list --provider docker-sandbox --json
bin/crabbox status --provider docker-sandbox --id cloud-proof
bin/crabbox stop --provider docker-sandbox cloud-proof
bin/crabbox cleanup --provider docker-sandbox --dry-run
bin/crabbox list --provider docker-sandbox --json
sbx --cloud ls --json
```

Record the immutable ID printed by warmup and confirm it is absent from the
final native inventory; Crabbox's final list must be empty for this isolated
scope. Existing unowned sandboxes must be unchanged. If stop reports pending,
inspect the native inventory and retry after deletion completes; preserve the
claim and state root until the outcome is confirmed. To prove idle cleanup,
repeat warmup with another slug and `--idle-timeout 1s`, wait at least a second,
and run `bin/crabbox cleanup --provider docker-sandbox` followed by both lists.
Do not use `sbx reset`, `prune`, or broad removal commands for proof cleanup.

## Doctor

`crabbox doctor --provider docker-sandbox` is non-mutating. It runs:

- `sbx version`
- `sbx ls --json`
- `sbx diagnose --output json` as optional diagnostic enrichment

Common blockers:

| Symptom | Action |
| --- | --- |
| `sbx` not found | Install the standalone Docker Sandbox CLI or set `dockerSandbox.cliPath`. |
| Authentication failure | Run `sbx login` and retry. |
| Virtualization, KVM, or hypervisor error | Fix host virtualization prerequisites for Docker Sandboxes. |
| Malformed `sbx ls --json` output | Upgrade or inspect the installed `sbx` CLI; Crabbox parses common array and object wrapper shapes. |

## Capabilities

- Target: Linux.
- Kind: delegated-run.
- Coordinator: never. Docker Sandbox always runs direct from the CLI.
- Aliases: none.
- SSH, desktop, browser, code-server, Tailscale, Actions hydration, VNC, and
  Crabbox rsync are not supported in v1. Inherited global SSH config is ignored
  for delegated `sbx exec` runs.
- `--lease-output` is supported for run cleanup metadata; the cleanup command
  points back to `crabbox stop --provider docker-sandbox <slug>`.
- `crabbox ports` and `crabbox cp` are supported for Crabbox-owned Docker
  Sandbox leases only.
- Agent: `shell` only. Other `dockerSandbox.agent` values are rejected until
  Crabbox has a stable non-shell agent contract.
- MCP attachments: supported as create-time passthrough. Crabbox forwards
  `dockerSandbox.mcp` / `--docker-sandbox-mcp` / `CRABBOX_DOCKER_SANDBOX_MCP`
  entries to repeatable `sbx create --mcp` flags without additional runtime
  discovery semantics.
- Forwarded env: supported through `sbx exec --env-file`.

## Safety Notes

- Crabbox never passes Docker credentials as command-line arguments.
- Docker Sandbox is inside the runtime trust boundary for this provider:
  `sbx exec` receives the command, workspace path, and any environment values
  explicitly selected by Crabbox env-forwarding rules.
- Crabbox-owned controls are provider selection, config validation, env
  allowlist selection, diagnostic redaction, temporary env-file creation and
  cleanup, local process-argument avoidance for forwarded values, and local
  claim filtering.
- Docker-owned controls are the `sbx` client/server contract, account
  authentication, command transport, microVM isolation, virtualization
  prerequisites, and sandbox-internal Docker runtime.
- `list`, `status`, and `stop` operate only on local Crabbox claims for
  `provider=docker-sandbox`.
- `ports` and `cp` also operate only on local Crabbox claims for
  `provider=docker-sandbox`; they do not cross into user-created sandboxes.
- `--docker-sandbox-clone` requires the main checkout of a normal Git
  repository before Crabbox calls `sbx create --clone`; linked Git worktrees
  are rejected.
- Successful one-shot clone-mode runs keep newly created sandboxes by default so
  unfetched in-sandbox commits are preserved until you run `crabbox stop`.
- Forwarded environment values are written to a temporary local env file and
  passed with `sbx exec --env-file`; Crabbox does not place selected values in
  local `sbx exec` process arguments, and the temporary file is removed after
  the run returns.
- The official Docker Sandboxes docs describe direct workspace mounts as part
  of the sandbox trust model and recommend stored secrets over raw environment
  variables. Crabbox's `--allow-env` forwarding remains an explicit operator
  choice, and Docker Sandbox remains inside the runtime trust boundary for
  those forwarded values.

## Live Smoke

When the host has a usable Docker Sandbox provider configuration, run:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=docker-sandbox scripts/live-smoke.sh
```

That top-level smoke dispatches to the provider-specific script:

```sh
scripts/live-docker-sandbox-smoke.sh
```

The script builds `bin/crabbox`, runs `crabbox doctor --provider docker-sandbox`
as the authoritative preflight, confirms the doctor output includes
`sbx_version`, creates a unique Crabbox-owned sandbox, runs `echo ok` and `pwd`,
lists it, and then stops it. The doctor step honors `dockerSandbox.cliPath`,
`--docker-sandbox-cli`, and `CRABBOX_DOCKER_SANDBOX_CLI`, so the smoke script
works even when `sbx` is not on `PATH`. If the provider is missing, logged out,
or blocked by host prerequisites, the script reports `environment_blocked` with
the failing command. If Docker Sandbox capacity or account quota blocks the run,
the script reports `quota_blocked`. If the doctor command succeeds but does not
emit the expected version diagnostic, the script reports `diagnostic_only` and
does not claim live success.

Use this matrix when proving a fresh install or upgrade:

| Proof row | Command or coverage | Expected result |
| --- | --- | --- |
| Fresh local CLI build | `go build -trimpath -o bin/crabbox ./cmd/crabbox` | `bin/crabbox` builds without generated files in git. |
| Provider discovery | `bin/crabbox providers --json` | Includes `docker-sandbox`, family `docker-sandbox`, kind `delegated-run`, target `linux`, coordinator `never`, and feature `run-session`. |
| Config upgrade surface | `crabbox config show --json` | Includes `dockerSandbox.cliPath`, `agent`, runtime sizing, `clone`, `workdir`, `extraWorkspaces`, `mcp`, and `kit`. |
| Alternate `sbx` path | `CRABBOX_DOCKER_SANDBOX_CLI=/path/to/sbx scripts/live-docker-sandbox-smoke.sh` | Doctor honors the configured CLI path instead of requiring `sbx` on `PATH`. |
| Missing CLI or login | `crabbox doctor --provider docker-sandbox` | Fails with guidance to install/configure `sbx` or run `sbx login`. |
| Host/runtime blockers | `crabbox doctor --provider docker-sandbox` | Surfaces virtualization, hypervisor, KVM, control-plane, quota, or capacity blockers without claiming live success. |
| Clone mode | `crabbox warmup --provider docker-sandbox --docker-sandbox-clone ...` | Requires the main checkout of a normal Git repository workspace before calling `sbx create --clone`; linked Git worktrees are rejected. |
| Unsupported delegated flags | Docker Sandbox run flags and config validation | Rejects unsupported agent, desktop, Tailscale, `--class`, and `--type` surfaces clearly while allowing create-time MCP passthrough. |
| Live lifecycle | `CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=docker-sandbox scripts/live-smoke.sh` or `scripts/live-docker-sandbox-smoke.sh` | Prints `classification=live_sbx_smoke_passed ... cleanup=complete` only after create, run, list, and stop complete. |
| Trust boundary handoff | `scripts/docker-sandbox-trust-boundary-smoke.sh` | Uses a fake `sbx` at the process boundary to prove Crabbox sends the workspace path, user command, and `--env-file`, while the forwarded value stays out of local argv and is present only in the env-file handoff. |

Related docs:

- [Provider reference](README.md)
- [Local Container](local-container.md)
- [Provider backends](../provider-backends.md)
- [run](../commands/run.md)
- [ports](../commands/ports.md)
- [cp](../commands/cp.md)
- [stop](../commands/stop.md)
