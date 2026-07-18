# Cloud Run Sandbox Provider

Read when:

- choosing `provider: cloud-run-sandbox` (aliases: `gcrun-sandbox`,
  `google-cloud-run-sandbox`, `cloudrun-sandbox`);
- configuring a Google Cloud Run sandbox gateway or the in-container `sandbox`
  CLI;
- changing `internal/providers/cloudrunsandbox`.

Cloud Run Sandbox is a delegated-run provider for
[Google Cloud Run sandboxes](https://docs.cloud.google.com/run/docs/code-execution)
(public preview). Sandboxes are lightweight isolated execution boundaries that
spawn inside a Cloud Run service instance with the sandbox launcher enabled.

Crabbox owns provider selection, local claims, archive sync, slugs, timing, and
list/status rendering. Google Cloud owns the isolation boundary (credential and
metadata isolation, deny-by-default egress, and the writable filesystem
overlay).

This provider is separate from [GCP](gcp.md) Compute Engine SSH leases
(`provider: gcp`).

## When To Use

Use Cloud Run sandboxes when you need to run untrusted or AI-generated commands
with Cloud Run's native isolation, either:

- **Remote mode** from a laptop or CI against a ComputeSDK-compatible gateway
  service (`CLOUD_RUN_SANDBOX_URL` + `CLOUD_RUN_SANDBOX_SECRET`); or
- **Direct mode** when Crabbox itself runs inside a Cloud Run service deployed
  with `--sandbox-launcher`, shelling out to `/usr/local/gcp/bin/sandbox`.

Use `gcp` when you need a full Compute Engine VM with Crabbox-managed SSH and
rsync.

## Prerequisites

### Enable sandboxes on Cloud Run

```sh
gcloud auth login
gcloud config set project <PROJECT_ID>
gcloud services enable run.googleapis.com artifactregistry.googleapis.com

# New service
gcloud beta run deploy <SERVICE_NAME> \
  --image <IMAGE_URL> \
  --sandbox-launcher \
  --region <REGION>

# Existing service
gcloud beta run services update <SERVICE_NAME> --sandbox-launcher
```

Inside that service, the sandbox CLI is available at
`/usr/local/gcp/bin/sandbox` (see the
[sandbox CLI reference](https://docs.cloud.google.com/run/docs/reference/sandbox-cli)):

```sh
sandbox do -- <COMMAND>
sandbox do -e KEY=VALUE -- <COMMAND>
sandbox do --allow-egress -- <COMMAND>
sandbox run <id> --detach
sandbox exec <id> -- <COMMAND>
sandbox delete <id> --force
```

### Remote gateway (laptop / CI)

Deploy a gateway that proxies the sandbox CLI (for example
[`npx @computesdk/cloud-run`](https://github.com/computesdk/computesdk/tree/main/packages/cloud-run)),
then set:

```sh
export CLOUD_RUN_SANDBOX_URL="https://…run.app"
export CLOUD_RUN_SANDBOX_SECRET="…"
# Optional when Cloud Run IAM still requires an identity token:
export CLOUD_RUN_AUTH_TOKEN="…"
```

Crabbox also accepts `CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL`,
`CRABBOX_CLOUD_RUN_SANDBOX_SECRET`, and `CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN`.

## Commands

```sh
crabbox doctor --provider cloud-run-sandbox
crabbox warmup --provider cloud-run-sandbox --slug live-smoke
crabbox run --provider cloud-run-sandbox -- echo ok
crabbox run --provider cloud-run-sandbox --id live-smoke -- pwd
crabbox list --provider cloud-run-sandbox --json
crabbox status --provider cloud-run-sandbox --id live-smoke
crabbox stop --provider cloud-run-sandbox live-smoke
```

## Config

```yaml
provider: cloud-run-sandbox
target: linux
cloudRunSandbox:
  cliPath: /usr/local/gcp/bin/sandbox
  workdir: /tmp/crabbox
  allowEgress: false   # Cloud Run default is deny-by-default
  write: true          # needed for archive sync into the sandbox overlay
  rootfs: /
  mode: ""             # optional: local | container
```

Gateway URL is **not** accepted from repository YAML (so a checked-in config
cannot redirect a local secret). Set it with flags or environment variables.

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

1. `warmup` or `run` without `--id` creates a stateful sandbox
   (`sandbox run <id> --detach`, or `POST /v1/sandbox/create` in remote mode)
   and records a local claim as `gcrs_<sandbox-id>`.
2. Crabbox archive-syncs the workspace into `cloudRunSandbox.workdir` (requires
   `write: true`).
3. Commands run through `sandbox exec <id> -- /bin/sh -c …` (or the gateway
   `/v1/sandbox/exec` endpoint). Selected env is forwarded with `-e` / the
   gateway body, never by writing secrets into repo config.
4. `list` / `status` / `stop` operate only on local Crabbox claims for
   `provider=cloud-run-sandbox`.
5. `stop` maps to `sandbox delete <id> --force` (or gateway destroy).

One-shot `run` without `--keep` destroys the sandbox after the command.

## Doctor

`crabbox doctor --provider cloud-run-sandbox` is non-mutating. It checks:

- remote: gateway reachability (`GET /v1/health` when available);
- direct: `sandbox --help` on the configured CLI path;
- local claim inventory for this provider.

Common blockers:

| Symptom | Action |
| --- | --- |
| Missing secret with gateway URL set | Export `CLOUD_RUN_SANDBOX_SECRET`. |
| CLI not found in direct mode | Deploy with `--sandbox-launcher` or set `cloudRunSandbox.cliPath`. |
| Unauthorized gateway | Check the shared secret; set `CLOUD_RUN_AUTH_TOKEN` if IAM requires it. |
| Project not allow-listed | Cloud Run sandboxes are preview; confirm project access with Google Cloud. |

## Capabilities

- Target: Linux.
- Kind: delegated-run.
- Coordinator: never.
- Features: `archive-sync`, `cleanup`, `run-session`.
- Aliases: `gcrun-sandbox`, `google-cloud-run-sandbox`, `cloudrun-sandbox`.
- SSH, desktop, browser, code-server, Tailscale, Actions hydration, and
  Crabbox rsync are not supported.
- `--class` / `--type` are rejected; sandboxes share the parent Cloud Run
  service's CPU and memory (no separate charge for the sandbox feature itself).

## Safety Notes

- Sandboxes do not inherit Cloud Run service environment variables or metadata
  server access.
- Network egress is deny-by-default; set `allowEgress: true` only when required.
- The writable rootfs is an isolated overlay; changes are discarded when the
  sandbox ends unless you export state yourself.
- Gateway secrets and IAM tokens stay in environment variables and request
  headers, never in Crabbox config files or process argv for secret values.
- Local claims are scoped to the gateway URL hash or direct CLI path so
  list/stop cannot cross endpoints by accident.

## Live Smoke

When remote gateway credentials or an in-container sandbox CLI are available:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=cloud-run-sandbox scripts/live-smoke.sh
```

That dispatches to:

```sh
scripts/live-cloud-run-sandbox-smoke.sh
```

Related docs:

- [Provider reference](README.md)
- [GCP Compute Engine](gcp.md)
- [sandbox CLI reference](https://docs.cloud.google.com/run/docs/reference/sandbox-cli)
- [Cloud Run sandboxes announcement](https://cloud.google.com/blog/topics/developers-practitioners/google-cloud-run-sandboxes-are-in-public-preview)
