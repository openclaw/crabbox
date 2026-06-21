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

The companion commands `actions register` and `actions dispatch` expose the
runner-registration and workflow-dispatch steps directly.

## Subcommands

```text
hydrate  --id <lease-id-or-slug> [--provider <provider>] [--target linux|macos|windows]
         [--windows-mode normal|wsl2] [--repo owner/name] [--workflow <file|name|id>]
         [--job <name>] [--ref <ref>] [--github-runner] [--wait-timeout 20m]
         [--keep-alive-minutes 90] [--reclaim] [--timing-json] [-f key=value] [--field key=value]

register --id <lease-id-or-slug> [--provider <provider>] [--target linux|macos|windows]
         [--windows-mode normal|wsl2] [--repo owner/name] [--name <runner-name>]
         [--labels <csv>] [--version latest] [--ephemeral=true] [--reclaim]

dispatch [--repo owner/name] [--workflow <file|name|id>] [--ref <ref>] [-f key=value] [--field key=value]
```

`hydrate` and `register` validate the current repo's lease claim before touching
the box. Pass `--reclaim` to intentionally move a lease to the current repo.
When local defaults point at another backend, repeat the same provider/target
routing flags used to create the lease.

### hydrate

Populates a lease's workspace from the configured workflow. Requires `--id` and
either `--workflow` or `actions.workflow`.

```sh
crabbox warmup
crabbox actions hydrate --id blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
```

Local hydration supports Linux and Windows WSL2 targets. With
`--github-runner`, native Windows is also supported. Static macOS hosts can run
commands through `provider=ssh`, but Actions hydration still requires Linux,
Windows WSL2, or native Windows with `--github-runner`.

Blacksmith Testbox IDs (`tbx_...`) and `--provider blacksmith-testbox` are
skipped, because Blacksmith owns Testbox hydration. Run commands against those
boxes directly with `crabbox run --provider blacksmith-testbox --id <tbx_id> -- ...`.

On success, `hydrate` prints a workspace summary and a total-duration line. Add
`--timing-json` to emit a final JSON timing record (stderr) with provider, lease
ID, slug, total duration, exit code, and — for the GitHub fallback — the Actions
run URL when the marker reports a run ID.

### register

Registers an existing box as a GitHub Actions self-hosted runner. Crabbox
obtains a repository registration token through `gh api`, installs the official
`actions/runner` package, and starts it under systemd on Linux/WSL2 or a
detached PowerShell process on native Windows. Supports Linux and Windows
targets only. Registration metadata and the short-lived token travel over SSH
stdin rather than the remote process command line.

```sh
crabbox actions register --id blue-lobster
```

Each runner gets the labels `crabbox`, the canonical lease label
`crabbox-<lease-id>`, profile/class labels, the slug label when available, and
any labels from `actions.runnerLabels` or `--labels`. Runner names use the
friendly slug when available; workflow inputs and state-file paths keep using the
canonical `cbx_...` ID.

### dispatch

Dispatches the configured workflow via `gh workflow run`. Requires `--workflow`
or `actions.workflow`.

```sh
crabbox actions dispatch -f testbox_id=cbx_abcdef123456
```

## Workflow inputs

Every `-f` / `--field` value must be `key=value`. CLI values override matching
`actions.fields` entries for that dispatch.

Crabbox inspects the selected workflow's `workflow_dispatch.inputs` (when the
workflow path is available under `.github/workflows/`). It sends only declared
inputs and requires `crabbox_id`, `crabbox_runner_label`, and
`crabbox_keep_alive_minutes`; `crabbox_job` is optional. If a GitHub dispatch
rejects `crabbox_job` as an unexpected input, Crabbox retries once without it so
older workflow refs stay usable.

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
setup actions: `actions/checkout`, `actions/setup-node`, `actions/setup-go`,
`actions/setup-python`, and `actions/cache/restore|save` (cache restore reports a
miss; save is skipped). Repo-local composite actions (`./path`) are supported.
Job containers and service containers are not; `actions/checkout` options that
change the repository, path, submodules, or LFS fail locally so you can rerun
with `--github-runner` when you need full GitHub Actions semantics.

Local hydration resolves simple expressions in env, run steps, working
directories, and supported action inputs: `${{ inputs.name }}`,
`${{ env.NAME }}`, `${{ github.workspace }}`, `${{ hashFiles(...) }}`, and
runner temp/toolcache references. Unsupported or complex expressions fail with a
suggestion to use `--github-runner`.

After checkout and setup, the workflow writes the ready marker under
`$HOME/.crabbox/actions/`. Map workflow inputs into step environment variables
such as `CRABBOX_ID` and `CRABBOX_JOB`; do not interpolate them directly into
shell source:

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
mkdir -p "$HOME/.crabbox/actions"
state="$HOME/.crabbox/actions/${CRABBOX_ID}.env"
env_file="$HOME/.crabbox/actions/${CRABBOX_ID}.env.sh"
services_file="$HOME/.crabbox/actions/${CRABBOX_ID}.services"
{
  echo "WORKSPACE=${GITHUB_WORKSPACE}"
  echo "RUN_ID=${GITHUB_RUN_ID}"
  echo "JOB=${job}"
  echo "ENV_FILE=${env_file}"
  echo "SERVICES_FILE=${services_file}"
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
`RUNNER_TOOL_CACHE` — never secrets or OIDC request tokens. Keep the workflow job
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
