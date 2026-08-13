# Read-Only Device Pairing

Read this when you are implementing or auditing a companion that displays
coordinator lease status without receiving lease credentials or lifecycle
authority.

## Security boundary

A device token is a distinct coordinator principal. It is not an alias for the
user who paired it. The token has audience `crabbox-device` and exactly one
scope, `leases:read`. It can call only:

```text
GET /v1/leases
GET /v1/leases/{id-or-slug}
```

Those routes return the leases currently visible to the paired owner, including
leases shared with that owner. Device responses always redact SSH host, user,
port, fallback ports, host key, work root, provider-access expiry, and Tailscale
access metadata. A device read never refreshes provider access, calls a provider,
or writes coordinator state.

All other routes are denied before normal routing with `403
device_scope_forbidden`. This includes create, run, heartbeat, stop, release,
share, authoritative provider metadata, bridge tickets, portal, and admin
surfaces.

## Pairing flow

Pairing begins only from the coordinator-hosted browser session. A signed GitHub
user token presented as an API Bearer token, a shared automation token, a trusted
proxy identity, or an admin token cannot initiate pairing.

Portal OAuth issues the browser a distinct `cbwp_` signed session credential.
Normal API authentication does not accept that audience, and copying a `cbxu_`
user Bearer token into a Cookie header does not create a pairing-capable browser
session. This distinction is enforced cryptographically rather than inferred
from client-controlled `Cookie` or `Origin` headers.

```text
POST /v1/pairing/grants
  Auth: __Host-crabbox_session browser cookie
  Body: { "name": "Alice's phone" }
  Result: one-use cbxp_ grant, expires after 5 minutes

POST /v1/pairing/exchange
  Auth: one-use grant in the JSON body
  Body: { "grant": "..." }
  Result: cbxd_ device token plus public device metadata
```

The grant is owner-bound and atomically consumed. Expired grants and replayed
grants cannot mint a token. The coordinator allows at most 10 active grants and
10 active device tokens per owner and organization. Revoke an old device before
pairing another when the device limit is reached.

A device token expires after at most 90 days and stops earlier if its paired
browser authorization grant expires or can no longer be revalidated. When the
stored OAuth grant has expired, been revoked, or can no longer be opened, the
coordinator returns `401 pairing_reauth_required` so the companion can ask the
user to pair again. Device-token revocation remains distinct and returns `401
device_token_invalid`.

Pairing codes and device tokens belong only in request bodies, the
`Authorization` header, and the device's protected credential store. Do not put
them in URLs, command arguments, logs, analytics, crash reports, screenshots, or
repository configuration.

## Owner revalidation

The coordinator stores the paired browser session's sealed GitHub authorization
grant beside the token hash. Every device request reads the durable token
verifier, checks the sealed grant's expiry and readability, and applies current
local revoked-user and allowed-organization/team policy. Successful remote GitHub
account and organization/team membership checks are cached for 60 seconds per
exact device token and normalized organization/team policy. The cache is bounded
and never shared by devices, owners, organizations, or policy versions. A policy
change is therefore enforced on the next request even while an older proof is warm;
malformed team configuration fails closed before cache lookup.

After a cache miss or expiry, GitHub errors, account mismatch, removed
membership, revoked users, or a no-longer-allowed organization fail closed with
`401`; a stale positive is never used as an error fallback. An expired or
revoked OAuth credential, or a recognized GitHub SAML/OAuth authorization
requirement, returns `pairing_reauth_required`. Other owner authorization
failures, including unrecognized GitHub `403` responses, return
`device_owner_unauthorized`.

The sealed grant never reaches the device. Only a hash of the device token is
stored, and the public device response contains no credential material.

## Device management and revocation

Device management also requires the same coordinator-hosted browser session:

```text
GET    /v1/devices       list this owner/org's active devices
DELETE /v1/devices/{id}  revoke one device
DELETE /v1/devices       revoke all devices for this owner/org
```

Device and grant indexes are scoped to the exact owner/org and bounded by their
caps; management does not scan other owners' records. Revocation removes the
server-side verifier and its in-memory membership entry, so the next request
with that token returns `401` even when it had a warm membership result. Lease
owner or sharing changes are evaluated from current lease state on every read,
so removed visibility fails closed on the next request as well.

## Origin policy

Pairing, exchange, device revocation, and device reads require both the request
destination and the `Origin` header to exactly match `CRABBOX_PUBLIC_URL`.
Read-only `GET /v1/devices` accepts the browser's normal missing `Origin` header,
but still requires the exact configured request destination and rejects a
different supplied Origin. The configured origin must be HTTPS. Loopback HTTP
is accepted only for local development. These credential-bearing routes do not
redirect to another origin.

## Threat model

The 60-second positive membership cache is an explicit availability and quota
tradeoff. A user removed from GitHub organization or team membership can retain
device read access only until that device token's current cache entry expires,
so the accepted removal latency is at most about 60 seconds. GitHub lookup
failures after that deadline deny access rather than extending the window.

The window does not apply to the durable device-token verifier, device-token
expiry, sealed-grant expiry or decryption, configured revoked-user policy,
allowed-organization/team policy, or current lease visibility. Those checks run
on every request. Individual or bulk device revocation deletes the verifier first
and takes effect on the next request regardless of cache state. Cache entries are
keyed by the exact device ID, token hash, and normalized org/team policy and are
held only in bounded ephemeral Worker memory, preventing cross-token,
cross-tenant, or cross-policy reuse.

## Non-goals

Device tokens do not grant provider API access, SSH access, portal cookies,
WebVNC or code access, lifecycle mutations, admin authority, marketplace or
payment access, or direct-provider behavior. Expanding the route or scope set
requires a separate security decision.

See [Coordinator](coordinator.md), [Auth and admin](auth-admin.md), and
[Operational security](../security.md) for the surrounding trust model.
