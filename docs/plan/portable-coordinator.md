# Portable Coordinator Runtime

Status: Node/PostgreSQL and Cloudflare adapters implemented.

Read when:

- evaluating a coordinator deployment outside Cloudflare Workers;
- separating coordinator behavior from Durable Object runtime primitives;
- planning a container, Kubernetes, or managed-service deployment.

Current behavior remains authoritative in
[Coordinator](../features/coordinator.md), [Architecture](../architecture.md),
and `worker/src/`. This document proposes a second runtime; it does not describe
shipped behavior.

## Decision

Add a Node.js coordinator runtime backed by PostgreSQL. Keep the existing
Cloudflare Worker and Durable Object deployment as an adapter over the same
coordinator behavior.

Use:

- PostgreSQL for durable records and coordination;
- [pg-boss](https://github.com/timgit/pg-boss) for delayed maintenance,
  retries, and recurring reconciliation;
- ordinary Node WebSockets for control, WebVNC, code, and egress bridges;
- one service replica initially, with reconnectable WebSocket clients;
- PostgreSQL advisory locks when multiple replicas become necessary.

This is a smaller and more operationally conventional change than adopting an
actor platform. Provider adapters already use portable TypeScript and `fetch`;
the runtime-specific surface is concentrated in Durable Object storage, alarms,
request routing, and WebSocket lifecycle.

## Required Semantics

The portable runtime must preserve:

| Current primitive | Required behavior | Portable implementation |
| --- | --- | --- |
| Durable Object storage | durable key/value records and prefix scans | PostgreSQL JSONB key/value table |
| One Fleet Durable Object | coordinated fleet mutations and cost reservations | one replica first; advisory lock for multi-replica |
| Durable Object alarms | expiry, cleanup retry, orphan sweep | pg-boss delayed and recurring jobs |
| Hibernating WebSockets | live bidirectional bridges | in-process WebSockets with ping, reconnect, and ticket replay |
| Worker entrypoint | auth, OAuth, portal, API routing | Node HTTP server using the existing route handlers |
| Worker secrets | provider and auth credentials | service secret injection |

SSH, rsync, command execution, and output streaming remain direct between the
CLI and runner. The portable coordinator changes only the control plane.

## Runtime Boundary

Runtime-dependent operations are now behind `CoordinatorRuntime`:

```ts
interface CoordinatorRuntime {
  storage: CoordinatorStorage;
  runExclusive<T>(callback: () => Promise<T>): Promise<T>;
  createWebSocketUpgrade(): CoordinatorWebSocketUpgrade;
  getWebSockets(): Iterable<WebSocket>;
  socketAttachment<T>(socket: WebSocket): T | undefined;
  setSocketAttachment(socket: WebSocket, attachment: unknown): void;
  acceptWebSocket(/* socket, attachment, tags, handlers */): void;
  scheduleAlarm(time: number): Promise<void>;
  clearAlarm(): Promise<void>;
}
```

`runExclusive` serializes lifecycle state transitions, alarms, and control
messages. Provider provisioning and bridge data traffic run outside that queue.

The first extraction should preserve the current key format (`lease:*`,
`run:*`, ticket keys, image keys, and cleanup keys). A normalized relational
schema can follow after both runtimes pass the same contract tests.

The Cloudflare adapter wraps `DurableObjectStorage`, alarms, and hibernating
WebSockets. The Node adapter wraps PostgreSQL, pg-boss, and a WebSocket server.

## PostgreSQL Shape

Start with one compatibility table:

```sql
create table coordinator_kv (
  key text primary key,
  value jsonb not null,
  value_text text,
  value_text_updated_at timestamptz,
  updated_at timestamptz not null default now()
);

create index coordinator_kv_updated_at_idx
  on coordinator_kv (updated_at);
```

Use parameterized prefix queries. Keep pg-boss in its own schema. Do not place
provider credentials in coordinator records.

The initial single-replica runtime serializes fleet lifecycle mutations with a
process queue. Lease creation uses the same queue for reservation and
finalization while provider provisioning runs outside it. A provider call
therefore cannot stall heartbeats, releases, control messages, or proxied code
traffic, while concurrent creates still make cost reservations in order. Before
enabling multiple replicas, replace the process queue with PostgreSQL advisory
locks for each fleet mutation. Provisioning is a phased state transition:

1. lock, validate limits, and persist a `provisioning` reservation;
2. unlock and call the provider;
3. lock and finalize the lease or record cleanup-required failure state.

The lease record is persisted before the cloud API call and checked again before
finalization, so concurrent release remains recoverable without holding a
database transaction across provisioning.

## Maintenance

Use a pg-boss `maintenance` job as the alarm equivalent:

- schedule it for the earliest lease expiry, cleanup retry, or provider sweep;
- let every lease mutation recompute the next due time;
- use a recurring reconciliation job as a backstop after deploys or crashes;
- make cleanup idempotent and retain the existing retry metadata;
- acquire the fleet mutation lock before each maintenance pass.

A process restart must not lose an expiry or cleanup request. Job delivery may
repeat, so handlers must remain idempotent.

## WebSockets

WebSocket upgrade creation, attachment storage, socket enumeration, and
lifecycle handlers are behind `CoordinatorRuntime`. The Cloudflare adapter
retains hibernating sockets and the existing non-hibernating fallback.

Portable deployments must assume that proxies and service rollouts can close
long-lived connections:

- ping at least every 30-60 seconds;
- reconnect with bounded exponential backoff;
- mint short-lived, one-use bridge tickets as today;
- restore logical subscriptions after reconnect;
- treat the in-memory socket registry as disposable state.

Run one replica until bridge ownership is externalized. Multi-replica options,
in preferred order:

1. reconnect clients to the replica owning the bridge;
2. use PostgreSQL only to discover the owning replica, then relay over an
   internal service connection;
3. add a binary-capable message bus when bridge volume justifies it.

## Packaging

Produce one OCI image containing:

- the Node HTTP/WebSocket service;
- database migrations;
- health and readiness routes;
- the existing portal assets;
- no embedded credentials.

Required deployment resources:

- managed PostgreSQL;
- secret injection for coordinator, OAuth, and provider credentials;
- outbound access to provider APIs;
- an ingress that supports WebSocket upgrades;
- one always-on replica for the initial release.

The portal should remain served by the coordinator initially. A static-site
host can serve a later extracted frontend, but cannot replace the coordinator
API, scheduler, durable state, or WebSocket bridges.

## Alternatives

### Rivet Actors

[Rivet Actors](https://rivet.dev/docs/actors/) is the closest open-source
semantic match: durable actors, state, timers, and WebSockets, with
self-hosting on containers or Kubernetes.

It is a useful spike target, but not the default:

- it introduces a new runtime and control plane;
- the current single global Fleet object becomes a large "god actor";
- scaling well would require partitioning by lease, run, or bridge;
- PostgreSQL is already sufficient for Crabbox's control-plane volume.

Reconsider Rivet if Crabbox intentionally adopts per-lease actors or needs
actor placement and hibernation across many replicas.

### Restate, Temporal, or Dapr Actors

These are strong durable-workflow or actor systems. They do not remove the need
for a separate WebSocket service and would require a larger rewrite than the
current coordinator needs. Reconsider one when lease lifecycle becomes a
multi-service workflow with long-running compensation and audit requirements.

### workerd

[workerd](https://github.com/cloudflare/workerd) can self-host Worker APIs, but
it is not by itself a production replacement for distributed Durable Object
storage, alarms, and hibernating sockets. It helps API compatibility, not the
coordinator's durable control-plane problem.

## Implementation Phases

### Phase 1: contracts

- [x] Add storage, scheduler, socket, and upgrade interfaces.
- [x] Wrap the current Durable Object implementation without changing behavior.
- [x] Run the Worker tests through the Cloudflare adapter.

### Phase 2: portable runtime

- [x] Add PostgreSQL storage and migrations.
- [x] Add Node HTTP and WebSocket entrypoints.
- [x] Add pg-boss maintenance and reconciliation jobs.
- [ ] Run the complete coordinator API suite against both runtimes.

### Phase 3: deployment proof

- Build the OCI image.
- Deploy one replica with managed PostgreSQL.
- Exercise auth, lease create/heartbeat/release, restart recovery, cleanup,
  usage, portal, run recording, WebVNC, code, and egress.
- Verify WebSocket reconnect during a rolling restart.

### Phase 4: cutover

- Add a Durable Object state export and PostgreSQL import tool.
- Pause mutations during migration, import, validate counts and active leases,
  then switch the coordinator URL.
- Keep provider cleanup reconciliation enabled throughout rollback coverage.

### Phase 5: scale only when required

- Add advisory-lock mutation coordination.
- Partition high-volume run logs or move them to object storage.
- Externalize bridge routing before increasing replica count.

## Acceptance Criteria

- The same coordinator contract tests pass against Cloudflare and Node.
- A service restart preserves leases, runs, tickets, and scheduled cleanup.
- Expired resources are deleted after a restart without client traffic.
- Cost and active-lease limits cannot be bypassed by concurrent creates.
- WebSocket clients reconnect after proxy or replica termination.
- Provider credentials never enter PostgreSQL, logs, images, or CLI output.
