# Infrastructure

Read this when you stand up, audit, or operate a self-hosted Crabbox broker: the
Cloudflare or Node.js/PostgreSQL coordinator, its secrets, the brokered providers
(Hetzner, AWS, Azure, GCP), and the network front door.

Crabbox runs in three modes (see [How It Works](how-it-works.md)). A *broker* is
only required for **brokered mode**, where lease lifecycle, cost limits, cleanup,
sharing, and `crabbox usage` are owned by the coordinator. Direct and delegated
providers run straight from the CLI and need none of this. The four brokerable
providers are `hetzner`, `aws`, `azure`, and `gcp`; even those run direct unless a
coordinator URL is configured.

Use neutral placeholders below — `broker.example.com`, `example-org`,
`alice@example.com`. Replace them with your own values. Keep every secret out of
the repository.

## Choose A Coordinator Runtime

Both runtimes execute the same `FleetCoordinator`, provider adapters, API,
GitHub login, portal, cost controls, cleanup rules, and WebSocket protocols.

| Runtime | Durable state | Scheduling | Typical deployment |
| --- | --- | --- | --- |
| **Cloudflare Workers** | One Fleet Durable Object | DO alarms plus a scheduled Worker trigger | Wrangler, workers.dev or a custom route, optional Cloudflare Access |
| **Node.js/PostgreSQL** | PostgreSQL `crabbox` schema | pg-boss `crabbox_jobs` jobs plus reconciliation | Initial single-replica runtime for a container, VM service, or Kubernetes pod behind TLS/WebSocket ingress |

Choose Cloudflare for the smallest operational footprint and native edge
routing. Choose Node/PostgreSQL when the coordinator must run on conventional
infrastructure, use a managed PostgreSQL service, or fit an existing container
platform. Cloudflare is the established deployment; Node/PostgreSQL is newly
shipped and should complete the environment-specific proof checklist in
[Portable Coordinator Runtime](plan/portable-coordinator.md) before production
cutover.

Run one Node replica initially. Lifecycle serialization and live bridge sockets
are process-local even though state and jobs are durable. Also treat a runtime
change as a new deployment: Crabbox does not currently export, import, or
automatically migrate state between Durable Object storage and PostgreSQL.

## Coordinator Endpoints

A deployment needs one canonical public origin:

```text
https://broker.example.com                          # canonical login + automation route
```

A Cloudflare deployment can expose the same Worker on additional routes:

```text
https://broker-access.example.com                   # same Worker behind Cloudflare Access
https://crabbox-coordinator.example.workers.dev     # workers.dev fallback for health checks
```

- `broker.example.com/*` is the stable route for browser login and automation.
  The coordinator still enforces Crabbox auth on every
  non-health route.
- `broker-access.example.com/*` is the same Worker behind a Cloudflare Access
  application, for service-token automation behind an outer Cloudflare gate.
- The workers.dev URL is useful for Cloudflare `/v1/health` checks if custom DNS is
  disrupted.

Point each CLI configuration directly at the final canonical or
Access-protected origin it should authenticate to. Credentialed Go requests
follow only same-origin redirects, and the curl fallback follows no redirects;
do not use a cross-origin redirecting alias as `CRABBOX_COORDINATOR`. Additional
workers.dev or custom domains remain useful as independently verified health
endpoints.

Node deployments should put TLS termination and WebSocket-capable ingress in
front of port `8080` (or `PORT`). Health checks use `/v1/health`; readiness
checks use `/v1/ready`.

See [Broker Auth And Routing](features/broker-auth-routing.md) for the full route
and auth model.

## Cloudflare

The Worker coordinator runs entirely on Cloudflare and provides:

- the HTTPS coordinator endpoint and Worker runtime;
- a single Durable Object (`FLEET`, `idFromName("default")`) holding all lease,
  run, usage, and bridge state;
- optional Cloudflare Access in front of the Access-protected route;
- DNS and custom-domain routing.

The Worker entry, routing, and Durable Object responsibilities are documented in
[Architecture](architecture.md). The cron trigger in `worker/wrangler.jsonc`
(`*/15 * * * *`) wakes the Durable Object every 15 minutes so scheduled cleanup
runs even when no leases are active.

## Node JS And PostgreSQL

The portable runtime runs the same `FleetCoordinator` behavior as an ordinary
Node.js service. PostgreSQL stores the existing coordinator key/value records;
pg-boss stores exact alarms, retries, and the 15-minute reconciliation job.
WebSocket bridges use the same tickets and protocol as Cloudflare.

Requirements:

- Node.js 22.12 or newer;
- PostgreSQL 13 or newer;
- one always-on service replica initially;
- an ingress that supports WebSocket upgrades;
- the same auth, provider, budget, and optional artifact environment variables
  documented below.

Build and run:

```sh
npm ci --prefix worker
npm run check:node --prefix worker
npm run build:node --prefix worker

DATABASE_URL='postgresql://crabbox:password@db.example.com/crabbox?sslmode=verify-full&sslrootcert=/run/secrets/postgres-ca.pem' \
CRABBOX_SHARED_TOKEN=replace-me \
CRABBOX_SHARED_OWNER=alice@example.com \
CRABBOX_DEFAULT_ORG=example-org \
CRABBOX_PUBLIC_URL=https://broker.example.com \
npm run start:node --prefix worker
```

The service creates the `crabbox` and `crabbox_jobs` schemas on startup. Health
and readiness routes:

```text
GET /v1/health
GET /v1/ready
```

On `SIGTERM` or `SIGINT`, the service stops accepting requests and drains active
HTTP, WebSocket, lifecycle, and provisioning operations before closing
PostgreSQL. `CRABBOX_SHUTDOWN_TIMEOUT_MS` bounds that wait and defaults to two
minutes.

Build the container with `worker/` as the build context:

```sh
docker build -f worker/Dockerfile.node -t crabbox-coordinator:local worker
docker run --rm -p 8080:8080 \
  --env-file /secure/path/crabbox.env \
  --mount type=bind,src=/secure/path/postgres-ca.pem,dst=/run/secrets/postgres-ca.pem,readonly \
  crabbox-coordinator:local
```

Long-lived WebSocket clients send periodic pings and must reconnect after
service restarts or ingress drains. Run one replica until bridge ownership is
externalized; PostgreSQL and pg-boss are ready for multiple service processes,
but live bridge sockets remain process-local.

Deployment shapes:

- **Container platform** - run the OCI image as one always-on service and attach
  managed PostgreSQL.
- **VM or bare process** - run `npm run start:node --prefix worker` under a
  service manager after `npm run build:node --prefix worker`.
- **Kubernetes** - use one replica, `Recreate` deployment strategy, readiness on
  `/v1/ready`, liveness on `/v1/health`, WebSocket-capable ingress, and a
  termination grace period longer than `CRABBOX_SHUTDOWN_TIMEOUT_MS`.

Required service settings:

```text
DATABASE_URL                         # PostgreSQL connection string
PORT                                 # optional; default 8080
CRABBOX_PUBLIC_URL                   # canonical external origin
CRABBOX_CODE_ORIGIN_TEMPLATE         # required for browser Code; https://{lease}.code.example.com
CRABBOX_SHUTDOWN_TIMEOUT_MS          # optional; default 120000
```

Put all auth, provider, budget, Tailscale, and artifact settings from this page
in the same service secret/config injection used by the platform. Do not bake
them into the image.

### Trusted reverse-proxy identity

An identity-aware ingress can authenticate browser and API requests without a
second Crabbox login by forwarding the verified user in a header:

```text
CRABBOX_TRUSTED_USER_HEADER=X-Authenticated-User
CRABBOX_TRUSTED_USER_ORG=example-org
CRABBOX_TRUSTED_PROXY_CIDRS=10.42.7.19/32,fd00:1234::19/128
CRABBOX_TRUSTED_PROXY_SECRET=replace-with-a-random-secret
```

The Node runtime accepts the identity only when the connection peer is within
`CRABBOX_TRUSTED_PROXY_CIDRS`. When `CRABBOX_TRUSTED_PROXY_SECRET` is set, the
ingress must also send the same value in `X-Crabbox-Proxy-Secret`; the coordinator
strips that header before routing the request. Enable this only when the ingress
removes caller-supplied identity and secret headers. Use exact proxy addresses or
dedicated subnets, or require the secret when direct coordinator access cannot be
blocked. The forwarded
identity receives non-admin scope; keep `CRABBOX_ADMIN_TOKEN` separate. The
Cloudflare Worker runtime does not expose a trusted socket peer, so use its
verified Access JWT support instead.

`X-Crabbox-Proxy-Secret` is reserved and cannot be used as
`CRABBOX_TRUSTED_USER_HEADER`.

The same peer allowlist controls whether the Node runtime honors forwarded host,
protocol, and client-IP headers. It walks the forwarded-for chain from the socket
inward and uses the nearest address outside the trusted proxy ranges for dynamic
provider ingress rules; direct callers always use the socket peer address.

### GitHub browser login

Browser login uses a GitHub OAuth app owned by your deployment org. Configure the
app callback against your canonical coordinator host:

```text
GitHub org:   example-org
App name:     Crabbox Coordinator
Homepage URL: https://broker.example.com
Callback URL: https://broker.example.com/v1/auth/github/callback
```

The coordinator derives the callback from `CRABBOX_PUBLIC_URL` (falling back to
the request origin). Inject the OAuth app values through the runtime's secret
mechanism:

```text
CRABBOX_GITHUB_CLIENT_ID
CRABBOX_GITHUB_CLIENT_SECRET
CRABBOX_SESSION_SECRET            # signs cbxu_ user tokens; required and distinct from CRABBOX_SHARED_TOKEN
CRABBOX_GITHUB_ALLOWED_ORG       # or CRABBOX_GITHUB_ALLOWED_ORGS (comma-separated)
CRABBOX_GITHUB_ALLOWED_TEAMS     # optional: restrict to org teams (alias CRABBOX_GITHUB_ALLOWED_TEAM)
```

### Cloudflare Access (optional)

To gate the Access-protected route, create a service-token Access application on
`broker-access.example.com` with a `non_identity` policy that includes the CLI
service token. The Worker verifies the Access JWT against:

```text
CRABBOX_ACCESS_TEAM_DOMAIN       # e.g. example-org.cloudflareaccess.com
CRABBOX_ACCESS_AUD               # Access application AUD tag
```

On the CLI side, store the service-token credentials locally as
`CRABBOX_ACCESS_CLIENT_ID` and `CRABBOX_ACCESS_CLIENT_SECRET`, or pass an already
minted Access JWT in `CRABBOX_ACCESS_TOKEN`.

### Tailscale (optional)

For brokered Tailscale reachability, the coordinator mints one ephemeral, pre-approved
auth key per lease and injects it only into cloud-init. Lease records store only
non-secret Tailscale metadata (hostname, FQDN, 100.x address, client version,
device id, state, and tags).

Create a Tailscale OAuth client with only the `auth_keys` scope, limited to the
tags Crabbox may assign (typically `tag:crabbox`), and inject the credentials as
coordinator secrets. Crabbox does not require device-management scope:

```text
CRABBOX_TAILSCALE_ENABLED=1
CRABBOX_TAILSCALE_CLIENT_ID
CRABBOX_TAILSCALE_CLIENT_SECRET
CRABBOX_TAILSCALE_TAILNET=-              # or an explicit tailnet/org
CRABBOX_TAILSCALE_TAGS=tag:crabbox      # requested-tag allowlist/default
CRABBOX_TAILSCALE_INSTALL_MODE=package  # or pinned
CRABBOX_TAILSCALE_VERSION=1.98.4        # pinned mode
CRABBOX_TAILSCALE_SHA256_AMD64=...      # pinned mode
CRABBOX_TAILSCALE_SHA256_ARM64=...      # pinned mode
```

For one tag, assign the same tag to the OAuth client. For multiple tags, either
request the OAuth client's complete tag set or configure `tagOwners` so an OAuth
client tag owns every subset tag Crabbox may request. Prefer one dedicated
deployment-owner tag over broad OAuth permissions. Tailscale rejects unowned subset
requests even when every tag is present in `CRABBOX_TAILSCALE_TAGS`.

Preflight the coordinator with `scripts/live-tailscale-smoke.sh --json`, then verify
end to end with `crabbox warmup --tailscale --network tailscale`. See
[Tailscale](features/tailscale.md).

### Deploy token scope

The Cloudflare token used to deploy the Worker should be scoped to the account
and routes this deployment manages. It needs Workers scripts, Access
applications, Access identity providers, Access keys, DNS records, and zone Worker
routes.

## DNS

Custom-domain path:

1. Manage `broker.example.com` (and `broker-access.example.com`) in the
   deployment Cloudflare account.
2. Proxy `broker.example.com/*` and `broker-access.example.com/*` to the
   `crabbox-coordinator` Worker.
3. Set `CRABBOX_PUBLIC_URL=https://broker.example.com`.
4. Point the GitHub OAuth callback at
   `https://broker.example.com/v1/auth/github/callback`.

Fallback path: use the workers.dev URL for `/v1/health` checks if DNS is
disrupted, and add a fallback custom route only when you need DNS recovery
independent of the canonical host.

## Brokered Providers

Provider credentials live in coordinator secret injection, never in repo
config. Configure at least one brokered provider before inviting users.
Per-provider details are in
[Hetzner](features/hetzner.md), [AWS](features/aws.md),
[Azure](features/azure.md), and the [provider docs](providers/README.md).

### Hetzner

```text
HETZNER_TOKEN                    # project that owns the disposable runners
```

Linux-only. The coordinator provisions through the Hetzner Cloud API directly; `hcloud`
is not required. Default Linux image `ubuntu-24.04`, SSH user `crabbox`, primary
SSH port `2222` with `22` as the ordered fallback. Cloud-init installs only
Crabbox plumbing (OpenSSH, curl/CA certificates, Git, rsync, jq, and a retrying
readiness probe); project runtimes come from Actions hydration or repo-owned
setup. See [Runner Bootstrap](features/runner-bootstrap.md).

### AWS EC2

AWS is the default burst backend. Brokered AWS launches EC2 Spot Linux by
default, can launch managed Windows and WSL2 targets, and can launch EC2 Mac
instances on an operator-provided Dedicated Host. The direct CLI provider remains
available with `--provider aws` when no broker is configured.

Brokered credentials and host pinning (coordinator secrets):

```text
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_SESSION_TOKEN                # optional
CRABBOX_HOST_ID                  # optional; admin-only except owner reactivation of a retained Mac instance
CRABBOX_AWS_MAC_HOST_ID          # optional legacy AWS alias for CRABBOX_HOST_ID
```

AWS-specific coordinator settings (all optional unless noted):

```text
CRABBOX_AWS_REGION                       # default eu-west-1
CRABBOX_AWS_AMI                          # AMI override for the selected target
CRABBOX_AWS_SECURITY_GROUP_ID            # bring your own SG (you own ingress)
CRABBOX_AWS_SUBNET_ID
CRABBOX_AWS_INSTANCE_PROFILE
CRABBOX_AWS_ROOT_GB
CRABBOX_AWS_SSH_CIDRS                     # comma-separated SSH source CIDRs
CRABBOX_AWS_ORPHAN_SWEEP_ENABLED         # defaults on when AWS broker credentials exist
CRABBOX_AWS_ORPHAN_SWEEP_DELETE          # set 1 to terminate coordinator-owned orphan instances
CRABBOX_AWS_ORPHAN_SWEEP_INTERVAL_SECONDS
CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS
```

When no security group is supplied, the AWS provider imports the local SSH public
key as an EC2 key pair, creates or reuses a `crabbox-runners` security group,
launches one-time instances, tags instances and volumes with lease metadata, and
terminates non-kept instances after the command.

SSH ingress is source-scoped. If `CRABBOX_AWS_SSH_CIDRS` is set, those CIDRs are
added; otherwise the CLI sends its detected outbound IPv4 `/32`, and the Worker
falls back to `CF-Connecting-IP` (`/32` or `/128`). Crabbox revokes any legacy
managed `0.0.0.0/0` SSH rule when it touches the managed security group. Supplying
`CRABBOX_AWS_SECURITY_GROUP_ID` makes network policy your responsibility.

#### AWS IAM

Grant the Worker AWS principal EC2 launch/list/tag/terminate permissions for
instances, key pairs, and managed security groups, plus the image lifecycle
permissions (`CreateImage`, `DeregisterImage`, `RegisterImage`,
`DescribeSnapshots`, `DeleteSnapshot`, `DescribeFastSnapshotRestores`,
`EnableFastSnapshotRestores`) and `servicequotas:GetServiceQuota`. The image
permissions cover `crabbox image`, native AWS checkpoints, macOS image bake
validation, and Fast Snapshot Restore promotion. Service Quotas access is
best-effort: when available, Crabbox skips known quota-impossible instance types
before calling `RunInstances`; when missing, launch errors are still classified
after the call.

Print the baseline provider policy with:

```sh
crabbox admin providers policy --provider aws
```

EC2 Mac host bakes need the additional Dedicated Host lifecycle grant (including
`servicequotas:ListServiceQuotas` fallback) printed by:

```sh
crabbox admin providers policy --provider aws --target macos
```

#### EC2 Mac host preflight (no spend)

Before approving paid EC2 Mac host allocation, run the no-spend region preflight
against the coordinator you intend to use:

```sh
CRABBOX_MACOS_REGIONS=eu-west-1,us-east-1,us-west-2 scripts/macos-host-region-preflight.sh
```

It checks `mac2.metal` then `mac1.metal` by default (override with
`CRABBOX_MACOS_TYPE`/`CRABBOX_MACOS_TYPES`; set `CRABBOX_MACOS_TYPES=all` to sweep
every known EC2 Mac family). It returns JSON with `ready-existing-host`,
`ready-allocation`, or `blocked`. For deeper diagnosis, see the
[Image Bake Runbook](features/image-bake-runbook.md) and the no-spend audit
helper:

```sh
scripts/macos-coordinator-remediation-audit.sh --region eu-west-1 --type mac2.metal --profile auto
```

When the only blocker is Dedicated Mac host quota, capture evidence and dry-run
the quota request before submitting:

```sh
crabbox admin providers identity --provider aws --region eu-west-1 --json > provider-identity.json
crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal --json > mac-host-quota.json
scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region eu-west-1 --profile auto
scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region eu-west-1 --profile auto --apply
```

The helper refuses to submit unless the selected AWS profile belongs to the same
account as the deployed coordinator identity, and exits without an AWS request
when the captured quota already meets the requested value.

### Azure and GCP

Azure and GCP are also brokerable. Their coordinator secrets follow the same pattern —
SDK credentials plus `CRABBOX_AZURE_*` / `CRABBOX_GCP_*` placement settings
(location/region, resource group or project, image, network). See
[Azure](features/azure.md) and the per-provider docs for the full set.

## Machine Classes

Leases request a *class* rather than a hardcoded instance type; the broker
resolves a class to an ordered candidate list per provider and target, then tries
them in turn with region/market fallback (see [Capacity
Fallback](features/capacity-fallback.md)). Profiles pick a default class; any
command can override with `--class`. The default class is `beast`.

Hetzner server types per class:

```text
standard  ccx33, cpx62, cx53
fast      ccx43, cpx62, cx53
large     ccx53, ccx43, cpx62, cx53
beast     ccx63, ccx53, ccx43, cpx62, cx53
```

AWS instance types per class:

```text
Linux
standard  c7a.8xlarge, c7i.8xlarge, m7a.8xlarge, m7i.8xlarge, c7a.4xlarge
fast      c7a.16xlarge, c7i.16xlarge, m7a.16xlarge, m7i.16xlarge, c7a.12xlarge, c7a.8xlarge
large     c7a.24xlarge, c7i.24xlarge, m7a.24xlarge, m7i.24xlarge, r7a.24xlarge, c7a.16xlarge, c7a.12xlarge
beast     c7a.48xlarge, c7i.48xlarge, m7a.48xlarge, m7i.48xlarge, r7a.48xlarge, c7a.32xlarge, ...

Windows
standard  m7i.large, m7a.large, t3.large
fast      m7i.xlarge, m7a.xlarge, t3.xlarge
large     m7i.2xlarge, m7a.2xlarge, t3.2xlarge
beast     m7i.4xlarge, m7a.4xlarge, m7i.2xlarge

Windows WSL2
standard  m8i.large, m8i-flex.large, c8i.large, r8i.large
fast      m8i.xlarge, m8i-flex.xlarge, c8i.xlarge, r8i.xlarge
large     m8i.2xlarge, m8i-flex.2xlarge, c8i.2xlarge, r8i.2xlarge
beast     m8i.4xlarge, m8i-flex.4xlarge, c8i.4xlarge, r8i.4xlarge, m8i.2xlarge

macOS (class is ignored; ordered Mac families tried unless --type is set)
mac2.metal, mac2-m2.metal, mac2-m2pro.metal, mac-m4.metal, mac-m4pro.metal,
mac-m4max.metal, mac2-m1ultra.metal, mac-m3ultra.metal, then mac1.metal
```

Azure resolves classes to `Standard_*` VM sizes per target; GCP resolves to
`c4`/`c3`/`n2` families. The authoritative lists live in `worker/src/config.ts`.

## Lease Defaults

The coordinator `LeaseConfig` applies these defaults (`worker/src/config.ts`):

```text
provider       hetzner
class          beast
ttl            5400s   (capped at 86400s)
idle timeout   1800s
ssh port       2222    (fallback 22)
work root      /work/crabbox (linux), C:\crabbox (windows normal),
               /Users/<user>/crabbox (macos)
```

Lease IDs are `cbx_<12 hex>`; signed user tokens are prefixed `cbxu_`. See
[Identifiers](features/identifiers.md).

Each leased machine carries Crabbox label metadata so it is attributable and
sweepable, for example:

```text
crabbox=true
class=beast
lease=cbx_...
slug=swift-crab
owner=<github-login-or-email>
created_at=<unix-seconds>
last_touched_at=<unix-seconds>
ttl_secs=<seconds>
idle_timeout_secs=<seconds>
expires_at=<unix-seconds>
```

## Self-Hosted Broker: Minimum Setup

Use this when you want broker-owned provider credentials, coordinator cleanup,
active-lease limits, monthly spend caps, and `crabbox usage`.

Shared prerequisites:

- a canonical HTTPS origin for the API, portal, OAuth callback, and WebSockets;
- runtime secret injection for auth and at least one brokered provider;
- budget limits sized before inviting users;
- outbound network access to provider, GitHub, Tailscale, and artifact APIs that
  the enabled features use.

Choose Cloudflare Workers/Durable Objects or the single-replica Node/PostgreSQL
runtime. For the container runbook, see
[Portable Coordinator](features/portable-coordinator.md). Design history,
production proof, and remaining scale work are tracked in
[Portable Coordinator Runtime](plan/portable-coordinator.md).

Cloudflare prerequisites:

- a Cloudflare account with Workers and Durable Objects enabled;
- a Worker route or workers.dev URL for the coordinator;
- the Durable Object binding from `worker/wrangler.jsonc` (`FLEET` ->
  `FleetDurableObject`);
- the scheduled trigger from `worker/wrangler.jsonc`.

Node/PostgreSQL prerequisites:

- Node.js 22.12+ or the image from `worker/Dockerfile.node`;
- PostgreSQL 13+ reachable through a TLS-verified `DATABASE_URL` for remote
  databases;
- one always-on service replica;
- TLS ingress with WebSocket upgrades and health/readiness probes.

Pick an auth model:

- **Browser login** — create the GitHub OAuth app (above) and set
  `CRABBOX_GITHUB_CLIENT_ID`, `CRABBOX_GITHUB_CLIENT_SECRET`,
  `CRABBOX_SESSION_SECRET`, and `CRABBOX_GITHUB_ALLOWED_ORG[S]`.
- **Shared-token automation** — set `CRABBOX_SHARED_TOKEN` and
  `CRABBOX_SHARED_OWNER`. GitHub OAuth is not required if every caller runs
  `crabbox login --url <your-url> --token-stdin`.
- **Admin token** — set `CRABBOX_ADMIN_TOKEN` for admin routes and image
  promotion.

Recommended limits for a small installation:

```text
CRABBOX_MAX_ACTIVE_LEASES=2
CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER=1
CRABBOX_CAPACITY_ADMIN_OWNERS=alice@example.com,bob@example.com
CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN=4
CRABBOX_MAX_MONTHLY_USD=25
CRABBOX_MAX_MONTHLY_USD_PER_OWNER=10
```

Per-org caps (`CRABBOX_MAX_ACTIVE_LEASES_PER_ORG`,
`CRABBOX_MAX_MONTHLY_USD_PER_ORG`) and elevated capacity-admin owner caps are
also available. Over-limit lease creation returns HTTP 429
`cost_limit_exceeded`. Cost is the hourly rate (`CRABBOX_COST_RATES_JSON`
override -> provider live price -> built-in defaults) times TTL; see
[Cost And Usage](features/cost-usage.md).

After deployment, point the CLI at the broker:

```sh
crabbox login --url https://broker.example.com --provider aws
crabbox doctor
crabbox usage
```

## Cloudflare Deployment

Worker source lives in `worker/`. Run the CI-equivalent gate, then deploy with
Wrangler (use `npx wrangler` unless `wrangler` is installed globally):

```sh
npm ci --prefix worker
npm run format:check --prefix worker
npm run lint --prefix worker
npm run check --prefix worker
npm test --prefix worker
npm run build --prefix worker
npx wrangler deploy --config worker/wrangler.jsonc
```

A full deploy should:

1. build the Worker;
2. create or update the Durable Object binding;
3. set Worker secrets;
4. deploy the Worker;
5. verify `/v1/health` on the workers.dev URL;
6. attach the route/custom domain on `broker.example.com`;
7. verify `/v1/health` on the canonical and fallback domains.

The `scripts/deploy-worker-smoke.sh` and `scripts/deploy-cloudflare-smoke.sh`
helpers cover post-deploy verification.

The equivalent Node build, container, readiness, shutdown, and ingress contract
is in [Node.js And PostgreSQL](#node-js-and-postgresql).

### Coordinator secrets and settings reference

```text
# Providers (at least one set)
HETZNER_TOKEN
AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN (optional)
CRABBOX_HOST_ID / CRABBOX_AWS_MAC_HOST_ID (optional; admin-only except owner reactivation of a retained Mac instance)
AZURE_* / CRABBOX_AZURE_* (Azure)
GCP_* / CRABBOX_GCP_* (GCP)

# Auth
CRABBOX_SHARED_TOKEN, CRABBOX_SHARED_OWNER
CRABBOX_ADMIN_TOKEN                       # admin routes + image promotion
CRABBOX_GITHUB_CLIENT_ID, CRABBOX_GITHUB_CLIENT_SECRET
CRABBOX_GITHUB_ALLOWED_ORG[S], CRABBOX_GITHUB_ALLOWED_TEAMS (optional)
CRABBOX_GITHUB_ADMIN_OWNERS, CRABBOX_GITHUB_ADMIN_LOGINS (optional)
CRABBOX_SESSION_SECRET
CRABBOX_DEFAULT_ORG
CRABBOX_ACCESS_TEAM_DOMAIN, CRABBOX_ACCESS_AUD   # Cloudflare Access route
CRABBOX_TRUSTED_USER_HEADER, CRABBOX_TRUSTED_USER_ORG
CRABBOX_TRUSTED_PROXY_CIDRS              # Node runtime peer allowlist
CRABBOX_PUBLIC_URL                       # canonical coordinator URL for OAuth callback
CRABBOX_CODE_ORIGIN_TEMPLATE             # required for browser Code; per-lease HTTPS origin template

# Cost / limits
CRABBOX_MAX_ACTIVE_LEASES[_PER_OWNER|_PER_ORG]
CRABBOX_MAX_MONTHLY_USD[_PER_OWNER|_PER_ORG]
CRABBOX_COST_RATES_JSON, CRABBOX_EUR_TO_USD

# Tailscale (optional)
CRABBOX_TAILSCALE_ENABLED
CRABBOX_TAILSCALE_CLIENT_ID, CRABBOX_TAILSCALE_CLIENT_SECRET
CRABBOX_TAILSCALE_TAILNET, CRABBOX_TAILSCALE_TAGS

# Artifacts storage (optional; storage-only S3-compatible keys)
CRABBOX_ARTIFACTS_BACKEND, CRABBOX_ARTIFACTS_BUCKET, CRABBOX_ARTIFACTS_PREFIX
CRABBOX_ARTIFACTS_BASE_URL, CRABBOX_ARTIFACTS_REGION, CRABBOX_ARTIFACTS_ENDPOINT_URL
CRABBOX_ARTIFACTS_ACCESS_KEY_ID, CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY
CRABBOX_ARTIFACTS_SESSION_TOKEN (optional)
CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS, CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS
```

Artifact credentials on the coordinator are storage-only S3/R2-compatible keys.
They let the coordinator sign one upload URL per artifact and return the final asset
URL; they are not Cloudflare deploy tokens, Crabbox bearer/admin tokens, or VM
provider credentials. Normal artifact publishing should go through the
coordinator; keep direct local S3/R2 credentials as an operator fallback only.
See [Artifacts](features/artifacts.md).

## Verification

After a deployment or before broad changes, run the live smoke against a repo
checkout you control:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

It exercises brokered AWS, direct Hetzner, a delegated runner, slug reuse,
`status`/`inspect`/`cache`/`history`/`logs`, `stop`, and final active-lease
cleanup. Auth- and doctor-only smokes are in `scripts/live-auth-smoke.sh` and
`scripts/live-doctor-smoke.sh`.

For operating the deployment day to day, see [Operations](operations.md),
[Observability](observability.md), and [Troubleshooting](troubleshooting.md).
