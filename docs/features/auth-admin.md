# Auth And Admin

Read when:

- changing broker login or identity;
- changing trusted operator controls;
- debugging who owns a lease or run.

Crabbox supports GitHub browser login for normal users, shared bearer-token login for trusted operator automation, and a separate admin token for fleet-wide routes. `crabbox login` opens GitHub, the coordinator exchanges the OAuth code, verifies active membership in the allowed GitHub org and optional allowed team slugs, and the CLI stores a signed user token in the user config. `crabbox login --token-stdin` stores the shared operator token instead.

Identity sent to the coordinator:

```text
signed GitHub login token from browser auth
X-Crabbox-Owner from CRABBOX_OWNER, Git email env, or git config user.email
X-Crabbox-Org from CRABBOX_ORG
Verified Cloudflare Access JWT email, when configured and present
CRABBOX_DEFAULT_ORG fallback in the Worker
```

Commands:

```sh
crabbox login
crabbox login --no-browser
crabbox login --url <url> --token-stdin
crabbox whoami
crabbox logout
crabbox share --id blue-lobster --user friend@example.com
crabbox share --id blue-lobster --org
crabbox unshare --id blue-lobster --user friend@example.com
```

Trusted operator controls:

```sh
crabbox admin leases --state active
crabbox admin lease-audit --state expired --provider aws
crabbox admin release blue-lobster
crabbox admin delete cbx_... --force
```

Admin commands require the separate admin token. GitHub browser-login tokens can create and use normal leases only after allowed-org membership, and configured team membership when present, is verified. They cannot call admin routes.

Normal user tokens are owner/org scoped:

```text
GET /v1/leases                 own and shared leases only
GET /v1/leases/{id-or-slug}    exact ID and slug lookup must be visible
POST /v1/leases/{id}/heartbeat own or shared leases
PUT/DELETE /v1/leases/{id}/share owner, manage share, or admin only
POST /v1/leases/{id}/release   owner, manage share, or admin only
GET /v1/runs and logs          own runs only
GET /v1/usage                  own usage only
GET /v1/pool                   admin token only
```

Lease sharing grants coordinator and portal access without distributing the
shared bearer token or admin token. A `use` share can see the lease and open
visible portal bridges such as WebVNC/code. A `manage` share can also change
sharing and stop the lease. `--org` shares with authenticated users whose org
matches the lease org. SSH-based CLI use still requires a local private key
accepted by the runner; sharing does not copy SSH private keys between users.

Do not distribute the shared token or admin token to untrusted users. Keep the admin token narrower and more closely held than the shared automation token.

Related docs:

- [login](../commands/login.md)
- [whoami](../commands/whoami.md)
- [admin](../commands/admin.md)
- [Security](../security.md)
