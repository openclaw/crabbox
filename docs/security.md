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

MVP:

- One-time PIN Access remains available for early fallback.
- GitHub Access IdP is configured for the `openclaw` org.
- `crabbox login` opens GitHub, receives a signed user token from the coordinator, and stores it in local config.
- Workers.dev automation can still use a shared bearer token via `crabbox login --token-stdin`.
- The CLI sends owner/org headers only for shared-token automation; GitHub login tokens carry owner/org inside the signed token.
- GitHub browser-login tokens are user tokens, not admin tokens. They can only see and mutate leases, runs, logs, and usage for their own owner/org identity.
- Missing shared-token config fails closed for non-health coordinator routes.

Target:

- Keep GitHub org membership as the normal access path.
- Optional team allowlist for admin commands.

## Authorization

Roles:

```text
user: acquire, heartbeat, release own leases, list own leases/runs/logs/usage
maintainer: shared warm pool access
admin: drain machines, cleanup, view all leases/runs/pool/usage, deploy
```

Until GitHub teams are wired, admin identity can be an explicit allowlist in Worker config.

## Secrets

No central project secret store in MVP.

Rules:

- Secrets stay local.
- CLI forwards env only by allowlist.
- Users can opt in additional env names with repo-local `env.allow` config.
- Never accept secret values as command-line flag values.
- Never log env values.
- Redact known secret-looking strings in diagnostics.
- `CRABBOX_SHARED_TOKEN` is stored as a Worker secret for trusted operator automation; local automation can use `CRABBOX_COORDINATOR_TOKEN`.
- `CRABBOX_GITHUB_CLIENT_ID`, `CRABBOX_GITHUB_CLIENT_SECRET`, and `CRABBOX_SESSION_SECRET` are Worker secrets for browser login.

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
- SSH listens on port 2222 in the verified direct-CLI path because port 22 was not reachable during Hetzner testing.
- The CLI generates per-lease SSH keys under the user config directory for new leases.
- Matching cloud SSH keys/key pairs are removed when Crabbox deletes the machine.
- Work happens under `/work/crabbox`.
- Machines are disposable or cleanable.

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

Coordinator run records keep bounded stdout/stderr tails and optional structured JUnit summaries for debugging.

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
