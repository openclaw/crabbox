# Actions Hydration

Read this when:

- wiring Crabbox into an existing GitHub Actions CI setup;
- changing `crabbox actions hydrate`;
- deciding whether toolchain/dependency setup belongs in Crabbox or in a repository workflow.

Actions hydration prepares a lease's workspace by reusing the repository's own
GitHub Actions setup, so repo-specific provisioning (toolchains, dependencies,
build caches) runs the same way CI runs it — without baking any of that setup
into the Crabbox binary. Crabbox owns workflow translation, runner registration,
marker waiting, SSH sync, and command execution. The project owns its setup
steps.

There are two hydration paths:

- **Local hydration (default)** — Crabbox reads the repo-local workflow under
  `.github/workflows/`, translates the supported steps, and executes them over
  SSH in the persistent workspace. No GitHub repository write access required.
- **GitHub-runner hydration (`--github-runner`)** — Crabbox registers the box as
  an ephemeral self-hosted runner, dispatches the workflow on GitHub, and waits
  for the readiness marker. Use this when the workflow needs full GitHub Actions
  semantics: repository secrets, OIDC, service containers, job containers, or
  `uses:` steps the local path does not support.

Both paths converge on the same readiness marker, so later `crabbox run`
commands attach to the hydrated workspace identically.

## Supported targets

| Path | Targets |
| --- | --- |
| Local hydration | Linux; Windows WSL2 |
| GitHub-runner hydration | Linux; Windows (native and WSL2) |

Static macOS and other unsupported targets stay on a direct `crabbox run` loop
(supply setup in the command or a prebaked image) until platform-specific
hydration lands. Blacksmith leases skip hydration entirely; that provider owns
its own workspace.

## The flow

1. `crabbox warmup` leases a box and prints both the `cbx_...` id and a friendly
   slug (for example `swift-crab`).
2. `crabbox run --id swift-crab -- <command>` syncs the dirty checkout.
3. If no readiness marker exists yet and `actions.workflow` is configured, `run`
   hydrates locally first — unless `--no-hydrate`, `--no-sync`, `--sync-only`,
   or `--fresh-pr` is set, or the target does not support local hydration.
4. Hydration writes `$HOME/.crabbox/actions/<lease>.env` containing `WORKSPACE`,
   `RUN_ID`, `JOB`, `ENV_FILE`, `SERVICES_FILE`, and `READY_AT`.
5. The command runs inside the workspace and sources the non-secret env file
   (`ENV_FILE`) when present.

You can also hydrate explicitly: `crabbox actions hydrate --id <id>` syncs the
current checkout and then runs the hydrate workflow. Automatic hydration during
`crabbox run` reuses the run's own sync rather than syncing twice.

## Local hydration details

For local hydration Crabbox picks the workflow job to run in this order:

1. the job keyed `hydrate`;
2. the job matching `actions.job` / `--job`, if set;
3. the only job in a single-job workflow.

`actions.job` / `--job` doubles as the `crabbox_job` workflow input and the
marker verifier; it does **not** have to match the YAML job key.

Local hydration translates a constrained subset of workflow syntax. It resolves
simple `${{ ... }}` expressions — `inputs.*`, `env.*`, `github.workspace`,
`github.ref`, `github.ref_name`, `github.sha`, `github.run_id`, `runner.temp`,
`runner.tool_cache`, `hashFiles(...)`, and prior `steps.<id>.outputs.<name>` —
and simple `if:` comparisons (`==`/`!=`, joined with `&&`). Anything more complex
fails with a clear error suggesting `--github-runner`.

Supported `uses:` steps:

- `actions/checkout@*` — satisfied by Crabbox sync / git seed (only the default
  checkout of the current repo; `path`, `ref`, `fetch-depth`,
  `persist-credentials`, `set-safe-directory`, and disabled `submodules`/`lfs`
  are accepted, anything else needs `--github-runner`);
- `actions/setup-node@*`, `actions/setup-go@*`, `actions/setup-python@*` —
  honoring `*-version` / `*-version-file`;
- `actions/cache/restore@*` — reports a cache miss;
- `actions/cache/save@*` — skipped;
- repo-local composite actions (`./...`).

Job containers and service containers are not supported locally; those require
`--github-runner`.

## `crabbox actions hydrate`

```sh
crabbox actions hydrate --id swift-crab --workflow .github/workflows/crabbox.yml
```

Key flags (`actions.*` config keys provide defaults; see [Repo config](#repo-config)):

| Flag | Default | Purpose |
| --- | --- | --- |
| `--id` | _(required)_ | Lease id or slug to hydrate. |
| `--workflow` | `actions.workflow` | Workflow file/name/id (required if not configured). |
| `--job` | `actions.job` | Expected hydrate job / `crabbox_job` input. |
| `--ref` | `actions.ref` | Workflow ref. |
| `--repo` | `actions.repo` | GitHub `owner/name` (GitHub-runner path only). |
| `--github-runner` | `false` | Use the GitHub self-hosted runner path instead of local SSH. |
| `--wait-timeout` | `20m` | How long to wait for the readiness marker. |
| `--keep-alive-minutes` | `90` | Minutes the GitHub-runner job keeps itself alive. |
| `-f`, `--field` `key=value` | — | Extra workflow inputs (repeatable). |
| `--reclaim` | `false` | Claim the lease for the current repo. |
| `--timing-json` | `false` | Print final timing as JSON. |

Every `-f` field must be `key=value`; malformed fields fail before any setup
runs. Inputs the inspected workflow does not declare are dropped with a warning.
For local hydration Crabbox sends `crabbox_keep_alive_minutes=0` so the
keep-alive step exits immediately after writing the marker.

Related commands: `crabbox actions register` (register a Linux/Windows lease as a
GitHub Actions runner) and `crabbox actions dispatch` (dispatch a workflow
without hydrating). See the [actions command reference](../commands/actions.md).

## Input compatibility

- `crabbox_id`, `crabbox_runner_label`, and `crabbox_keep_alive_minutes` must be
  declared by the workflow whenever Crabbox can inspect it.
- `crabbox_job` is optional. Crabbox sends it only when the workflow declares it,
  and on the GitHub-runner path retries once without it if GitHub reports an
  unexpected input. It identifies the readiness marker, not the YAML job key.
- Extra `-f key=value` fields are sent only when the inspected workflow declares
  those inputs.
- Workflow input `default:` values are applied locally; a declared, defaulted,
  but unsupplied required input fails before setup starts.

## Repo config

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

`crabbox init` scaffolds a compatible workflow; see
[Repository onboarding](repository-onboarding.md).

## Authoring a hydrate workflow

The workflow must accept the Crabbox inputs:

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

The hydrate job runs on the dynamic runner label (GitHub-runner path):

```yaml
runs-on: [self-hosted, "${{ inputs.crabbox_runner_label }}"]
```

After setup, write the readiness marker. Map workflow inputs into step
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
mkdir -p "$HOME/.crabbox/actions"
state="$HOME/.crabbox/actions/${CRABBOX_ID}.env"
env_file="$HOME/.crabbox/actions/${CRABBOX_ID}.env.sh"
services_file="$HOME/.crabbox/actions/${CRABBOX_ID}.services"
tmp="${state}.tmp"
{
  echo "WORKSPACE=${GITHUB_WORKSPACE}"
  echo "RUN_ID=${GITHUB_RUN_ID}"
  echo "JOB=${job}"
  echo "ENV_FILE=${env_file}"
  echo "SERVICES_FILE=${services_file}"
  echo "READY_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$tmp"
mv "$tmp" "$state"
```

The env file (`ENV_FILE`) should hold only stable, non-secret context that later
SSH commands need — `GITHUB_WORKSPACE`, `GITHUB_RUN_ID`, `GITHUB_JOB`,
`RUNNER_TEMP`, `RUNNER_TOOL_CACHE`, and similar. Secrets and OIDC request tokens
are step-scoped GitHub material and should stay inside the workflow unless the
project intentionally persists its own short-lived credentials.

The final step should keep the job alive while later commands run, exiting when
`$HOME/.crabbox/actions/${CRABBOX_ID}.stop` appears or when its timeout expires.
Map `CRABBOX_ID` and `CRABBOX_KEEP_ALIVE_MINUTES` through step environment:

```sh
case "$CRABBOX_ID" in
  ""|*[!A-Za-z0-9_-]*)
    echo "::error::crabbox_id must match [A-Za-z0-9_-]+"
    exit 2
    ;;
esac
minutes="$CRABBOX_KEEP_ALIVE_MINUTES"
case "$minutes" in ''|*[!0-9]*) minutes=90 ;; esac
stop="$HOME/.crabbox/actions/${CRABBOX_ID}.stop"
deadline=$(( $(date +%s) + minutes * 60 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  [ -f "$stop" ] && exit 0
  sleep 15
done
```

`crabbox stop` and non-kept `crabbox run` leases write the stop marker before
releasing the box. On the local path `crabbox_keep_alive_minutes=0` makes this
step exit immediately, so keep the long-running loop primarily for
`--github-runner` hydration where services or job-scoped setup must stay live.

## Related docs

- [actions command](../commands/actions.md)
- [run command](../commands/run.md)
- [warmup command](../commands/warmup.md)
- [Repository onboarding](repository-onboarding.md)
- [jobs](jobs.md)
