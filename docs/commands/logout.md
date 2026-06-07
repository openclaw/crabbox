# logout

`crabbox logout` clears the stored broker token from your user config so the CLI
no longer authenticates to the broker.

```sh
crabbox logout
crabbox logout --json
```

## What it changes

Logout edits only the user config file (the path printed by
[`crabbox config path`](config.md)). It clears both token fields the broker
reads:

- the broker section's `token`, and
- the top-level `coordinatorToken`.

Everything else is left in place:

- the broker `url` and `provider`, so a later [`crabbox login`](login.md) can
  reuse the configured endpoint;
- the broker **admin token** (`broker.adminToken`), if one is stored — logout
  does not touch it;
- per-lease SSH keys, repo claims, and recorded run history.

On success it prints:

```text
logged out config=/path/to/config.yaml broker_auth=missing
```

With `--json` it emits the resolved config path and the new auth state:

```json
{ "config": "/path/to/config.yaml", "brokerAuth": "missing" }
```

## Flags

| Flag     | Description                |
| -------- | -------------------------- |
| `--json` | Print machine-readable JSON. |

## After logout

- Broker-backed commands such as [`crabbox whoami`](whoami.md), `crabbox run`,
  and `crabbox warmup` fail to authenticate against the broker because no token
  is sent. Re-run [`crabbox login`](login.md) (or `crabbox login --token-stdin`)
  to restore access.
- Direct-provider mode keeps working when local provider credentials are present
  (for example AWS SDK credentials or `HCLOUD_TOKEN`), since direct mode talks to
  the cloud API and does not need a broker token.

## When to use it

- A token has leaked or you want to rotate it.
- You are switching the operator identity on a shared workstation.
- You are exercising the unauthenticated path.

To remove everything — broker URL, provider, both tokens, and profile defaults —
edit the user config file directly.

## Related

- [login](login.md)
- [whoami](whoami.md)
- [config](config.md)
- [Auth and admin](../features/auth-admin.md)
- [Configuration](../features/configuration.md)
