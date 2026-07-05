# login

`crabbox login` authenticates the CLI against a coordinator, stores the
resulting credentials in your user config, and verifies them with
`GET /v1/whoami`. The coordinator may run on Cloudflare or Node/PostgreSQL.

There is no built-in hosted broker. You must supply a broker URL with `--url`, or
have one already configured (`crabbox config set-broker`). Without a broker, the
command exits with an error.

## Usage

```sh
crabbox login --url <broker-url>
```

This starts a GitHub OAuth login: Crabbox opens the login URL in your browser and
listens on a random `127.0.0.1` port for a one-use browser confirmation. The
broker releases the user-scoped bearer token only when the initiating terminal
presents both that confirmation and its private polling secret.

If the browser cannot open automatically, print the URL and open it manually in
a browser on the same device. Cross-device completion intentionally fails
closed because the browser must return to the initiating CLI's loopback listener:

```sh
crabbox login --url https://broker.example.com --no-browser
```

On success the command prints the resolved broker, default provider, GitHub
identity, and the config path it wrote:

```text
logged in broker=https://broker.example.com provider=aws user=alice@example.com org=example-org config=/Users/alice/.config/crabbox/config.yaml
```

## Flags

```text
--url <url>                       broker URL (falls back to the configured broker)
--provider hetzner|aws|azure|gcp  default brokered provider to store with the broker
--no-browser                      print the same-device GitHub login URL instead of opening it
--token-stdin                     read a broker token from stdin (operator automation)
--json                            print machine-readable JSON
```

When `--url` is omitted, the configured broker URL is used; `--provider` likewise
falls back to the configured default provider when unset.

## Operator automation with `--token-stdin`

Trusted operator automation can skip the browser flow and write a shared broker
token instead. The token is read from stdin so it never lands in shell history or
the process argument list:

```sh
printf '%s' "$CRABBOX_COORDINATOR_TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

This path stores the shared operator token verbatim and should stay limited to
trusted maintainers. Prefer interactive GitHub login, which issues a per-user
token, for everyday use.

## What gets stored

Login writes the broker URL and token to your user config (and, when
`--provider` is set, the default provider). Inspect the result without exposing
secrets:

```sh
crabbox config show
crabbox config path
```

## Self-hosted brokers

Each broker owns its own GitHub OAuth credentials and admission policy. A
separate organization or private deployment needs its own coordinator runtime,
secret injection, and GitHub OAuth app.

The coordinator requires current CLIs to provide an ephemeral HTTP loopback
callback using the literal `127.0.0.1` address, a random port, and a random path.
Old clients that can only poll for a completed browser token are rejected before
OAuth starts.

Configure that GitHub OAuth app with a callback URL that exactly matches the
broker's public origin:

```text
https://<your-broker-host>/v1/auth/github/callback
```

Set the same public origin in `CRABBOX_PUBLIC_URL` on the coordinator, then deploy
`CRABBOX_GITHUB_CLIENT_ID`, `CRABBOX_GITHUB_CLIENT_SECRET`,
`CRABBOX_SESSION_SECRET` (distinct from `CRABBOX_SHARED_TOKEN`), and the relevant
`CRABBOX_GITHUB_ALLOWED_ORG(S)` or
`CRABBOX_GITHUB_ALLOWED_TEAMS` values. A GitHub `Invalid redirect_uri` error means
the callback URL generated during `crabbox login` does not match one configured on
that OAuth app.

## Related

- [whoami](whoami.md) — show the broker identity for the stored credentials
- [logout](logout.md) — remove the stored broker token
- [config](config.md) — manage broker URL and tokens directly
- [Broker auth and routing](../features/broker-auth-routing.md)
