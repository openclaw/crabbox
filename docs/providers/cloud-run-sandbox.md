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
| **Remote** | Laptop or CI | HTTP to a durable-routing gateway that owns the stateful lifecycle | Maintainers, CI, local proof |
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
  containerregistry.googleapis.com \
  secretmanager.googleapis.com
```

Install the `beta` component if `gcloud beta run` is missing:

```sh
gcloud components install beta
```

IAM needed to deploy (project Owner is enough for a personal project):

- `roles/run.developer` on the service
- `roles/iam.serviceAccountUser` on the runtime service account
- Cloud Build + Artifact Registry write for image builds
- The public reference deployment also needs `run.services.setIamPolicy`
  (included in `roles/run.admin`) to apply `--allow-unauthenticated`. For a
  least-privilege private gateway, omit that flag and use per-command IAM tokens.

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
2. Durably binds the supplied ownership token to the created sandbox and routes
   every request for a sandbox ID to the instance that owns it
3. Runs create/exec/destroy/writeFile through the in-container `sandbox` CLI
4. Returns from destroy only after deletion is confirmed

Cloud Run session affinity alone is not a durable router: affinity is
cookie-based and best-effort, so the owning instance can change. The gateway
must maintain authoritative sandbox-ID routing outside one instance and must
not acknowledge deletion before `sandbox delete` has completed.

The [`@computesdk/cloud-run`](https://github.com/computesdk/computesdk/tree/main/packages/cloud-run)
package is a useful protocol and deployment reference, but its stock gateway
does not currently provide Crabbox's durable-routing and confirmed-destroy
contract. Do not point Crabbox at the stock helper for stateful leases.

Build a gateway image that implements the following routes:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/health` | Authenticated readiness and lifecycle guarantees |
| POST | `/v1/sandbox/create` | `sandbox run <id> --detach -- /bin/sh -c <idle-loop>` |
| POST | `/v1/sandbox/status` | Confirm the exact owned sandbox is still running |
| POST | `/v1/sandbox/exec` | `sandbox exec <id> -- /bin/sh -c …` |
| POST | `/v1/sandbox/destroy` | `sandbox delete <id> --force` |
| POST | `/v1/sandbox/writeFile` | File write for archive sync |

Auth header: `X-ComputeSDK-Cloud-Run-Secret: <secret>` (optional
`Authorization: Bearer <identity-token>` when the service requires IAM).

Successful health and create responses must include the lifecycle contract:

```json
{
  "status": "ok",
  "lifecycle": {
    "routing": "durable",
    "destroy": "synchronous",
    "exec": "ndjson-stream"
  }
}
```

Create additionally receives and returns the exact `sandboxId`,
`ownershipToken`, and `status: "running"`. The gateway must atomically bind the
token to a newly created sandbox and must return HTTP 409 rather than overwrite
an existing binding.
Status receives the exact `sandboxId` and `ownershipToken` and returns both with
`status: "running"` and `success: true`; missing sandboxes use the structured
404 contract below. Exec responds as `application/x-ndjson`: bounded stdout or
stderr frames carry the exact `sandboxId`, `stream`, and `data`, followed by one
terminal frame with the exact `sandboxId`, `status: "completed"`,
`success: true`, and an explicit integer `exitCode`. A missing, duplicate, or
failed terminal frame is rejected. WriteFile accepts an `append` boolean so
Crabbox can stream base64 archive data in bounded chunks and returns the exact
`sandboxId`, `status: "written"`, and `success: true`.
Destroy receives the bound `ownershipToken` and returns only after confirmed
deletion with the exact `sandboxId`, `ownershipToken`, `status: "destroyed"`,
and `success: true`; a token mismatch must not delete the sandbox. An absent
sandbox is recognized only from HTTP 404 with `code: "sandbox_not_found"` and
the exact `sandboxId` and `ownershipToken`; a generic 404 never proves absence.
Likewise, only an HTTP 409 `sandbox_already_exists` response that echoes both
exact values is treated as a definitive create conflict.

Example deploy sketch:

```sh
# Provision the gateway credential through Secret Manager without putting its
# value on argv, and grant only the gateway runtime identity access.
printf '%s' "$CLOUD_RUN_SANDBOX_SECRET" | \
  gcloud secrets create crabbox-sandbox-gateway-secret --data-file=-
gcloud secrets add-iam-policy-binding crabbox-sandbox-gateway-secret \
  --member="serviceAccount:GATEWAY_SERVICE_ACCOUNT" \
  --role=roles/secretmanager.secretAccessor

# Build/push the image to Artifact Registry, then:
gcloud beta run deploy crabbox-sandbox-gateway \
  --image=REGION-docker.pkg.dev/PROJECT_ID/REPO/gateway:latest \
  --region=REGION \
  --service-account=GATEWAY_SERVICE_ACCOUNT \
  --sandbox-launcher \
  --no-cpu-throttling \
  --cpu=1 \
  --memory=2Gi \
  --concurrency=1 \
  --min=1 \
  --max=1 \
  --timeout=360s \
  --allow-unauthenticated \
  --set-secrets=SANDBOX_SECRET=crabbox-sandbox-gateway-secret:latest
```

The reference deployment intentionally keeps exactly one instance alive because
the preview sandbox process is instance-local. Do not increase the maximum,
deploy a new revision, or otherwise replace that instance while leases are
active. A production HA gateway needs an external owner router and explicit
instance-loss recovery; session affinity is not sufficient. Crabbox caps exec
requests at five minutes, while the 360-second Cloud Run timeout leaves room for
the terminal frame and HTTP overhead.

If org policy blocks `allUsers` invoker, keep the service private and resolve a
fresh Google-signed identity token for every Crabbox invocation. Do not export
one long-lived token across retained leases.

### 4. Point Crabbox at the gateway

```sh
export CLOUD_RUN_SANDBOX_URL="https://SERVICE-….run.app"
export CLOUD_RUN_SANDBOX_SECRET="…"

crabbox doctor --provider cloud-run-sandbox
crabbox run --provider cloud-run-sandbox -- echo ok

# Private IAM gateway: refresh the token on every separate invocation.
CLOUD_RUN_AUTH_TOKEN="$(gcloud auth print-identity-token --audiences="$CLOUD_RUN_SANDBOX_URL")" \
  crabbox doctor --provider cloud-run-sandbox
CLOUD_RUN_AUTH_TOKEN="$(gcloud auth print-identity-token --audiences="$CLOUD_RUN_SANDBOX_URL")" \
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
sandbox run <id> --detach --write -- /bin/sh -c 'while :; do sleep 3600; done'
sandbox exec <id> -- /bin/mkdir -p /tmp/crabbox
sandbox exec <id> --workdir=/tmp/crabbox -- /bin/sh -c 'uname -a'
sandbox delete <id> --force

# Full help
/usr/local/gcp/bin/sandbox -h
```

Crabbox maps:

| Crabbox | CLI / gateway |
| --- | --- |
| `warmup` / create lease | `sandbox run <id> --detach -- /bin/sh -c <idle-loop>` |
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
```

Provider flags:

```text
--cloud-run-sandbox-gateway-url
--cloud-run-sandbox-cli
--cloud-run-sandbox-workdir
--cloud-run-sandbox-allow-egress
--cloud-run-sandbox-write
--cloud-run-sandbox-rootfs
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
```

## Lifecycle

1. `warmup` or `run` without `--id` creates a stateful sandbox and a local claim
   `gcrs_<sandbox-id>` scoped to the gateway URL hash or direct CLI path. The
   claim is durably born in `creating` state with a random 128-bit ownership
   token and becomes durably ready under the same claim lock that covers the
   provider create result. Direct mode uses that unguessable token as the
   sandbox ID; remote gateways bind it to the sandbox lifecycle.
2. The claim records any configured absolute `--ttl`; cleanup treats that
   deadline independently of idle activity.
3. Unless `--no-sync`, Crabbox archive-syncs into `cloudRunSandbox.workdir`.
4. Commands run via `sandbox exec` (or streaming gateway `/v1/sandbox/exec`). Selected
   env is forwarded explicitly; direct mode streams environment values and
   workspace content on stdin so they never appear in host process arguments.
5. `list` / `status` / `stop` only touch Crabbox-owned claims; list and status
   probe provider liveness rather than treating the local claim as proof that a
   sandbox is still running.
6. One-shot `run` without `--keep` destroys the sandbox after the command.
7. `cleanup` deletes idle- or TTL-expired claimed sandboxes. Claim locking
   serializes cleanup against create, archive sync, command execution, stop,
   and reclaim so cleanup cannot remove ownership while work is in flight.

## Doctor

`crabbox doctor --provider cloud-run-sandbox` is non-mutating:

- remote: authenticated `GET /v1/health` plus durable lifecycle contract
- direct: `sandbox --help` on the configured CLI path
- local claim inventory

| Symptom | Action |
| --- | --- |
| Missing secret with gateway URL set | Export `CLOUD_RUN_SANDBOX_SECRET` |
| CLI not found in direct mode | Deploy with `--sandbox-launcher` or set `cliPath` |
| Unauthorized gateway | Check secret; set `CLOUD_RUN_AUTH_TOKEN` if IAM requires it |
| Gateway lifecycle contract rejected | Add durable sandbox-ID routing and synchronous destroy confirmation; session affinity alone is insufficient |
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
- In-flight create/sync/exec and cleanup share an unchanged-claim lock; a
  timed-out create remains tracked for later stop or cleanup. If Crabbox exits
  during create, the durable `creating` claim becomes cleanup-eligible after
  its active deadline, and destruction still requires its exact ownership token.
- A structured, exact-ID create conflict drops the provisional claim instead
  of taking destructive ownership of a sandbox that already existed. Conflict
  classification and claim removal occur under the same claim lock.
- Cleanup retains the claim and returns an error whenever destruction fails,
  so automation can retry and alert on possible billable residue.
- `list` and `status` expose recovery and expired claims as non-running state;
  recovery claims can be stopped or cleaned up but cannot be used for exec.
- Session cleanup commands preserve the effective gateway URL or custom direct
  CLI path so flag-only routing can reach the same provider scope later.
- Remote claims are removed only after a durable-routing gateway synchronously confirms deletion.
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
```

Related docs:

- [Provider reference](README.md)
- [GCP Compute Engine](gcp.md)
- [Sandbox CLI reference](https://docs.cloud.google.com/run/docs/reference/sandbox-cli)
