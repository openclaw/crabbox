# Portable Coordinator

Read this when you want to run the Crabbox coordinator on an ordinary container
platform instead of Cloudflare Workers, or when you are reviewing the runtime,
database, ingress, and failure-recovery requirements of that deployment.

The portable coordinator is a shipped Node.js 22 service backed by PostgreSQL.
It runs the same `FleetCoordinator` behavior, routes, portal, and bridge
protocols as the Cloudflare Worker deployment. The runtime choice changes how
state, alarms, HTTP, and WebSockets are hosted; it does not change the CLI or
provider contracts.

## Runtime choices

| Concern | Cloudflare runtime | Portable runtime |
| --- | --- | --- |
| HTTP and portal | Worker | Node.js HTTP server |
| Durable records | Durable Object storage | PostgreSQL key/value compatibility table |
| Scheduled cleanup | Durable Object alarms and cron | pg-boss delayed jobs and reconciliation schedule |
| WebSockets | Durable Object sockets | `ws` sockets owned by the Node process |
| Mutation ordering | Durable Object event ordering | explicit lifecycle mutex |
| Packaging | Wrangler deployment | OCI image from `worker/Dockerfile.node` |

Choose Cloudflare when its Worker, Durable Object, routing, and Access products
already fit the deployment. Choose Node/PostgreSQL when the coordinator must
run on a conventional container platform, use an existing PostgreSQL service,
or sit behind an identity-aware reverse proxy.

## Topology

```text
Crabbox CLI ---- HTTPS / WebSocket ---- ingress ---- Node coordinator
     |                                               |        |
     +---- SSH + rsync directly to runner -----------+        +---- PostgreSQL
                                                              +---- pg-boss
```

The coordinator remains a control plane. SSH readiness, repository sync,
command execution, and stdout/stderr streaming stay direct between the CLI and
the runner. The coordinator handles authentication, leases, run records,
sharing, usage, cleanup, and optional live bridges.

## Runtime boundary

Shared coordinator behavior depends on the `CoordinatorRuntime` interface:

- key/value `get`, `put`, `delete`, and prefix `list` storage;
- serialized lifecycle operations through `runExclusive`;
- WebSocket upgrade, attachment, enumeration, and event handling;
- alarm scheduling and cancellation.

`CloudflareCoordinatorRuntime` maps those operations to Durable Object APIs.
`NodeCoordinatorRuntime` maps them to PostgreSQL, pg-boss, a process mutex, and
the Node WebSocket server. Routes and fleet behavior stay in shared modules.

## Build and run

Build from the repository root:

```sh
npm ci --prefix worker
npm run check:node --prefix worker
npm run build:node --prefix worker
docker build -f worker/Dockerfile.node -t crabbox-coordinator:local worker
```

Run the built service with a protected environment file populated by the
platform secret manager:

```sh
docker run --rm -p 8080:8080 \
  --env-file /secure/path/crabbox-coordinator.env \
  crabbox-coordinator:local
```

The image exposes port `8080` and starts `dist-node/server.mjs`. Terminate TLS
at the ingress and forward WebSocket upgrades without buffering. The protected
file supplies the variables below, including a `DATABASE_URL` with
`sslmode=verify-full` and a trusted CA for any remote database. Keep database
credentials and bearer tokens out of shell history and process arguments.

## Required platform resources

The initial production shape is intentionally small:

- one always-on coordinator replica;
- one PostgreSQL 13 or newer database reachable through `DATABASE_URL`;
- durable PostgreSQL storage and backups;
- an HTTPS ingress that supports WebSocket upgrades;
- secret injection for coordinator auth and any managed-provider credentials;
- outbound access to configured provider APIs;
- an HTTP timeout long enough for persistent WebSockets.

Do not scale the Node service above one replica yet. Durable records and jobs
are shared, but active socket ownership and lifecycle serialization are still
process-local. Multi-replica operation requires distributed lifecycle locking
and bridge-owner routing.

## Configuration groups

The portable runtime consumes the same coordinator and provider variables as
the Worker, plus Node-specific settings.

Core runtime:

```text
DATABASE_URL                         required PostgreSQL connection string; use verified TLS remotely
PORT                                 listener port, default 8080
CRABBOX_PUBLIC_URL                   canonical external origin
CRABBOX_DATABASE_POOL_SIZE           PostgreSQL pool size, default 10
CRABBOX_DATABASE_CONNECT_TIMEOUT_MS  connection timeout, default 10000
CRABBOX_SHUTDOWN_TIMEOUT_MS          graceful shutdown budget, default 120000
```

Authentication requires at least one supported coordinator identity path. Use
signed user sessions, a shared operator token, an admin token, or a trusted
reverse-proxy identity as documented in [Coordinator](coordinator.md) and
[Security](../security.md). Managed provider credentials remain environment
secrets; registered direct leases need no provider credentials in the service.

## PostgreSQL state

Startup creates the `crabbox` schema and a compatibility table named
`coordinator_kv`. Existing coordinator keys such as leases, runs, bridge
tickets, image records, and cleanup records remain unchanged. Values are stored
as JSONB with a text-preserving representation for exact compatibility.

pg-boss uses its own `crabbox_jobs` schema. It owns:

- the next exact fleet alarm;
- retryable alarm delivery;
- a reconciliation job every 15 minutes;
- a startup reconciliation pass after deployment or restart.

Back up both schemas together. If coordinator records are intact but job state
is missing, startup and recurring reconciliation can rebuild scheduling from
those records. Job state alone cannot reconstruct missing leases, runs, or
cleanup obligations; recover `crabbox.coordinator_kv` before resuming the
service or managed resources may be stranded. Provider operations and cleanup
are idempotent, so repeated job delivery is expected and safe.

## Lifecycle serialization

Node does not inherit Durable Object event ordering, so lifecycle mutations use
an explicit process mutex. Slow provider calls do not hold that mutex.
Provisioning follows three phases:

1. reserve the lease and cost under the lifecycle lock;
2. call the provider outside the lock;
3. finalize the lease or persist cleanup-required failure state under the lock.

This keeps heartbeats, releases, portal requests, and bridge traffic responsive
while a provider is slow. A release racing an in-flight create remains
recoverable because the reservation exists before provider I/O begins.

HTTP routes are classified as lifecycle or direct operations. Direct routes
perform their own smaller critical sections. Bridge data frames bypass the
global lifecycle mutex and retain per-socket ordering; control messages that
mutate fleet state stay serialized.

## WebSockets and shutdown

The Node runtime supports the same control, WebVNC, code, and egress bridge
protocols as Cloudflare. It disables per-message compression, caps individual
messages, pings sockets every 30 seconds, and terminates connections that stop
answering.

On `SIGTERM` or `SIGINT`, the service:

1. marks the runtime as shutting down, stops new upgrades, and closes the HTTP
   listener plus idle connections;
2. drains active request handlers and lifecycle operations;
3. closes live sockets with restart code `1012` and drains their handlers;
4. waits for the current alarm, stops pg-boss gracefully, and closes PostgreSQL;
5. waits for the HTTP server to finish closing.

The supervised WebVNC daemon restarts foreground bridge processes and obtains
fresh tickets after interruption. A foreground `crabbox webvnc`, code bridge,
or ordinary egress bridge may exit on coordinator restart; rerun
`crabbox webvnc`, `crabbox code`, or `crabbox egress start` unless that path has
its own supervisor. Ingress rollouts should still use a termination grace
period at least as long as `CRABBOX_SHUTDOWN_TIMEOUT_MS`.

## Trusted reverse proxies

Identity-aware ingress is opt-in. Configure all three:

```text
CRABBOX_TRUSTED_USER_HEADER     header containing the authenticated user
CRABBOX_TRUSTED_USER_ORG        fixed organization for authenticated users
CRABBOX_TRUSTED_PROXY_CIDRS    comma-separated ingress peer CIDRs
```

The service trusts forwarded identity, host, protocol, and caller-address
headers only when the actual TCP peer matches the configured CIDRs. It removes
caller-supplied connection identity and derives the client address from the
rightmost untrusted member of the forwarded chain. The ingress must strip any
incoming copy of the trusted user header before setting its authenticated
value. Set a stable trusted-user organization before creating records; without
it, proxy-authenticated requests use the literal organization `unknown`.

Trust only the actual ingress addresses or a dedicated ingress subnet, and use
network policy to block direct access from other workloads. Do not configure
`0.0.0.0/0`, `::/0`, or an entire shared private network. If the ingress source
range cannot be made stable and narrow, use Crabbox bearer authentication
instead of a trusted identity header.

## Health and request limits

Use these endpoints:

```text
GET /v1/health   liveness; no database query
GET /v1/ready    readiness; verifies PostgreSQL
HEAD /v1/ready   readiness without a response body
```

The Node server limits unauthenticated bodies to 1 MiB, normal authenticated
bodies to 16 MiB, and run-finish payloads to 64 MiB. Oversized requests return
`413` and close the connection. Configure the ingress with limits at least as
large as these application limits, but do not make unauthenticated limits
larger.

## Deployment checklist

Before serving users:

- create a dedicated PostgreSQL database and least-privilege role;
- enable database backups and test a restore;
- deploy exactly one service replica;
- use a Recreate/no-surge rollout so old and new processes never overlap;
- enable TLS and WebSocket upgrades at ingress;
- keep request buffering disabled for upgrades;
- inject credentials through the platform secret store, never image layers or argv;
- require verified TLS and a trusted CA for remote PostgreSQL;
- configure canonical public URL and auth;
- configure exact trusted ingress peers and block direct application access;
- set a termination grace period for orderly shutdown;
- verify liveness and readiness separately;
- verify unauthenticated requests fail closed;
- create, heartbeat, inspect, share, and release a test lease;
- connect and reconnect one WebVNC or code bridge;
- restart the service and confirm records and scheduled cleanup survive.

## Upgrade and rollback

The Node and Cloudflare adapters share the same logical record keys, but the
repository does not ship supported Durable Object export or PostgreSQL import
tooling. Stateful cross-runtime migration is therefore unsupported. Do not move
active leases, tickets, jobs, or pending cleanup between runtimes. A fresh
cutover is safe only after draining and releasing all resources and verifying
that the old coordinator has no remaining cleanup obligations.

Ordinary Node image upgrades are simpler. Back up PostgreSQL, deploy the new
image with Recreate/no-surge semantics, wait for the old process to finish its
graceful shutdown before starting the replacement, then wait for `/v1/ready`
and verify startup reconciliation. Roll back the image with the same
non-overlapping sequence and without rolling back the database unless release
notes explicitly require it.

## Verification from source

```sh
npm run format:check --prefix worker
npm run lint --prefix worker
npm run check --prefix worker
npm run check:node --prefix worker
npm test --prefix worker
npm run build:node --prefix worker
npm run build --prefix worker
```

The Worker test suite covers shared fleet behavior. Node-specific tests cover
PostgreSQL storage, request handling, proxy trust, WebSockets, lifecycle
ordering, maintenance, and shutdown.

## Related documentation

- [Coordinator](coordinator.md)
- [Bring Your Own Infrastructure](bring-your-own-infrastructure.md)
- [Infrastructure](../infrastructure.md)
- [Security](../security.md)
- [Browser portal](portal.md)
- [Lifecycle cleanup](lifecycle-cleanup.md)
