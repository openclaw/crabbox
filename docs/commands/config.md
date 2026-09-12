# config

`crabbox config` inspects and updates user configuration. It has three
subcommands:

```text
crabbox config path
crabbox config show [--provider <provider>] [--json]
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
crabbox config show --provider local-container --json
```

The merge combines, in order: the user config file, then any repo-local
`crabbox.yaml` or `.crabbox.yaml` found in the current directory (a repo file
overrides user defaults for that checkout), then environment variables. When
`CRABBOX_CONFIG` is set, only that file is read (the repo-local files are
skipped). This changes selection only: an explicit path inside the active
repository, or a symlink that resolves into it, still has repository trust.
`config show` reflects the resulting effective values, including
provider defaults applied at load time. Apart from its own provider-selection
override, it does not replay flags from a previous `run` or another command;
it does not accept every provider-specific run flag. The provider line includes
`provider_selected` and `provider_source`
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

Phala settings appear in the JSON `phala` section and the text `phala` line.
Its `attest` value preserves the configured state: JSON `null` (text `default`)
means no explicit override; `true` and `false` remain distinct. This is a
configuration value, not evidence that remote attestation has run or passed.
Inspection does not read Phala's stored credentials or invoke its CLI.

Freestyle and Crownest also have value sections in both formats, including
when unselected. Freestyle shows the loaded URL, relative workdir, CPU/memory
settings and API-key presence (`auth: configured` or `missing`), never the key.
Zero sizing remains zero rather than a guessed service-plan default. Crownest
shows its loaded URL, project, template, timeout and cleanup preference without
looking up credentials. Zero timeout and explicit false remain visible. URLs
are redacted; these values are configuration, not live-provider proof.

OpenComputer, OpenSandbox and CUA expose their loaded settings in the JSON
`openComputer`, `openSandbox` and `cua` sections and corresponding lowercase
text lines, even when unselected. URLs are redacted; raw zero, false and empty
values are not replaced with service defaults. These sections do not discover
credentials or read external CLI configuration. CUA's bridge command and SDK
package/import names are configured references, not evidence that an executable
or SDK is installed or working. Displaying them does not execute the bridge or
enable CUA provisioning.

### Offline provider status

JSON adds a `providerStatus` object with `schemaVersion: 1`, `kind: "offline"`,
and a `providers` map keyed by canonical provider name. Existing effective-value
and auth-presence fields remain available. Text includes an offline-inspection
notice and `provider_status` lines describing the same distinctions.

Each entry separates these questions:

| Field | Meaning |
| --- | --- |
| `supported` | The provider is registered in this compiled binary, not necessarily available on this host or account. |
| `selection.selected` / `selection.source` | Whether this load explicitly selects the provider and the selection layer; an unselected entry has a null source. Compiled compatibility defaults alone do not select a provider. |
| `configuration` | Accepted provider-specific and generic configuration inputs in this load, described below. |
| `authentication` | Declared possible provider-access interfaces, with status `unchecked`; not credential discovery or a successful login. |
| `readiness` | Always `unchecked` in this offline report. |

`authentication.scope` is `provider_access`. `authentication.methods` lists
possible interfaces across the declared routes; `authentication.routes` holds
objects with `route`, `methods`, and `description` to qualify those interfaces.
Neither list establishes which route is active or which credentials are
available. `authentication.status` remains `unchecked`. Guest SSH, desktop,
bootstrap, registry, and deployment authentication are separate scopes.

`configuration.providerInput` and `configuration.genericInput` each contain
`state`, `sources`, and `complete`. Input state is `present` when an accepted
value or explicit intent was recorded, `none` only when complete accounting
establishes no input, and otherwise `unknown`. Source lists use `user_config`,
`repo_config`, `environment`, and `flag` in that order. They record contributing
accepted layers, **not winning field origins**: an equal-value assignment can
count, an ignored input does not, and a later override does not erase an earlier
accepted layer. Provider selection by itself is not provider configuration.

The enclosing configuration state follows this contract:

| State | Meaning |
| --- | --- |
| `explicit` | Provider-specific input was accepted. It need not be sufficient or valid for execution. |
| `generic_inputs_present` | Complete accounting establishes no provider-specific input, but generic input was accepted. This does not mean every generic setting applies to that provider. |
| `defaults_only` | Complete accounting establishes no provider-specific or generic input. Visible values may still come from defaults. |
| `unknown` | The available input history cannot establish one of the preceding states. |

Input-history completeness is established only after the canonical loader
successfully accounts for all configuration layers, and only for its audited
provider roster. Untracked providers, partial overlays, programmatically
constructed configurations, and snapshots with synthesized job arguments remain
incomplete. Without complete accounting, absent observed input stays `unknown`
rather than becoming `defaults_only`. Completeness concerns input history only;
it is never a readiness or authorization result.

`config show --provider <name>` changes selection and resolves display defaults
for that provider; it does not test access, replay earlier run flags, or add
provider configuration merely by selecting it. Use
`crabbox doctor --provider <name>` for the provider's diagnostic path. Doctor
may inspect local tooling and use provider authentication or network checks;
its results apply to the checks it actually performs, not every provider listed
by this offline report.

The top-level JSON `ttl` and `idleTimeout` fields report the effective generic
lease durations. Text output reports them on a separate
`lease ttl=<duration> idle_timeout=<duration>` line immediately after the provider
summary. Both formats use normalized Go duration strings: the existing `90m`
TTL and `30m` idle-timeout defaults appear as `1h30m0s` and `30m0s`.
Within each config file, nested `lease.ttl` and `lease.idleTimeout` override
top-level `ttl` and `idleTimeout`; the file layering described above still
applies, and `CRABBOX_TTL` / `CRABBOX_IDLE_TIMEOUT` take precedence over files.
These are merged configuration values, not per-command flag overrides or
observations of an existing lease or server. Provider-specific durations
(such as `blaxel.ttl` or `githubCodespaces.idleTimeout`) and per-job durations
under `jobs` remain separate from these generic values.

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
`accessAuth`, and `cloudflare.auth` for the same values. These legacy
`configured`/`missing` or method-presence labels retain their existing meaning:
they are not authentication success or readiness, and are separate from
`providerStatus` authentication metadata.

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

Private local run recording is off by default. Set `history.local.enabled: true`
only in your user config to enable it for runs; repository files cannot change
this policy. An explicit `run --record-local=false` overrides the user setting.
See [local history](../features/history-logs.md#private-local-history) for storage
bounds, output scope, and offline readers.

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
