# Auth And Admin

Read when:

- changing broker login or identity;
- changing trusted operator controls;
- debugging who owns a lease or run.

Crabbox supports GitHub browser login for normal users and shared bearer-token login for trusted operator automation. `crabbox login` opens GitHub, the coordinator exchanges the OAuth code, verifies active membership in the allowed GitHub org and optional allowed team slugs, and the CLI stores a signed user token in the user config. `crabbox login --token-stdin` stores the shared operator token instead.

Identity sent to the coordinator:

```text
Cloudflare Access email, when present
signed GitHub login token from browser auth
X-Crabbox-Owner from CRABBOX_OWNER, Git email env, or git config user.email
X-Crabbox-Org from CRABBOX_ORG
CRABBOX_DEFAULT_ORG fallback in the Worker
```

Commands:

```sh
crabbox login
crabbox login --no-browser
crabbox login --url <url> --token-stdin
crabbox whoami
crabbox logout
```

Trusted operator controls:

```sh
crabbox admin leases --state active
crabbox admin release blue-lobster
crabbox admin delete cbx_... --force
```

Admin commands require the shared operator token. GitHub browser-login tokens can create and use normal leases only after allowed-org membership, and configured team membership when present, is verified. They cannot call admin routes.

Normal user tokens are owner/org scoped:

```text
GET /v1/leases                 own leases only
GET /v1/leases/{id-or-slug}    exact ID and slug lookup must match owner/org
POST /v1/leases/{id}/heartbeat own leases only
POST /v1/leases/{id}/release   own leases only
GET /v1/runs and logs          own runs only
GET /v1/usage                  own usage only
GET /v1/pool                   shared-token admin only
```

Do not distribute the shared token to untrusted users.

Related docs:

- [login](../commands/login.md)
- [whoami](../commands/whoami.md)
- [admin](../commands/admin.md)
- [Security](../security.md)
