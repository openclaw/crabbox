# config

`crabbox config` inspects and updates user configuration. It has three
subcommands:

```text
crabbox config path
crabbox config show [--json]
crabbox config set-broker --url <url> [--provider <provider>] [--mode managed|registered] [--auto-webvnc=false] [--token-stdin] [--admin-token-stdin]
```

## config path

Prints the absolute path of the user config file:

```sh
crabbox config path
```

The file lives at `<os-user-config-dir>/crabbox/config.yaml` (for example
`~/.config/crabbox/config.yaml` on Linux or
`~/Library/Application Support/crabbox/config.yaml` on macOS). Set
`CRABBOX_CONFIG` to point at a different file; that override is used for both
reads and writes.

## config show

Prints the merged effective configuration with secret values redacted:

```sh
crabbox config show
crabbox config show --json
```

The merge combines, in order: the user config file, then any repo-local
`crabbox.yaml` or `.crabbox.yaml` found in the current directory (a repo file
overrides user defaults for that checkout), then environment variables. When
`CRABBOX_CONFIG` is set, only that file is read (the repo-local files are
skipped). `config show` reflects the resulting effective values, including
provider defaults applied at load time; per-command flags are not part of what
it reports.

Secrets are never printed. Token-bearing fields are reduced to a status word:

- Broker tokens, Cloudflare/Proxmox/Upstash tokens: `configured` or `missing`.
- Cloudflare Access auth: `missing`, `service-token` (client ID + secret),
  `token` (service token), `service-token+token`, or `incomplete` (only one of
  ID/secret set).

The text output labels broker auth as `auth` / `admin_auth`, and Access auth as
`access_auth`. The `--json` output uses the keys `brokerAuth`, `brokerAdminAuth`,
`accessAuth`, and `cloudflare.auth` for the same values.

## config set-broker

Stores the broker URL and optional tokens in the user config file:

```sh
# Set the broker URL and default brokered provider.
crabbox config set-broker --url https://broker.example.com --provider aws

# Register direct-provider leases without transferring lifecycle ownership.
crabbox config set-broker --url https://broker.example.com --mode registered

# Store a user token (read from stdin so it never lands in shell history).
printf '%s' "$TOKEN" | crabbox config set-broker --url https://broker.example.com --token-stdin

# Store an admin token.
printf '%s' "$ADMIN_TOKEN" | crabbox config set-broker --url https://broker.example.com --admin-token-stdin
```

Flags:

- `--url <url>` (required) — broker URL.
- `--provider <provider>` — default provider. Managed mode supports the
  coordinator providers; registered mode accepts any configured direct provider.
  When set, it also becomes the default `provider` in user config.
- `--mode managed|registered` — `managed` lets supported providers use the
  broker control plane; `registered` keeps provider lifecycle local and mirrors
  lease metadata to the broker.
- `--auto-webvnc=false` — disable automatic portal WebVNC startup for kept
  registered desktop leases. The default is true.
- `--token-stdin` — read the broker token from stdin.
- `--admin-token-stdin` — read the broker admin token from stdin.

Only `--url` is required; tokens and provider are optional. Reading tokens from
stdin keeps them out of the process table and shell history. The command writes
the user config file (creating the directory with `0700` and the file with
`0600`) and prints the resulting path and auth status, for example:

```text
wrote /home/alice/.config/crabbox/config.yaml broker=https://broker.example.com mode=registered auth=configured admin_auth=missing
```

`crabbox login` performs the same broker write as part of GitHub login; use
`config set-broker` when you already hold a token and only need to record it.

## Where secrets belong

Prefer user config, environment variables, or a credential manager for broker
tokens, provider tokens, and Access secrets. Repository config is trusted
project automation and may intentionally define a complete custom
endpoint-and-credential pair, but Crabbox refuses to combine a
repository-defined destination with an inherited credential. The user config
file is written with `0600` permissions, and `crabbox doctor` flags it when the
permissions are broader than that.

## Repo-local config

User config holds machine-wide defaults and secrets; repo-local config holds
project-specific, checkout-shareable settings. Keep sync rules, environment
allow-lists, capacity policy, and Actions hydration settings in repo config so
they travel with the project:

```yaml
profile: project-check
tailscale:
  enabled: true
  network: auto
  tags:
    - tag:crabbox
  hostnameTemplate: crabbox-{slug}
  authKeyEnv: CRABBOX_TAILSCALE_AUTH_KEY
  exitNode: build-host.example.ts.net
  exitNodeAllowLanAccess: true
capacity:
  market: spot
  strategy: most-available
  fallback: on-demand-after-120s
actions:
  workflow: .github/workflows/crabbox.yml
sync:
  checksum: false
  gitSeed: true
  fingerprint: true
  timeout: 15m
  warnFiles: 50000
  warnBytes: 5368709120
  failFiles: 150000
  failBytes: 21474836480
  allowLarge: false
  exclude:
    - node_modules
    - dist
env:
  allow:
    - CI
    - NODE_OPTIONS
    - PROJECT_*
```

`tailscale.enabled` requests a tailnet join for new managed Linux leases.
`tailscale.network` selects how the SSH target is resolved:

- `auto` — prefer Tailscale when lease metadata exists and SSH is reachable;
- `tailscale` — require the tailnet path;
- `public` — force the provider/public host.

Brokered `--tailscale` leases use Worker-minted one-off auth keys. Direct
provider leases read a local one-off key from the variable named by
`tailscale.authKeyEnv`; do not store that key in repo config.
`tailscale.exitNode` routes lease egress through an approved tailnet exit node,
and `tailscale.exitNodeAllowLanAccess` keeps LAN access available while that
exit node is in use.

## See also

- [login](login.md) — GitHub login that also writes broker credentials.
- [doctor](doctor.md) — local and broker/provider readiness checks.
- [init](init.md) — scaffold repo-local config and workflow files.
