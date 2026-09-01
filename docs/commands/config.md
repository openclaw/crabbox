# config

`crabbox config` inspects and updates user configuration. It has three
subcommands:

```text
crabbox config path
crabbox config show [--json]
crabbox config set-broker --url <url> [--provider <provider>] [--mode managed|registered] [--auto-webvnc=false] [--token-stdin] [--admin-token-stdin]
```

## config path

Prints the selected user config path:

```sh
crabbox config path
```

The file lives at `<os-user-config-dir>/crabbox/config.yaml` (for example
`~/.config/crabbox/config.yaml` on Linux or
`~/Library/Application Support/crabbox/config.yaml` on macOS). Set
`CRABBOX_CONFIG` to point at a different file; that override is used for both
reads and writes. `config path` and the `config=` header from `config show`
report that override exactly as supplied, including a relative or symlink path.
Without an override, they report the absolute OS user-config path. Reporting a
path does not create the file or change its trust classification.

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
skipped). This changes selection only: an explicit path inside the active
repository, or a symlink that resolves into it, still has repository trust.
`config show` reflects the resulting effective values, including
provider defaults applied at load time; per-command flags are not part of what
it reports. The provider line includes `provider_selected` and `provider_source`
(JSON: `providerSelected` and `providerSource`). With only compatibility
metadata, the public `provider` value is the empty string and the state is
`provider_selected=false provider_source=compiled_default`; it is not an
actionable provider selection. The public top-level `serverType` / text `type`
is also empty in that state so provider-specific compatibility defaults are not
presented as effective; provider-specific configuration sections remain visible.
Selections in `user_config`, `repo_config`, or
the `environment` retain the canonical provider name and report selected=true. Passing
`config show --provider <name>` reports `flag` because that command-scoped
override wins the merge.

`architecture` (text: `arch`) describes the configured architecture or the
provider's implicit selection, not a host observation or proof of runtime
support. `config show` is offline and does not acquire or probe a host.
JSON `architectureExplicit` (text:
`architecture_explicit`) is true for a nonempty YAML `architecture` or
`CRABBOX_ARCH`, and false for the omitted default. Execution commands also treat
an explicit `--arch`, including `--arch amd64`, as an assertion; their flags are
not part of `config show` output.

For `local-container`, an omitted architecture is reported as `arch=native`
(JSON: `"architecture":"native"`). This describes the runtime's native selection,
not a resolved daemon architecture; config inspection does not probe Docker or
Podman. Explicit `amd64` or `arm64` selections remain unchanged. `native` is a
diagnostic value, not a new accepted `--arch` or configuration input.

Static SSH now checks these assertions against fresh host evidence, including
inherited `amd64` values. See [Upgrading existing static-host configuration](../providers/ssh.md#upgrading-existing-static-host-configuration)
to keep a strict constraint or remove the contributing values for automatic
discovery; a blank override does not clear an inherited assertion.

The JSON `localContainer` object and text `local_container` line include these
public settings:

| JSON field | Meaning and default |
| --- | --- |
| `runtime` | Configured Docker-compatible executable; defaults to `docker`. |
| `image` | Configured image, or the reviewed OCI image selected by `os`. |
| `user` | Container SSH user; defaults to `crabbox`. |
| `workRoot` | Provider workspace root, resolved as described below. |
| `cpus` | Numeric CPU limit; `0` leaves the runtime default. |
| `memory` | Memory limit such as `6g`; an empty string leaves the runtime default. |
| `network` | Container network; defaults to `bridge`. |
| `dockerSocket` | Whether socket pass-through is configured; defaults to `false`. |

When `local-container` is selected, both its `workRoot` and the top-level
`workRoot` use the provider's effective defaulting rules: an explicit provider
root wins over the generic root, and socket mode uses its host-visible cache
root on POSIX when neither root is explicit. Windows retains the Linux guest
root. See [socket pass-through](../providers/local-container.md#socket-pass-through).
When another provider or no provider is selected, the section retains its
merged settings, including an empty provider root when omitted.

Inspection stays offline: `runtime=docker` is a configured/default value, not
evidence of an installed executable or a reachable daemon. It does not discover
Docker/Podman, inspect sockets, or acquire a container. Zero, false, and empty
JSON values remain present; text uses `-` for an empty work root or memory limit.
CLI-only volumes and internal checkpoint metadata are excluded.

The JSON `incus` object includes the merged Incus settings: connection selectors,
instance type, image, profile, SSH/proxy settings, release policy, timeout, and
TLS options. `address` and `remoteImageServer` use the endpoint redaction below;
`socket` and `tlsServerCert` report configured paths, not file contents.
Inspection does not resolve named Incus remotes or contact the daemon, so an
empty `project` remains empty until Incus resolves its remote/project default.

Secrets are never printed. Token-bearing fields are reduced to a status word:

- Provider endpoint URL userinfo is replaced with `<redacted>@`; query and
  fragment components are omitted because they may carry credentials.
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
  gitOverlay: false
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
