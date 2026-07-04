# Agent Runtime Bridge

Read this when:

- designing run-a-harness-in-the-box behavior for Crabbox;
- deciding whether a coding-agent daemon belongs under Station;
- reviewing bridge, HTTP/SSE, model credential, or agent runtime changes.

**Status:** contract only. Crabbox does not yet ship a generic
agent-runtime bridge, `crabbox station`, sandbox-agent daemon launcher, or
modelAccess delivery path. This page records the security and lifecycle
boundary for [openclaw/crabbox#530](https://github.com/openclaw/crabbox/issues/530)
as a follow-up to the Station roadmap in
[openclaw/crabbox#193](https://github.com/openclaw/crabbox/issues/193).

The first implementation should be a Station phase, not a new provider family
and not a replacement for `crabbox run`.

## Product Boundary

The proposed bridge starts a repo-owned or operator-approved agent harness
inside a leased workspace, then exposes one HTTP/SSE control API through
Crabbox-managed reachability. The harness might be a generic adapter such as a
sandbox-agent binary, or a repo-owned daemon that wraps a specific tool.

Crabbox owns:

- lease and station lifecycle;
- workspace sync and grounding;
- daemon launch, supervision, stop, TTL, and idle policy;
- bridge authentication and authorization;
- port routing through coordinator or local tunnel machinery;
- log, event, artifact, and evidence retention;
- redaction, egress, and credential policy.

The harness owns:

- model choice and prompt loop;
- task planning and edits;
- harness-specific HTTP/SSE schema;
- test interpretation;
- any tool-specific plugin behavior.

Crabbox must not own model reasoning, store private thoughts, or infer whether a
plan is good. It records boundaries, receipts, logs, commands, and evidence.

## Phase Placement

Ship this after generic Station behavior is stable:

1. Generic Station with no model credentials.
2. Agent runtime bridge with no model credentials by default.
3. modelAccess, only after a separate security-reviewed delivery path exists.

The bridge may use the Agent Station profile, but it must not enable
modelAccess implicitly. If the harness needs model/tool credentials, those
credentials wait for the modelAccess phase and its revocation/redaction gates.

## Minimal Bridge Contract

The bridge contract should be small and explicit:

- start one daemon per station attempt;
- bind the daemon to lease loopback only;
- route client traffic through an authenticated Crabbox bridge;
- use a short-lived bridge ticket scoped to org, owner, station id, attempt id,
  lease id, path prefix, and expiry;
- support HTTP and SSE without exposing raw provider credentials;
- record the daemon command hash, binary/source identity, port, pid, start time,
  and bridge path in station evidence;
- stop the daemon process group before lease release or credential revocation.

The bridge is not a public ingress feature. A daemon must never listen on a
public provider interface by default.

## Authorization Rules

Access to the bridged daemon must be at least as strict as access to the
station:

- require the current authenticated principal on every bridge request;
- reject stale tickets after station stop, attempt replacement, lease release,
  manager revocation, or ticket expiry;
- do not accept bridge tickets in URL query strings by default;
- strip coordinator auth headers before forwarding to the daemon;
- do not forward Crabbox broker, coordinator, cloud provider, or admin tokens;
- keep bridge cookies and tickets scoped to an isolated origin or path that
  cannot be reused by lease-controlled content.

If the daemon has its own auth header, Crabbox may inject a generated
daemon-local credential, but that credential must be attempt-scoped, redacted,
and revoked on stop.

## Egress And Network Policy

Agent runtimes are credential-adjacent even before modelAccess exists. Default
network posture should be conservative:

- daemon control plane: loopback only;
- outbound egress: deny by default unless a named egress profile allows it;
- model/tool destinations: denied until modelAccess is explicitly enabled;
- private, metadata, link-local, and coordinator-internal destinations:
  rejected unless a reviewed profile allows them;
- downloads and uploads: captured in evidence when possible and bounded by
  policy.

Mediated egress can be reused later, but the bridge contract should not assume
that all providers have the same network enforcement layer.

## Stop And Cleanup

Stop behavior must be deterministic:

- `station stop` marks desired state before signaling the daemon.
- Crabbox sends graceful termination to the daemon process group.
- After the grace window, Crabbox kills remaining child processes.
- The bridge stops accepting new connections once desired state is stopping.
- Active SSE streams close with a terminal station event.
- Evidence records stop reason, signal path, exit status, and cleanup result.
- Lease release waits for daemon cleanup or records why cleanup failed.

Repeating stop must be safe and preserve the original terminal reason.

## Evidence

Agent runtime evidence should extend Station evidence. Minimum fields:

- station id, attempt id, lease id, run id if one exists;
- daemon command, shell/argv mode, command hash, and workdir;
- daemon binary/source identity and version when known;
- loopback port and bridge path, not raw secret tickets;
- start and stop timestamps;
- pid/process group, exit status, and stop reason;
- ticket issuer, expiry, principal scope, and redaction policy version;
- egress profile and denied/allowed destination summary;
- log/event retention limits;
- artifact manifest, if the daemon writes bounded artifacts;
- modelAccess receipt metadata only when modelAccess later exists.

Evidence must not include bearer tokens, daemon-local credentials, bridge
tickets, provider API keys, model/tool secrets, raw prompt secrets, signed
upload URLs, or private local paths.

## Unsupported Providers

Phase 1 should target SSH-backed Linux leases. Delegated-run providers should
reject the bridge until they expose an explicit Station/bridge capability.

Do not infer support from:

- `ProviderKindDelegatedRun`;
- archive sync support;
- URL bridge support;
- code-server support;
- desktop or browser support.

An adapter that owns command transport must define how it starts, supervises,
bridges, and stops a long-running daemon before it can claim support.

## Review Checklist

Before merging implementation code, verify:

- The daemon binds to loopback, never a public interface by default.
- Bridge requests require current authorization and scoped short-lived tickets.
- Coordinator auth headers are stripped before forwarding to lease content.
- Revoked managers and stopped stations cannot keep bridge access.
- Stop closes active HTTP/SSE sessions and kills the daemon process group.
- Bridge credentials, tickets, model/tool credentials, and provider tokens are
  redacted from logs, timing, events, artifacts, and evidence.
- modelAccess is not implied by agent runtime bridge support.
- Unsupported providers fail before daemon launch.
- Tests cover stale tickets, attempt replacement, stop/revoke behavior, header
  stripping, loopback-only binding, and redaction.

## Non-goals

- No modelAccess credential delivery in the bridge phase.
- No public ingress to arbitrary lease ports.
- No harness-specific prompt/tool schema in Crabbox core.
- No reasoning trace capture or model-output judgement.
- No automatic restart for agent daemons in v1.
- No delegated-run provider bridge until a provider-specific Station contract
  exists.

## Related Docs

- [Station profiles](station-profiles.md)
- [Mediated egress](egress.md)
- [Browser portal](portal.md)
- [Runtime adapter stack](runtime-adapter-stack.md)
- [Security](../security.md)
