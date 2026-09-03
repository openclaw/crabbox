# Operations

Read this when you:

- deploy or validate either coordinator runtime;
- change coordinator secrets, routes, ingress, or provider credentials;
- check cost limits or lease cleanup behavior;
- need to decide whether a failure lives in the local CLI, the broker, a provider, or runner state.

Crabbox operations span three layers:

```text
local CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider VM
```

The CLI owns local config, per-lease SSH keys, sync, and remote command execution. The coordinator owns auth, lease state, provider credentials, cost guardrails, and cleanup. Providers own VM creation, network reachability, and deletion. For the full request flow see [Architecture](architecture.md) and [How It Works](how-it-works.md).

## Daily Health Check

Run these before a release or after changing secrets:

```sh
go test ./...
npm run check --prefix worker
npm test --prefix worker
node scripts/generate-linux-readiness.mjs --check
node --test scripts/*.test.js scripts/*.test.mjs
scripts/check-docs.sh
bin/crabbox doctor
bin/crabbox whoami
bin/crabbox list --json
bin/crabbox usage --scope all --json
bin/crabbox history --limit 5
```

- `crabbox doctor` checks local prerequisites and coordinator/provider readiness.
- `crabbox whoami` verifies broker identity.
- `crabbox list` confirms the broker can answer lease state.
- `crabbox usage` proves the cost-accounting path is reachable.
- `crabbox history` proves recorded-run history is reachable.

When broker/provider credentials are available and infra changed, run the live smoke:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

`scripts/live-smoke.sh` defaults to `aws,hetzner`. Narrow the matrix with `CRABBOX_LIVE_PROVIDERS`:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws               CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=hetzner           CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=azure CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_AZURE_TYPE=Standard_D2s_v5 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blacksmith-testbox CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=e2b               CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=modal             CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=coder CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_CODER_TEMPLATE=go-dev CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=daytona           CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=namespace-devbox  CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=namespace-instance CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=semaphore         CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=sprites           CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=tenki CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=machine0 CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_MACHINE0_SIZE=medium CRABBOX_LIVE_MACHINE0_IMAGE=ubuntu-24-04 CRABBOX_LIVE_MACHINE0_REGION=eu CRABBOX_LIVE_MACHINE0_KEY=ci-key CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=wandb CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=kubevirt CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_KUBEVIRT_TEMPLATE=/path/to/vm.yaml scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=external CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_EXTERNAL_COMMAND=/path/to/provider scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=morph CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_MORPH_SNAPSHOT=snapshot_xxx scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=scaleway CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=docker-sandbox CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=smolvm CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=superserve CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=vercel-sandbox CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws-lambda-microvm CRABBOX_AWS_LAMBDA_MICROVM_IMAGE=arn:aws:lambda:eu-west-1:123456789012:microvm-image:crabbox-runner scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=apple-container CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=local-container CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=multipass CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=tart CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=apple-vm CRABBOX_LIVE_COORDINATOR=0 CRABBOX_BIN=./bin/crabbox scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=linode scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=digitalocean scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nebius scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=ovh scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nvidia-brev scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=phala CRABBOX_LIVE_COORDINATOR=0 CRABBOX_BIN=./bin/crabbox scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=anthropic-sandbox-runtime scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=opensandbox CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=github-codespaces CRABBOX_LIVE_COORDINATOR=0 CRABBOX_GITHUB_CODESPACES_SMOKE_REPO=example-org/my-app CRABBOX_GITHUB_CODESPACES_SMOKE_REF=main scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=proxmox CRABBOX_LIVE_COORDINATOR=0 CRABBOX_BIN=./bin/crabbox scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=xcp-ng CRABBOX_LIVE_COORDINATOR=0 CRABBOX_BIN=./bin/crabbox scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=agent-sandbox CRABBOX_LIVE_COORDINATOR=0 scripts/live-smoke.sh
```

Per-provider smoke prerequisites:

- **Blacksmith** — a workflow containing a `useblacksmith/testbox`,
  `useblacksmith/begin-testbox`, or `useblacksmith/run-testbox` step; set
  `CRABBOX_BLACKSMITH_WORKFLOW` when the default path is wrong.
  `scripts/live-smoke.sh` refuses to call Blacksmith until it can derive an org
  and validate the selected Testbox workflow, then lists inventory and runs one
  delegated command through the configured workflow/job/ref.
- **E2B** — `CRABBOX_E2B_API_KEY` or `E2B_API_KEY`.
  `scripts/live-smoke.sh` refuses to call E2B until an API key is exported, then
  creates one sandbox, runs one no-sync command, lists normalized inventory, and
  stops the lease.
- **Modal** — an authenticated Modal Python client (`python3 -m modal setup`
  or Modal token env vars). `scripts/live-smoke.sh` refuses to call Modal until
  the configured Python binary can import the Modal client, then creates one
  sandbox, waits for status, runs one no-sync command, lists normalized
  inventory, and stops the lease.
- **Coder** — authenticated `coder` CLI on `PATH` plus an explicit disposable
  template from `CRABBOX_LIVE_CODER_TEMPLATE`, `CRABBOX_CODER_TEMPLATE`, or
  `coder.template`. `scripts/live-smoke.sh` refuses to mutate Coder until a
  template is selected, then proves doctor, dry-run cleanup, stop-by-default
  warmup/run/stop/status, delete-on-release warmup/run/stop, list, and final
  dry-run cleanup.
- **Sealos DevBox** — `kubectl`, an inherited kubeconfig or readable configured
  kubeconfig, an explicit `sealosDevbox.context`, namespace RBAC for the
  DevBox CRD, `sealosDevbox.image`, and a
  configured SSHGate or NodePort route. `scripts/live-smoke.sh` refuses to
  mutate Sealos resources until those prerequisites and `doctor --json` pass,
  then proves dry-run cleanup, one retained DevBox warmup, status, SSH command
  rendering, a synced command, stop, post-stop status, and final dry-run
  cleanup.
- **Semaphore** — `CRABBOX_SEMAPHORE_HOST`, `CRABBOX_SEMAPHORE_PROJECT`,
  and `CRABBOX_SEMAPHORE_TOKEN`, or the equivalent user config.
  `scripts/live-smoke.sh` refuses to call Semaphore until those values are
  configured, then creates one testbox, runs one no-sync command, lists
  normalized inventory, and stops the lease.
- **Daytona** — `CRABBOX_DAYTONA_SNAPSHOT`, `DAYTONA_SNAPSHOT`, or
  `daytona.snapshot`. `scripts/live-smoke.sh` refuses to call Daytona until a
  snapshot is configured, then runs one delegated command and normalized list
  proof.
- **Namespace** — the authenticated `devbox` CLI on `PATH`.
  `scripts/live-smoke.sh` refuses to call Namespace until `devbox` is available,
  then creates a delete-on-release Devbox, runs one no-sync command, and prints a
  normalized list proof.
- **Namespace Compute** — the authenticated `nsc` CLI on `PATH`; run `nsc login` first.
  `scripts/live-smoke.sh` runs `doctor`, snapshots existing Crabbox-owned
  inventory, creates one short-lived Compute Instance, runs the normal SSH lease
  lifecycle, stops the lease, and fails if the post-smoke inventory changed.
- **Sprites** — the authenticated `sprite` CLI on `PATH` plus a Sprites token
  in the environment. `scripts/live-smoke.sh` refuses to call Sprites until the
  CLI is available, then creates one sprite, verifies SSH, runs one command,
  lists normalized inventory, and stops the lease.
- **Tenki** — the authenticated `tenki` CLI on `PATH`; run `tenki login` and
  complete the browser flow. `scripts/live-smoke.sh` refuses to call Crabbox
  Tenki lifecycle commands until `tenki status --json` reports a logged-in CLI,
  then creates one session, runs one no-sync command, verifies paused-session
  status waits do not resume it, and stops the lease.
- **Machine0** — the authenticated `machine0` CLI on `PATH` or
  `CRABBOX_LIVE_MACHINE0_CLI`, plus a registered disposable SSH key named by
  `CRABBOX_LIVE_MACHINE0_KEY`. The runner performs read-only auth, size/region,
  and key checks before mutation; builds the current Crabbox binary unless
  `CRABBOX_BIN` is explicit; creates one `medium` `ubuntu-24-04` VM in `eu` by
  default; proves no-sync execution and ID-stable suspend/resume with a fresh
  IP; creates, verifies, and deletes a named native checkpoint by default; then
  destroys the VM and proves the provider resource, checkpoint image, and local
  claim are absent. Override the test capacity with
  `CRABBOX_LIVE_MACHINE0_SIZE`, `CRABBOX_LIVE_MACHINE0_IMAGE`, and
  `CRABBOX_LIVE_MACHINE0_REGION`; set
  `CRABBOX_LIVE_MACHINE0_CHECKPOINT=0` only when checkpoint coverage is
  intentionally unavailable.
- **KubeVirt** — `kubectl`, `virtctl`, a namespace with KubeVirt access, and an SSH-ready VM template.
- **Agent Sandbox** — `kubectl`, an absolute kubeconfig or inherited
  `KUBECONFIG`, an explicit context, a namespace, and a configured
  `SandboxWarmPool`. `scripts/live-agent-sandbox-smoke.sh` is coordinator-free,
  creates a short-lived `SandboxClaim`, verifies archive sync, env forwarding,
  retained-claim reuse, replacement sync, status/list, and claim deletion.
- **External** — a configured provider executable through `external.command` or
  `CRABBOX_LIVE_EXTERNAL_COMMAND`, or a declarative
  `external.lifecycle.acquire` configuration. `scripts/live-smoke.sh` refuses
  to call External lifecycle commands until one path is configured, then runs
  the normal SSH lease lifecycle, lists normalized inventory, and stops the
  lease.
- **Morph** — `CRABBOX_MORPH_API_KEY`, `MORPH_API_KEY`, or `morph.apiKey`,
  plus `CRABBOX_LIVE_MORPH_SNAPSHOT`. `scripts/live-smoke.sh` refuses to call
  Morph until both are configured, then creates one delete-on-release instance,
  runs the normal SSH lease lifecycle, lists normalized inventory, and stops the
  lease.
- **Scaleway** — `SCW_ACCESS_KEY`, `SCW_SECRET_KEY`,
  `SCW_DEFAULT_ORGANIZATION_ID`, `SCW_DEFAULT_PROJECT_ID`,
  `SCW_DEFAULT_REGION`, and `SCW_DEFAULT_ZONE`, or equivalent
  `CRABBOX_SCALEWAY_*` overrides for project/location fields.
  `scripts/live-scaleway-smoke.sh` is coordinator-free, requires an empty
  Crabbox-owned inventory before provisioning, creates one short-lived
  `DEV1-S` instance by default, verifies status/run/list, stops the lease, and
  proves the inventory is empty afterward.
- **Docker Sandbox** — the standalone `sbx` CLI on `PATH` or configured with `CRABBOX_DOCKER_SANDBOX_CLI`; run `sbx login` first when your account requires authentication.
- **SmolVM** — `CRABBOX_SMOLVM_API_KEY`, `SMOLMACHINES_API_KEY`, or `SMK_API_KEY`.
- **Superserve** — `CRABBOX_SUPERSERVE_API_KEY` or `SUPERSERVE_API_KEY`.
- **Vercel Sandbox** — authenticated `sandbox` CLI on `PATH`; project and team scope may come from `CRABBOX_VERCEL_SANDBOX_PROJECT_ID` plus `CRABBOX_VERCEL_SANDBOX_TEAM_ID`, `CRABBOX_VERCEL_SANDBOX_SCOPE`, or the Vercel OIDC environment.
- **AWS Lambda MicroVM** — standard AWS SDK credentials, an explicitly exported `CRABBOX_AWS_LAMBDA_MICROVM_IMAGE`, a launch Region, and quota. `scripts/live-aws-lambda-microvm-smoke.sh` is coordinator-free; it proves archive sync, retained reuse, pause/resume, termination, and empty local inventory after cleanup.
- **Apple Container** — Apple silicon macOS with Apple's `container` CLI on
  `PATH` and `container system start` already run. `scripts/live-smoke.sh` uses
  the normal SSH lease lifecycle with a short TTL and no coordinator.
- **Local Container** — a working Docker-compatible CLI and daemon such as
  Docker Desktop, OrbStack, or Colima. `scripts/live-smoke.sh` uses the normal
  SSH lease lifecycle with a short TTL and no coordinator.
- **Multipass** — Canonical Multipass installed locally and able to launch
  Ubuntu VMs. `scripts/live-smoke.sh` uses the normal SSH lease lifecycle with a
  short TTL and no coordinator.
- **Tart** — Apple silicon macOS with the `tart` CLI installed, a reachable base
  image, and guest login credentials configured when the selected image needs a
  password. `scripts/live-smoke.sh` uses the normal SSH lease lifecycle with a
  longer TTL and no coordinator.
- **Apple VZ** — Apple silicon macOS, a locally built Crabbox binary
  (`CRABBOX_BIN`), and the bundled or explicit `crabbox-apple-vm-helper`.
  `scripts/live-smoke.sh` uses the normal SSH lease lifecycle and preserves
  `CRABBOX_LIVE_APPLE_VM_HELPER` for the whole run when set.
- **W&B** — `WANDB_ENTITY_NAME` plus `CRABBOX_WANDB_API_KEY` or
  `WANDB_API_KEY` (from `wandb login`). `scripts/live-smoke.sh` refuses to call
  W&B until an API key is exported, then runs `doctor`, executes one no-sync
  command, and prints normalized inventory.
- **Incus** — local `jq` and `rg`, plus Crabbox config or env resolving
  `incus.socket`, `incus.address`, or `incus.remote`. `scripts/live-smoke.sh`
  refuses to call Incus until local preflight tools are available, then proves
  delete-on-release and retained-reuse SSH lease lifecycles.
- **Linode** — `LINODE_TOKEN` with Linode instance, image, type, SSH key, and tag access.
- **DigitalOcean** — `DIGITALOCEAN_TOKEN` with account-read, Droplet, image-read, SSH key, and tag scopes. `scripts/live-digitalocean-smoke.sh` is coordinator-free, requires an empty Crabbox-owned inventory, creates a small short-lived Droplet, verifies status and execution, and prints a final cleanup classification.
- **Nebius** — authenticated Nebius CLI profile plus `nebius.parentId` and
  `nebius.subnetId`. `scripts/live-nebius-smoke.sh` is coordinator-free,
  requires explicit `CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nebius`, creates one
  short-lived CPU-default VM, verifies status and `echo ok`, stops the lease,
  runs dry-run cleanup, and prints a final classification.
- **OVHcloud** — OVH application credentials plus `ovh.projectId` and
  `ovh.region`. `scripts/live-ovh-smoke.sh` is coordinator-free, requires an
  empty Crabbox-owned OVH inventory, creates one short-lived `b3-8` instance by
  default, verifies status and `echo ok`, stops the lease, runs dry-run cleanup,
  and prints a final classification.
- **NVIDIA Brev** — authenticated `brev` CLI on `PATH` plus enough GPU quota
  and capacity for the selected workspace type. `scripts/live-nvidia-brev-smoke.sh`
  is coordinator-free, creates one short-lived GPU workspace, verifies
  `nvidia-smi`, deletes the workspace with `crabbox stop`, and prints a final
  classification.
- **Phala** — authenticated `phala` CLI on `PATH` (or `CRABBOX_PHALA_CLI`),
  Phala auth, and enough confidential CVM quota. `scripts/live-phala-smoke.sh`
  is coordinator-free, classifies missing auth/quota before or during
  provisioning, verifies sync/env forwarding on a tiny Git fixture, and
  hard-deletes any created CVM on failure.
- **Anthropic Sandbox Runtime** — `srt` and `curl` on `PATH` plus host sandbox
  support. `scripts/live-anthropic-sandbox-runtime-smoke.sh` is coordinator-free
  and local-only; it verifies `doctor`, one-shot command execution, allowed
  temp-file access, denied secret reads, and denied network access.
- **OpenSandbox** — `CRABBOX_OPENSANDBOX_API_KEY` or `OPEN_SANDBOX_API_KEY`,
  plus `CRABBOX_OPENSANDBOX_API_URL` or `OPEN_SANDBOX_API_URL`.
  `scripts/live-opensandbox-smoke.sh` is coordinator-free, proves archive sync,
  off-argv environment forwarding, retained sandbox reuse, staged `sync.delete`
  replacement, list/status, and cleanup.
- **GitHub Codespaces** — an authenticated `gh` CLI, Python 3, an explicit
  `CRABBOX_GITHUB_CODESPACES_SMOKE_REPO`, and either `GH_TOKEN`,
  `GITHUB_TOKEN`, or `CRABBOX_GITHUB_CODESPACES_USE_GH_AUTH=1`.
  The ref may be explicit or is detected from the repository default branch;
  the devcontainer path is passed only when explicitly configured.
  `scripts/live-smoke.sh` delegates to
  `scripts/live-github-codespaces-smoke.sh`, which is coordinator-free,
  creates one short-lived Codespace lease, verifies status, sync/run, SSH
  command generation, list, stop, and dry-run cleanup.
- **Proxmox** — a locally built Crabbox binary (`CRABBOX_BIN`), Proxmox API
  credentials/config, and `jq`/`perl`. `scripts/proxmox-live-smoke.sh` is
  coordinator-free and read-only by default; set `CRABBOX_PROXMOX_LIVE_SMOKE=1`
  only after `doctor` is green to run the guarded warmup/status/ssh/stop proof.
- **XCP-ng** — a locally built Crabbox binary (`CRABBOX_BIN`), XCP-ng API
  credentials/config, and `python3`. `scripts/xcpng-live-smoke.sh` is
  coordinator-free and read-only by default; pass `--mutate` through the
  standalone script and set `CRABBOX_XCP_NG_LIVE_MUTATE=1` only after `doctor`
  is green.

For a direct-provider smoke (no coordinator), disable the broker with a scratch config and run the same lease lifecycle manually:

```sh
tmp="$(mktemp)"
printf 'provider: hetzner\n' > "$tmp"
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= bin/crabbox warmup --provider hetzner --class standard --ttl 15m --idle-timeout 4m
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= bin/crabbox run --provider hetzner --id <slug> --no-sync -- echo direct-hetzner-ok
CRABBOX_CONFIG="$tmp" CRABBOX_COORDINATOR= bin/crabbox stop --provider hetzner <slug>
rm -f "$tmp"
```

Use `--provider aws` with AWS SDK credentials for the direct AWS equivalent.
Use `scripts/live-smoke.sh` or `scripts/live-digitalocean-smoke.sh` for the
repeatable direct DigitalOcean equivalent; it builds or reuses `bin/crabbox`,
creates a guarded `digitalocean` scratch config, and verifies the Crabbox-owned
inventory is empty before create and after stop/cleanup.
Use `scripts/live-smoke.sh` or `scripts/live-nebius-smoke.sh` for the repeatable
direct Nebius equivalent; it builds or reuses `bin/crabbox`, uses the documented
Nebius config and CLI profile, creates a unique `nebius-smoke-*` lease, and
verifies the slug is absent after stop and dry-run cleanup.
Use `scripts/live-smoke.sh` or `scripts/live-ovh-smoke.sh` for the repeatable
direct OVHcloud equivalent; it builds or reuses `bin/crabbox`, uses the
documented OVH credentials and project settings, creates a unique
`ovh-smoke-*` lease, and verifies the Crabbox-owned inventory is empty after
stop and dry-run cleanup.

## Deployment

Choose one runtime for a coordinator installation:

| Runtime | Durable state and scheduling | Deployment shape |
| --- | --- | --- |
| Cloudflare | Fleet Durable Object, alarms, scheduled Worker trigger | Wrangler-managed edge service |
| Node.js | PostgreSQL 13+ plus pg-boss | Initial single-replica container or process behind TLS/WebSocket ingress |

Both expose the same API and portal. They do not automatically copy state
between Durable Object storage and PostgreSQL. Cloudflare is the established
deployment; complete the Node deployment-proof checklist in
[Portable Coordinator Runtime](plan/portable-coordinator.md) before production
cutover.

### Cloudflare Worker

Worker source lives in `worker/`. Run the gate, then deploy:

```sh
npm ci --prefix worker
npm run format:check --prefix worker
npm run lint --prefix worker
npm run check --prefix worker
npm test --prefix worker
npm run build --prefix worker
npx wrangler deploy --config worker/wrangler.jsonc
```

The repeatable deploy proof is:

```sh
scripts/deploy-worker-smoke.sh
```

It runs Worker format, lint, typecheck, tests, dry-run build, deploy, and public health checks for the comma-separated deployment URLs in `CRABBOX_DEPLOY_SMOKE_URLS`. To include a short AWS lease smoke after deploy:

```sh
CRABBOX_DEPLOY_SMOKE_URLS="https://$BROKER_HOST/v1/health" \
  CRABBOX_DEPLOY_SMOKE_AWS=1 \
  CRABBOX_LIVE_REPO=/path/to/my-app \
  scripts/deploy-worker-smoke.sh
```

### Node.js And PostgreSQL

Requirements: Node.js 22.12+, PostgreSQL 13+, one always-on service replica, and
an ingress that preserves WebSocket upgrades. Use TLS with hostname and CA
verification for a remote database. Build and run directly:

```sh
npm ci --prefix worker
npm run format:check --prefix worker
npm run lint --prefix worker
npm run check:node --prefix worker
npm test --prefix worker
npm run build:node --prefix worker

DATABASE_URL='postgresql://crabbox:password@db.example.com/crabbox?sslmode=verify-full&sslrootcert=/run/secrets/postgres-ca.pem' \
CRABBOX_PUBLIC_URL=https://broker.example.com \
npm run start:node --prefix worker
```

Or build the OCI image with `worker/` as context:

```sh
docker build -f worker/Dockerfile.node -t crabbox-coordinator:local worker
docker run --rm -p 8080:8080 \
  --env-file /secure/path/crabbox.env \
  --mount type=bind,src=/secure/path/postgres-ca.pem,dst=/run/secrets/postgres-ca.pem,readonly \
  crabbox-coordinator:local
```

The checked-in runtime Dockerfiles keep readable base-image tags but pin them to
multi-platform manifest digests. When refreshing a base image, resolve the
current manifest-list digest, update the tag and digest together, build the
affected image, and run the repository check:

```sh
docker buildx imagetools inspect <image>:<tag> --format '{{.Manifest.Digest}}'
node scripts/check-docker-base-images.mjs
```

The service creates PostgreSQL schemas `crabbox` and `crabbox_jobs`. Use
`GET /v1/health` for liveness and `GET /v1/ready` for database readiness.
`SIGTERM` and `SIGINT` stop new requests, drain active HTTP/WebSocket and
provisioning work, then close PostgreSQL;
`CRABBOX_SHUTDOWN_TIMEOUT_MS` defaults to 120000.

For a VM, run the same image or Node process under the host service manager. For
Kubernetes or another scheduler, use one replica, a `Recreate` deployment
strategy, readiness on `/v1/ready`, and a termination grace period longer than
the shutdown timeout.
PostgreSQL state and pg-boss jobs are durable, but lifecycle serialization and
live bridge ownership remain process-local. Do not horizontally scale yet.

Durable provisioning uses the existing KV table for its private sorted due
index and transactional wake outbox. pg-boss is a notification hint for this
work: immediate startup reconciliation and a one-second scanner inspect the
committed due index independently of the legacy alarm queue. Queue deletion,
enqueue failure, or a missing queued job must not strand an admitted operation.
Cloudflare commits due changes and native alarms in the same Durable Object
transaction; legacy reschedule/clear operations preserve the earliest durable
due time. Constructor repair performs bounded storage work only.

Alarms await the bounded provisioning tick and wake commit. Slow legacy
maintenance is a runtime-owned single-flight task, with sanitized failures and
retry scheduling. Node tracks and drains that task during shutdown. Manual
admin sweep endpoints still await their actual operation. `waitUntil` and
pg-boss do not replace the durable operation/claim records.

Keep `CRABBOX_DURABLE_PROVISIONING_ADMISSION` unset or `false` until the
journal-aware version and a stable existing `CRABBOX_SESSION_SECRET` are ready.
Setting the gate to `false` stops new admissions but resumes existing journals.
The session secret must be distinct from the shared token and at least 32
characters; missing, changed or lost encryption material blocks affected
forward replay without deleting cleanup evidence. Do not provision or rotate a
secret as an implicit part of enabling this feature. Resolve existing shared
infrastructure and cleanup debt before enabling new admissions; never erase
old cleanup records based on the presence of a new operation.

### Dedicated AWS private-workspace service

Use the checked-in ECS Fargate deployment when one Node/PostgreSQL coordinator
must own small, private, SSM-only Linux workspaces in one AWS account and
Region. The stack owns its single-replica ECS control plane, task roles,
workspace instance role/profile, retained SSM log group, internal HTTPS load balancer,
and separate security groups. It injects the database URL and route-scoped
workspace bearer from Secrets Manager and uses refreshable task-role
credentials rather than static AWS keys.

The runbook in [Private AWS Workspaces](features/aws-private-workspaces.md)
covers the exact account/Region preflight, small instance allowlist, 20 GiB
encrypted gp3 disk, private subnet, no public IP/SSH, IMDSv2, SSM bootstrap,
CloudWatch evidence, client URL/bearer contract, idempotent cleanup, rollback,
and retirement. Deployment and the paid create/delete canary require a separate
AWS GO; code merge alone is not approval.

### Minimum coordinator configuration

Configure `CRABBOX_PUBLIC_URL`, one auth model, and at least one brokered
provider. Shared-token automation needs `CRABBOX_SHARED_TOKEN` and
`CRABBOX_SHARED_OWNER`; browser login needs the GitHub OAuth settings below.
Provider choices are `HETZNER_TOKEN`, AWS credentials from the default chain or
an existing static credential contract, an Azure service principal, a GCP
service account, or `DAYTONA_CRABBOX_KEY`. Node additionally requires
`DATABASE_URL`.

GitHub OAuth start routes remain unauthenticated so a new user can bootstrap login.
The coordinator limits active attempts to ten per caller source and 100 globally for
both CLI and portal login, after removing expired attempts. Node deployments behind a
reverse proxy must configure `CRABBOX_TRUSTED_PROXY_CIDRS`; otherwise caller limits use
the direct peer address and ignore forwarded addresses. The Node runtime rejects malformed
allowlist entries at startup and rate-limits warnings when an untrusted socket peer sends
`X-Forwarded-For`, which usually means the reverse-proxy allowlist is missing or incomplete.

For any portal that exposes browser Code, configure
`CRABBOX_CODE_ORIGIN_TEMPLATE=https://{lease}.code.example.com` and route the
matching wildcard hostname to the same coordinator with TLS and WebSocket
support. This preserves the normal Code links while moving each lease's
proxied HTML and JavaScript to a separate browser origin.

### Conditional coordinator secrets and settings

```text
AWS_SESSION_TOKEN                  optional
CRABBOX_HOST_ID                    optional; admin-only except owner reactivation of a retained Mac instance
CRABBOX_AWS_MAC_HOST_ID            optional legacy AWS alias for CRABBOX_HOST_ID
CRABBOX_SHARED_OWNER              optional fixed owner identity for shared-token automation
CRABBOX_ADMIN_TOKEN               required for admin routes and image promotion
CRABBOX_WORKSPACE_SSH_PUBLIC_KEY  required for SSH-based /v1/workspaces provisioning; unused by private AWS mode
CRABBOX_WORKSPACE_SSH_PRIVATE_KEY required for SSH-based terminal attachment; unused by private AWS mode
CRABBOX_WORKSPACE_PROVIDER        optional workspace provider; hetzner, aws, azure, or gcp
CRABBOX_WORKSPACE_CLASS           optional workspace machine class; default standard
CRABBOX_WORKSPACE_PREWARM_COUNT   optional ready spares per active organization; default 0, maximum 4
CRABBOX_RUNTIME_ADAPTER_TOKEN     route-scoped workspace API credential
CRABBOX_RUNTIME_ADAPTER_OWNER     stable service owner for route-scoped access
CRABBOX_RUNTIME_ADAPTER_ORG       stable organization for route-scoped access
DAYTONA_CRABBOX_KEY               required for brokered Daytona leases
CRABBOX_DAYTONA_*                 optional Daytona API, snapshot, target, user, work-root, and SSH-token settings
CRABBOX_RUN_RETENTION_DAYS        terminal run history retention; default 30 days, minimum 1
CRABBOX_GITHUB_CLIENT_ID          required for browser login
CRABBOX_GITHUB_CLIENT_SECRET      required for browser login
CRABBOX_SESSION_SECRET            required for browser login; must differ from CRABBOX_SHARED_TOKEN
CRABBOX_CODE_ORIGIN_TEMPLATE      required for browser Code; per-lease HTTPS origin template
CRABBOX_GITHUB_ALLOWED_ORG or CRABBOX_GITHUB_ALLOWED_ORGS
CRABBOX_GITHUB_ALLOWED_TEAMS      optional
CRABBOX_GITHUB_REVOKED_USERS      optional github:<numeric-id> revocation list; mutable entries fail GitHub auth closed
CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS optional; default 300, range 0-3600
CRABBOX_ACCESS_TEAM_DOMAIN        required for Access JWT verification
CRABBOX_ACCESS_AUD                required for Access JWT verification
CRABBOX_TAILSCALE_CLIENT_ID       required for brokered --tailscale
CRABBOX_TAILSCALE_CLIENT_SECRET   required for brokered --tailscale
CRABBOX_TAILSCALE_TAILNET         optional
CRABBOX_TAILSCALE_TAGS            optional
CRABBOX_TAILSCALE_ENABLED         optional; set 0 to disable brokered Tailscale
CRABBOX_TAILSCALE_INSTALL_MODE    optional; package (default) or pinned static archive
CRABBOX_TAILSCALE_VERSION         optional pinned static build version
CRABBOX_TAILSCALE_SHA256_AMD64    optional pinned amd64 archive checksum
CRABBOX_TAILSCALE_SHA256_ARM64    optional pinned arm64 archive checksum
CRABBOX_ARTIFACTS_BACKEND         optional; enables brokered artifact publishing
CRABBOX_ARTIFACTS_BUCKET          required when artifact backend is enabled
CRABBOX_ARTIFACTS_PREFIX          optional
CRABBOX_ARTIFACTS_BASE_URL        optional; public URL prefix used only with public reads
CRABBOX_ARTIFACTS_PUBLIC_READS    optional; set 1 to intentionally return public non-expiring links
CRABBOX_ARTIFACTS_REGION          optional
CRABBOX_ARTIFACTS_ENDPOINT_URL    optional; required for R2/custom S3 endpoints
CRABBOX_ARTIFACTS_ACCESS_KEY_ID   required when artifact backend is enabled
CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY required when artifact backend is enabled
CRABBOX_ARTIFACTS_SESSION_TOKEN   optional
CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS optional
CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS    optional
CRABBOX_AWS_ORPHAN_SWEEP_ENABLED  optional; defaults on when AWS broker credentials exist
CRABBOX_AWS_ORPHAN_SWEEP_DELETE   optional; set 1 to terminate coordinator-owned orphan EC2 instances
CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS optional; default 3600
CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS    optional; default 900
CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE optional; set 1 to release stale pending EC2 Mac hosts during orphan sweep
CRABBOX_AZURE_ORPHAN_SWEEP_ENABLED optional; defaults on when Azure broker credentials exist
CRABBOX_AZURE_ORPHAN_SWEEP_DELETE   optional; set 1 to release coordinator-owned Azure orphan resources
CRABBOX_AZURE_ORPHAN_SWEEP_INTERVAL_SECONDS optional; default 3600
CRABBOX_AZURE_ORPHAN_SWEEP_GRACE_SECONDS    optional; default 900
```

Normal SSH-based AWS workspace bridges use a dedicated `crabbox-workspaces`
security group, separate from ordinary runner ingress. Workers TCP egress has
no published allowlist, so that group accepts key-only SSH from `0.0.0.0/0`;
workspace keys are deployment specific, host keys are pinned, and leases expire
automatically. The dedicated private AWS mode does not use this group or any SSH
key: it requires a separate no-ingress group and SSM-only bootstrap.

Workspace leases currently use their hard TTL for provider expiry because the
adapter does not yet receive a trustworthy activity signal. Workspace TTLs must
be at least 1,800 seconds so a durable claim and ambiguity-recovery window both
fit before hard TTL.

The workspace SSH public and private keys must be a matching dedicated key
pair. The coordinator installs the public key on provisioned workspaces and
uses the private key only for authenticated terminal attachment. Each workspace
also receives a coordinator-generated SSH host identity whose fingerprint is
persisted before provisioning, so first attachment does not rely on TOFU.
The versioned workspace `attachUrl` is a bearer-authenticated server-to-server
endpoint for control planes such as Crabfleet, not a browser portal URL.

Private AWS workspaces intentionally omit terminal attachment. Their client
contract is the dedicated service URL, `CRABBOX_RUNTIME_ADAPTER_TOKEN`, and
`POST`/`GET`/`DELETE /v1/workspaces`; a `ready` status means SSM registration and
bootstrap succeeded. Client labels and Region-shaped metadata do not choose
placement.

Desktop workspaces report `capabilities.nativeVnc=true` when the native CLI
handoff is available. This does not imply a browser desktop endpoint:
`capabilities.vnc` and `capabilities.desktop` remain false unless
`POST /v1/workspaces/:id/connections/desktop` is supported.
`POST /v1/workspaces/:id/connections/native-vnc` mints a one-minute,
single-use grant. The native CLI passes that grant on stdin, and the coordinator
uses its dedicated workspace SSH key to relay the loopback VNC service over an
authenticated WebSocket. The workspace SSH private key never leaves the
coordinator.

When `CRABBOX_WORKSPACE_PREWARM_COUNT` is positive, the coordinator keeps that
many hidden ready workspaces for each organization with active workspace demand.
Any owner in the organization can atomically adopt a matching spare. The
coordinator replenishes adopted spares and drains them after the organization
has no provisioning or ready workspaces.

### Artifact backend

The artifact backend vars are ordinary coordinator settings except
`CRABBOX_ARTIFACTS_ACCESS_KEY_ID`,
`CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY`, and optional
`CRABBOX_ARTIFACTS_SESSION_TOKEN`, which must use the runtime's secret
injection. These object-store keys let the coordinator sign short-lived
artifact upload/read URLs. Scope them to the artifact bucket or prefix; they
should not carry Cloudflare account, Worker deployment, lease-provider, or VM
permissions.

The shipped production configuration in `worker/wrangler.jsonc` wires the
existing R2 artifact backend; signing keys remain deployed secrets, and reads
use expiring signed URLs by default. The **Deploy Coordinator** workflow
(`.github/workflows/coordinator-deploy.yml`) owns redeploys when these settings
change on `main`. Missing required backend configuration fails with
`artifact_upload_unavailable` before any file upload. Checking artifact
publishing configuration requires no compute lease.

For workflow-managed R2 signing keys, set the optional GitHub environment
secret `CRABBOX_ARTIFACTS_CREDENTIALS_JSON` in `coordinator` to exactly:

```json
{
  "CRABBOX_ARTIFACTS_ACCESS_KEY_ID": "<access-key-id>",
  "CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY": "<secret-access-key>"
}
```

Both values must be nonempty strings; no other fields are accepted. Use
dedicated keys with Object Read & Write access scoped to the existing artifact
bucket, not shared deployment keys. Replace the entire bundle to rotate the
pair atomically, then run **Deploy Coordinator**, the existing serialized
deployment owner. It deploys both runtime secrets in the same Worker version,
together with `CRABBOX_DAYTONA_SNAPSHOT` when configured. An absent bundle
preserves the existing artifact credentials; it does not clear them or change
unrelated secret bindings.

A typical R2-compatible configuration looks like:

```text
CRABBOX_ARTIFACTS_BACKEND=r2
CRABBOX_ARTIFACTS_BUCKET=my-crabbox-artifacts
CRABBOX_ARTIFACTS_PREFIX=crabbox-artifacts
CRABBOX_ARTIFACTS_BASE_URL=https://artifacts.example.com
CRABBOX_ARTIFACTS_PUBLIC_READS=1
CRABBOX_ARTIFACTS_REGION=auto
CRABBOX_ARTIFACTS_ENDPOINT_URL=<account>.r2.cloudflarestorage.com
```

Omit `CRABBOX_ARTIFACTS_PUBLIC_READS` to return expiring signed read URLs even
when a base URL is configured. Enable it only when the artifact origin is
intentionally public; each public grant receives a random capability namespace.

**Security consideration:** Artifact object paths embed the authenticated
organization and owner as reversible base64url values, not hashes or encrypted
values. This applies to both expiring signed URLs and public URLs. The owner can
be a non-public GitHub verified email, and `crabbox artifacts publish --pr` can
place the resulting URLs in public pull-request comments. The random capability
namespace makes a public URL difficult to guess but does not conceal identity
from someone who has the URL. Weigh that disclosure before enabling public
reads. This identity disclosure is an accepted Low/P3 residual risk.

Deploy the matching access key id and secret access key as coordinator secrets,
not local CLI defaults. End users run `crabbox artifacts publish` without
holding any S3/R2 credentials.

### Cost-control secrets and settings

```text
CRABBOX_COST_RATES_JSON
CRABBOX_EUR_TO_USD
CRABBOX_MAX_ACTIVE_LEASES
CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER
CRABBOX_MAX_ACTIVE_LEASES_PER_ORG
CRABBOX_MAX_CHECKPOINTS                       default 64
CRABBOX_MAX_CHECKPOINTS_PER_OWNER             default 16
CRABBOX_MAX_CHECKPOINTS_PER_ORG               default 32
CRABBOX_MAX_CHECKPOINT_USE_CLAIMS             default 16 per checkpoint
CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER   default 64
CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL       default 256
CRABBOX_MAX_MONTHLY_USD
CRABBOX_MAX_MONTHLY_USD_PER_OWNER
CRABBOX_MAX_MONTHLY_USD_PER_ORG
CRABBOX_DEFAULT_ORG
```

Monthly cost checks use reserved cost, not only elapsed runtime. Long TTLs,
prewarmed leases, and failed provisioning attempts can therefore consume budget
headroom faster than the provider bill. Keep active-lease and per-owner limits
as the primary safety rails, and size fleet/org monthly caps with enough room
for TTL-based reservations during busy test bursts.

Managed checkpoint limits are independent of lease cost accounting. The
shipped Cloudflare production and preview configuration sets checkpoint caps
to 20 globally, 10 per owner, and 20 per organization; claim caps retain their
16/64/256 defaults. Creation and use reject excess work transactionally with
HTTP 429 `checkpoint_limit_exceeded` or `checkpoint_claim_limit_exceeded`.
Checkpoint events retain only the most recent 256 transitions, so operators
must not interpret the event endpoint as complete checkpoint lifetime history.

## Routes And Access

A deployment exposes one canonical route:

```text
https://broker.example.com          # CLI, API, portal, browser login, WebSockets
```

Cloudflare deployments can expose the same Worker at
`https://broker-access.example.com` behind Cloudflare Access. Node deployments
can use a conventional TLS/WebSocket ingress and optionally trust an
authenticated user header only from `CRABBOX_TRUSTED_PROXY_CIDRS`. Bearer-token
CLI automation authenticates with `CRABBOX_SHARED_TOKEN` /
`CRABBOX_COORDINATOR_TOKEN`; GitHub browser login stores a user-scoped signed
token (prefix `cbxu_`). See [Auth and Admin](features/auth-admin.md) and
[Broker Auth and Routing](features/broker-auth-routing.md).

Test the Cloudflare Access layer through the protected route:

```sh
CRABBOX_COORDINATOR=https://broker-access.example.com bin/crabbox doctor
CRABBOX_COORDINATOR=https://broker-access.example.com bin/crabbox whoami
CRABBOX_LIVE=1 CRABBOX_AUTH_SMOKE_ACCESS=1 \
  CRABBOX_COORDINATOR=https://broker-access.example.com \
  CRABBOX_BIN=bin/crabbox scripts/live-auth-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws \
  CRABBOX_COORDINATOR=https://broker-access.example.com \
  CRABBOX_BIN=bin/crabbox scripts/live-smoke.sh
```

`doctor` should report `access=service-token`. `scripts/live-auth-smoke.sh` proves the auth boundary without leasing a machine: requests missing Access headers are denied at the edge, shared-token user auth works, raw Access-identity spoofing is ignored, shared-token admin calls fail, and admin-token admin calls pass. A raw request without Access headers to `https://broker-access.example.com/v1/health` should return a Cloudflare Access `403`.

Confirm which URL and provider the CLI will use:

```sh
bin/crabbox config show
```

## Cleanup

Brokered cleanup belongs to the coordinator scheduler: Durable Object alarms on
Cloudflare or pg-boss jobs on Node. The CLI refuses provider cleanup when a
coordinator is configured, because deleting machines behind the broker can
remove live leases:

```text
machine cleanup is disabled when a coordinator is configured;
coordinator TTL alarms own brokered cleanup
```

For brokered fleets, inspect and end leases through the broker:

```sh
bin/crabbox list
bin/crabbox admin leases --state active
bin/crabbox inspect --id blue-lobster --json
bin/crabbox stop blue-lobster
```

Azure brokered release persists a durable claim for the observed canonical
resource set, including stable resource identities. Retries recheck live
attachments and identity before each deletion; names or Crabbox-written
ownership tags are never sufficient proof. Successful per-member deletion
progress is transactionally merged before cleanup advances, and an absent member
without that exact progress makes the remaining set report-only. Fresh reads
also reject changed ownership labels and any VM data-disk attachment. See
[Lifecycle and cleanup](features/lifecycle-cleanup.md) for the VM-less orphan-set
and quarantine rules. Legacy claims may upgrade only from a fully present
canonical set (including the explicit ephemeral-OS-disk shape); partial legacy
claims require manual resolution. New claims use a version-2 transactional
preparation state, which older workers reject safely.

Trusted operators can use `crabbox admin release` or `crabbox admin delete --force` for stuck leases.

After AWS credential or account rotation, scan old provider accounts directly for Crabbox-tagged EC2 instances that the current coordinator can no longer see:

```sh
scripts/aws-crabbox-orphan-audit.sh --profile old-crabbox-account
```

The audit is read-only. It skips `keep=true` instances, protects active coordinator leases by lease tag or EC2 instance ID, and applies the same grace window as the broker sweep before reporting stale labels. The script intentionally refuses `--terminate`: a local AWS scan cannot atomically lock coordinator lease state before deleting an instance. For broker-owned accounts, use the coordinator AWS orphan sweep below. For rotated legacy accounts, treat the JSON output as investigation evidence and delete through an explicit operator or infrastructure workflow only after confirming no active coordinator can still claim the instance.

Direct-provider cleanup is only for debug mode without a coordinator:

```sh
bin/crabbox cleanup --dry-run
bin/crabbox cleanup
```

### AWS orphan sweep

The coordinator schedules an AWS orphan sweep when AWS broker credentials are
configured. Cloudflare uses the Durable Object alarm plus its scheduled trigger;
Node uses pg-boss plus recurring reconciliation, so cleanup does not depend on
new lease traffic after deploy. The sweep scans `CRABBOX_AWS_REGION` plus
`CRABBOX_CAPACITY_REGIONS` for `crabbox=true` EC2 instances and compares their
lease tags with active coordinator leases. Active matching leases always win,
because provider `expires_at` tags are written at launch and can be older than a
heartbeat-extended lease.

The sweep reports a candidate when an instance is past its provider `expires_at` tag, has no active lease, is missing a lease label, or points at an active lease whose current cloud ID differs. It skips `keep=true` instances and applies the grace window before reporting missing or mismatched lease state. Provider tags are discovery metadata, not deletion authority. Set `CRABBOX_AWS_ORPHAN_SWEEP_DELETE=1` to terminate candidates only when an exact retained coordinator lease binds the same instance and region; tag-only and legacy candidates remain report-only. With `CRABBOX_AWS_MAC_HOST_SWEEP_RELEASE=1` also set, the same rule permits release of a stale pending EC2 Mac Dedicated Host only when a retained coordinator lease binds that exact host.

Trusted admins can inspect or trigger the sweep:

```sh
curl -H "Authorization: Bearer $CRABBOX_COORDINATOR_ADMIN_TOKEN" \
  https://broker.example.com/v1/admin/aws-orphan-sweep

curl -X POST -H "Authorization: Bearer $CRABBOX_COORDINATOR_ADMIN_TOKEN" \
  https://broker.example.com/v1/admin/aws-orphan-sweep
```

See [Lifecycle and Cleanup](features/lifecycle-cleanup.md) for the full lease-expiry model.

### Azure orphan sweep

Trusted admins can inspect or trigger the coordinator's Azure orphan sweep:

```sh
curl -H "Authorization: Bearer $CRABBOX_COORDINATOR_ADMIN_TOKEN" \
  https://broker.example.com/v1/admin/azure-orphan-sweep

curl -X POST -H "Authorization: Bearer $CRABBOX_COORDINATOR_ADMIN_TOKEN" \
  https://broker.example.com/v1/admin/azure-orphan-sweep
```

The sweep uses the configured Azure resource group and the Azure orphan-sweep
settings listed above. See [Lifecycle and Cleanup](features/lifecycle-cleanup.md)
for its inventory, quarantine, and deletion rules.

## AWS Security Guardrails

Apply the cheap account-wide guardrails before adding heavier audit services:

```sh
account_id="$(aws sts get-caller-identity --query Account --output text)"

aws s3control put-public-access-block \
  --account-id "$account_id" \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

aws iam update-account-password-policy \
  --minimum-password-length 14 \
  --require-symbols \
  --require-numbers \
  --require-uppercase-characters \
  --require-lowercase-characters \
  --allow-users-to-change-password \
  --max-password-age 90 \
  --password-reuse-prevention 24
```

S3 account-level Block Public Access and the IAM account password policy are account-wide. IAM Access Analyzer external-access analyzers are regional, so create one in each AWS capacity region:

```sh
for region in eu-west-1 eu-west-2 eu-central-1 us-east-1 us-west-2; do
  if ! aws accessanalyzer get-analyzer \
    --region "$region" \
    --analyzer-name crabbox-external-access >/dev/null 2>&1; then
    aws accessanalyzer create-analyzer \
      --region "$region" \
      --analyzer-name crabbox-external-access \
      --type ACCOUNT
  fi
done
```

List active external-access findings across the same pool:

```sh
for region in eu-west-1 eu-west-2 eu-central-1 us-east-1 us-west-2; do
  arn="$(aws accessanalyzer get-analyzer \
    --region "$region" \
    --analyzer-name crabbox-external-access \
    --query 'analyzer.arn' \
    --output text)"
  aws accessanalyzer list-findings \
    --region "$region" \
    --analyzer-arn "$arn" \
    --filter '{"status":{"eq":["ACTIVE"]}}'
done
```

Do not treat these as spend caps or compliance audit trails. CloudTrail, AWS Config, Security Hub, and GuardDuty are separate choices with different cost and retention tradeoffs. See [Security](security.md).

## Cost Guardrails

The coordinator reserves worst-case lease cost before provisioning. A request that would exceed active-lease or monthly cost limits fails (HTTP 429 `cost_limit_exceeded`) before any VM is created.

```sh
bin/crabbox usage
bin/crabbox usage --scope user --user alice@example.com
bin/crabbox usage --scope org --org example-org
bin/crabbox usage --scope all --json
```

Cost is an estimate for compute leases, not an invoice. See [Cost and Usage](features/cost-usage.md).

## Default Container Image Pins

The Local Container and Apple Container defaults are reviewed, tagless OCI
index references in `recipes/bootstrap/v1/os-catalog.json` (`ContainerName`).
One index fixes the supported platform manifests transitively while preserving
native architecture selection. Explicit custom image settings remain operator
controlled. These pins constrain the base filesystem, not packages subsequently
installed from authenticated distribution repositories.

Reviewed on 2026-08-30 against Docker Hub's official `library/ubuntu` manifests:

| OS selector | Reviewed index digest |
| --- | --- |
| `ubuntu:24.04` | `sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517` |
| `ubuntu:26.04` | `sha256:2260313b31c8c011cd2eebe728008efac1b3982be73eb71348ea2648d2c0e09b` |

Review these pins before each release and when upstream image security fixes
require an earlier update. Rotate through an ordinary reviewed PR:

1. Resolve the official Ubuntu tag's index, verify the raw manifest SHA-256,
   then verify the referenced Linux amd64 and arm64 manifests/configurations.
   Check their Ubuntu version, platform, entrypoint, and publication metadata;
   retain the digest evidence with the PR. Do not substitute a tag for a missing
   or unavailable digest.
2. Update only the intended `ContainerName` catalog entries, the table above,
   and the review date. `DockerImage` is a separate provider setting and is not
   part of this policy. Run `node scripts/generate-bootstrap.mjs`; never edit
   generated Go or TypeScript catalog output by hand.
3. Run the generator checks, configuration/provider regressions, and a real
   create/bootstrap/command/cleanup smoke on available supported runtimes.
   Check the actual selected platform and image identity. For Apple Container,
   verify the created configuration's index digest before `start`, and prove
   that missing/mismatched metadata cannot reach bootstrap. Disclose unavailable
   native coverage rather than claiming it from fixture tests.

Existing leases keep their original image. New leases use the new pin, while a
fixed-ID replay can reject a changed default as a different creation intent.
Pins never refresh silently at build time or runtime. See Docker's
[image-digest model](https://docs.docker.com/dhi/explore/security-concepts/digests/)
and the [Apple Container lifecycle](providers/apple-container.md#configuration).

## Tart Default Image

The built-in macOS Sequoia image is pinned by `DefaultTartImage` in
`internal/cli/config.go`. Its raw OCI manifest and SHA-bound VM configuration
are stored in `internal/providers/tart/images`. The Tart adapter verifies the
cloned disk, NVRAM, and configuration before configuration or boot; neither a
digest-shaped cache path nor a successful download is sufficient proof.

Review this pin before each release and after relevant upstream security fixes.
Resolve the publisher's intended macOS Sequoia image over authenticated HTTPS,
hash the raw manifest bytes, and fetch that same digest to confirm identical
bytes. Fetch and hash the referenced VM config blob, then update the constant,
both metadata files, and provider documentation together. Do not reformat the
metadata files: their exact bytes are part of the reviewed identity.

Validate a fresh pull and a warm-cache clone using native Tart. Run a real SSH
workload and stop the lease; verify tampered configuration, NVRAM, disk chunks,
wrong disk size, and suspended state cannot reach VM configuration or startup.
Measure verification time because each new default clone reads its whole disk.
Record the upstream manifest digest, metadata hashes, native Tart version,
macOS workload result, and cleanup evidence in the PR. Preserve explicit custom
image references and never silently fall back to a mutable tag.

## Release Checklist

The authoritative serialized release contract is [Release engineering](RELEASING.md).
One explicit full release/publish request authorizes the complete normal sequence
through closeout, without renewed chat approval at each stage. Narrow requests
stay narrow. The original request supplies authorization; GitHub events alone
do not. No event automatically publishes a tag. Publication makes the release eligible
for the ordinary tap updater and independent generic reconciliation. Technical
gates, identity binding, credential isolation, immutability, exact frozen inputs,
actual exclusive-writer coordination, and cancellation boundaries still apply.

Before creating or reusing a signed release tag:

- Rebase release preparation on the current `main`, restore missing published history from the latest tag while preserving `Unreleased` and other new entries, and verify every published version remains represented.
- Finalize the `Unreleased` entries maintained as work lands into a versioned, dated release section in `CHANGELOG.md`, with user-facing changes first and contributor thanks / co-author notes intact.
- Update every package metadata file that carries the project version. The current release surface is `worker/package.json` plus both root package entries in `worker/package-lock.json`; the removed root plugin package must not be recreated.
- `go vet ./...`
- `go test -race -timeout=15m ./...`
- `scripts/test-go-modules.sh`
- `scripts/verify-go-install.sh v0.0.0 "$(git rev-parse HEAD)"`
- `go build -trimpath -o bin/crabbox ./cmd/crabbox`
- `scripts/check-go-coverage.sh 90.0`
- Worker gate: `npm run format:check --prefix worker && npm run lint --prefix worker && npm run check --prefix worker && npm test --prefix worker && npm run build --prefix worker`
- `node scripts/generate-linux-readiness.mjs --check`
- `node --test scripts/*.test.js scripts/*.test.mjs`
- `scripts/check-docs.sh`
- `git diff --check`
- Live smoke at least one coordinator-backed `crabbox run`, then verify `crabbox attach`, `crabbox events`, `crabbox logs`, and lease cleanup.
- Push, pull, and wait for CI green on the release commit.

Then advance sequentially under that authorization as each technical gate passes:

1. **Tag trust.** Create or reuse an annotated signed `vX.Y.Z` tag. Verify it
   against the repository-pinned signer policy, capture the tag-object and
   peeled commit IDs, and require that commit to be an ancestor of the current
   protected `main`. If a valid tag already exists, preserve it; never move or
   recreate it merely because verifier hardening landed later.
2. **Local candidate.** Build the exact eight-asset payload described in
   [Release engineering](RELEASING.md#immutable-release-record). Ordinary builds
   remain credential-free. The macOS producer uses the managed release keychain
   to sign both native CLI architectures, the Apple Silicon helper, and its
   embedded VMD as
   `Developer ID Application: OpenClaw Foundation (FWJYW4S8P8)`, with hardened
   runtime and secure timestamps, then requires accepted notarization and
   online `codesign --check-notarization` proof before packaging.
3. **Private draft.** After local verification succeeds, create exactly one
   GitHub draft for the captured pre-existing signed tag. Its title, exact eight
   assets, and body copied byte-for-byte from the tagged `CHANGELOG.md` section
   are immutable candidate inputs.
4. **Native draft verification.** Dispatch only the protected-default verifier
   for that numeric draft ID and pinned tag identities. Its Apple Silicon and
   Intel jobs download assets with narrowly scoped credentials, remove all API,
   Actions, OIDC, and Homebrew credentials, then verify and execute the matching
   candidates in a clean environment.
5. **Publication.** Establish and verify the administrative freeze of all release
   writers required by [Release engineering](RELEASING.md#serialized-gates);
   the release request is not evidence that the freeze is active. Re-read and
   compare the unchanged draft, successful native proofs, tag, protected verifier
   SHA, notes, asset IDs, sizes, and digests. Publication is a single draft-state
   transition; it does not rebuild, replace, or delete anything.
6. **Homebrew update.** Publication establishes eligibility. Explicitly dispatch
   the tap's ordinary `update-formula.yml` with `formula=crabbox`, the tag,
   `repository=openclaw/crabbox`, and the four-target `assets` JSON constructed
   by the runnable [handoff](RELEASING.md#operator-command-sequence). Do not wait
   for public native or Go smoke results. The updater owns all-four URL/hash
   maintenance and preserves maintained formula code. Retry the same handoff
   after a failure; an already-current update is success. Never rebuild,
   recreate a draft, or republish to retry Homebrew. Generic tap reconciliation
   remains a valid fallback.
7. **Independent channel smokes.** Run public-download/native verification,
   fresh proxy-only public Go installation, and the installed-Homebrew verifier
   independently. Homebrew needs only tag, assets, tag object, source commit,
   verifier commit, and release ID, not public run IDs or proof ZIPs. Before
   formula evaluation it checks immutable public bytes and static provenance.
   Tap maintainers own executable Ruby, evaluated only credential-free; native
   structured metadata must match the exact formula identity, version, URL,
   and checksum. This is not a Ruby sandbox. Fresh fetch/install or reinstall,
   installed-byte, signature/notarization, architecture, version, and arm64 VMD
   trust checks are bounded smokes; they do not authorize unrelated provider mutations.
8. **Closeout.** Record publication, tap update, and independent smoke results
   (including outstanding failures). Verify release notes match the finalized
   release section in the changelog. Keep later user-visible work under
   `Unreleased` on `main`, without rewriting the frozen tagged source or
   published notes for downstream retries. Finish authorized release commits
   and leave the intended checkout clean and synchronized.

On cancellation, stop this operator’s release and tap writes. Cancellation
cannot stop independent reconciliation of an already-public release. Before
publication, a failed gate or uncertainty also stops release writes.
Inspect and record the exact draft/public release and tap state, but do not
delete a partial draft or release, replace assets, rewrite the tag, redispatch, publish, or
update Homebrew while stopped. Explicit cancellation requires renewed direction
authorizing the next mutation. For a failed gate or uncertain state, resolve the
blocker and re-establish the exact frozen state and required proofs before
continuing under the original release authorization.

### Durable provisioning record diagnostics

The private `provisioning-quarantine:` namespace records unsupported operation
schemas or inconsistent attempt revisions. Their original operation, attempt and
material records remain untouched, and continue to fence legacy cleanup. The
controller removes their runnable due entry so unrelated jobs can progress.
Inspect these records with the corresponding implementation version before any
manual repair; deleting a marker or changing a schema number does not establish
provider ownership or successful cleanup. Stale due entries without a matching
live operation are removed transactionally during the bounded controller tick.

Nonsecret plan/attempt histories and exact completed Azure deletion claims are
retained alongside lease history without automatic pruning. Do not remove
retained or unresolved histories to clear a cleanup incident. The shared Azure
scope lock is released after settled terminal/retained completion; an unresolved
shared-infrastructure write intentionally keeps its lock pending resolution.
