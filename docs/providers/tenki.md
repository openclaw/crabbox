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

## Auth

Authenticate with the Tenki CLI's browser flow:

```sh
tenki login
```

Crabbox shells out to `tenki`, so it reuses the Tenki CLI's normal config and
auth state. Do not pass Tenki auth tokens as command-line arguments.

## Config

```yaml
provider: tenki
target: linux
tenki:
  cliPath: tenki
  endpoint: https://api.example.test
  gateway: wss://sandbox-gateway.example.test
  workspace: ws_...
  project: proj_...
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
5. Run `tenki sandbox terminate <session-id>` on release.

The provider does not expose Tenki's internal node-agent, mesh IPs, or guest IPs.
All SSH traffic goes through Tenki's supported cert-backed `ssh-proxy` path.

## Capabilities

- SSH: yes, through Tenki `ssh-proxy`.
- Crabbox sync: yes, normal SSH/rsync sync.
- Desktop / browser / code: no.
- Actions hydration: yes, as a normal Linux SSH lease.
- Cleanup: no. Tenki TTL/idle timeout own stale-session cleanup; `stop`
  terminates known Crabbox leases.
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

The repository live-smoke harness also checks that a paused session stays
paused while `status --wait` times out:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_COORDINATOR=0 \
CRABBOX_LIVE_PROVIDERS=tenki \
scripts/live-smoke.sh
```

Expected results:

- `warmup` prints `provider=tenki`, the Crabbox lease ID, slug, and Tenki session
  ID.
- `status --wait` reports the session as ready.
- `run --no-sync` prints `crabbox-tenki-ok`.
