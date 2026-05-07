# config

`crabbox config` manages user config.

```sh
crabbox config path
crabbox config show
crabbox config show --json
printf '%s' "$TOKEN" | crabbox config set-broker --url https://crabbox.openclaw.ai --provider aws --token-stdin
printf '%s' "$ADMIN_TOKEN" | crabbox config set-broker --url https://crabbox.openclaw.ai --admin-token-stdin
```

Subcommands:

```text
path
show [--json]
set-broker --url <url> [--token-stdin] [--admin-token-stdin] [--provider hetzner|aws|azure]
```

`config show` reports broker auth as `auth` and `admin_auth`, plus
`access_auth` as `missing`, `service-token`, `token`, `service-token+token`, or
`incomplete`, without printing secret values. Store broker tokens and Access
secrets only in user config or environment variables, not repo-local config.
User config is written with `0600` permissions, and `crabbox doctor` flags
broader permissions.

User config lives under the OS user config directory. Repo-local `crabbox.yaml` or `.crabbox.yaml` can override user defaults for a checkout. Keep project-specific sync, env, capacity, and Actions policy in repo config, not in the Crabbox binary:

```yaml
profile: project-check
tailscale:
  enabled: true
  network: auto
  tags:
    - tag:crabbox
  hostnameTemplate: crabbox-{slug}
  authKeyEnv: CRABBOX_TAILSCALE_AUTH_KEY
  exitNode: mac-studio.example.ts.net
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

`tailscale.enabled` requests tailnet join for new managed Linux leases.
`tailscale.network` selects the SSH target resolution path:

- `auto`: prefer Tailscale when lease metadata exists and SSH is reachable;
- `tailscale`: require the tailnet path;
- `public`: force the provider/public host.

Brokered `--tailscale` leases use Worker-minted one-off auth keys. Direct
provider leases read a local one-off key from `tailscale.authKeyEnv`; do not
store that key in repo config.

`tailscale.exitNode` routes lease egress through an approved tailnet exit node.
`tailscale.exitNodeAllowLanAccess` keeps LAN access available while using that
exit node.
