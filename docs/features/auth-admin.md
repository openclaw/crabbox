# Auth And Admin

Read when:

- changing how users log in to the broker or how identity reaches it;
- changing trusted-operator or admin controls;
- debugging who can see, manage, or release a lease or run.

Crabbox has three credential kinds for the broker, with three escalating
privilege levels:

| Credential | How it is obtained | Privilege |
| --- | --- | --- |
| **GitHub user token** | `crabbox login` (browser OAuth) | Own and shared leases/runs only |
| **Shared bearer token** | `crabbox login --token-stdin` or `config set-broker --token-stdin` | All non-admin routes, acting as one shared identity |
| **Admin token** | `config set-broker --admin-token-stdin` or `CRABBOX_COORDINATOR_ADMIN_TOKEN` | Fleet-wide admin routes |

The broker only authorizes brokered providers (`aws`, `azure`, `gcp`,
`hetzner`); all other providers run direct from the CLI and never see these
tokens. For the route and Cloudflare Access model, see
[Broker Auth And Routing](broker-auth-routing.md).

## GitHub browser login (normal users)

`crabbox login --url <broker-url>` opens GitHub in a browser. The broker
exchanges the OAuth code, then verifies the user is an **active member of an
allowed GitHub org** (`CRABBOX_GITHUB_ALLOWED_ORGS`, or the singular
`CRABBOX_GITHUB_ALLOWED_ORG`) and, if any **allowed teams** are configured
(`CRABBOX_GITHUB_ALLOWED_TEAMS` / `CRABBOX_GITHUB_ALLOWED_TEAM`), a member of one
of them. On success the broker issues a signed user token (prefix `cbxu_`,
HMAC-SHA256, default 180-day expiry) and the CLI stores it in the user config.

```sh
crabbox login --url https://broker.example.com
crabbox login --url https://broker.example.com --no-browser   # print the URL instead of opening it
crabbox login --url https://broker.example.com --provider aws # also set the default brokered provider
```

`--provider` accepts `hetzner`, `aws`, `azure`, or `gcp`. With no `--url`,
`login` reuses the broker URL already in config. After storing the token, the
CLI calls `whoami` to confirm the credential works.

GitHub user tokens can create and use **normal leases only**. They cannot reach
admin routes, and the token payload can never carry an `admin` claim.

## Shared and admin tokens (automation, operators)

For automation that should not run an interactive browser flow, store a token
directly:

```sh
printf '%s' "$SHARED_TOKEN" | crabbox login --url https://broker.example.com --token-stdin
printf '%s' "$SHARED_TOKEN" | crabbox config set-broker --url https://broker.example.com --token-stdin
printf '%s' "$ADMIN_TOKEN"  | crabbox config set-broker --url https://broker.example.com --admin-token-stdin
```

The broker matches an incoming bearer token in this precedence:

1. `CRABBOX_ADMIN_TOKEN` -> admin request.
2. `CRABBOX_SHARED_TOKEN` -> non-admin shared identity (owner defaults to
   `CRABBOX_SHARED_OWNER`, org to `CRABBOX_DEFAULT_ORG`).
3. Otherwise, a valid signed `cbxu_` user token from GitHub login.

On the CLI side the admin token is read from `broker.adminToken` in config or
the `CRABBOX_COORDINATOR_ADMIN_TOKEN` / `CRABBOX_ADMIN_TOKEN` environment
variable; the normal broker token comes from `broker.token` or
`CRABBOX_COORDINATOR_TOKEN`. Admin commands fail fast if no admin token is
configured.

Never distribute the shared or admin token to untrusted users. Keep the admin
token narrower and more closely held than the shared automation token.

## Identity sent to the broker

Every request carries a bearer token plus identity hints. The broker resolves
the effective owner and org and re-injects them as trusted headers
(`x-crabbox-owner`, `-org`, `-auth`, `-admin`, `-github-login`) before handling
the request:

- GitHub user token -> `owner`/`org`/`login` come from the signed token.
- Admin or shared token -> `owner` from `X-Crabbox-Owner` (CLI sends
  `CRABBOX_OWNER`, a Git email env var, or `git config user.email`) or, for the
  shared token, `CRABBOX_SHARED_OWNER`; `org` from `X-Crabbox-Org`
  (`CRABBOX_ORG`) or the broker's `CRABBOX_DEFAULT_ORG`.
- A verified Cloudflare Access JWT (`cf-access-jwt-assertion`), when the broker
  has `CRABBOX_ACCESS_TEAM_DOMAIN` and `CRABBOX_ACCESS_AUD` set, overrides the
  `owner` with the email from the assertion.

## Identity commands

```sh
crabbox whoami            # show resolved owner, org, and auth method
crabbox logout            # remove the stored broker token from config
crabbox config show       # merged config; tokens shown only as present/missing
```

`whoami` prints `user=… org=… auth=… broker=…`, where `auth` is `github` for a
user token or `bearer` for a shared/admin token. `logout` clears
`broker.token`; it does not touch the admin token.

## Authorization model

Normal user tokens are scoped to their own and shared resources:

```text
GET  /v1/leases                  own and shared leases only
GET  /v1/leases/{id-or-slug}     resolves only if visible to the caller
POST /v1/leases/{id}/heartbeat   owner, manage share, or admin
POST /v1/leases/{id}/release     owner, manage share, or admin
POST /v1/leases/{id}/tailscale   owner, manage share, or admin
PUT/DELETE /v1/leases/{id}/share owner, manage share, or admin
GET  /v1/runs and logs/events    own runs only
GET  /v1/usage                   own usage only
GET  /v1/pool                    admin token only
POST /v1/leases with hostId      admin token only
/v1/admin/*                      admin token only
```

A lease is **visible** to a caller who is the owner (matching owner email and
org), an admin, or a share recipient. It is **manageable** by the owner, an
admin, or a `manage` share recipient. Non-admin admin-route requests are
rejected with `403 admin token required`.

Provider host inventory is also capacity administration. Normal portal users
see a Dedicated Host only when it backs an active lease already visible to
them; unattached host inventory and explicit host-pinned lease creation require
admin authentication.

## Lease sharing

Sharing grants broker and portal access to a lease without distributing the
shared bearer or admin token. Roles:

- **use** — see the lease and open visible portal bridges (WebVNC, code).
- **manage** — everything `use` allows, plus heartbeat/touch the lease, update
  non-secret Tailscale metadata, change sharing, and stop the lease.

```sh
crabbox share --id swift-crab --user alice@example.com           # default role: use
crabbox share --id swift-crab --user alice@example.com --role manage
crabbox share --id swift-crab --org                              # share with the lease org
crabbox share --id swift-crab --list                             # show current sharing
crabbox unshare --id swift-crab --user alice@example.com
crabbox unshare --id swift-crab --org
crabbox unshare --id swift-crab --all
```

`--user` is repeatable. `--org` shares with any authenticated user whose org
matches the lease org. Sharing affects broker/portal access only: SSH use still
requires a private key the runner accepts, and sharing never copies SSH private
keys between users.

## Trusted-operator and admin commands

These require the admin token:

```sh
crabbox admin leases --state active
crabbox admin lease-audit --state expired --provider aws --fail-on-live
crabbox admin release --id swift-crab            # mark a lease released
crabbox admin release --id swift-crab --delete   # release and delete the server
crabbox admin delete --id cbx_0123456789ab --force
```

`admin lease-audit` cross-checks broker lease records against live cloud state
(default `--provider aws`, `--state expired`); `--fail-on-live` exits non-zero
when expired leases still have live instances or audit errors. `admin delete`
requires `--force`. Both `release` and `delete` accept the lease id or slug as a
positional argument or via `--id`.

Provider and host administration (AWS-only today) also lives under `admin`:

```sh
crabbox admin aws-identity                       # broker AWS caller identity
crabbox admin aws-policy --target macos          # print baseline brokered AWS IAM policy
crabbox admin hosts list --provider aws --target macos
crabbox admin mac-hosts allocate --region eu-west-1 --force
```

See the [admin command reference](../commands/admin.md) for the full subcommand
and flag set.

## Related docs

- [login](../commands/login.md)
- [whoami](../commands/whoami.md)
- [admin](../commands/admin.md)
- [Broker Auth And Routing](broker-auth-routing.md)
- [Security](../security.md)
