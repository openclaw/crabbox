# Architecture

## System Overview

Crabbox has three main parts:

- CLI: local Go binary used by maintainers and agents.
- Coordinator: Cloudflare Worker plus Durable Object state.
- Workers: managed cloud or SSH-accessible machines that run commands.

The coordinator leases machines. The CLI executes work. Machines do not need to call back to the coordinator in the MVP.

```text
developer laptop
  crabbox CLI
    |
    | HTTPS JSON API, Crabbox auth
    v
Cloudflare Worker
  Durable Object lease state
    |
    | Hetzner API or AWS EC2 API
    v
cloud machines

developer laptop
  |
  | SSH + rsync
  v
leased machine
```

## Lease Flow

1. CLI loads config and authenticates with a signed GitHub login token or shared operator token.
2. CLI creates a per-lease SSH key.
3. CLI sends `POST /v1/leases` with lease ID, slug, profile, TTL, idle timeout, desired machine class, and SSH public key.
4. Coordinator validates identity and policy.
5. Durable Object chooses a provider from config and creates a Hetzner server or AWS EC2 instance.
6. Coordinator returns lease ID, slug, machine address, SSH user, workdir, and expiry.
7. CLI waits for `crabbox-ready`.
8. CLI seeds remote Git when possible, compares sync fingerprints, and syncs changed files with `rsync --delete`.
9. CLI runs sync sanity and configured base-ref hydration.
10. CLI runs the command over SSH and streams stdout/stderr.
11. CLI heartbeats while the command runs; heartbeats touch `lastTouchedAt`, recompute idle expiry up to the TTL cap, and attach a best-effort latest Linux telemetry snapshot when SSH is reachable.
12. CLI releases the lease when done.
13. Durable Object alarm cleans up stale leases and expired machines.

## Coordinator API

Implemented endpoints:

```text
GET  /v1/health
GET  /v1/pool
GET  /v1/whoami
POST /v1/leases
GET  /v1/leases
GET  /v1/leases/{id-or-slug}
POST /v1/leases/{id-or-slug}/heartbeat
POST /v1/leases/{id-or-slug}/release
GET  /v1/runs
POST /v1/runs
GET  /v1/runs/{run-id}
GET  /v1/runs/{run-id}/logs
POST /v1/runs/{run-id}/finish
GET  /v1/usage
GET  /v1/admin/leases
POST /v1/admin/leases/{id-or-slug}/release
POST /v1/admin/leases/{id-or-slug}/delete
```

Admin endpoints and `GET /v1/pool` require the separate admin token. GitHub browser-login tokens are user tokens for normal lease operations and are minted only after allowed GitHub org membership is verified. User-token list, exact-ID lookup, slug lookup, heartbeat, release, run history, logs, and usage are scoped to the token owner/org.

Heartbeat bodies may include a `telemetry` object. The coordinator stores the latest sanitized snapshot on the lease record and retains a bounded `telemetryHistory` ring of the latest 60 samples for portal trend charts. Current CLI snapshots include Linux load average, memory use, root-disk use, uptime, source, and capture timestamp. Runs also accept `POST /v1/runs/{run-id}/telemetry` samples while they are active, and completed run records keep bounded start/mid/end Linux telemetry so history can show resource deltas and short trends without keeping an unbounded time series.

## Durable Object State

Use one fleet Durable Object for MVP. It owns all atomic scheduling decisions.

Core stored records:

```sql
leases(id, slug, provider, cloud_id, region, owner, org, profile, class, server_type, server_id, server_name, provider_key, host, ssh_user, ssh_port, work_root, keep, ttl_seconds, idle_timeout_seconds, estimated_hourly_usd, max_estimated_usd, state, telemetry_json, telemetry_history_json, created_at, updated_at, last_touched_at, expires_at, released_at, ended_at)
runs(id, lease_id, slug, owner, org, provider, class, server_type, command_json, state, exit_code, sync_ms, command_ms, duration_ms, log_bytes, log_truncated, results_json, telemetry_json, started_at, ended_at)
runlog(run_id, bounded_stdout_stderr_capture)
```

State transitions:

```text
machine: provisioning -> idle -> leased -> idle
machine: provisioning -> failed
machine: leased -> draining -> idle|deleted
lease: pending -> active -> released
lease: pending|active -> expired
lease: active -> failed
```

## Backends

Owned backends:

- `hetzner-static`: pre-created warm machines.
- `hetzner-ephemeral`: created per lease or overflow.
- `aws`: one-time EC2 instances for burst capacity, managed Windows/WSL2, and EC2 Mac.
- `azure`: one-time Azure VMs for Linux and native Windows SSH/sync/run.
- `ssh-static`: manually managed machines reachable by SSH.

Brokered backends, later:

- `github-actions`: register or dispatch real Actions-backed runner work when workflow parity is required.
- `external-runner`: adapter boundary for other hosted runner systems if needed.

The current broker implements `hetzner-ephemeral`, `aws`, and `azure`, and leaves interfaces ready for `hetzner-static`.

## Machine Bootstrap

Bootstrap should produce machines with:

- `crabbox` user.
- SSH key-only auth.
- Git.
- rsync.
- curl.
- jq.
- writable `/work/crabbox`.

Language runtimes, Docker, services, dependencies, and secrets are project setup, not Crabbox base bootstrap. Use GitHub Actions hydration, devcontainers, Nix, mise/asdf, or repository scripts for that layer.

Prefer snapshots/images once bootstrap is proven. Cloud-init is acceptable for first pass.

## Config Sources

Config precedence:

```text
flags > env > repo-local crabbox.yaml/.crabbox.yaml > user config > defaults
```

User config is YAML and can define:

- coordinator URL.
- coordinator bearer token.
- profiles.
- machine classes.
- backend defaults.
- sync excludes.
- env allowlists.
- capacity market/strategy/fallback.
- Actions workflow/job/ref hints.
- trusted projects.
- sync behavior such as checksum mode, Git seeding, and fingerprint skipping.

It must not store:

- live leases.
- SSH private keys.
- provider secrets.

Per-lease SSH private keys live under the user config directory, outside repo config. Provider secrets live in the broker environment, such as Cloudflare Worker secrets for AWS and Hetzner.

## Failure Model

Assume:

- CLI can crash.
- SSH can disconnect.
- Machines can fail boot.
- Hetzner API calls can race or partially complete.
- Cloudflare Worker can retry requests.

Therefore:

- Lease creation must be idempotent where practical.
- TTL cleanup must be authoritative.
- Provider resources need labels for orphan cleanup.
- Release should be safe to call multiple times.
- Machine delete should tolerate already-deleted resources.
