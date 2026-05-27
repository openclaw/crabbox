# Live Provider E2E

`.github/workflows/live-provider-e2e.yml` is a maintainer-run workflow for
proving Crabbox against every built-in provider from GitHub Actions.

The workflow is intentionally manual (`workflow_dispatch`) because it can create
billable provider resources. It builds the current checkout, fans out one job
per selected provider, and uploads a per-provider log artifact. Missing secrets
are skipped by default so maintainers can add providers incrementally.

## Running It

1. Open **Actions > Live Provider E2E > Run workflow**.
2. Use `providers=all` to run the full matrix, or pass a comma-separated subset
   such as `aws,hetzner,e2b,cloudflare`.
3. Leave `allow_missing=true` while bringing secrets online. Set it to `false`
   when the repository should fail if any selected provider is not configured.
4. Leave `sync_checkout=false` for the fastest smoke. Set it to `true` when a
   change needs to exercise Crabbox file sync as part of the provider run.
5. Keep `runner_label=ubuntu-latest` for hosted Linux smoke tests, or set it to
   a self-hosted runner label for providers that require local tools or private
   network access.

The smoke command is intentionally small: create or resolve a provider lease or
sandbox, run `echo crabbox-<provider>-e2e-ok`, print basic runtime context, and
clean up where the provider supports cleanup.

## Required Secrets

Add these under **Settings > Secrets and variables > Actions**. Values that are
not actually sensitive can still be stored as secrets to keep setup uniform.

| Provider | Required GitHub secrets |
| --- | --- |
| Brokered AWS/Azure/GCP/Hetzner | `CRABBOX_COORDINATOR`, `CRABBOX_COORDINATOR_TOKEN` |
| AWS direct | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN`, `AWS_REGION` or `CRABBOX_AWS_REGION` |
| Azure direct | `AZURE_SUBSCRIPTION_ID`, `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, optional `CRABBOX_AZURE_LOCATION`, `CRABBOX_AZURE_RESOURCE_GROUP` |
| GCP direct | `GCP_SERVICE_ACCOUNT_JSON`, `GOOGLE_CLOUD_PROJECT` or `GCP_PROJECT_ID`, optional `CRABBOX_GCP_ZONE` |
| Hetzner direct | `HCLOUD_TOKEN` or `HETZNER_TOKEN` |
| Proxmox | `CRABBOX_PROXMOX_API_URL`, `CRABBOX_PROXMOX_TOKEN_ID`, `CRABBOX_PROXMOX_TOKEN_SECRET`, `CRABBOX_PROXMOX_NODE`, `CRABBOX_PROXMOX_TEMPLATE_ID`, optional storage, bridge, user, and TLS secrets |
| Parallels | `CRABBOX_PARALLELS_TEMPLATE` or `CRABBOX_PARALLELS_SOURCE` plus `CRABBOX_PARALLELS_SOURCE_SNAPSHOT`; remote hosts also need `CRABBOX_PARALLELS_HOST`, `CRABBOX_PARALLELS_HOST_USER`, and `CRABBOX_SSH_PRIVATE_KEY` |
| Local Container | No provider secret; the runner must have Docker available |
| Static SSH | `CRABBOX_STATIC_HOST`, optional `CRABBOX_STATIC_USER`, `CRABBOX_STATIC_PORT`, `CRABBOX_STATIC_WORK_ROOT`, `CRABBOX_TARGET`, `CRABBOX_WINDOWS_MODE`, and `CRABBOX_SSH_PRIVATE_KEY` |
| exe.dev | `CRABBOX_SSH_PRIVATE_KEY` for the SSH identity accepted by exe.dev; optional `CRABBOX_EXE_DEV_CONTROL_HOST`, `CRABBOX_EXE_DEV_IMAGE` |
| Blacksmith Testbox | `CRABBOX_BLACKSMITH_ORG`, `CRABBOX_BLACKSMITH_WORKFLOW`, optional `CRABBOX_BLACKSMITH_JOB`, `CRABBOX_BLACKSMITH_REF`, plus the Blacksmith CLI auth secret expected by the runner |
| Namespace Devbox | Optional `CRABBOX_NAMESPACE_IMAGE`, `CRABBOX_NAMESPACE_SIZE`, `CRABBOX_NAMESPACE_REPOSITORY`, `CRABBOX_NAMESPACE_SITE`; the runner must have an authenticated `devbox` CLI |
| Semaphore | `CRABBOX_SEMAPHORE_HOST`, `CRABBOX_SEMAPHORE_PROJECT`, `CRABBOX_SEMAPHORE_TOKEN` |
| Sprites | `CRABBOX_SPRITES_TOKEN`; the runner must have the `sprite` CLI |
| Daytona | `DAYTONA_API_KEY` or `DAYTONA_JWT_TOKEN`, `CRABBOX_DAYTONA_SNAPSHOT`; JWT auth also needs `DAYTONA_ORGANIZATION_ID` |
| Islo | `ISLO_API_KEY` |
| E2B | `CRABBOX_E2B_API_KEY` or `E2B_API_KEY` |
| Modal | `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET` |
| Upstash Box | `CRABBOX_UPSTASH_BOX_API_KEY` or `UPSTASH_BOX_API_KEY` |
| Tensorlake | `CRABBOX_TENSORLAKE_API_KEY` or `TENSORLAKE_API_KEY`, optional `TENSORLAKE_ORGANIZATION_ID`, `TENSORLAKE_PROJECT_ID` |
| Cloudflare | `CRABBOX_CLOUDFLARE_RUNNER_URL`, `CRABBOX_CLOUDFLARE_RUNNER_TOKEN` |
| Railway | `CRABBOX_RAILWAY_API_TOKEN` or `RAILWAY_API_TOKEN`, `CRABBOX_RAILWAY_SERVICE_ID`, `CRABBOX_RAILWAY_PROJECT_ID`, `CRABBOX_RAILWAY_ENVIRONMENT_ID` |
| RunPod | `CRABBOX_RUNPOD_API_KEY` or `RUNPOD_API_KEY`; the RunPod account must already trust the SSH public key matching `CRABBOX_SSH_PRIVATE_KEY` |
| W&B Sandboxes | `CRABBOX_WANDB_API_KEY` or `WANDB_API_KEY`, `WANDB_ENTITY_NAME`, optional `WANDB_PROJECT` |

## Notes

- The workflow installs `modal` and `tensorlake` Python packages for those
  providers. Other providers that rely on a local CLI need that CLI available on
  the selected runner before the job starts.
- `GCP_SERVICE_ACCOUNT_JSON` is written to a temporary file and exposed through
  `GOOGLE_APPLICATION_CREDENTIALS`.
- `CRABBOX_SSH_PRIVATE_KEY` is written to a temporary `0600` file and exposed
  as `CRABBOX_SSH_KEY`.
- Railway is a redeploy-and-stream provider. Its smoke redeploys the configured
  existing service rather than executing an arbitrary shell command inside it.
- The workflow never runs on pull requests automatically, so forked PRs cannot
  access provider secrets or create provider resources.

Related docs:

- [Providers](providers.md)
- [Provider reference](../providers/README.md)
- [Security](../security.md)
