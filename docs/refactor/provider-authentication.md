# Static provider authentication metadata

Each provider owns `ProviderSpec.Authentication`: a list of possible
provider-access routes and their authentication interfaces. This is declaration
data only. It does not select a route, inspect credentials, discover a CLI,
perform authentication, or establish readiness.

Provider access means the control-plane interface, or the sole connection
interface for SSH/external adapters. Guest SSH and desktop authentication,
bootstrap credentials, image-registry logins, and deployment credentials remain
separate requirements. In particular, `local_context` does not mean that the
complete provider lifecycle needs no credentials.

## Vocabulary

| Method | Meaning |
|---|---|
| `api_key` | An explicit service API key, even when passed through a CLI or SDK. |
| `api_token` | A service access, bearer, ACL, or API token. |
| `session_token` | An interactive provider session credential. |
| `api_credentials` | A multi-part API credential bundle. |
| `username_password` | Provider API username/password login. |
| `cli` | A provider-owned executable resolves and uses its own authentication state. |
| `sdk_credentials` | A documented SDK credential chain, profile, or auth helper resolves credentials. |
| `native_config` | Native client connection/trust configuration chooses authentication. |
| `ssh` | Existing SSH identity/configuration authenticates the provider control or sole connection. |
| `local_context` | The local runtime/host process context and permissions. |
| `external_contract` | An external executable owns the otherwise unspecified authentication contract. |
| `shared_secret` | A documented gateway shared secret. |
| `identity_token` | A service-audience identity token. |
| `coordinator` | Client access to the Crabbox coordinator; provider credentials remain server-side. |
| `none` | A documented conditional mode without authentication, never inferred from missing input. |

`DirectProviderAuthentication` snapshots one direct route's method list. Multiple
methods list possible interfaces, not necessarily alternatives or jointly
required credentials. Route descriptions qualify conditional combinations.
Providers with genuinely different routes declare them explicitly:

- AWS, Azure, Daytona, GCP, and Hetzner separate direct from brokered access.
- Cloud Run Sandbox separates gateway access from an embedded local launcher.
  A private IAM gateway additionally needs an identity token alongside its
  shared secret.
- Parallels separates local-host access from a remote Mac reached over SSH.
- Nomad distinguishes ACL-enabled clusters from its documented ACL-disabled
  local/development mode; metadata does not determine the current cluster mode.

Provider methods are not inferred from kind, CLI paths, token presence, or
comparison with default values. The linked-provider metadata test checks
coverage, vocabulary, unique routes/methods, and independent method snapshots
without configuring providers or invoking clients.

## Public offline projections

The metadata now feeds public offline output: `config show` adds
`providerStatus` (schema version 1, kind `offline`), while the provider matrix,
`providers describe`, and `providers recommend` share `metadataKind: "static"`
and the same authentication/readiness metadata. The matrix and recommendation
JSON formats remain arrays; describe retains its existing schema version.

Authentication status and readiness are always `unchecked` here. The scope is
`provider_access`: control-plane authentication, or the sole connection
interface for SSH/external adapters. It excludes guest/desktop login, bootstrap,
registry, and deployment credentials. The wire keys are
`authentication.methods` (possible interfaces) and `authentication.routes`
(qualified objects with `route`, `methods`, and `description`), alongside
`authentication.scope` and `authentication.status`. These are declarations,
not detected availability, a selected route,
required credential combinations, or authentication success.

Accepted-input attribution is separate from these declarations. The config
status distinguishes compiled support, explicit provider selection, and
provider/generic input histories. Sources name accepted layers rather than
winning field origins; values equal to defaults can still be explicit input.
Only the successful canonical loader certifies input history, using its explicit
audited owner roster rather than assuming every registered provider is tracked.
Partial, untracked and synthesized execution snapshots remain incomplete.
Neither complete input accounting nor this metadata establishes readiness.

Existing `configured`/`missing` and auth-presence fields in config output retain
their previous meaning; they are not replaced by, or evidence for, a successful
authentication status. `config show --provider` only selects which provider's
effective settings to display. It does not replay run flags or probe access.
`doctor --provider` is the separate provider diagnostic boundary, and
`providers sizes` remains a live catalog command rather than static metadata.

See [config status](../commands/config.md#offline-provider-status) and
[provider discovery](../commands/providers.md#offline-status-versus-live-checks)
for the output contract.
