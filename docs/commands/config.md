# config

`crabbox config` manages user config.

```sh
crabbox config path
crabbox config show
crabbox config show --json
printf '%s' "$TOKEN" | crabbox config set-broker --url https://crabbox.openclaw.ai --provider aws --token-stdin
```

Subcommands:

```text
path
show [--json]
set-broker --url <url> --token-stdin [--provider hetzner|aws]
```

`config show` reports `access_auth` as `missing`, `service-token`, `token`, `service-token+token`, or `incomplete` so operators can confirm whether Cloudflare Access credentials are configured without printing secret values. Store Access secrets only in user config or environment variables, not repo-local config.

User config lives under the OS user config directory. Repo-local `crabbox.yaml` or `.crabbox.yaml` can override user defaults for a checkout. Keep project-specific sync, env, capacity, and Actions policy in repo config, not in the Crabbox binary:

```yaml
profile: project-check
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
