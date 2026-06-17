# Broker Auth and Routing

Read this when you are:

- changing how the coordinator (broker) authenticates callers;
- adding or moving a coordinator route, trusted reverse proxy, or Cloudflare Access application;
- debugging bearer-token automation, service-token access, or the GitHub browser login.

The broker is the coordinator control plane. The CLI talks to it over HTTPS for
lease lifecycle, runs, usage, and admin operations; SSH, rsync, and command
execution still go straight from the CLI to the runner host and never traverse
the broker. The same auth and route behavior runs on Cloudflare Workers or the
Node.js/PostgreSQL service.

## Routes

A deployment needs one canonical HTTPS origin:

```text
https://broker.example.com                       # public CLI + browser-login route
```

For Code portal origin isolation, the same coordinator can also receive a
wildcard route configured through:

```text
CRABBOX_CODE_ORIGIN_TEMPLATE=https://{lease}.code.example.com
https://*.code.example.com                       # same service; wildcard TLS + WebSockets
```

The `{lease}` label is replaced with an opaque stable hash, not a lease ID or
slug. The canonical portal authorizes the initial Code URL and issues a
single-use bootstrap; the per-lease origin then uses only a lease-scoped Code
session. It does not accept the portal cookie as coordinator authority.

A Cloudflare deployment may additionally publish the same Worker on:

```text
https://broker-access.example.com                # same Worker, behind Cloudflare Access
https://crabbox-coordinator.example.workers.dev   # workers.dev fallback
https://fallback.example.com                       # additional fallback
```

`https://broker.example.com` is the canonical route. It must let `crabbox login`
complete a browser GitHub OAuth flow. The coordinator still requires Crabbox
auth on every API route; the unauthenticated
exceptions are `GET /v1/health`, the GitHub login/OAuth routes (`/v1/auth/*`,
`/portal/login`, `/portal/logout`), and the per-lease websocket agent upgrades that
authenticate via short-lived bridge tickets instead.

`https://broker-access.example.com` is the **same** Worker fronted by a Cloudflare Access
application. It exists for automation and for proving that Crabbox works when an operator
wants an outer Cloudflare gate. Requests there clear two independent checks:

1. **Cloudflare Access** accepts the service-token headers before the request reaches the
   Worker.
2. The **coordinator** accepts one of: the shared operator bearer token, the separate admin
   bearer token (for admin routes), or a signed Crabbox user token.

A Cloudflare Access service token is therefore not a Crabbox admin token. It only gets the
HTTP request past Cloudflare Access; the coordinator still decides what the caller may do. Use a
`non_identity` (service-token-only) Access policy scoped to the specific Crabbox CLI service
token rather than any token in the account, so automated clients prove both layers
independently.

Node deployments commonly put TLS and identity-aware routing in front of the
service. The ingress must support WebSocket upgrades. If it injects a trusted
user header, configure `CRABBOX_TRUSTED_USER_HEADER`,
`CRABBOX_TRUSTED_USER_ORG`, and `CRABBOX_TRUSTED_PROXY_CIDRS`; the service
accepts that identity only from an allowlisted socket peer. If direct access
cannot be blocked, also configure `CRABBOX_TRUSTED_PROXY_SECRET` and send it in
`X-Crabbox-Proxy-Secret`. The ingress must remove caller-supplied copies of both
headers; the coordinator strips the secret before routing the request.

## How The Coordinator Authenticates A Request

Every authenticated route normally requires an `Authorization: Bearer <token>`
header. The coordinator
matches the token in this precedence (`worker/src/auth.ts`):

1. **Admin token** — equals `CRABBOX_ADMIN_TOKEN`. Grants admin.
2. **Shared token** — equals `CRABBOX_SHARED_TOKEN`. Authorized but not admin; this is
   normal trusted automation.
3. **Signed user token** — a token with the `cbxu_` prefix, an HMAC-SHA256 signature over a
   base64url payload, verified with `CRABBOX_SESSION_SECRET` (falling back to
   `CRABBOX_SHARED_TOKEN`). Minted by `crabbox login`, with a default 180-day expiry.
   User tokens are non-admin unless their GitHub email or login matches
   `CRABBOX_GITHUB_ADMIN_OWNERS` or `CRABBOX_GITHUB_ADMIN_LOGINS`.

Anything else returns `401 unauthorized`.

After a successful match the coordinator forwards the request to `FleetCoordinator` with a
trusted identity injected as `x-crabbox-auth`, `x-crabbox-admin`, `x-crabbox-owner`,
`x-crabbox-org`, and (for user tokens) `x-crabbox-github-login`. Any inbound
`cf-access-authenticated-user-email` / `cf-access-jwt-assertion` headers are stripped before
forwarding, so raw Access headers can never spoof identity.

### Owner and org on a request

The CLI computes a local owner email (`localCoordinatorOwner`) in this order and sends it as
`x-crabbox-owner`, with `CRABBOX_ORG` as `x-crabbox-org`:

```text
CRABBOX_OWNER
GIT_AUTHOR_EMAIL
GIT_COMMITTER_EMAIL
git config user.email
```

How the coordinator resolves owner/org depends on the token:

- **Admin token** — owner comes from the CLI's `x-crabbox-owner` header (falling back to
  `unknown`); org comes from `x-crabbox-org` (falling back to `CRABBOX_DEFAULT_ORG`).
- **Shared token** — owner comes from the Worker's own `CRABBOX_SHARED_OWNER` env (not the
  CLI header); org comes from `CRABBOX_DEFAULT_ORG`.
- **Signed user token** — owner/org come from the signed GitHub user token, not from CLI
  headers.

The Cloudflare-specific override: when the Worker can verify a Cloudflare Access JWT and that JWT carries an
email, the verified Access email becomes the request owner for bearer (admin or shared)
callers. Raw, unverified Cloudflare Access email headers are stripped and never set identity.

The Node-specific alternative is trusted reverse-proxy identity. Requests from
configured proxy CIDRs may use `CRABBOX_TRUSTED_USER_HEADER` without a Crabbox
bearer token. The resulting identity is non-admin; admin routes still require
`CRABBOX_ADMIN_TOKEN`.

## GitHub browser login

`crabbox login --url <broker-url>` opens GitHub, runs the OAuth flow, and stores a signed
Crabbox user token locally. The coordinator needs a GitHub OAuth app whose callback URL is
the public coordinator origin plus the callback path:

```text
https://broker.example.com/v1/auth/github/callback
```

The OAuth app requests the scopes `read:user user:email read:org`. A self-hosted coordinator
needs its own OAuth app: the callback URL must exactly match the public origin, and the
`CRABBOX_PUBLIC_URL` must use that same origin (it is used to build the callback and
to canonicalize portal redirects).

Login is gated by GitHub org membership before a user token is minted:

- The allowed org set comes from `CRABBOX_GITHUB_ALLOWED_ORG` or comma-separated
  `CRABBOX_GITHUB_ALLOWED_ORGS`; if neither is set, it falls back to `CRABBOX_DEFAULT_ORG`.
  If no allowed org resolves, login is rejected.
- The user must be an **active** member of an allowed org.
- If `CRABBOX_GITHUB_ALLOWED_TEAMS` (or `CRABBOX_GITHUB_ALLOWED_TEAM`) is set, the user must
  also belong to at least one listed team after org membership passes. Entries are team
  slugs: use `team-slug` for the resolved org, or `org/team-slug` to qualify the org.

### Coordinator secrets for login

```text
CRABBOX_GITHUB_CLIENT_ID
CRABBOX_GITHUB_CLIENT_SECRET
CRABBOX_GITHUB_ALLOWED_ORG       # or CRABBOX_GITHUB_ALLOWED_ORGS (comma-separated)
CRABBOX_GITHUB_ALLOWED_TEAMS     # optional; comma-separated team slugs
CRABBOX_GITHUB_ADMIN_OWNERS      # optional; comma-separated GitHub verified emails with admin
CRABBOX_GITHUB_ADMIN_LOGINS      # optional; comma-separated GitHub logins with admin
CRABBOX_SESSION_SECRET           # signs user tokens; falls back to CRABBOX_SHARED_TOKEN
CRABBOX_USER_TOKEN_TTL_SECONDS   # optional; default 15552000 (180 days), clamped to 1h-365d
```

## Sending Cloudflare Access credentials from the CLI

When a route is also protected by Cloudflare Access, the CLI must satisfy Access
before the coordinator sees the request. Configure either a service token or a
pre-minted JWT:

- `CRABBOX_ACCESS_CLIENT_ID` + `CRABBOX_ACCESS_CLIENT_SECRET` — sent as the
  `CF-Access-Client-Id` and `CF-Access-Client-Secret` headers (service token).
- `CRABBOX_ACCESS_TOKEN` — an already-minted Access JWT, forwarded as the `cf-access-token`
  header.

(`CF_ACCESS_CLIENT_ID`, `CF_ACCESS_CLIENT_SECRET`, and `CF_ACCESS_TOKEN` are accepted as
fallbacks.) These credentials satisfy Cloudflare Access only — the Worker still requires the
Crabbox bearer or signed user token.

For coordinators behind an upstream identity proxy that consumes the `Authorization` header,
set `CRABBOX_COORDINATOR_TOKEN_COMMAND` to a JSON argv array. Crabbox executes it directly,
without a shell, before each HTTP request and WebSocket reconnect. The command must print one
bearer token line and takes precedence over `CRABBOX_COORDINATOR_TOKEN`. The proxy must inject a
trusted identity header accepted by the coordinator after validating that token.

```bash
export CRABBOX_COORDINATOR_TOKEN_COMMAND='["identity-cli","token","--audience","coordinator"]'
```

Only set this in trusted machine-level configuration. Project config files cannot define token
commands.

Server-side, when `CRABBOX_ACCESS_TEAM_DOMAIN` and `CRABBOX_ACCESS_AUD` are configured, the
Worker verifies the `Cf-Access-Jwt-Assertion` header against Cloudflare Access certs (RS256,
matching `aud`, `iss`, and expiry) before trusting any Access identity. Without both
configured, Access identity is ignored.

## Local config

```yaml
broker:
  url: https://broker.example.com
  token: <crabbox-shared-token-or-user-token>
  adminToken: <crabbox-admin-token>
  access:
    clientId: <cloudflare-access-client-id>
    clientSecret: <cloudflare-access-client-secret>
provider: aws
```

Set `CRABBOX_COORDINATOR=https://broker-access.example.com` to point a single command at the
Access-protected route without changing the default `broker.url`. `crabbox config show`
reports the Access credential state as `access_auth=service-token` (or similar) without
printing secrets.

## Proof commands

```sh
# Should fail at Cloudflare Access without credentials.
curl -i https://broker-access.example.com/v1/health

# Should pass once Access creds + shared + admin broker auth are configured.
CRABBOX_COORDINATOR=https://broker-access.example.com bin/crabbox doctor
CRABBOX_COORDINATOR=https://broker-access.example.com bin/crabbox whoami

# End-to-end auth and provider smoke against the Access route.
CRABBOX_LIVE=1 CRABBOX_AUTH_SMOKE_ACCESS=1 \
  CRABBOX_COORDINATOR=https://broker-access.example.com \
  CRABBOX_BIN=bin/crabbox scripts/live-auth-smoke.sh

CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=aws \
  CRABBOX_COORDINATOR=https://broker-access.example.com \
  CRABBOX_BIN=bin/crabbox scripts/live-smoke.sh
```

The auth smoke proves both layers (Access plus the Worker bearer/admin tokens); the provider
smoke additionally proves the same route can lease, run, and release a real machine.

## Summary

- `broker.example.com/*` is the canonical CLI and browser-login endpoint.
- Cloudflare may add `broker-access.example.com/*`, workers.dev, and fallback
  routes; Node may use any TLS/WebSocket-capable ingress.
- The Access service token only clears Cloudflare Access; it is not a Crabbox admin token.
- Trusted proxy identity is Node-only, CIDR-gated, and never grants admin.
- Signed GitHub user tokens are never admin tokens — admin routes require the separate admin
  token.

## Related docs

- [Coordinator](coordinator.md)
- [Security](../security.md)
- [Infrastructure](../infrastructure.md)
- [config command](../commands/config.md)
