# Tenki Provider

Read when:

- choosing `provider: tenki`;
- running Crabbox on Tenki sandbox VMs;
- changing `internal/providers/tenki`.

Tenki is an SSH-lease provider. Crabbox asks the Tenki CLI to create and delete
sandbox sessions, then runs normal Crabbox sync/commands over SSH through
Tenki's sandbox SSH WebSocket proxy using the Tenki-managed SSH key and
per-session cert.

## When To Use

Use Tenki when the remote Linux machine should be a Tenki sandbox session but
Crabbox should still own repo sync, command execution, `ssh`, and artifact
collection.

Tenki is Linux-only. Desktop, browser, code, and Tailscale features are not
enabled by this provider.

## Commands

```sh
crabbox warmup --provider tenki
crabbox run --provider tenki -- pnpm test
crabbox run --provider tenki --id swift-crab -- pnpm test
crabbox ssh --provider tenki --id swift-crab
crabbox stop --provider tenki swift-crab
crabbox list --provider tenki --json
```

## Fixed Lease IDs For Orchestration

Use `warmup --lease-id` when a controller must retry allocation after an
interrupted dispatch. Choose one canonical ID (`cbx_` plus 12 lowercase hex
characters) per operation, and keep that ID and the same allocation settings
for every retry:

```sh
lease="cbx_$(openssl rand -hex 6)"
crabbox warmup --provider tenki --network public --tailscale=false \
  --lease-id "$lease" --slug worker-job --keep=true \
  --ttl 1h --idle-timeout 10m

# Repeat the same warmup command after an interrupted dispatch.
crabbox inspect --provider tenki --network public --id "$lease" --json
crabbox run --provider tenki --network public --tailscale=false \
  --id "$lease" --keep=true --no-sync --script-stdin <<'SH'
printf 'worker-ready\n'
SH
crabbox heartbeat --provider tenki --id "$lease" --idle-timeout 10m --json
crabbox stop --provider tenki --id "$lease"
```

Crabbox saves a durable create attempt before it calls Tenki. A retry verifies
that attempt against the exact session and reuses the session; it does not
allocate another sandbox when a create reply or readiness check is lost.
Changed allocation settings, conflicting ownership, and multiple matching
sessions fail closed. An empty inventory after an attempted create is not
proof that creation failed: retain the claim and retry inspection or stop when
the provider can confirm the session.

Keep the controller's Crabbox state directory on persistent storage. Do not
delete claims to force a retry, move the operation to another state directory,
or switch the authenticated Tenki workspace during an operation. A successful
stop keeps a terminal receipt for a fixed ID. Repeated stop is safe, but that
ID cannot allocate another session; use a new ID for the next operation.

For Linux worker clients, omit `class` and use Tenki-specific sizing in Crabbox
config. Disable native warm-image/checkpoint reuse (for clients with a
`warmImage` setting, use `false`). Desktop, browser, and native checkpoint
features are not part of this integration. `inspect --json` checks SSH
readiness without resuming a paused sandbox.

`--keep=true` selects a sticky Tenki session. Tenki disables automatic idle
pause and discards `--max-duration` for sticky sessions, so the controller must
call `stop` when it is done. Heartbeat refreshes the local Crabbox lease record;
it does not add a provider-side expiry to a sticky session.

## Auth

Authenticate with the Tenki CLI's browser flow:

```sh
tenki login
```

Crabbox shells out to `tenki`, so it reuses the Tenki CLI's normal config and
auth state. The current Tenki CLI selects the workspace from the authenticated
API key; it does not accept separate sandbox `--workspace` or `--project`
selectors. Run `tenki login` again when you need a different workspace. Do not
pass Tenki auth tokens as command-line arguments.

## Config

```yaml
provider: tenki
target: linux
tenki:
  cliPath: tenki
  endpoint: https://api.example.test
  gateway: wss://sandbox-gateway.example.test
  image: ubuntu:tenki
  workRoot: /home/tenki/crabbox
  cpus: 4
  memoryMB: 8192
  diskGB: 40
```

Provider flags:

```text
--tenki-cli
--tenki-endpoint
--tenki-gateway
--tenki-workspace
--tenki-project
--tenki-image
--tenki-snapshot
--tenki-work-root
--tenki-cpus
--tenki-memory-mb
--tenki-disk-gb
```

Environment overrides:

```text
CRABBOX_TENKI_CLI / TENKI_CLI
CRABBOX_TENKI_ENDPOINT / TENKI_ENDPOINT
CRABBOX_TENKI_GATEWAY / TENKI_GATEWAY
CRABBOX_TENKI_WORKSPACE
CRABBOX_TENKI_PROJECT
CRABBOX_TENKI_IMAGE
CRABBOX_TENKI_SNAPSHOT
CRABBOX_TENKI_WORK_ROOT
CRABBOX_TENKI_CPUS
CRABBOX_TENKI_MEMORY_MB
CRABBOX_TENKI_DISK_GB
```

`tenki.image` and `tenki.snapshot` are mutually exclusive.

Workspace and project settings are retained only to match claims created by
older Crabbox versions. Current Tenki authentication selects the workspace from
the API key, and Crabbox rejects these settings for new leases. Keep them only
while stopping an older scoped lease, then remove them and run `tenki login` for
the intended workspace.

## Sizing

Set sandbox size per run with Tenki-specific flags:

```sh
crabbox run \
  --provider tenki \
  --tenki-cpus 4 \
  --tenki-memory-mb 8192 \
  --tenki-disk-gb 40 \
  -- pnpm test
```

For reusable leases, pass the same flags to `warmup`:

```sh
crabbox warmup \
  --provider tenki \
  --slug big-tenki-box \
  --tenki-cpus 8 \
  --tenki-memory-mb 16384 \
  --tenki-disk-gb 80
```

These map to Tenki create flags as `--cpu`, `--memory-mb`, and
`--disk-size-gb`.

## Lifecycle

1. Run `tenki sandbox create` with Crabbox metadata and tags.
2. Run `tenki sandbox ssh-command --output json --session <session-id>` to let
   the Tenki CLI resolve `~/.config/tenki/ssh/id_ed25519` and mint the session
   cert under `~/.config/tenki/ssh-certs/<session-id>/`.
3. Return an SSH target using `ProxyCommand tenki sandbox ssh-proxy --session
   <session-id>` plus OpenSSH `CertificateFile=<cert-path>`.
4. Let core Crabbox perform rsync, command execution, `ssh`, and artifacts.
5. On release, verify the exact local claim, session ID, and fresh
   provider-side lease metadata, then run `tenki sandbox terminate <session-id>`
   under the claim lock. Only after the same session reports `TERMINATING` or
   `TERMINATED` does Crabbox remove an ordinary claim or replace a fixed-ID
   claim with a terminal receipt. A mismatched or missing session ID,
   lookup error, or cancellation preserves the claim for a safe retry; generic
   "not found" diagnostics are not proof of session deletion.

Session IDs and inventory metadata only discover sandboxes; they never
authorize termination. Explicit `--reclaim` can adopt a session through a normal
reuse command before stop. `stop --force` is unsupported because arbitrary
Tenki sessions cannot independently prove lost-claim ownership.

The provider does not expose Tenki's internal node-agent, mesh IPs, or guest IPs.
All SSH traffic goes through Tenki's supported cert-backed `ssh-proxy` path.
Sandbox restores can present a different ephemeral SSH host key on consecutive
proxy connections, so Crabbox mirrors the Tenki CLI's host-key policy instead
of maintaining a `known_hosts` entry. Server authentication depends on the
trusted TLS gateway; the SSH client certificate does not authenticate the server.
Use a trusted `wss://` gateway. Do not bypass the proxy or reuse this policy for
a direct network SSH target.

The cert-backed gateway selects the sandbox from the signed SSH certificate.
Changing the proxy's session URL alone does not grant access to another sandbox:
a certificate for session A still selects A. Crabbox obtains the certificate and
proxy command together for the requested session.

An issued SSH certificate is an access credential until it expires. Expired
certificates are rejected on new connections, but Crabbox does not guarantee
that revoking an API key immediately invalidates a cached SSH certificate or
closes an existing SSH connection. Do not treat an API-key authentication error
as proof that earlier SSH access has ended.

## Capabilities

- SSH: yes, through Tenki `ssh-proxy`.
- Crabbox sync: yes, normal SSH/rsync sync.
- Fixed lease IDs: yes, with durable local attempt recovery and single-use IDs.
- Desktop / browser / code: no.
- Native checkpoints / warm images: no.
- Actions hydration: yes, as a normal Linux SSH lease.
- Cleanup: no. Tenki duration/idle settings apply to non-sticky sessions;
  explicitly `stop` reusable sticky leases when they are no longer needed.
- Coordinator (broker): no — always direct from the CLI.

## Live Smoke

```sh
tenki login
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox warmup --provider tenki --timing-json
lease=<slug-or-cbx_id-from-warmup-output>

bin/crabbox status --provider tenki --id "$lease" --wait
bin/crabbox run --provider tenki --id "$lease" --no-sync -- echo crabbox-tenki-ok
bin/crabbox stop --provider tenki "$lease"
bin/crabbox list --provider tenki --json
```

The repository live-smoke harness also checks full inventory, claim cleanup,
and that a paused session stays paused while `status --wait` times out:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=tenki \
scripts/live-smoke.sh
```

The smoke exits before any Crabbox `doctor`, `warmup`, `status`, `run`, `list`,
or `stop` call when `tenki status --json` does not report a logged-in CLI. With
an authenticated CLI, it creates one sandbox session, waits for status, runs one
no-sync command, pauses the Tenki session directly, verifies a Crabbox
`status --wait` timeout does not resume it, then stops the lease.

Expected results:

- `warmup` prints `provider=tenki`, the Crabbox lease ID, slug, and Tenki session
  ID.
- `status --wait` reports the session as ready.
- `run --no-sync` prints `crabbox-tenki-ok`.
