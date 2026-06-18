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
node --test scripts/*.test.js
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
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=blacksmith-testbox CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=e2b               CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=modal             CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=daytona           CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=namespace-devbox  CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=semaphore         CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=sprites           CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=tenki CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=wandb CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=kubevirt CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_KUBEVIRT_TEMPLATE=/path/to/vm.yaml scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=external CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_EXTERNAL_COMMAND=/path/to/provider scripts/live-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=digitalocean scripts/live-digitalocean-smoke.sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nebius scripts/live-nebius-smoke.sh
```

Per-provider smoke prerequisites:

- **Blacksmith** — a workflow containing a `useblacksmith/testbox`, `useblacksmith/begin-testbox`, or `useblacksmith/run-testbox` step; set `CRABBOX_BLACKSMITH_WORKFLOW` when the default path is wrong.
- **E2B** — `E2B_API_KEY`.
- **Modal** — an authenticated Modal Python client (`python3 -m modal setup` or Modal token env vars).
- **Semaphore** — `CRABBOX_SEMAPHORE_HOST`, `CRABBOX_SEMAPHORE_PROJECT`, and `CRABBOX_SEMAPHORE_TOKEN`, or the equivalent user config.
- **Daytona** — `CRABBOX_DAYTONA_SNAPSHOT`, `DAYTONA_SNAPSHOT`, or `daytona.snapshot`.
- **Namespace** — the authenticated `devbox` CLI on `PATH`.
- **Namespace Compute** — the authenticated `nsc` CLI on `PATH`; run `nsc login` first.
- **Sprites** — the authenticated `sprite` CLI on `PATH` plus a Sprites token in the environment.
- **Tenki** — the authenticated `tenki` CLI on `PATH`; run `tenki login` and complete the browser flow.
- **KubeVirt** — `kubectl`, `virtctl`, a namespace with KubeVirt access, and an SSH-ready VM template.
- **External** — a configured provider executable through `external.command` or `CRABBOX_LIVE_EXTERNAL_COMMAND`.
- **W&B** — `WANDB_ENTITY_NAME` plus `CRABBOX_WANDB_API_KEY` or `WANDB_API_KEY` (from `wandb login`). `scripts/wandb-smoke.sh` is a coordinator-free, wandb-only gate.
- **DigitalOcean** — `DIGITALOCEAN_TOKEN` with account-read, Droplet, image-read, SSH key, and tag scopes. `scripts/live-digitalocean-smoke.sh` is coordinator-free, requires an empty Crabbox-owned inventory, creates a small short-lived Droplet, verifies status and execution, and prints a final cleanup classification.
- **Nebius** — authenticated Nebius CLI profile plus `nebius.parentId` and
  `nebius.subnetId`. `scripts/live-nebius-smoke.sh` is coordinator-free,
  requires explicit `CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=nebius`, creates one
  short-lived CPU-default VM, verifies status and `echo ok`, stops the lease,
  runs dry-run cleanup, and prints a final classification.

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
Use `scripts/live-digitalocean-smoke.sh` for the repeatable direct DigitalOcean
equivalent; it builds `bin/crabbox`, creates a guarded `digitalocean` scratch
config, and verifies the Crabbox-owned inventory is empty before create and
after stop/cleanup.
Use `scripts/live-nebius-smoke.sh` for the repeatable direct Nebius equivalent;
it builds or reuses `bin/crabbox`, uses the documented Nebius config and CLI
profile, creates a unique `nebius-smoke-*` lease, and verifies the slug is absent
after stop and dry-run cleanup.

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

### Minimum coordinator configuration

Configure `CRABBOX_PUBLIC_URL`, one auth model, and at least one brokered
provider. Shared-token automation needs `CRABBOX_SHARED_TOKEN` and
`CRABBOX_SHARED_OWNER`; browser login needs the GitHub OAuth settings below.
Provider choices are `HETZNER_TOKEN`, an AWS credential set, an Azure service
principal, or a GCP service account. Node additionally requires `DATABASE_URL`.

For a shared portal, configure
`CRABBOX_CODE_ORIGIN_TEMPLATE=https://{lease}.code.example.com` and route the
matching wildcard hostname to the same coordinator with TLS and WebSocket
support. This preserves the normal Code links while moving each lease's
proxied HTML and JavaScript to a separate browser origin.

### Conditional coordinator secrets and settings

```text
AWS_SESSION_TOKEN                  optional
CRABBOX_HOST_ID                    optional; pins a brokered host such as an EC2 Mac Dedicated Host
CRABBOX_AWS_MAC_HOST_ID            optional legacy AWS alias for CRABBOX_HOST_ID
CRABBOX_SHARED_OWNER              optional fixed owner identity for shared-token automation
CRABBOX_ADMIN_TOKEN               required for admin routes and image promotion
CRABBOX_WORKSPACE_SSH_PUBLIC_KEY  required for /v1/workspaces lease provisioning
CRABBOX_WORKSPACE_SSH_PRIVATE_KEY required for /v1/workspaces terminal attachment
CRABBOX_WORKSPACE_PROVIDER        optional workspace provider; hetzner, aws, azure, or gcp
CRABBOX_WORKSPACE_CLASS           optional workspace machine class; default standard
CRABBOX_WORKSPACE_PREWARM_COUNT   optional ready spares per active organization; default 0, maximum 4
CRABBOX_GITHUB_CLIENT_ID          required for browser login
CRABBOX_GITHUB_CLIENT_SECRET      required for browser login
CRABBOX_SESSION_SECRET            required for browser login; must differ from CRABBOX_SHARED_TOKEN
CRABBOX_CODE_ORIGIN_TEMPLATE      optional per-lease Code origin isolation
CRABBOX_GITHUB_ALLOWED_ORG or CRABBOX_GITHUB_ALLOWED_ORGS
CRABBOX_GITHUB_ALLOWED_TEAMS      optional
CRABBOX_ACCESS_TEAM_DOMAIN        required for Access JWT verification
CRABBOX_ACCESS_AUD                required for Access JWT verification
CRABBOX_TAILSCALE_CLIENT_ID       required for brokered --tailscale
CRABBOX_TAILSCALE_CLIENT_SECRET   required for brokered --tailscale
CRABBOX_TAILSCALE_TAILNET         optional
CRABBOX_TAILSCALE_TAGS            optional
CRABBOX_TAILSCALE_ENABLED         optional; set 0 to disable brokered Tailscale
CRABBOX_TAILSCALE_INSTALL_MODE    optional; package or pinned
CRABBOX_TAILSCALE_VERSION         optional pinned static build version
CRABBOX_TAILSCALE_SHA256_AMD64    optional pinned amd64 archive checksum
CRABBOX_TAILSCALE_SHA256_ARM64    optional pinned arm64 archive checksum
CRABBOX_ARTIFACTS_BACKEND         optional; enables brokered artifact publishing
CRABBOX_ARTIFACTS_BUCKET          required when artifact backend is enabled
CRABBOX_ARTIFACTS_PREFIX          optional
CRABBOX_ARTIFACTS_BASE_URL        optional; public final artifact URL prefix
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
```

AWS workspace bridges use a dedicated `crabbox-workspaces` security group, separate
from ordinary runner ingress. Workers TCP egress has no published allowlist, so
that group accepts key-only SSH from `0.0.0.0/0`; workspace keys are deployment
specific, host keys are pinned, and leases expire automatically.

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

A typical R2-compatible configuration looks like:

```text
CRABBOX_ARTIFACTS_BACKEND=r2
CRABBOX_ARTIFACTS_BUCKET=my-crabbox-artifacts
CRABBOX_ARTIFACTS_PREFIX=crabbox-artifacts
CRABBOX_ARTIFACTS_BASE_URL=https://artifacts.example.com
CRABBOX_ARTIFACTS_REGION=auto
CRABBOX_ARTIFACTS_ENDPOINT_URL=<account>.r2.cloudflarestorage.com
```

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

## Release Checklist

Before tagging a release:

- Rebase release preparation on the current `main`, restore the full changelog from the latest tag if concurrent work regressed it, and verify every published version remains represented.
- Reorder `CHANGELOG.md` with the user-facing changes first, date the release section, and keep contributor thanks / co-author notes intact.
- Update every package metadata file that carries the project version. The current release surface is `worker/package.json` plus both root package entries in `worker/package-lock.json`; the removed root plugin package must not be recreated.
- `go vet ./...`
- `go test -race ./...`
- `scripts/test-go-modules.sh`
- `go build -trimpath -o bin/crabbox ./cmd/crabbox`
- `scripts/check-go-coverage.sh 90.0`
- Worker gate: `npm run format:check --prefix worker && npm run lint --prefix worker && npm run check --prefix worker && npm test --prefix worker && npm run build --prefix worker`
- `node --test scripts/*.test.js`
- `scripts/check-docs.sh`
- `git diff --check`
- Live smoke at least one coordinator-backed `crabbox run`, then verify `crabbox attach`, `crabbox events`, `crabbox logs`, and lease cleanup.
- Push, pull, and wait for CI green on the release commit.
- Tag and push `vX.Y.Z`, then wait for the release workflow. The workflow publishes GitHub release assets, copies the matching `CHANGELOG.md` section into the GitHub release body, and pushes the generated `Formula/crabbox.rb` update to `openclaw/homebrew-tap` with `HOMEBREW_TAP_GITHUB_TOKEN`; missing tap access is a release failure.
- Verify the GitHub release assets and the Homebrew formula update.
- `brew update`, install or upgrade `openclaw/tap/crabbox`, run `crabbox --version`, and run a short live smoke from the installed binary.
