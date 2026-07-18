# whoami

`crabbox whoami` calls the broker's `/v1/whoami` endpoint and prints the
identity the coordinator resolved for your request. Use it to confirm broker
auth is working before starting any long-running workflow.

```sh
crabbox whoami
crabbox whoami --json
```

`whoami` requires a configured coordinator (broker URL plus a token). If none
is configured it fails with exit code `2` and the message
`whoami requires a configured coordinator`. See [login](login.md) and
[config](config.md) for how the broker URL and token are stored.

## Flags

- `--json` - print the raw coordinator response as JSON instead of the
  human-readable line.

`whoami` takes no positional arguments.

## Human output

```text
user=github:12345 org=example-org auth=github broker=https://broker.example.com
```

The fields:

- `user` - the resolved owner identity, as the coordinator sees it.
- `org` - the organization namespace, when set (empty otherwise).
- `auth` - the auth mode the coordinator accepted: `github` for signed
  login tokens, `bearer` for shared automation tokens.
- `broker` - the coordinator URL the CLI is configured to use. This is a
  local field added by the CLI from your config, not returned by the broker.

## JSON output

```json
{
  "owner": "github:12345",
  "org": "example-org",
  "auth": "github"
}
```

The JSON form is exactly the coordinator's `/v1/whoami` response: `owner`,
`org`, and `auth`. The broker URL is not included in JSON output.

## How identity is resolved

When you log in through the browser flow, the broker issues a signed user
token (prefix `cbxu_`) that embeds your immutable `github:<numeric-id>` owner and
allowed-org membership. A verified GitHub email remains an eligibility check,
not ownership. For requests carrying that token the coordinator derives
`owner`/`org` from the token itself and reports `auth=github`.

Shared bearer-token automation has no embedded identity, so the broker reads
`owner`/`org` from the `X-Crabbox-Owner` and `X-Crabbox-Org` request headers
and reports `auth=bearer`. The CLI fills those headers from, in order of
precedence:

- `CRABBOX_OWNER` (owner header);
- `GIT_AUTHOR_EMAIL`, then `GIT_COMMITTER_EMAIL`;
- `git config --get user.email`;
- `CRABBOX_ORG` (org header).

A verified Cloudflare Access JWT (`cf-access-jwt-assertion`, validated against
the team's public keys) can also supply the owner email on the broker side.
Raw, unverified Access identity headers are ignored.

## Exit codes

```text
0   identity resolved successfully
1   request failed (token rejected, org membership missing, network error)
2   no coordinator configured (broker URL or token missing)
```

Run `whoami` early in CI scripts to fail fast on auth problems before
provisioning any boxes.

## Related docs

- [login](login.md)
- [logout](logout.md)
- [config](config.md)
- [Auth and admin](../features/auth-admin.md)
- [Broker auth and routing](../features/broker-auth-routing.md)
