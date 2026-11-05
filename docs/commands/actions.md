# actions

`crabbox actions` prepares a leased box from your repository's own GitHub
Actions workflow, so repo-owned setup (toolchains, dependencies, caches,
services) runs the same way CI does before `crabbox run` attaches.

Hydration has two modes:

- **Local hydration (default)** runs the hydrate workflow over SSH on the leased
  box. Crabbox reads the workflow file from `.github/workflows/`, syncs the
  current checkout, executes supported setup steps in the persistent workspace,
  and waits for the ready marker. This path needs no GitHub repository write
  access.
- **GitHub runner hydration (`--github-runner`)** registers the box as a
  self-hosted runner, dispatches the configured workflow with the canonical
  lease label, and waits for the workflow to write the marker. Use it when the
  workflow needs full GitHub Actions semantics — repository secrets, OIDC,
  service containers, or unsupported actions.

The companion commands `actions register`, `actions unregister`, and
`actions dispatch` expose runner registration, exact deregistration, and workflow dispatch.

## Subcommands

```text
hydrate  --id <lease-id-or-slug> [--provider <provider>] [--target linux|macos|windows]
         [--windows-mode normal|wsl2] [--repo owner/name] [--workflow <file|name|id>]
         [--job <name>] [--ref <ref>] [--github-runner] [--wait-timeout 20m]
         [--keep-alive-minutes 90] [--reclaim] [--timing-json] [-f key=value] [--field key=value]

register --id <lease-id-or-slug> [--provider <provider>] [--target linux|macos|windows]
         [--windows-mode normal|wsl2] [--repo owner/name | --org organization]
         [--runner-group <group-name>] [--name <runner-name>]
         [--labels <csv>] [--version latest] [--ephemeral=true] [--wait-timeout 20m]
         [--reclaim] [--receipt <file>]

unregister --id <lease-id-or-slug> | --receipt <file>

dispatch [--repo owner/name] [--workflow <file|name|id>] [--ref <ref>] [-f key=value] [--field key=value]
```

`hydrate` and `register` validate the current repo's lease claim before touching
the box. Pass `--reclaim` to intentionally move a lease to the current repo.
When local defaults point at another backend, repeat the same provider/target
routing flags used to create the lease.

### hydrate

Populates a lease's workspace from the configured workflow. Requires `--id` and
either `--workflow` or `actions.workflow`.

An empty warmup workspace can be hydrated directly; a preliminary `run` or
sync is not required.

```sh
crabbox warmup
crabbox actions hydrate --id blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
```

Local hydration supports Linux and Windows WSL2 targets. With
`--github-runner`, native Windows is also supported. Static macOS hosts can run
commands through `provider=ssh`, but Actions hydration still requires Linux,
Windows WSL2, or native Windows with `--github-runner`.

For GitHub-runner hydration, `--wait-timeout` is one total budget for runner
installation and registration, GitHub online readiness, workflow dispatch, and
the hydration marker. Crabbox prints secret-free setup stages such as
`release-metadata`, `archive-download`, `checksum`, `extraction`, `git-bash`
(native Windows guests without Git Bash), `configure`, and `service-start`. It
does not dispatch the workflow until GitHub reports the exact registered runner
(the ID recorded in the lease claim) as online. It warns first when the lease
reaches its TTL before the budget ends; warm runner-hydration leases with a
longer `--ttl`.

If setup or online readiness consumes the budget, the command fails before
workflow dispatch and reports the last completed stage or runner status. Guest
configuration, service/Scheduled Task, and runner diagnostic logs are retained
under the runner directory; bounded diagnostic tails are included when
available. Registration tokens and Windows guest passwords remain redacted.

Blacksmith Testbox IDs (`tbx_...`) and `--provider blacksmith-testbox` are
skipped, because Blacksmith owns Testbox hydration. Run commands against those
boxes directly with `crabbox run --provider blacksmith-testbox --id <tbx_id> -- ...`.

On success, `hydrate` prints a workspace summary and a total-duration line. Add
`--timing-json` to emit a final JSON timing record (stderr) with provider, lease
ID, slug, total duration, exit code, and — for the GitHub fallback — the Actions
run URL when the marker reports a run ID.

### register

Registers an existing box as a GitHub Actions self-hosted runner. Crabbox
obtains a registration token through `gh api`, installs the official
`actions/runner` package, records its exact GitHub identity in the existing lease
claim, then starts it under systemd on Linux/WSL2 or an owned scheduled task or
detached PowerShell process on native Windows, and waits until GitHub reports the
exact runner online. Supports Linux and Windows targets only. Registration
metadata and the short-lived token travel over SSH stdin rather than the remote
process command line.

```sh
crabbox actions register --id blue-lobster
crabbox actions register --id blue-lobster --org example-org --runner-group release-controls
```

Repository registration remains the default. `--org` selects organization
registration instead; it cannot be combined with an explicit `--repo`.
`--runner-group` requires `--org` and selects an existing group. Crabbox does not
create groups or change their repository/workflow access policy. The configured
`gh` credential needs repository Administration write or organization Self-hosted
runners write permission for the selected scope.

Both ephemeral (default) and persistent (`--ephemeral=false`) registrations are
supported. Registration does not change the provider's lease lifetime. A matching
existing registration is reused. Changes to its name, group, labels, mode, or a
specific installed version require unregistering it and waiting for its listener
to stop. `latest` reuse keeps the installed runner; it does not force an upgrade.
Consumed ephemeral registrations can be configured again on the same lease once
GitHub confirms their absence and the owned listener is inactive. Unknown guest
identity or listener state stops reconciliation instead of replacing a runner.
Existing claims without a runner receipt are adopted only when the guest identity
matches the requested lease registration and GitHub confirms its ownership or absence.

`--wait-timeout` bounds setup and online readiness as one operation. Each runner
gets the labels `crabbox`, the canonical lease label `crabbox-<lease-id>`,
profile/class labels, the slug label when available, and any labels from
`actions.runnerLabels` or `--labels`. Runner names use the friendly slug when
available; workflow inputs and state-file paths keep using the canonical
`cbx_...` ID.

Use `--receipt <file>` when another machine will remove the registration. Crabbox
writes this file atomically before starting a newly configured listener. A write
failure leaves that listener stopped and retains its local cleanup claim. The
file contains only a version, canonical lease ID, GitHub scope, runner ID, and
runner name. It contains no SSH access, tokens, local paths, or VM configuration.
Keep it tied to the trusted provisioning run when transferring it between jobs.

### unregister

Removes only the exact runner ID and scope recorded in the local lease claim:

```sh
crabbox actions unregister --id blue-lobster
crabbox stop --id blue-lobster
```

On a fresh cleanup machine, use the receipt and a current GitHub credential with
runner-management permission for its scope:

```sh
crabbox actions unregister --receipt runner-receipt.json
```

Crabbox verifies the exact ID, name, and canonical lease label with GitHub before
deletion. A conflicting local runner claim stops cleanup. This mode does not
import a lease, grant SSH access, or remove the VM; provider cleanup remains a
separate operation. `--id` and `--receipt` cannot be combined.

No SSH connection or running VM is required. GitHub determines active status;
an already-absent runner is a successful result after scope access is verified.
The historical receipt stays in the claim for repeat cleanup and later guest
reconciliation. Errors preserve it too. Fixed-lease terminal claims retain this
receipt after VM deletion; otherwise unregister before releasing the local claim.
Unregister does not stop a running job or delete the VM. The caller still owns
workload settlement and provider cleanup.

Runner listeners start only after the receipt is durable. If that write fails,
Crabbox attempts exact-ID deregistration and leaves the listener stopped. A
failure of both operations reports the known scope and ID for operator recovery.
The guest install runs without holding the lease claim, so lease renewals keep
committing while it runs. Before recording the runner, Crabbox checks the claim
again: if the lease changed hands or started retiring meanwhile, it deletes the
exact configured runner instead of recording it.
If registration is canceled after configuration, Crabbox spends up to two
minutes recovering and verifying the exact identity for cleanup. While the lease
still owns it, a verified receipt is retained without starting the listener;
`actions unregister` can then remove it.

### dispatch

Dispatches the configured workflow via `gh workflow run`. Requires `--workflow`
or `actions.workflow`.

```sh
crabbox actions dispatch -f testbox_id=cbx_abcdef123456
```

## Workflow inputs

Every `-f` / `--field` value must be `key=value`. During `actions hydrate`, CLI
values override matching `actions.fields` entries. Automatic hydration also uses
`actions.fields`.

Standalone `actions dispatch` sends only inputs explicitly supplied with `-f` /
`--field`; it still uses configured repository, workflow, and ref defaults.

For GitHub runner hydration, Crabbox inspects the selected workflow's
`workflow_dispatch.inputs` (when the workflow path is available under
`.github/workflows/`). It sends only declared inputs and requires `crabbox_id`,
`crabbox_runner_label`, and `crabbox_keep_alive_minutes`; `crabbox_job` is optional.
If that hydration dispatch rejects `crabbox_job` as an unexpected input, Crabbox
retries once without it so older workflow refs stay usable.

A hydrate workflow must accept these inputs:

```yaml
on:
  workflow_dispatch:
    inputs:
      crabbox_id:
        required: true
        type: string
      crabbox_runner_label:
        required: true
        type: string
      crabbox_job:
        required: false
        default: "hydrate"
        type: string
      crabbox_keep_alive_minutes:
        required: false
        default: "90"
        type: string
```

For local hydration, Crabbox runs the YAML job named `hydrate` when present, or
the only job in a single-job workflow. `actions.job` is the marker-input value,
not the YAML job selector; older workflows can omit both. When `actions.job` is
set and the workflow declares `crabbox_job`, Crabbox sends it and verifies the
ready marker came from that job. For `--github-runner`, the job should target the
dynamic lease label:

```yaml
runs-on: [self-hosted, "${{ inputs.crabbox_runner_label }}"]
```

## Config

```yaml
actions:
  repo: example-org/my-app
  workflow: .github/workflows/crabbox.yml
  job: hydrate
  ref: main
  fields:
    - crabbox_docker_cache=true
  runnerLabels:
    - crabbox
  runnerVersion: latest
  ephemeral: true
```

Use `actions.fields` for repository-specific workflow inputs that should be sent
on every hydration.

## Hydration flow

Use hydration when CI already knows how to prepare the repository and you want a
fast local-style loop:

```sh
crabbox warmup
crabbox actions hydrate --id blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
```

The workflow owns repository-specific setup: checkout, dependency install,
caches, and project tools. Local hydration supports `run` steps plus common
setup actions: `actions/checkout`, `actions/setup-node`, `pnpm/action-setup`,
`actions/setup-go`, `actions/setup-python`, and `actions/cache/restore|save`
(cache restore reports a miss; save is skipped). `pnpm/action-setup` requires an
explicit exact `version` (`major.minor.patch`) and installs into the managed tool
cache without lifecycle scripts; it bootstraps Node 24 if Node or npm is absent,
so pnpm setup may precede setup-node. A later setup-node selects the project Node
version without displacing the managed pnpm. Other pnpm inputs and version
inference/ranges require `--github-runner`; install project dependencies in a
separate `run` step. Setup-node accepts `cache: pnpm` as an explicitly uncached
local run. Repo-local composite actions (`./path`) are supported.
Job containers and service containers are not; `actions/checkout` options that
change the repository, path, submodules, or LFS fail locally so you can rerun
with `--github-runner` when you need full GitHub Actions semantics.

For native Windows, the GitHub runner is started as the selected guest user by a
Scheduled Task, not as LocalSystem, so it remains online after the SSH
registration command returns. Fresh Hyper-V Windows acquisition creates the
protected credential escrow used to register that task. The exact UTF-8
password is not trimmed and is not persisted in Crabbox claims or logs. Existing
retained Hyper-V Windows leases are intentionally not backfilled during
`actions hydrate`; release and recreate them before using `--github-runner`.

Local hydration resolves simple expressions in env, run steps, working
directories, and supported action inputs: `${{ inputs.name }}`,
`${{ env.NAME }}`, `${{ github.workspace }}`, `${{ hashFiles(...) }}`, and
runner temp/toolcache references. Unsupported or complex expressions fail with a
suggestion to use `--github-runner`.

After checkout and setup, the workflow writes the ready marker under
`$HOME/.crabbox/actions/`. On native Windows runners, write the marker files under
`%ProgramData%\crabbox\actions` instead (`/c/ProgramData/crabbox/actions` in Git
Bash): native Windows Crabbox reads them there, a directory the runner account
and the SSH user share. Map workflow inputs into step
environment variables such as `CRABBOX_ID` and `CRABBOX_JOB`; do not interpolate
them directly into shell source:

```sh
case "$CRABBOX_ID" in
  ""|*[!A-Za-z0-9_-]*)
    echo "::error::crabbox_id must match [A-Za-z0-9_-]+"
    exit 2
    ;;
esac
case "$CRABBOX_JOB" in
  *$'\n'*|*$'\r'*)
    echo "::error::crabbox_job must not contain line breaks"
    exit 2
    ;;
esac
job="$CRABBOX_JOB"
[ -n "$job" ] || job=hydrate
state_dir="$HOME/.crabbox/actions"
if [ "${RUNNER_OS:-}" = "Windows" ]; then
  program_data="${PROGRAMDATA:-C:\\ProgramData}"
  if command -v cygpath >/dev/null 2>&1; then
    program_data="$(cygpath -u "$program_data")"
  fi
  state_dir="${program_data}/crabbox/actions"
fi
mkdir -p "$state_dir"
state="${state_dir}/${CRABBOX_ID}.env"
env_file="${state_dir}/${CRABBOX_ID}.env.sh"
services_file="${state_dir}/${CRABBOX_ID}.services"
env_ref="$env_file"
services_ref="$services_file"
if [ "${RUNNER_OS:-}" = "Windows" ] && command -v cygpath >/dev/null 2>&1; then
  env_ref="$(cygpath -w "$env_file")"
  services_ref="$(cygpath -w "$services_file")"
fi
{
  echo "WORKSPACE=${GITHUB_WORKSPACE}"
  echo "RUN_ID=${GITHUB_RUN_ID}"
  echo "JOB=${job}"
  echo "ENV_FILE=${env_ref}"
  echo "SERVICES_FILE=${services_ref}"
  echo "READY_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "${state}.tmp"
mv "${state}.tmp" "$state"
```

`crabbox run --id <lease-id-or-slug>` reads that marker, syncs into the hydrated
`$GITHUB_WORKSPACE`, and sources the non-secret env file when present. If no
marker exists and `actions.workflow` is configured, `run` performs local
hydration automatically after sync unless `--no-hydrate` or `--no-sync` is set.
The env file should hold stable GitHub/runner context such as
`GITHUB_WORKSPACE`, `GITHUB_RUN_ID`, `GITHUB_JOB`, `RUNNER_TEMP`, and
`RUNNER_TOOL_CACHE` — never secrets or OIDC request tokens. On native Windows
runners, write it with single-quoted Windows paths and a `;`-separated `PATH`; see
[Actions hydration](../features/actions-hydration.md). Keep the workflow job
alive (via `--keep-alive-minutes`) only when using `--github-runner` and service
containers or job-scoped setup must stay running for the remote command loop.
`crabbox stop <lease-id-or-slug>` writes the `.stop` marker before releasing the
box.

## Warm with a runner

`crabbox warmup --actions-runner` leases a box and registers it as an ephemeral
runner in one step:

```sh
crabbox warmup --actions-runner
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox actions hydrate --provider aws --target windows --windows-mode wsl2 --id blue-lobster
crabbox run --id blue-lobster -- pnpm test
```

## See also

- [run](run.md) — sync and run commands against a lease.
- [warmup](warmup.md) — lease and ready a box.
- [capsule](capsule.md) — capture and replay failures from Actions runs.
