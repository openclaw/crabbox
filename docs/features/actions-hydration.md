# Actions Hydration

Read when:

- wiring Crabbox into an existing GitHub Actions CI setup;
- changing `crabbox actions hydrate`;
- deciding whether setup belongs in Crabbox or in a repository workflow.

Actions hydration lets a repository reuse its existing GitHub Actions setup without putting repository-specific setup code in the Crabbox binary.

Runner registration supports POSIX targets: brokered Hetzner/AWS/Azure/GCP
Linux, direct Proxmox Linux, and AWS Windows WSL2. Static macOS and native Windows targets are for direct
`crabbox run` loops until platform-specific runner installation is added.

The flow:

1. `crabbox warmup` leases a machine and prints both `cbx_...` and a friendly slug.
2. `crabbox actions hydrate --id blue-lobster` registers that machine as an ephemeral self-hosted runner for the repository.
3. Crabbox inspects the configured workflow's `workflow_dispatch.inputs` when it can read the workflow path, then dispatches it with the lease ID, dynamic runner label, keepalive timeout, and optional expected hydrate job.
4. The workflow runs on `[self-hosted, crabbox-cbx-...]`; the runner also carries a readable slug label such as `crabbox-blue-lobster`.
5. The workflow writes `$HOME/.crabbox/actions/<lease>.env` with `WORKSPACE`, `RUN_ID`, `JOB`, `ENV_FILE`, `SERVICES_FILE`, and `READY_AT`.
6. `crabbox run --id blue-lobster -- <command>` reads that marker, syncs the local dirty checkout into `$GITHUB_WORKSPACE`, and sources the non-secret env file when present.

The important boundary: project setup lives in the repository workflow. Crabbox owns runner registration, dispatch, marker waiting, SSH sync, and command execution. It does not contain repository-specific setup code.

Input compatibility:

- `crabbox_id`, `crabbox_runner_label`, and `crabbox_keep_alive_minutes` must be declared when Crabbox can inspect the workflow.
- `crabbox_job` is optional. Crabbox sends it only when the workflow declares it, or retries once without it when GitHub reports an unexpected input.
- Extra `-f key=value` fields are sent only when the inspected workflow declares those inputs.

Repo config:

```yaml
actions:
  workflow: .github/workflows/crabbox.yml
  job: hydrate
  ref: main
  runnerLabels:
    - crabbox
  runnerVersion: latest
  ephemeral: true
```

Hydrate workflows must accept:

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

The job should run on the dynamic label:

```yaml
runs-on: [self-hosted, "${{ inputs.crabbox_runner_label }}"]
```

The workflow marks readiness after setup:

```sh
mkdir -p "$HOME/.crabbox/actions"
state="$HOME/.crabbox/actions/${{ inputs.crabbox_id }}.env"
env_file="$HOME/.crabbox/actions/${{ inputs.crabbox_id }}.env.sh"
services_file="$HOME/.crabbox/actions/${{ inputs.crabbox_id }}.services"
tmp="${state}.tmp"
{
  echo "WORKSPACE=${GITHUB_WORKSPACE}"
  echo "RUN_ID=${GITHUB_RUN_ID}"
  echo "JOB=${{ inputs.crabbox_job }}"
  echo "ENV_FILE=${env_file}"
  echo "SERVICES_FILE=${services_file}"
  echo "READY_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$tmp"
mv "$tmp" "$state"
```

The env file should contain only stable, non-secret context that SSH commands need, such as `GITHUB_WORKSPACE`, `GITHUB_RUN_ID`, `RUNNER_TEMP`, and `RUNNER_TOOL_CACHE`. Secrets and OIDC request tokens are step-scoped GitHub material and should stay inside the hydration workflow unless the project intentionally persists its own short-lived credentials.

The final workflow step should keep the job alive while agents run commands. It can exit when `$HOME/.crabbox/actions/${{ inputs.crabbox_id }}.stop` appears or when its timeout expires. `crabbox stop` and non-kept `crabbox run` leases write that stop marker before releasing the machine.

Related docs:

- [actions command](../commands/actions.md)
- [run command](../commands/run.md)
- [warmup command](../commands/warmup.md)
- [Repository onboarding](repository-onboarding.md)
