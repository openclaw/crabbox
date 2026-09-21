# Tenki Provider

Read when:

- choosing `provider: tenki`;
- running Crabbox on Tenki sandbox VMs;
- changing `internal/providers/tenki`.

Tenki is an SSH-lease provider. Crabbox asks the Tenki CLI to create and delete
sandbox sessions, then runs normal Crabbox sync/commands over SSH through
Tenki's sandbox SSH WebSocket proxy using the Tenki-managed SSH key and
per-session cert.

Crabbox verifies gateway host certificates against Tenki's certificate authority.
If the Tenki CLI reports a `known_hosts_file`, that file is authoritative. When
the CLI omits it, Crabbox uses the same workspace key and API endpoint as the CLI
to retrieve the CA and discover the session's gateway identity. It writes a
separate authority file alongside the native session certificate, without
changing Tenki's private keys or certificates. This path requires a workspace
API key from `tenki onboard` or `TENKI_API_KEY`.

Gateway certificates signed by the trusted CA can rotate without enrolling
individual leaf keys. Failed discovery, unknown authorities, or missing trust
files fail closed; Crabbox does not enroll leaf keys or consult other host-trust
sources. Each new SSH connection rereads the authority file instead of reusing
a multiplexed connection. Existing connections are not retroactively revoked.

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

## Auth

Authenticate with the Tenki CLI's browser flow:

```sh
tenki login
```

Crabbox reuses the Tenki CLI's normal config and auth state. For host-authority
discovery, it reads `~/.config/tenki/config.yaml` (or the basename selected by
`TENKI_CONFIG_FILE`), with the CLI's `TENKI_AUTH_TOKEN`, `TENKI_API_KEY`, and
`TENKI_API_ENDPOINT` overrides. The API endpoint must use HTTPS; Crabbox never
follows authority requests to another origin. `TENKI_API_URL` and
`XDG_CONFIG_HOME` do not change the native CLI's credential context.

The current Tenki CLI selects the workspace from the authenticated
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
   under the claim lock. Crabbox removes the claim only after the same session
   reports `TERMINATING` or `TERMINATED`. A mismatched or missing session ID,
   lookup error, or cancellation preserves the claim for a safe retry; generic
   "not found" diagnostics are not proof of session deletion.

Session IDs and inventory metadata only discover sandboxes; they never
authorize termination. Explicit `--reclaim` can adopt a session through a normal
reuse command before stop. `stop --force` is unsupported because arbitrary
Tenki sessions cannot independently prove lost-claim ownership.

The provider does not expose Tenki's internal node-agent, mesh IPs, or guest IPs.
All SSH traffic goes through Tenki's supported cert-backed `ssh-proxy` path.

## Capabilities

- SSH: yes, through Tenki `ssh-proxy`.
- Crabbox sync: yes, normal SSH/rsync sync.
- Desktop / browser / code: no.
- Actions hydration: yes, as a normal Linux SSH lease.
- Cleanup: no. Explicit `stop` terminates known Crabbox leases; paused sessions
  still require cleanup according to Tenki's retention policy.
- Coordinator (broker): no — always direct from the CLI.

## Lifetime

Current Tenki sandboxes do not auto-pause or stop on inactivity. Crabbox retains
idle-timeout and timing metadata for claim compatibility, but these values are
bookkeeping and do not enforce native idle expiry. A touch does not extend
Tenki's maximum duration.

Kept leases use Tenki's `--sticky` mode and have no maximum duration. In
particular, `warmup` defaults to `--keep=true`, so its TTL does not bound a sticky
sandbox. Use `--keep=false --ttl 15m` for a bounded test. For non-kept leases,
Crabbox passes TTL as `--max-duration`; reaching that limit pauses the sandbox,
not destroys it. Always finish with `crabbox stop --provider tenki <lease>`.

## Live Smoke

```sh
tenki login
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox warmup --provider tenki --keep=false --ttl 15m --timing-json
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
