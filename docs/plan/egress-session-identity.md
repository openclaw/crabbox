# Server-Bound Egress Session Identity

Status: proposal, not implemented. Design follow-up to
[#1152](https://github.com/openclaw/crabbox/pull/1152), which made replaced
egress sessions stay dead via persisted per-lease tombstones and fatal
replacement closes in the CLI.

Read when:

- evaluating the remaining egress session resurrection window;
- changing egress ticket-mint or session-activation semantics in
  `worker/src/fleet.ts`;
- changing how `internal/cli/egress.go` chooses session IDs.

Current behavior remains authoritative in `worker/src/fleet.ts`
(`createEgressTicket`, `egressAgent`, `activateEgressSession`,
`replacedEgressSessions`) and `internal/cli/egress.go`.

## Problem

Egress sessions are identified by client-chosen IDs
(`newLocalEgressSessionID` in the CLI; any string matching
`validEgressSessionID` is accepted at ticket mint). The coordinator therefore
cannot distinguish a fresh session from a superseded daemon re-minting its old
ID except by remembering replaced IDs. That memory is bounded: tombstones are a
persisted per-lease FIFO capped at `replacedEgressSessionsPerLease` (256).

Residual window (documented in the code comment beside
`replacedEgressSessions`): a daemon that stays offline through 256+ subsequent
replacements of one lease, then returns, finds its ID evicted and can mint a
ticket, reactivate, and clobber the current session. Every zombie that
reconnects earlier dies fatally at the 409/1012 handling shipped in #1152. The
cap cannot simply be removed: tombstones live in one Durable Object storage
value (128 KiB limit), and unbounded growth from a runaway restart loop is its
own failure mode.

A second, smaller gap from the #1152 review: the persisted active-session
record carries no principal, so revocation paths that discover sessions by
scanning sockets cannot attribute a socketless active record to a revoked user.

## Design

Make the coordinator the issuer of session identity. A server-issued session ID
embeds a per-lease monotonic generation and an HMAC, so the coordinator can
reject any superseded session with O(1) persisted state and no history list.

### ID format

Server-issued IDs stay inside the existing charset and length budget
(`^egress_[A-Za-z0-9_.:-]{6,80}$`) so every deployed CLI can carry, echo, and
relay them unchanged:

```text
egress_1.<generation>.<random>.<mac>
        │  │            │        └─ base32hex HMAC-SHA-256 tag, 16 chars (80 bits)
        │  │            └─ 22-char base32hex random (110 bits)
        │  └─ decimal generation, monotonic per lease
        └─ format version
```

MAC input: `leaseID || "." || generation || "." || random`, keyed with a
per-lease 256-bit random key. An ID whose MAC does not verify is not an error —
it is simply a legacy client-chosen ID and takes the legacy path.

### Per-lease persisted state (O(1))

One new storage record per lease, hydrated and torn down exactly like the
tombstone and active-session records introduced in #1152:

```text
egress-identity:<leaseID> → { key: <32 random bytes>, counter: <number>, activeGen: <number> }
```

- `key` is created on first use, lease-scoped, never rotated (lease lifetimes
  are bounded; teardown deletes the record). No dependency on
  `CRABBOX_SESSION_SECRET` or any global secret.
- `counter` is the generation allocator: incremented and persisted on every
  fresh mint. Never used for rejection.
- `activeGen` is the highest generation that has ever **activated** on this
  lease: supersession is recorded at activation, not at mint. Used for all
  rejection decisions.

Both numbers are deleted with the lease, like all other egress state.

### Mint and admission semantics

`createEgressTicket` classifies the requested session ID:

1. **No session requested** (or invalid format): fresh start. `counter += 1`
   (persisted), issue a server-bound ID embedding `g = counter`. This is
   already the server-generated fallback path; it changes from random-only to
   generation-bearing. Minting alone changes nothing else — an abandoned mint
   can never invalidate the current session.
2. **Server-bound ID, MAC verifies, embedded generation `g`**:
   - `g < activeGen` → `409 egress_session_replaced`. This holds even when the
     active-session record is missing (crash, hibernation edge): rejection
     depends only on the persisted `activeGen`.
   - `g >= activeGen` → admitted. `g == activeGen` is a reconnect of the
     current session; `g > activeGen` is a freshly minted successor that has
     not yet activated.
3. **Legacy client ID** (no or invalid MAC): current #1152 semantics unchanged
   — tombstone check, last-writer-wins activation. Current guarantees, no
   better, no worse.

`egressAgent` (connect) applies the same admission rule to the consumed
ticket's session ID. `activateEgressSession` activates a server-bound session
iff `g >= activeGen`, and on activation persists `activeGen = g` — this is the
moment the previous generation becomes permanently invalid. Generation
ordering replaces the timestamp comparison for server-bound sessions;
timestamp last-writer-wins remains only for legacy-vs-legacy conflicts. Racing
fresh mints resolve deterministically: the highest generation to activate
wins, and lower ones are rejected from then on.

Invariant: `activeGen` is strictly greater than the generation of every
superseded server-bound session, independent of the active record's survival.
Two rules maintain it: activation persists `activeGen = g`, and a legacy
session replacing a server-bound active session persists
`activeGen = counter + 1` (allocating and burning a generation). Without the
second rule, a server-bound session at generation G replaced by a legacy
session could resurrect after the active record is lost to a crash — its
embedded G would not be below `activeGen`.

Atomicity: every activation that advances `activeGen` commits the identity
record and the active-session record in a single atomic multi-key
`storage.put({...})` (Durable Object multi-key puts commit atomically), and
socket bookkeeping happens after that commit. A crash therefore leaves either
the old state fully intact (current session unaffected) or the transition
fully applied (successor active, predecessor fenced) — never an advanced fence
without its recorded successor, which would permanently reject the
still-current session.

### Why the MAC is required

Without it, the generation ordering trusts a client-supplied string: any caller
could craft `egress_1.999999.x.y` and jump the ordering. With the MAC, a
crafted ID fails verification and demotes to the legacy path, where tombstones
still apply. The MAC binds `leaseID`, so IDs cannot replay across leases.

### CLI change (minimal)

`egressStart` stops generating `newLocalEgressSessionID`. It mints the client
ticket without a requested session ID and adopts the server-issued
`created.SessionID` — the exact adoption path `connectEgressBridge` already
implements. The returned ID flows into the remote client command and the host
daemon args as today. No new flags, no daemon/supervisor changes; the
supervisor's re-mint of `--session <server-ID>` becomes a reconnect mint and is
valid precisely while that session is current.

Deployed CLIs need no changes: their self-generated IDs take the legacy path,
and when they relay a server-bound ID (from `--session` or status resolution)
they silently inherit the stronger guarantee.

## Compatibility matrix

| Zombie session | Replacement session | Outcome |
| --- | --- | --- |
| server-bound | server-bound | zombie generation < current → 409 always, no history needed |
| server-bound | legacy | legacy replacement durably advances `activeGen` (see invariant above), so the zombie's generation is below it → 409 even if the active record is later lost |
| legacy | server-bound | legacy re-mint hits tombstones — current guarantee (bounded) |
| legacy | legacy | current #1152 guarantee (bounded tombstones) |

The residual window shrinks to legacy-zombie cases only and disappears as CLIs
update. Tombstones stay for the legacy path; once legacy traffic ages out they
can be retired in a later cleanup.

## Constraints check

- **Backward compatible**: legacy `egress_*` IDs keep working at exactly the
  #1152 guarantees; old CLIs relay server IDs without knowing.
- **Bounded storage**: one fixed-size record per lease; tombstone FIFO
  unchanged for legacy, removable later.
- **Data plane untouched**: only ticket mint, connect admission, and activation
  ordering change; the WebSocket bridge protocol and proxy behavior are
  identical.

## Optional rider: principal on the active record

Since activation already rewrites the persisted active-session record, add the
minting principal (owner/org, admin flag) to it. Revocation paths can then
attribute and tombstone a socketless active session, closing the second #1152
review residual. Independent of the identity scheme; cheap to include in the
same schema change.

## Alternatives considered

- **Unbounded tombstones**: hits the 128 KiB single-value limit under runaway
  restart loops; unbounded growth is itself a defect.
- **Fresh/retry intent flag without server-issued IDs**: fresh mints would
  still accept client-chosen IDs, so a lying or buggy client re-opens the
  window; converges to this design once fresh IDs are server-assigned, minus
  the crash-robust generation ordering.
- **Global HMAC secret** (e.g. derived from `CRABBOX_SESSION_SECRET`): couples
  egress identity to secret rotation and portable-runtime configuration;
  per-lease keys have lease-bounded lifetime and need no rotation story.
- **Unsigned generation IDs**: forgeable ordering; see MAC rationale.

## Test plan

Worker (`worker/test/fleet.test.ts`, beside the #1152 suites):

- fresh mint issues generation-bearing IDs; generation persists across
  simulated restart (second coordinator over same storage).
- zombie server-bound ID → 409 on mint and on connect, including with the
  active record deleted (crash simulation) — the case tombstones cannot cover.
- crafted ID with bad MAC → legacy path (tombstone semantics apply, no
  generation bump).
- legacy session replaces a server-bound session → `activeGen` advanced; the
  server-bound zombie gets 409 after active-record loss (the invariant's
  second rule).
- abandoned fresh mint (ticket issued, never connected) → the current session
  keeps reconnecting; `activeGen` unchanged until a successor activates.
- torn-write simulation: identity and active-session records are committed
  atomically — no interleaved state may reject the current session without a
  recorded successor (assert via a storage stub that fails between logical
  writes if the implementation ever splits the commit).
- legacy interplay per the compatibility matrix.
- eviction test inversion: after >256 replacements, a server-bound zombie is
  still rejected (the exact residual #1152 documents).

CLI (`internal/cli`):

- `egress start` mints without a session ID and adopts the server-issued one
  end-to-end (fake coordinator asserting no `sessionID` in the mint body).
- supervisor restart re-mints the same server ID (reconnect path).

Live proof: repeat the #1152 production-lease scenario, plus a forced-eviction
variant on a dev coordinator once the worker half is deployed.

## Rollout

1. Worker deploy: accepts and issues server-bound IDs; all existing traffic is
   legacy and unaffected.
2. CLI release: `egress start` adopts server-issued IDs.
3. Later cleanup (separate change): retire tombstones when legacy CLI share is
   negligible.

No flag day; each step is independently shippable and revertible.
