# Cloud Run Sandbox Provider

Read when:

- choosing `provider: cloud-run-sandbox` (aliases: `gcrun-sandbox`,
  `google-cloud-run-sandbox`, `cloudrun-sandbox`);
- setting up Google Cloud Run sandboxes (preview) for Crabbox;
- configuring a remote gateway or the in-container `sandbox` CLI;
- changing `internal/providers/cloudrunsandbox`.

Cloud Run Sandbox is a delegated-run provider for
[Google Cloud Run sandboxes](https://docs.cloud.google.com/run/docs/code-execution)
(**public preview**). Sandboxes are lightweight isolated execution boundaries
that spawn **inside** an existing Cloud Run service instance after you enable
the sandbox launcher.

Crabbox owns provider selection, local claims, archive sync, slugs, timing,
cleanup, and list/status rendering. Google Cloud owns the isolation boundary
(no host env/metadata access, deny-by-default egress, tmpfs overlay).

This provider is **not** [GCP Compute Engine](gcp.md) (`provider: gcp`). Use
`gcp` for full VMs with Crabbox SSH + rsync. Use `cloud-run-sandbox` for
untrusted command execution inside Cloud Run isolation.

## Mental model

```text
Your laptop / CI                    Cloud Run service (gen2 + sandbox launcher)
-----------------                   ------------------------------------------
crabbox CLI  --HTTPS-->  gateway    app process
                         (optional)    |
                                       +--> /usr/local/gcp/bin/sandbox
                                            run / exec / do / delete
                                            (gVisor-isolated nested sandbox)
```

Important constraints from Google’s docs:

| Fact | Implication for Crabbox |
| --- | --- |
| Sandboxes only exist **inside** a Cloud Run revision with `sandboxLauncher: true` / `--sandbox-launcher` | A laptop cannot call `sandbox` directly; use a remote gateway or run Crabbox on Cloud Run |
| Sandboxes share the host container’s **CPU and memory** | Size the Cloud Run service for app + concurrent sandboxes; there is no separate sandbox SKU |
| Sandboxes do **not** inherit host env vars, secrets, or the metadata server | Forward only explicit env; never rely on workload identity inside the sandbox |
| Egress is **deny-by-default** | Set `allowEgress: true` only when the command needs the network |
| Rootfs is read-only unless `--write` / overlay / bind mounts | Crabbox archive sync needs `write: true` |
| Preview / Pre-GA terms apply | Expect limited support and possible project allow-listing |

Official references:

- [Code execution in Cloud Run](https://docs.cloud.google.com/run/docs/code-execution)
- [Configure sandboxes for services](https://docs.cloud.google.com/run/docs/configuring/services/sandboxes)
- [Sandbox CLI reference](https://docs.cloud.google.com/run/docs/reference/sandbox-cli)
- [Public preview announcement](https://cloud.google.com/blog/topics/developers-practitioners/google-cloud-run-sandboxes-are-in-public-preview)

## Choose a mode

| Mode | Who runs Crabbox | How commands execute | Best for |
| --- | --- | --- | --- |
| **Remote** | Laptop or CI | HTTP to a gateway that shells out to `sandbox` on Cloud Run | Maintainers, CI, local proof |
| **Direct** | Process already on Cloud Run | `Runtime.Exec` → `/usr/local/gcp/bin/sandbox` | Agents/services that already run on Cloud Run |

Remote mode is selected when `CLOUD_RUN_SANDBOX_URL` (or
`CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL`) **and** a secret are set. Otherwise
Crabbox uses direct mode.

## GCP setup (end-to-end)

### 1. Project, billing, CLI

```sh
gcloud auth login
gcloud auth application-default login   # optional; useful for other GCP tools

# Create a disposable project for experiments (recommended for preview)
gcloud projects create PROJECT_ID --name="Crabbox Cloud Run Sandbox"
gcloud config set project PROJECT_ID
gcloud billing projects link PROJECT_ID --billing-account=BILLING_ACCOUNT_ID

# Enable APIs used by deploy + image build
gcloud services enable \
  run.googleapis.com \
  artifactregistry.googleapis.com \
  cloudbuild.googleapis.com \
  containerregistry.googleapis.com
```

Install the `beta` component if `gcloud beta run` is missing:

```sh
gcloud components install beta
```

IAM needed to deploy (project Owner is enough for a personal project):

- `roles/run.developer` on the service
- `roles/iam.serviceAccountUser` on the runtime service account
- Cloud Build + Artifact Registry write for image builds

### 2. Enable the sandbox launcher on a service

Sandboxes force the **second-generation** execution environment. Deploy or
update with `--sandbox-launcher`:

```sh
# New service
gcloud beta run deploy SERVICE \
  --image=IMAGE_URL \
  --region=REGION \
  --sandbox-launcher \
  --no-cpu-throttling \
  --cpu=1 \
  --memory=2Gi

# Existing service
gcloud beta run services update SERVICE --sandbox-launcher
```

YAML equivalent (export, edit, replace):

```yaml
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: SERVICE
  annotations:
    run.googleapis.com/launch-stage: BETA
spec:
  template:
    spec:
      containers:
        - name: CONTAINER
          image: IMAGE_URL
          sandboxLauncher: true
```

Disable later with `--no-sandbox-launcher` if needed.

Inside that revision the binary is always:

```text
/usr/local/gcp/bin/sandbox
```

### 3. Remote gateway for laptop/CI

The sandbox CLI is **not** on your laptop. For Crabbox remote mode you need a
small HTTPS gateway that:

1. Authenticates callers (shared secret header; optional Cloud Run IAM token)
2. Proxies create/exec/destroy/writeFile to the in-container `sandbox` CLI

Two practical paths:

**A. ComputeSDK helper (quick start)**

```sh
export CLOUD_RUN_PROJECT_ID=PROJECT_ID
export CLOUD_RUN_REGION=us-central1
npx @computesdk/cloud-run
# prints CLOUD_RUN_SANDBOX_URL + CLOUD_RUN_SANDBOX_SECRET
```

See [`@computesdk/cloud-run`](https://github.com/computesdk/computesdk/tree/main/packages/cloud-run).

**B. Your own gateway image**

Build any container that implements the ComputeSDK-compatible routes Crabbox
uses:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/health` | Doctor reachability |
| POST | `/v1/sandbox/create` | `sandbox run <id> --detach` |
| POST | `/v1/sandbox/exec` | `sandbox exec <id> -- /bin/sh -c …` |
| POST | `/v1/sandbox/destroy` | `sandbox delete <id> --force` |
| POST | `/v1/sandbox/writeFile` | File write for archive sync |

Auth header: `X-ComputeSDK-Cloud-Run-Secret: <secret>` (optional
`Authorization: Bearer <identity-token>` when the service requires IAM).

Example deploy sketch:

```sh
# Build/push image to Artifact Registry, then:
gcloud beta run deploy crabbox-sandbox-gateway \
  --image=REGION-docker.pkg.dev/PROJECT_ID/REPO/gateway:latest \
  --region=REGION \
  --sandbox-launcher \
  --no-cpu-throttling \
  --cpu=1 \
  --memory=2Gi \
  --concurrency=1 \
  --allow-unauthenticated \
  --set-env-vars=SANDBOX_SECRET=LONG_RANDOM_SECRET
```

If org policy blocks `allUsers` invoker, keep the service private and set
`CLOUD_RUN_AUTH_TOKEN` to a Google-signed identity token for the service URL.

### 4. Point Crabbox at the gateway

```sh
export CLOUD_RUN_SANDBOX_URL="https://SERVICE-….run.app"
export CLOUD_RUN_SANDBOX_SECRET="…"
# optional IAM:
# export CLOUD_RUN_AUTH_TOKEN="$(gcloud auth print-identity-token --audiences="$CLOUD_RUN_SANDBOX_URL")"

crabbox doctor --provider cloud-run-sandbox
crabbox run --provider cloud-run-sandbox -- echo ok
```

Crabbox aliases:

```text
CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL
CRABBOX_CLOUD_RUN_SANDBOX_SECRET
CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN
```

**Never** put the secret or gateway URL in repository YAML. Gateway URL is
flag/env only so a checked-in config cannot redirect a local secret.

### 5. Direct mode (Crabbox on Cloud Run)

When Crabbox (or a process that invokes it) already runs in a
`--sandbox-launcher` service and no gateway URL is set:

```yaml
provider: cloud-run-sandbox
cloudRunSandbox:
  cliPath: /usr/local/gcp/bin/sandbox
  workdir: /tmp/crabbox
  write: true
  allowEgress: false
```

Crabbox shells out to the local CLI. Ensure the service image includes any
tools your commands need (`python3`, `node`, package managers, etc.).

## Native sandbox CLI (what Google mounts)

```sh
# One-shot ephemeral
sandbox do -- <COMMAND>
sandbox do -e KEY=VALUE -- <COMMAND>
sandbox do --allow-egress -- curl -sI https://example.com
sandbox do --write --export-tar=/tmp/work.tar -- /bin/bash -c 'echo hi > /tmp/x'

# Stateful (what Crabbox uses for leases)
sandbox run <id> --detach --write --workdir=/tmp/crabbox
sandbox exec <id> --workdir=/tmp/crabbox -- /bin/sh -c 'uname -a'
sandbox delete <id> --force

# Full help
/usr/local/gcp/bin/sandbox -h
```

Crabbox maps:

| Crabbox | CLI / gateway |
| --- | --- |
| `warmup` / create lease | `sandbox run <id> --detach` |
| `run` command | `sandbox exec <id> -- /bin/sh -c …` |
| `stop` / cleanup | `sandbox delete <id> --force` |
| archive sync | writeFile + extract into `workdir` (needs write) |

## Commands

```sh
crabbox doctor --provider cloud-run-sandbox
crabbox warmup --provider cloud-run-sandbox --slug live-smoke
crabbox run --provider cloud-run-sandbox -- echo ok
crabbox run --provider cloud-run-sandbox --id live-smoke -- uname -a
crabbox list --provider cloud-run-sandbox --json
crabbox status --provider cloud-run-sandbox --id live-smoke
crabbox stop --provider cloud-run-sandbox live-smoke
crabbox cleanup --provider cloud-run-sandbox   # idle-expired claims
```

## Config

```yaml
provider: cloud-run-sandbox
target: linux
cloudRunSandbox:
  cliPath: /usr/local/gcp/bin/sandbox
  workdir: /tmp/crabbox
  allowEgress: false   # Google default: deny outbound network
  write: true          # required for archive sync into the overlay
  rootfs: /
  mode: ""             # optional: local | container
```

Provider flags:

```text
--cloud-run-sandbox-gateway-url
--cloud-run-sandbox-cli
--cloud-run-sandbox-workdir
--cloud-run-sandbox-allow-egress
--cloud-run-sandbox-write
--cloud-run-sandbox-rootfs
--cloud-run-sandbox-mode
```

Environment overrides:

```text
CLOUD_RUN_SANDBOX_URL / CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL
CLOUD_RUN_SANDBOX_SECRET / CRABBOX_CLOUD_RUN_SANDBOX_SECRET
CLOUD_RUN_AUTH_TOKEN / CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN
CLOUD_RUN_SANDBOX_BINARY / CRABBOX_CLOUD_RUN_SANDBOX_CLI
CRABBOX_CLOUD_RUN_SANDBOX_WORKDIR
CRABBOX_CLOUD_RUN_SANDBOX_ALLOW_EGRESS
CRABBOX_CLOUD_RUN_SANDBOX_WRITE
CRABBOX_CLOUD_RUN_SANDBOX_ROOTFS
CRABBOX_CLOUD_RUN_SANDBOX_MODE
```

## Lifecycle

1. `warmup` or `run` without `--id` creates a stateful sandbox and a local claim
   `gcrs_<sandbox-id>` scoped to the gateway URL hash or direct CLI path.
2. Unless `--no-sync`, Crabbox archive-syncs into `cloudRunSandbox.workdir`.
3. Commands run via `sandbox exec` (or gateway `/v1/sandbox/exec`). Selected
   env is forwarded explicitly.
4. `list` / `status` / `stop` only touch Crabbox-owned claims.
5. One-shot `run` without `--keep` destroys the sandbox after the command.
6. `cleanup` deletes idle-expired claimed sandboxes.

## Doctor

`crabbox doctor --provider cloud-run-sandbox` is non-mutating:

- remote: `GET /v1/health` when available
- direct: `sandbox --help` on the configured CLI path
- local claim inventory

| Symptom | Action |
| --- | --- |
| Missing secret with gateway URL set | Export `CLOUD_RUN_SANDBOX_SECRET` |
| CLI not found in direct mode | Deploy with `--sandbox-launcher` or set `cliPath` |
| Unauthorized gateway | Check secret; set `CLOUD_RUN_AUTH_TOKEN` if IAM requires it |
| Build push denied (Artifact Registry) | Grant `roles/artifactregistry.writer` to the Cloud Build / compute SA |
| Sandbox create fails with binary missing | Revision lacks `--sandbox-launcher` / `sandboxLauncher: true` |
| Preview / allow-list errors | Confirm project access for Cloud Run sandboxes preview |

## Capabilities

- Target: Linux.
- Kind: delegated-run.
- Coordinator: never.
- Features: `archive-sync`, `cleanup`, `run-session`.
- Aliases: `gcrun-sandbox`, `google-cloud-run-sandbox`, `cloudrun-sandbox`.
- SSH, desktop, browser, code-server, Tailscale, Actions hydration, and
  Crabbox rsync are not supported.
- `--class` / `--type` are rejected; sandboxes share the parent service CPU/RAM.

## Safety

- Sandboxes do not see Cloud Run service env vars or the metadata server.
- Egress is deny-by-default; opt in only when needed.
- Writable state is an isolated overlay unless you bind-mount or export tar.
- Secrets stay in env + request headers only (never Crabbox config or argv).
- Local claims are scoped so list/stop cannot cross gateways by accident.
- Prefer private gateway + IAM when the service is not disposable.

## Cost notes

There is no separate “sandbox” charge. You pay for the Cloud Run service’s
allocated CPU/memory while instances run (and for build/storage). Size
CPU/memory for concurrent sandboxes; use concurrency 1 if each request runs a
heavy sandbox.

## Live smoke

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=cloud-run-sandbox scripts/live-smoke.sh
# or
scripts/live-cloud-run-sandbox-smoke.sh
```

## Tear down a proof project

```sh
gcloud run services delete SERVICE --region=REGION --quiet
gcloud artifacts repositories delete REPO --location=REGION --quiet
gcloud projects delete PROJECT_ID --quiet
gcloud auth revoke --all
gcloud auth application-default revoke 2>/dev/null || true
```

Related docs:

- [Provider reference](README.md)
- [GCP Compute Engine](gcp.md)
- [Sandbox CLI reference](https://docs.cloud.google.com/run/docs/reference/sandbox-cli)
