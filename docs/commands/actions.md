# actions

`crabbox actions` bridges a leased Crabbox machine into real GitHub Actions.

It uses GitHub's runner and workflow APIs:

- `actions register` gets a repository runner registration token through `gh api`, installs the official `actions/runner` package on an existing box, and starts it with systemd.
- `actions hydrate` registers the runner, dispatches the configured workflow with the canonical lease label, waits for the workflow to write the hydrated workspace marker, and then returns.
- `actions dispatch` calls `gh workflow run` for the configured workflow.

For `actions hydrate`, Crabbox inspects the selected workflow's `workflow_dispatch.inputs` when the workflow path is available under `.github/workflows/`. It only sends declared inputs, requires `crabbox_id`, `crabbox_runner_label`, and `crabbox_keep_alive_minutes`, and treats `crabbox_job` as optional. If GitHub still rejects `crabbox_job` as an unexpected input, Crabbox retries once without it so older workflow refs remain usable.

Runner names and extra labels use the friendly slug when available, but workflow inputs and state-file paths keep using the canonical `cbx_...` ID.

Runner registration supports Linux and Windows WSL2 targets. Static macOS and
native Windows hosts can run commands through `provider=ssh`, but `actions
hydrate` and `actions register` still install the Linux GitHub Actions runner
package.

On success, `actions hydrate` prints a concise total duration line. Add `--timing-json` to emit a final JSON timing record with provider, lease ID, slug, total duration, exit code, and the GitHub Actions run URL when the workflow marker reports a run ID.

```sh
crabbox warmup --actions-runner
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox actions hydrate --provider aws --target windows --windows-mode wsl2 --id blue-lobster
crabbox actions register --id blue-lobster
crabbox actions dispatch -f testbox_id=cbx_abcdef123456
crabbox run --id blue-lobster -- pnpm test
```

Subcommands:

```text
hydrate --id <lease-id-or-slug> [--provider <provider>] [--target linux|macos|windows] [--windows-mode normal|wsl2] [--repo owner/name] [--workflow <file|name|id>] [--job <name>] [--ref <ref>] [--wait-timeout 20m] [--keep-alive-minutes 90] [--reclaim] [--timing-json] [-f key=value] [--field key=value]
register --id <lease-id-or-slug> [--provider <provider>] [--target linux|macos|windows] [--windows-mode normal|wsl2] [--repo owner/name] [--name <runner-name>] [--labels <csv>] [--version latest] [--ephemeral=true] [--reclaim]
dispatch [--repo owner/name] [--workflow <file|name|id>] [--ref <ref>] [-f key=value] [--field key=value]
```

Hydrate/register validate the local repo claim before touching the lease. Use `--reclaim` when intentionally moving a lease to the current repo.
Pass the same provider/target routing flags used to create the lease when local defaults point at another backend.

Config:

```yaml
actions:
  repo: owner/name
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

Workflow jobs should target the dynamic label printed by registration, for example `crabbox-cbx-123`, plus any static labels configured for the project.
When `actions.job` is set and the workflow declares `crabbox_job`, Crabbox sends it and verifies that the ready marker came from that job. Older workflows can omit both.
Use `actions.fields` for repository-specific workflow inputs that should be sent on every hydration. CLI `-f key=value` / `--field key=value` values override matching configured fields for that dispatch.

## Hydration Flow

Use hydration when CI already knows how to prepare the repository and an agent needs a fast local-style loop:

```sh
crabbox warmup
crabbox actions hydrate --id blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
```

The Actions workflow owns repository-specific setup: checkout, dependency install, services, caches, secrets, and any project tools. Crabbox only registers the runner, dispatches the workflow, waits for the marker, and later syncs local edits into the marked workspace. There is no project-specific setup code in the Crabbox binary.

Hydrate workflows must accept these inputs:

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

The hydrate job should run on the dynamic label:

```yaml
runs-on: [self-hosted, "${{ inputs.crabbox_runner_label }}"]
```

After checkout and dependency/service setup, the workflow writes the ready marker:

```sh
mkdir -p "$HOME/.crabbox/actions"
state="$HOME/.crabbox/actions/${{ inputs.crabbox_id }}.env"
env_file="$HOME/.crabbox/actions/${{ inputs.crabbox_id }}.env.sh"
services_file="$HOME/.crabbox/actions/${{ inputs.crabbox_id }}.services"
{
  echo "WORKSPACE=${GITHUB_WORKSPACE}"
  echo "RUN_ID=${GITHUB_RUN_ID}"
  echo "JOB=${{ inputs.crabbox_job }}"
  echo "ENV_FILE=${env_file}"
  echo "SERVICES_FILE=${services_file}"
  echo "READY_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "${state}.tmp"
mv "${state}.tmp" "$state"
```

`crabbox run --id <lease-id-or-slug>` reads that marker, syncs into the hydrated `$GITHUB_WORKSPACE`, and sources the non-secret env file when present. The env file should contain stable GitHub/runner context such as `GITHUB_WORKSPACE`, `GITHUB_RUN_ID`, `RUNNER_TEMP`, and `RUNNER_TOOL_CACHE`; do not persist secrets or OIDC request tokens there. Keep the workflow job alive when service containers or job-scoped setup must remain running for the remote command loop. `crabbox stop <lease-id-or-slug>` writes the `.stop` marker before releasing the box.
