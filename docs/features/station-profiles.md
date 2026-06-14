# Station Profiles Roadmap

Station profiles are a lifecycle primitive for supervised long-running
workloads.

**Status:** an initial, disabled-by-default primitive now exists in
`internal/station`. It provides the `StationProfile` config struct with parsing
and validation, the agent-profile boundary type, feature-gated phase
enforcement (every phase returns a clear "not yet enabled" error until turned
on), and the security gating that keeps `modelAccess` a separate, audited field
that is never sourced from `env.allow`. There is still no `crabbox station`
command, no top-level `stationProfile` config wiring, and no live `modelAccess`
credential delivery; those land in later, separately reviewed phases.

This page records the product and security boundary for
[issue #193](https://github.com/openclaw/crabbox/issues/193) so future PRs can
ship the feature in reviewable pieces.

## Terms

**Station** is the proposed user-facing primitive: a durable supervised workload
record bound to a warm lease. A station is still a Crabbox box; the difference
is that Crabbox records one long-running workload lifecycle instead of only a
single `run` invocation.

**stationProfile** is the proposed config selector for named station policies,
for example `default` or `agent`.

**Agent Station** is shorthand for a station using `stationProfile: agent`.
The agent loop is repo-owned code. Crabbox supervises and records it, but does
not own the prompt loop, planning strategy, model choice, or test
interpretation.

**modelAccess** is the proposed explicit credential-delivery policy for
model/tool access. It must be separate from ordinary `env.allow` forwarding.

Use **reasoning access** only in explanatory copy. It means the repo-owned
workload may run a reasoning loop because a user granted scoped model/tool
access. Crabbox must not expose, judge, store, or reconstruct model reasoning.

## Phase Gates

Ship Station in phases, with separate review for each phase:

1. Generic Station, no model credentials.
2. `agent` station profile, still no model credentials by default.
3. `modelAccess`, only after Station behavior and evidence are stable.

Phase 1 should target SSH-backed lease providers first. Delegated-run providers
should wait until they expose an explicit station-capable contract.

## Phase 1 Contract

A Station is not just `warmup && run --keep` under a new name. The minimum
contract is:

- one durable station id;
- one or more attempts;
- one lease id per attempt;
- one supervisor boot id, pid/start time, command hash, and workdir per
  attempt;
- explicit TTL and idle timeout;
- separate lease heartbeat and station heartbeat;
- station-centered status, logs, stop reason, and terminal state;
- idempotent stop that preserves the original terminal reason;
- evidence written on stop, failure, TTL expiry, idle expiry, or lost
  supervisor.

Station status should keep `state`, `desiredState`, `attempt`,
`lastObservedAt`, and `stopReason` separate. It should distinguish lease state,
supervisor liveness, command state, and cleanup/revocation state.

## Agent Profile

The first `agent` profile should remain repo-owned and conservative:

```yaml
station:
  profiles:
    agent:
      enabled: true
      command: scripts/agent-loop.sh
      ttl: 10h
      idleTimeout: 45m
      restartPolicy: never
```

`restartPolicy` should default to `never`. Agent loops are not safely replayable
by default, so retries are a later product decision.

## modelAccess Security Gates

`modelAccess` must not ship until all of these are true:

- No Crabbox broker, coordinator, cloud provider, or admin credential enters the
  station unless explicitly part of the workload policy.
- Only workload-scoped model/tool credentials enter the station.
- Credentials expire at or before station TTL.
- Credentials are scoped, where possible, to repo/org/user, station id, attempt
  id, gateway, command hash, allowed models/tools, and budget.
- Credentials are never written to logs, timing JSON, events, compliance
  reports, artifacts, screenshots, terminal captures, failure bundles, or
  process arguments.
- Evidence records receipts, not secrets: profile, gateway, budget, token
  expiry, redaction policy version, issuer, and revocation reason.
- Egress defaults to deny and only allows named approved egress profiles.
- Stop, TTL expiry, idle expiry, budget exhaustion, or compliance failure
  revokes credentials before lease teardown.
- Model/tool credentials do not use ordinary repo `env.allow`; they use a
  separate audited delivery path.

Do not return stopped `modelAccess` stations to warm pools in v1. Treat them as
dirty by default because they hosted credentialed long-running work.

## Evidence

Station evidence should extend run evidence instead of inventing an unrelated
format. Minimum fields:

- station id, attempt id, lease id, and run id if one exists;
- start and stop timestamps;
- repo SHA, branch, and dirty summary;
- command and shell/argv mode;
- station profile;
- supervisor boot id, pid, start time, and exit status;
- TTL, idle timeout, stop reason, and final state;
- log/event retention limits;
- model access policy and credential receipt metadata, if enabled;
- revocation timestamp and reason, if model access was enabled.

## Non-goals

Station should not add an agent memory database, vector index, semantic judge,
scenario state machine, or model reasoning store to Crabbox core.

Crabbox owns lifecycle, synchronization, supervision, cleanup, credential policy,
logs, artifacts, and evidence. The repo-owned workload owns the brain.
