# Security

## Trust Model

MVP is for trusted OpenClaw maintainers, not arbitrary untrusted users.

Assumptions:

- Users can run arbitrary commands on leased machines.
- Machines may see forwarded local env values.
- Users are trusted not to attack other users intentionally.
- Bugs and crashes still happen, so cleanup must be defensive.

## Authentication

Cloudflare Access can protect custom coordinator routes. The Worker also enforces auth for every non-health route.

The Access-protected coordinator route is a defense-in-depth layer, not a
replacement for Crabbox auth. `crabbox-access.openclaw.ai` first requires
Cloudflare Access service-token credentials at the edge. After that, the same
Worker still requires a Crabbox signed user token or shared operator bearer
token before lease, run, log, usage, or admin routes are allowed.

MVP:

- One-time PIN Access remains available for early fallback.
- GitHub Access IdP is configured for the `openclaw` org.
- `crabbox-access.openclaw.ai` is service-token-only and the policy is scoped to the local Crabbox CLI service token.
- `crabbox login` opens GitHub, receives a signed user token from the coordinator, and stores it in local config.
- Workers.dev automation can still use a shared bearer token via `crabbox login --token-stdin`.
- Admin routes require a separate admin bearer token configured as `CRABBOX_ADMIN_TOKEN` in the Worker and `broker.adminToken` or `CRABBOX_COORDINATOR_ADMIN_TOKEN` locally.
- The CLI sends owner/org headers only for shared-token automation; GitHub login tokens carry owner/org inside the signed token.
- `CRABBOX_GITHUB_ALLOWED_TEAMS` can restrict browser-login tokens to selected GitHub team slugs after allowed-org membership passes.
- GitHub browser-login tokens are user tokens, not admin tokens. They can only see and mutate leases, runs, logs, and usage for their own owner/org identity.
- The Worker forwards signed GitHub token owner/org identity to the Fleet Durable Object and strips caller-supplied Access identity headers from that forwarded request.
- Raw Cloudflare Access identity headers are not trusted. If the Worker uses an Access identity, it first verifies `Cf-Access-Jwt-Assertion` against the configured Access team certs and application audience.
- Missing shared-token config fails closed for non-health coordinator routes.

Target:

- Keep GitHub org membership as the normal access path.
- Optional team allowlist for narrower browser-login access.

## Authorization

Roles:

```text
user: acquire, heartbeat, release own leases, list own leases/runs/logs/usage
maintainer: shared warm pool access
admin: drain machines, cleanup, view all leases/runs/pool/usage, deploy
```

Admin identity uses a separate admin token. Shared operator tokens are for normal automation. Browser-login users can optionally be limited by GitHub team slug in Worker config.

## Secrets

No central project secret store in MVP.

Rules:

- Secrets stay local.
- CLI forwards env only by allowlist.
- Users can opt in additional env names with repo-local `env.allow` config.
- Never accept secret values as command-line flag values.
- Never log env values.
- Redact known secret-looking strings in diagnostics.
- `CRABBOX_SHARED_TOKEN` is stored as a Worker secret for trusted operator automation; local automation can use `CRABBOX_COORDINATOR_TOKEN`. Shared-token requests do not trust caller-supplied `X-Crabbox-Owner` or `X-Crabbox-Org`; configure a fixed `CRABBOX_SHARED_OWNER` for that automation identity, or use verified Cloudflare Access / signed browser tokens for per-user identity.
- `CRABBOX_ADMIN_TOKEN` is stored as a Worker secret for admin and image lifecycle routes; local admin commands use `CRABBOX_COORDINATOR_ADMIN_TOKEN` or `broker.adminToken`.
- `CRABBOX_GITHUB_CLIENT_ID`, `CRABBOX_GITHUB_CLIENT_SECRET`, and `CRABBOX_SESSION_SECRET` are Worker secrets for browser login.
- `CRABBOX_TAILSCALE_CLIENT_ID` and `CRABBOX_TAILSCALE_CLIENT_SECRET` are
  Worker secrets for minting one-off Tailscale auth keys when brokered
  `--tailscale` leases are requested.
- `CRABBOX_ARTIFACTS_ACCESS_KEY_ID`, `CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY`,
  and optional `CRABBOX_ARTIFACTS_SESSION_TOKEN` are Worker secrets for
  brokered artifact publishing. They should be scoped to the artifact
  bucket/prefix and used only to sign short-lived upload/read URLs.
- `CRABBOX_ARTIFACTS_BACKEND`, `CRABBOX_ARTIFACTS_BUCKET`,
  `CRABBOX_ARTIFACTS_PREFIX`, `CRABBOX_ARTIFACTS_BASE_URL`,
  `CRABBOX_ARTIFACTS_REGION`, `CRABBOX_ARTIFACTS_ENDPOINT_URL`,
  `CRABBOX_ARTIFACTS_UPLOAD_EXPIRES_SECONDS`, and
  `CRABBOX_ARTIFACTS_URL_EXPIRES_SECONDS` are Worker config values, not secret
  material.
- `CRABBOX_GITHUB_ALLOWED_ORG(S)` and `CRABBOX_GITHUB_ALLOWED_TEAMS` are Worker config values for browser-login authorization.
- `CRABBOX_TAILSCALE_TAGS` is the coordinator allowlist/default for requested
  Tailscale ACL tags. Do not allow arbitrary user-supplied tags.
- `CRABBOX_ACCESS_TEAM_DOMAIN` and `CRABBOX_ACCESS_AUD` let the Worker verify Cloudflare Access JWTs before using Access-provided identity.
- `CRABBOX_ACCESS_CLIENT_ID` and `CRABBOX_ACCESS_CLIENT_SECRET` are local Cloudflare Access service-token credentials. Store them only in user config or env, never repo config. They only satisfy Cloudflare Access; they do not authorize Crabbox actions by themselves.
- `CRABBOX_TAILSCALE_AUTH_KEY` is local direct-provider-only. Do not forward it
  to commands, print it, or store it in repo config.
- User config files are written `0600`; `crabbox doctor` reports overly broad local config permissions because broker tokens may be stored there.

Project allowlist example:

```json
{
  "env": {
    "allow": ["CI", "NODE_OPTIONS", "PROJECT_*"]
  }
}
```

## SSH

MVP SSH posture:

- SSH allowed only for worker machines.
- AWS security groups use `CRABBOX_AWS_SSH_CIDRS` when configured. Brokered leases otherwise use the CLI-detected outbound IPv4 CIDR or, as a fallback, the Cloudflare request source IP for the lease request.
- Hetzner direct mode still relies on provider networking/firewall defaults unless a profile supplies tighter controls.
- Key-only authentication.
- Dedicated `crabbox` user.
- No password login.
- No root login.
- SSH listens on the configured primary port, default `2222`, plus configured fallback ports, default `22`, because port 22 is not reliable from every operator network path.
- The CLI generates per-lease SSH keys under the user config directory for new leases.
- Matching cloud SSH keys/key pairs are removed when Crabbox deletes the machine.
- Work happens under `/work/crabbox`.
- Machines are disposable or cleanable.

Tailscale does not replace this SSH model in v1. Crabbox still uses OpenSSH,
per-lease keys, scoped known_hosts, SSH tunnels, lease expiry, and cleanup.
Tailscale only changes which host the SSH client dials.

Managed VNC remains tunnel-only even on Tailscale-enabled leases. Do not bind
Crabbox-managed VNC to public interfaces or to the Tailscale 100.x interface.

MVP hardening before first shared use:

- Keep long-lived maintainer keys out of machine images.
- Restrict Hetzner firewalls to known callers when practical.
- Redact command diagnostics before printing.
- Treat profiles that forward secrets as higher risk; prefer ephemeral machines for those profiles.

Later hardening:

- Cloudflare Tunnel or Access SSH.
- SSH CA with short-lived certs.
- Per-lease Unix users.
- Per-lease workdir ownership and cleanup.

## Cleanup

Cleanup is security-sensitive.

Required:

- Lease TTL cap.
- Idle timeout and heartbeat/touch deadline.
- Explicit release.
- Durable Object alarm cleanup.
- Provider label sweep for clearly expired, inactive orphan machines.
- Boot-time cleanup of stale `/work/crabbox/*` dirs.

Direct-CLI cleanup uses provider labels. It skips kept machines, deletes expired ready/leased/active machines, and only removes running/provisioning machines after the extra stale safety window. When a coordinator is configured, provider-side cleanup is disabled because the Durable Object alarm owns brokered cleanup.

Release must be idempotent. Delete must tolerate already-deleted provider resources.

## Data Retention

Store only operational metadata:

- lease ID.
- owner identity.
- machine ID.
- profile.
- timestamps.
- state transitions.
- command string, unless disabled.

Do not store:

- unbounded stdout/stderr logs in the coordinator;
- env values;
- file contents;
- SSH keys.

Coordinator run records keep bounded stdout/stderr captures and optional
structured JUnit summaries for debugging. For binary or sensitive-by-format
stdout, use `crabbox run --capture-stdout <path>` so stdout is written to a
local file and skipped by coordinator log/event capture. Use `run --download
remote=local` for binary proof files that should stay local.

## Future Audit Trail

Durable Object run and lease records already provide operational history. A fuller event audit trail should record:

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

The audit trail is for debugging and cleanup, not compliance.
