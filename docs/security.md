# Operational Security

Read this when you are standing up a shared broker, deciding what secrets to
forward, or reasoning about who can reach a leased box.

The root [Security Policy](https://github.com/openclaw/crabbox/blob/main/SECURITY.md)
defines the supported security boundary and reporting scope. This guide covers
deployment and operational hardening within that boundary.

Crabbox spans three trust layers, and each owns a different part of the security
posture:

```text
local CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider VM
```

The CLI owns local config, per-lease SSH keys, sync, and remote command
execution. The coordinator owns authentication, authorization, lease
state, provider credentials, cost guardrails, and cleanup. Providers own VM
creation, network reachability, and deletion. Delegated-run providers such as
Docker Sandbox also own the command transport and runtime that receive commands
and explicitly forwarded environment values.

## Trust Model

Crabbox is built for trusted operators on a shared team, not for arbitrary
untrusted tenants. Assume:

- The local OS user and configured provider tooling are trusted.
- Repository configuration is executable project automation and must be
  reviewed like a Makefile, package script, or CI workflow.
- Operators can run arbitrary commands on the boxes they lease.
- A box may observe any local environment value the CLI forwards to it.
- Operators are trusted not to attack each other deliberately.
- Bugs and crashes still happen, so cleanup must be defensive and idempotent.

Do not place mutually untrusted tenants on the same broker, in the same
[pond](features/pond.md), or behind a single shared token. Per-lease and
per-tenant isolation is not the current security boundary. Local providers and
portal bridges are development execution surfaces, not a uniform sandbox for
hostile project configuration or mutually adversarial workloads.

For any portal that exposes browser Code, configure the required per-lease
`CRABBOX_CODE_ORIGIN_TEMPLATE` described in [Browser portal](features/portal.md).
It keeps lease-controlled code-server HTML and JavaScript off the coordinator
origin and off other leases' browser origins without changing the normal Code
entry URL. Browser Code fails closed when this setting is missing or invalid.
This isolation does not turn the broker into a hostile multitenant sandbox.

## Authentication

Every non-health route normally requires a Bearer token; requests without one
are rejected `401 unauthorized`. The Node runtime can instead accept an
explicitly configured trusted reverse-proxy identity from allowlisted peer
CIDRs. The generally unauthenticated routes are `GET /v1/health`, the GitHub
login/OAuth and portal login routes, and bridge agent upgrades that use
short-lived tickets. The workspace lifecycle and desktop-connection routes also
accept the dedicated `CRABBOX_RUNTIME_ADAPTER_TOKEN` as a non-admin service
identity. That credential cannot attach workspace terminals and is rejected from
every other coordinator route. Normal authentication is resolved in
`worker/src/auth.ts` in this precedence:

1. **Admin token** — the request token equals the coordinator secret
   `CRABBOX_ADMIN_TOKEN`. Grants admin scope.
2. **Shared operator token** — the token equals `CRABBOX_SHARED_TOKEN`. Grants a
   non-admin shared identity for automation.
3. **Signed user token** — a `cbxu_`-prefixed token issued by GitHub browser
   login. It is an HMAC-SHA256 signature (verified in constant time) over a
   base64url payload signed with `CRABBOX_SESSION_SECRET`, which must be set and
   must differ from `CRABBOX_SHARED_TOKEN`. The versioned payload carries a
   verified-email `owner`, its `github-verified-email` provenance, `org`, and
   GitHub `login`, has a default 180-day expiry, and contains the OAuth
   credential encrypted under a key derived from the session secret. Every
   request revalidates current allowed-org/team membership through GitHub, with
   successful checks cached for five minutes. GitHub errors fail closed after
   the cache expires. The token is rejected if it carries an `admin` claim —
   browser login can never mint admin tokens. Tokens from older schemas are
   rejected; users must log in again after this security upgrade.

### GitHub browser login

`crabbox login --url <broker-url>` opens a GitHub OAuth flow and stores the
returned signed user token in local config. Authorization during login is gated
by coordinator config:

- `CRABBOX_GITHUB_ALLOWED_ORG` / `CRABBOX_GITHUB_ALLOWED_ORGS` restrict login to
  members of the listed GitHub org(s).
- `CRABBOX_GITHUB_ALLOWED_TEAMS` (or `CRABBOX_GITHUB_ALLOWED_TEAM`) further
  narrows access to selected team slugs after org membership passes.
- `CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS` controls successful request-time
  membership caching (default 300, maximum 3600; set 0 to check every request).
- `CRABBOX_GITHUB_REVOKED_USERS` immediately rejects comma-separated GitHub
  logins or verified emails. Optional `login:` / `owner:` prefixes disambiguate
  values. Use this for narrow emergency revocation without rotating every token.
- The GitHub account must expose at least one verified email through the OAuth
  `user:email` scope. Public profile and unverified emails are never trusted as
  the token owner.

CLI login also binds token release to the initiating device. The CLI opens a
one-use listener on a random `127.0.0.1` port and path; after GitHub authorization,
the browser receives a random confirmation through that loopback URL. Polling
requires both the CLI-held secret and the browser confirmation, so forwarding an
authorization URL cannot deliver the resulting user token to the sender.

Unauthenticated CLI and portal login starts share a ten-pending-attempt limit per
caller source and a 100-attempt global backstop. Admission and storage are serialized,
expired attempts are removed first, and only a session-secret-keyed source hash is
stored with each attempt. Cloudflare supplies the caller address; the portable Node
server derives it from the socket or an explicitly trusted proxy instead of trusting
a client-provided forwarding header.

User tokens can only mutate or read leases, runs, and usage for their own `owner`/`org`
identity. Lease owners also have read-only audit access to run history, logs,
events, telemetry, and live event subscriptions for work recorded against
their leases, including runs that later replace the active backing lease. See
[auth and admin](features/auth-admin.md) and [broker auth
routing](features/broker-auth-routing.md) for the full flow.

### Cloudflare Access (optional defense-in-depth)

Cloudflare Access can sit in front of a custom broker hostname (for example
`broker.example.com`) as an edge layer. It does **not** replace Crabbox auth: a
request must clear Access at the edge *and* present a valid Crabbox token before
any lease, run, log, usage, or admin route is reached.

The Worker never trusts raw Access identity headers. If it uses an
Access-provided email, it first verifies the `cf-access-jwt-assertion` JWT
(RS256, key fetched from `https://<team-domain>/cdn-cgi/access/certs`) against
`CRABBOX_ACCESS_TEAM_DOMAIN` and `CRABBOX_ACCESS_AUD`, checking issuer,
audience, and expiry. Before forwarding any request to the Fleet Durable Object,
the Worker strips caller-supplied `cf-access-authenticated-user-email` and
`cf-access-jwt-assertion` headers and injects its own derived identity
(`x-crabbox-auth`, `-admin`, `-owner`, `-org`, `-github-login`).

The local service-token credentials `CRABBOX_ACCESS_CLIENT_ID` and
`CRABBOX_ACCESS_CLIENT_SECRET` only satisfy the Access edge; they authorize no
Crabbox action by themselves. Prefer user config or env. If a trusted
repository intentionally defines both a custom broker URL and matching Access
credentials, Crabbox treats them as one source; it will not send inherited
Access credentials to a repository-defined broker URL.

### Trusted reverse proxy identity

The Node runtime can accept an identity header from an ingress:

```text
CRABBOX_TRUSTED_USER_HEADER=X-Authenticated-User
CRABBOX_TRUSTED_USER_ORG=example-org
CRABBOX_TRUSTED_PROXY_CIDRS=10.42.7.19/32,fd00:1234::19/128
CRABBOX_TRUSTED_PROXY_SECRET=replace-with-a-random-secret
```

The socket peer must match the CIDR allowlist. The ingress must authenticate the
caller and remove caller-supplied copies of the configured identity and
`X-Crabbox-Proxy-Secret` headers. When `CRABBOX_TRUSTED_PROXY_SECRET` is set, the
ingress must send that value in `X-Crabbox-Proxy-Secret`; the coordinator strips
it before routing the request. Allow only exact proxy addresses or dedicated
subnets, or require the secret when direct access cannot be blocked. This path
grants non-admin scope only; keep `CRABBOX_ADMIN_TOKEN` separate. The same proxy allowlist controls
whether forwarded host, protocol, and client-IP headers affect URL construction
and provider ingress rules.

`X-Crabbox-Proxy-Secret` is reserved and cannot be used as
`CRABBOX_TRUSTED_USER_HEADER`.

## Authorization

There are three effective roles:

```text
user        acquire/heartbeat/release own leases; read own and owned-lease run audit data
operator    shared automation identity via a shared bearer token
admin       view all leases/runs/pool/usage; drain/delete machines; image lifecycle
```

Admin scope comes from `CRABBOX_ADMIN_TOKEN`, or from a signed GitHub user token
whose verified email or login matches `CRABBOX_GITHUB_ADMIN_OWNERS` or
`CRABBOX_GITHUB_ADMIN_LOGINS`. Locally, admin commands can still send the admin
bearer via `CRABBOX_COORDINATOR_ADMIN_TOKEN` or `broker.adminToken`.

Organization labels are exact, case-sensitive identities: 1-63 printable ASCII
characters with no leading or trailing spaces. The coordinator stores them in a
reversible, versioned authorization namespace, so labels such as `science team`
and `science_team` remain distinct. Missing organization identity is also
distinct from a configured organization literally named `unknown`, although
both display as `unknown` in user-facing status. Missing-org principals can
still own resources and receive explicit user shares, but never receive an org
share.

Records created before this namespace was introduced contain only a lossy
organization value, so their original identity cannot be recovered safely.
Those leases, runs, workspaces, ready-pool entries, runners, bridge sessions,
and tickets fail closed for non-admin access after upgrade. Admin lease cleanup
and scheduled provider cleanup remain available; recreate active records under
their exact organization label. Legacy workspace records are not reused or
replenished as prewarms. Per-org admission limits conservatively count legacy
records against every exact label they could have represented.

Long-lived control, WebVNC, Code, and egress sessions bind cached admin authority
to the exact GitHub identity or bearer token plus a deployment grant version.
Changing any configured admin source revokes older active, restored, ticketed,
or durable-session grants on the next authenticated request or scheduled
reconciliation; active senders and recipients are also revalidated while data
flows. Legacy admin attachments without a verifiable source and version fail
closed after upgrade. Restored Code viewer sessions without a complete
organization-bound principal also fail closed during restore and whenever lease
sharing access shrinks.
Non-admin GitHub WebVNC, Code, and egress sessions bind to the exact portal
session and preserve only the encrypted GitHub credential from its signed user
token. Agent-ticket consumption and active traffic recheck portal logout and
emergency user revocation, and revalidate GitHub membership under the configured
membership-cache window. Membership errors fail closed. New or restored legacy
GitHub bridge tickets and attachments without a complete encrypted grant and
portal-session binding fail before carrying traffic; plaintext GitHub
credentials are never stored in bridge state.
Shared-operator requests do **not** trust caller-supplied `X-Crabbox-Owner` /
`X-Crabbox-Org` headers — pin that automation's identity with
`CRABBOX_SHARED_OWNER` (and `CRABBOX_DEFAULT_ORG`), or prefer per-user signed
tokens / verified Access identity instead. Missing shared-token config fails
closed for non-health routes.

## Secrets

There is no central project secret store. Remote command environment values
stay on the operator's machine unless explicitly allowed for forwarding.
Local helper processes, including External provider lifecycle commands, inherit
the Crabbox process environment unless their own runtime provides stronger
isolation.

Handling rules:

- The CLI forwards environment variables only by allowlist. The default allow
  list is `CI` and `NODE_OPTIONS`; extend it with repo-local `env.allow` config
  (or a profile's `env.allow`).
- Future `modelAccess` credentials for Station profiles must not use ordinary
  repo `env.allow`; they need a separate scoped, auditable, revocable delivery
  path. See [Station profiles](features/station-profiles.md).
- Never pass a secret value as a command-line flag.
- Never log environment values; redact secret-looking strings in diagnostics.
- Treat delegated-run providers as part of the runtime trust boundary: when you
  allow a variable for a Docker Sandbox run, Docker Sandbox receives that value
  through its `sbx exec --env-file` path even though Crabbox keeps the value out
  of local process arguments.
- User config files are written `0600`. `crabbox doctor` flags any local config
  whose permissions are broader, because broker tokens may live there.

Example `env.allow` in `.crabbox.yaml`:

```yaml
env:
  allow:
    - CI
    - NODE_OPTIONS
    - PROJECT_*
```

See [environment forwarding](features/env-forwarding.md) for matching and
profile behavior.

### Credential destinations and diagnostics

Credential-bearing coordinator requests follow redirects only when scheme,
hostname, and effective port remain unchanged. The curl transport fallback
does not follow redirects and disables ambient curl configuration before
loading Crabbox's generated request config. Configure the CLI with the final
canonical coordinator or Access-protected origin, not a redirecting alias.
GitHub browser login follows the same destination rule by default: a callback
origin that differs from the selected broker is rejected before browser open,
polling, or config write. Operators who are deliberately migrating between
same-deployment broker aliases can allow specific callback origins with trusted
user config `broker.loginRedirectOrigins` or
`CRABBOX_BROKER_LOGIN_REDIRECT_ORIGINS`.
The coordinator separately requires `CRABBOX_PUBLIC_URL` before GitHub OAuth can
start, stores that exact callback with each pending login, and rejects callbacks
from any other origin before exchanging the authorization code.

Provider clients apply the same destination principle where custom endpoints
are supported. Cloudflare runner, Morph, Railway, and RunPod requests reject
cross-origin redirects before replaying authorization headers or request
bodies. E2B API endpoints require HTTPS except for explicit localhost/loopback
development URLs, and AWS region values are validated before constructing
SigV4 service hosts.

`CRABBOX_CONFIG` changes which file is loaded, not its trust domain. An explicit
path inside the active repository remains repository-sourced for credential
destination checks, including a symlink outside the checkout that resolves
back into it. The canonical user config and explicit files outside the active
repository remain operator-trusted sources.

Repository-selected Static SSH, remote Parallels, External SSH, and exe.dev
control hosts cannot inherit a key, SSH agent, or local SSH configuration from
a more trusted source. Static SSH and Parallels may pair the destination with
a relative, symlink-resolved key file contained by the same repository;
absolute, missing, and repository-escaping key paths do not count as
same-source credentials. They can instead use the matching host flag or
environment variable for approval. External SSH always requires the exact SSH
endpoint (user, host, key, port, fallback ports, and proxy settings) plus any
referenced `resourceName` to be repeated in trusted user config because
operator SSH config and nested proxies can add
authentication independently of an outer key. Repository-controlled template
inputs keep the resulting destination in the repository trust domain, and the
SSH environment-expansion opt-in is honored only from trusted user config.
Protocol or declarative JSON output may supply SSH coordinates only when the
exact output-producing adapter contract and `ssh.trustProviderOutput` are
approved together in trusted user config; repository changes invalidate that
approval.
Because exe.dev control authentication is always ambient, a repository-defined
custom control host requires an explicit
`--exe-dev-control-host` or `CRABBOX_EXE_DEV_CONTROL_HOST` override.

Cookie-authenticated portal mutations and portal viewer WebSocket upgrades
require an exact same-origin browser `Origin` matching `CRABBOX_PUBLIC_URL` (or
the request origin when no public URL is configured). Missing or sibling-origin
intent is rejected before the portal cookie is converted into bearer authority;
explicit bearer API clients remain independent of this browser-only boundary.
Portal logout follows the same boundary: `GET /portal/logout` only renders a
confirmation page, and only a same-origin `POST` clears the portal cookie and
revokes all WebVNC, Code, and mediated-egress bridges bound to that portal
session.

Configured provider credentials are redacted from documented HTTP or streamed
error diagnostics, including Azure Dynamic Sessions, Cloudflare runner, Daytona,
E2B, Freestyle, Islo, Morph, OpenComputer, Railway, RunPod, Semaphore, SmolVM,
Sprites, and Upstash Box. The same final redaction covers `doctor` text and JSON
messages/details. It removes exact configured secrets, authorization and API-key
headers, credential-bearing URL query/userinfo components, common secret JSON
fields, bearer values, and PEM private keys while retaining non-secret routing
context. Providers contribute runtime-only environment and local CLI-store
credentials to that exact-value pass, keeping credential discovery beside the
provider that owns it. Header and bearer fallbacks treat whitespace or an
unescaped JSON quote as the credential boundary, so punctuation inside an
otherwise unknown credential cannot expose a suffix. Generated stop and
failure-routing commands retain provider endpoint routing but remove URL userinfo
before they are printed or stored.
GitHub Actions registration metadata and its short-lived runner token travel
over SSH stdin rather than the remote command line. These guarantees apply to
Crabbox-generated diagnostics and process arguments, not to arbitrary command
output, downloaded artifacts, screenshots, or failure bundles.

### Resource and artifact boundaries

Lifecycle recovery from raw provider identifiers is provider-specific and
fails closed when ownership cannot be established. Cloudflare containers need
a matching local claim. W&B sandbox reuse, status, and stop require an exact
local claim bound to the sandbox ID, API endpoint, entity, and project as well
as the provider-side Crabbox inventory tag. Freestyle and Islo canonical names
remain available for read-only discovery, but delete, pause, resume, SSH reuse,
and delegated reuse require an exact local claim. When local state was
intentionally lost, an operator can use the provider's supported `--reclaim`
reuse path to persist a new claim before any provider mutation. Hyper-V,
Multipass, and Parallels likewise require exact resource-bound local claims
before release or cleanup; provider names and `crabbox-` resource prefixes are
discovery hints, not destructive ownership proof. Railway services are created
out-of-band, so stop requires either an exact local endpoint/project/environment/service/deployment
claim or explicit `stop --reclaim` adoption of the currently inspected
deployment; a successful stop removes that one-deployment claim.
Direct Hetzner release and cleanup similarly require canonical provider labels
plus an unchanged local claim whose lease and cloud ID match the exact server.
Direct GCP release and cleanup require an unchanged local claim bound to the
project, zone, instance name and immutable numeric ID, lease, slug, and provider
key. Direct Azure release and cleanup likewise require an unchanged local claim
bound to the subscription, resource group, VM name and immutable VM ID, lease,
slug, and provider key. Before deleting a VM, Crabbox also persists immutable
NIC, public IP, disk, and quarantine-NSG identities so an interrupted cleanup
can resume without trusting reused names. Cleanup skips weakly labeled,
unclaimed, and stale-claim servers instead of turning provider inventory into
ownership proof.

Artifact publishing rejects symlinks, directories at reserved generated-output
paths, and other non-regular bundle entries before upload side effects.
Publishing copies validated file objects into a private snapshot before broker,
S3, R2, or Cloudflare uploads, so later bundle path replacement cannot change
uploaded bytes. Local and dry-run manifests hash through rooted validated file
handles without duplicating the bundle. Generated manifest and Markdown files
replace reserved outputs through root-confined temporary files without
following symlinks. Required artifact paths must resolve to regular files.
Automatic remote failure bundles confine member names and link targets to their
generated subtree and omit
escaping, rooted, empty, or special-file entries. These filesystem checks do
not redact the contents of accepted regular files.

### Coordinator secrets and config

Inject these as Cloudflare Worker secrets or Node service secrets, never in the
repo:

- `CRABBOX_ADMIN_TOKEN` — admin and image-lifecycle routes.
- `CRABBOX_RUNTIME_ADAPTER_TOKEN` — route-scoped service access to workspace
  lifecycle and desktop-connection APIs only; it cannot attach terminals.
- `CRABBOX_SHARED_TOKEN` — trusted operator automation only.
- `CRABBOX_GITHUB_CLIENT_ID`, `CRABBOX_GITHUB_CLIENT_SECRET`,
  `CRABBOX_SESSION_SECRET` — GitHub browser login, user-token signing, and
  encryption of the OAuth credential used for membership revalidation. The
  session secret is required, must be independent from the shared token, and
  should be rotated separately.
- `CRABBOX_GITHUB_ADMIN_OWNERS`, `CRABBOX_GITHUB_ADMIN_LOGINS` — optional
  comma-separated GitHub verified emails and logins whose user tokens become
  admin at request time; set these per deployment, not in the reusable repo
  config.
- `CRABBOX_TAILSCALE_CLIENT_ID`, `CRABBOX_TAILSCALE_CLIENT_SECRET` — minting
  one-off Tailscale auth keys for brokered `--tailscale` leases.
- `CRABBOX_ARTIFACTS_ACCESS_KEY_ID`, `CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY`,
  and optional `CRABBOX_ARTIFACTS_SESSION_TOKEN` — brokered artifact publishing.
  Scope these to the artifact bucket/prefix and use them only to sign
  short-lived upload/read URLs. New grants encode the exact authenticated
  owner/org identity as reversible base64url values, not hashes or encrypted
  values; this is visible in both signed and public object URLs. Existing object
  URLs are not rewritten or resolved through a legacy lookup path. Reads remain
  signed unless `CRABBOX_ARTIFACTS_PUBLIC_READS=1` explicitly opts into
  non-expiring public links; public grants add an unguessable per-grant
  namespace.

Set the non-secret `CRABBOX_RUNTIME_ADAPTER_OWNER` and
`CRABBOX_RUNTIME_ADAPTER_ORG` to stable deployment identities when the
route-scoped token is enabled. Callers cannot override that identity with
request headers.

Deployments that previously relied on `CRABBOX_SHARED_TOKEN` as the implicit
user-token signing key must configure a new `CRABBOX_SESSION_SECRET`. Existing
`cbxu_` tokens from the old key stop authenticating, so users must run
`crabbox login` again. Direct shared-token automation is unaffected.

Coordinator config values (not secret material):

- `CRABBOX_GITHUB_ALLOWED_ORG(S)`, `CRABBOX_GITHUB_ALLOWED_TEAMS`,
  `CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS`, `CRABBOX_GITHUB_REVOKED_USERS` —
  browser-login and continuing user-token authorization.
- `CRABBOX_TAILSCALE_TAGS` — allowlist/default for requested Tailscale ACL tags.
  Do not allow arbitrary user-supplied tags.
- `CRABBOX_ACCESS_TEAM_DOMAIN`, `CRABBOX_ACCESS_AUD` — Access JWT verification.
- `CRABBOX_ARTIFACTS_BACKEND`, `CRABBOX_ARTIFACTS_BUCKET`,
  `CRABBOX_ARTIFACTS_PREFIX`, `CRABBOX_ARTIFACTS_BASE_URL`,
  `CRABBOX_ARTIFACTS_PUBLIC_READS`,
  `CRABBOX_ARTIFACTS_REGION`, `CRABBOX_ARTIFACTS_ENDPOINT_URL`,
  `CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS`,
  `CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS` — artifact storage settings.

Local-only direct-provider secret: `CRABBOX_TAILSCALE_AUTH_KEY`. Do not forward
it to commands, print it, or store it in repo config.

## Release Integrity

Production releases use separate trust domains and serialized mutation gates;
no tag push or repository event automatically publishes assets or changes the
Homebrew tap. The source identity is an annotated `vMAJOR.MINOR.PATCH` tag whose
signature verifies against the repository-pinned signer policy. Verification
captures the exact tag-object and peeled commit IDs, confirms the remote tag has
not moved, and requires the peeled commit to be an ancestor of protected
`main`. Existing valid tags are preserved when release hardening lands later;
they are never rewritten to point at verifier code.

Release orchestration and verification come from the exact protected-default
workflow commit, not from the tagged candidate. Trusted and candidate trees are
separate, checkout never persists credentials, and candidate builds and
execution do not receive GitHub, Actions runtime/OIDC, Homebrew, signing, or
publication credentials. A narrowly scoped token may download the captured
numeric draft and asset IDs, but it is removed before archive inspection,
signature/notarization verification, or candidate execution.

Every macOS executable archive member, plus the Apple VM helper's eventually
executed embedded VMD, is signed as `Developer ID Application: OpenClaw
Foundation (FWJYW4S8P8)` with the expected identifier and thin native
architecture, hardened runtime, and secure timestamp. The VMD additionally
requires the exact tracked entitlements. Notarization must be accepted before
packaging. Raw command-line binaries cannot carry a stapled ticket, so Apple
Silicon and Intel verification requires the online gate:

```sh
codesign --verify --strict --check-notarization -R=notarized <binary>
```

The private draft has an exact eight-asset inventory, immutable source and
build provenance, and release notes byte-equal to the tagged changelog section.
Both native macOS verifier jobs must bind their proof to the same tag object,
source commit, protected workflow SHA, numeric release ID, asset IDs, sizes, and
digests. Publication requires a separate authorization and changes only the
verified draft state. A fresh public verification must finish after every
release mutation before a separately authorized Homebrew update can begin.
Homebrew proof re-fetches the current public record and exact successful public
verifier run without credentials, authenticates both native proof ZIPs against
GitHub's published artifact digests, requires the run to postdate publication
and every release or asset update, binds formula URLs and checksums to that
record, installs on a clean host, and re-verifies the installed binary and
helper. It repeats the complete public metadata/proof comparison after the
installed-candidate execution and fails if anything changed during the gate.

Cancellation is fail-closed and non-destructive. Operators record the exact
draft, public release, assets, and tap state, but never heuristically delete a
partial release, replace assets, rewrite a tag, redispatch, publish, or change
Homebrew while the gate is stopped. See [Release engineering](RELEASING.md) for
the complete record and gate sequence.

The preserved `v0.37.0` tag is explicitly publication-blocked in its protected
release record because the tagged Apple VM helper ad-hoc re-signs the embedded
VMD before execution. A new signed tag containing the byte-preserving runtime
trust fix is required; the existing tag and source commit must not be moved.

## Managed Windows Artifact Integrity

Managed Windows bootstrap pins the OpenSSH-Win64 archive, Git for Windows
installer, TightVNC installer, and versioned Ubuntu WSL rootfs to SHA-256
digests embedded alongside their URLs. Generated PowerShell verifies every
download before extraction, execution, or WSL import, removes mismatched bytes,
and fails bootstrap closed. URL and digest updates are reviewed together.

The documented Windows developer-image prep applies the same rule to its
versioned Chocolatey package, Node MSI, and Docker Engine archive. The bundled
versions use embedded reviewed digests. Operator-selected Node or Docker
versions require matching SHA-256 environment overrides, and missing, malformed,
or mismatched values fail closed before installation, extraction, or service
registration.

## Managed Linux APT Trust

Managed Linux developer-image bootstrap pins the active NodeSource, Docker, and
Google Linux package-signing primary fingerprints. Local-container Docker
socket bootstrap applies the same Docker pin before adding Docker's CLI
repository. Each path imports downloaded key material into an isolated
temporary GnuPG home, exports only the approved primary key and its signing
subkeys into a repository-scoped keyring, and binds the matching APT source to
that keyring with `signed-by`.

A missing or changed primary key fails closed before replacing the prior
keyring or repository source. Managed image preparation stops rather than
trusting unexpected NodeSource or Docker key material. Browser setup instead
tries the distro Chromium package, and local-container Docker CLI setup instead
tries the distro `docker.io` package.

Signing-subkey rotations beneath an approved primary key continue without a
Crabbox update. A primary-key rotation requires a reviewed fingerprint update
against the official [NodeSource setup](https://github.com/nodesource/distributions/blob/master/scripts/deb/setup_24.x),
[Docker Ubuntu](https://docs.docker.com/engine/install/ubuntu/),
[Docker Debian](https://docs.docker.com/engine/install/debian/), or
[Google Linux repository key](https://www.google.com/linuxrepositories/) source
and fresh bootstrap proof; there is no fallback to an unpinned key.

## SSH

SSH is the control and data path to a leased box; the broker manages leases but
never proxies SSH traffic. The posture:

- Key-only authentication. No password login, no root login.
- A dedicated `crabbox` user; work happens under the platform work root
  (`/work/crabbox` on Linux).
- The CLI generates a per-lease key under the user config directory
  (`<user-config>/crabbox/testboxes/<lease-id>/id_ed25519`; RSA for AWS/Azure
  Windows). Matching cloud key pairs are removed when Crabbox deletes the box.
  See [SSH keys](features/ssh-keys.md).
- CLI-managed SSH, rsync, SCP, VNC, and port-forward connections explicitly
  disable agent and X11 forwarding, overriding broad settings inherited from
  the operator's OpenSSH configuration.
- SSH listens on the configured primary port (default `2222`) plus configured
  fallback ports (default `22`), because port 22 is not reliably reachable from
  every operator network.
- AWS security groups use `CRABBOX_AWS_SSH_CIDRS` when set. Brokered leases
  otherwise scope ingress to the CLI-detected outbound IPv4 CIDR, falling back
  to the Cloudflare request source IP for the lease. Hetzner direct mode relies
  on provider firewall defaults unless a profile tightens them.
- Machines are disposable and cleanable; boot-time cleanup clears stale
  work-root directories.

[Tailscale](features/tailscale.md) does not change this model. Crabbox still
uses OpenSSH, per-lease keys, scoped `known_hosts`, SSH tunnels, lease expiry,
and cleanup — Tailscale only changes which host the SSH client dials.

Hardening worth applying before first shared use:

- Keep long-lived operator keys out of machine images.
- Restrict provider firewalls to known callers where practical.
- Treat profiles that forward secrets as higher risk, and prefer ephemeral
  machines for them.

## Pond Networking

[Ponds](features/pond.md) are a trusted-operator surface. A pond is a lease
grouping plus transport metadata, not an isolation boundary.

- With `--tailscale` on a direct, Tailscale-capable provider, the local CLI may
  add a `tag:cbx-pond-<owner>-<pond>` tag owner and a same-tag allow rule to the
  operator's tailnet policy — but only when both `TS_API_KEY` and
  `CRABBOX_POND_ACL_BOOTSTRAP=1` are set. `TS_API_KEY` alone enables read-only
  `doctor --pond` verification. The broker never receives the Tailscale API key.
- Brokered leases keep using the coordinator's `CRABBOX_TAILSCALE_TAGS` allowlist and
  do not receive generated `tag:cbx-pond-*` tags. Admins who want brokered
  tailnet reachability must configure and review that policy explicitly.

Security notes:

- Same-pond Tailscale members can reach each other by default once the policy
  row exists. Do not share a pond across mutually untrusted tenants.
- URL-bridge peers expose only provider-native HTTP(S) ingress, not arbitrary
  TCP/UDP reachability into the tailnet.
- The SSH-mesh is operator-side `ssh -L` forwarding; it does not create
  lease-to-lease networking.
- Removing a pond does not prune historical Tailscale policy rows. Audit and
  remove stale `tag:cbx-pond-*` entries when rotating preview environments.

Managed VNC stays tunnel-only even on Tailscale-enabled leases. Do not bind
Crabbox-managed VNC to public interfaces or to the Tailscale `100.x` interface.

## Cleanup

Cleanup is operationally important for cost control and stale resource
management. Missing reconciliation or provider coverage is normally
reliability hardening. Deleting a resource that Crabbox cannot strongly
identify as its own crosses a safety boundary. See
[lifecycle and cleanup](features/lifecycle-cleanup.md).

Layered protections:

- A lease TTL cap and an idle timeout enforced against a heartbeat/touch
  deadline.
- Explicit release (`crabbox stop` / `release`).
- A Durable Object alarm or pg-boss job that expires leases and reschedules the
  next pending deadline, plus periodic reconciliation.
- A coordinator-side AWS orphan sweep over current broker credentials and
  capacity regions.
- A provider-label sweep for clearly expired, inactive orphan machines.

In direct-CLI mode, cleanup runs from the CLI using provider labels: it skips
`keep` machines, deletes expired ready/leased/active machines, and only removes
running/provisioning machines after an extra stale-safety window. When a
coordinator is configured, provider-side cleanup is disabled — the coordinator
scheduler owns brokered cleanup.

Brokered cloud orphan sweeps treat coordinator lease state as the authority.
Provider tags discover candidates and explain why they look stale, but do not
authorize a destructive action. Automatic AWS or Azure deletion requires an
exact retained coordinator lease binding for the same provider resource and
region; EC2 Mac host release likewise requires an exact retained host binding.
Before coordinator Azure cleanup deletes a VM, it also persists the managed
disk's immutable ID while the live `managedBy` association still matches that
VM. Later disk deletion revalidates that identity and any current attachment;
an interrupted cleanup may continue after the VM is gone without trusting
self-written tags, while missing or mismatched claims fail closed.
Tag-only and legacy candidates remain report-only. Sweeps skip `keep=true`
resources and apply a grace window before reporting missing labels or stale
lease mappings.

Release is idempotent, and delete tolerates already-deleted provider resources.

### Dedicated private AWS workspaces

The [private AWS workspace service](features/aws-private-workspaces.md) narrows
the workspace boundary beyond the normal SSH lease path:

- the ECS task uses refreshable task-role credentials through the AWS default
  provider chain; the task definition contains no static AWS access keys;
- startup verifies the exact account and Region through task metadata and STS,
  then keeps readiness closed if placement or policy differs;
- the stack owns a separate least-privilege workspace instance role/profile
  and a retained SSM log group;
- the workspace subnet has no public-address assignment or direct
  internet-gateway default route;
- EC2 receives no public IP or key pair, IMDSv2 is mandatory, the workspace
  security group has no ingress, and every egress rule is TCP 443;
- SSM is the only bootstrap/control path. `ready` follows SSM registration and
  successful command completion, not merely EC2 running state;
- the instance and volume carry exact lease and ownership tags, but cleanup
  still requires the durable workspace/lease/resource binding;
- SSM and coordinator logs are evidence, not secret stores. Bearers, AWS
  credentials, database URLs, and signed requests must remain redacted.

Keep the route-scoped workspace bearer separate from the database secret, ECS
execution role, ECS task role, workspace instance role, human AWS identity, and
any broader Crabbox shared/admin token. The client reaches this isolation
boundary by using the dedicated service URL; a client-side label does not
change placement. Live deployment and canary mutation require a separate AWS
GO.

## AWS Account Guardrails

For AWS accounts, apply low-cost default-deny guardrails rather than relying on
lease cleanup alone:

- Enable account-level S3 Block Public Access (all four settings). This applies
  across regions after propagation.
- Set an IAM account password policy when IAM users exist. Prefer SSO for human
  access; do not leave IAM user passwords on the AWS default policy.
- Create IAM Access Analyzer external-access analyzers in every region where
  Crabbox can allocate resources — external analyzers are regional, so one in
  the launch region does not cover the full capacity pool.

For a default brokered AWS capacity pool, run Access Analyzer in `eu-west-1`,
`eu-west-2`, `eu-central-1`, `us-east-1`, and `us-west-2`. Review active findings
before deleting trusts: SSO roles and deliberately scoped artifact-publishing
roles can appear as expected cross-account access.

## Data Retention

The coordinator stores only operational metadata:

- lease ID, owner identity, machine ID, profile;
- timestamps and state transitions;
- the command string, unless disabled.

The coordinator does **not** store unbounded logs, environment values, file
contents, or SSH private keys. For supported leases it stores the authoritative
public SSH host key injected before first boot as operational identity metadata.
Run records keep bounded stdout/stderr captures (chunked, with a stored cap) and
optional structured JUnit summaries for debugging.

For binary or sensitive-by-format output, use `crabbox run --capture-stdout
<path>` or `--capture-stderr <path>` so the stream is written to a local file
and skipped by coordinator log/event capture. Failed SSH-backed and Blacksmith
delegated runs write local failure bundles by default, and `run --download
remote=local` keeps successful binary proof files local. Crabbox does not redact
those local files — review them before sharing.

## Audit Trail

Durable Object run and lease records already provide operational history for
debugging and cleanup (not compliance). A fuller event audit trail would record
lease and machine lifecycle events such as:

```text
lease.created
machine.provisioned
lease.heartbeat
lease.extended
lease.released
lease.expired
machine.drained
machine.deleted
provider.error
```
