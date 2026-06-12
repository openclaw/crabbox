Read this when you:

- pick `provider: morph`;
- configure Morph Cloud API auth, snapshots, or SSH gateway access;
- change `internal/providers/morph`.

[Morph Cloud](https://cloud.morph.so/web/product/devboxes) is a direct-only
Linux SSH-lease provider. Crabbox boots a Morph instance from a configured
snapshot, tags it with Crabbox lease metadata, fetches the per-instance SSH
private key from the Morph API, and connects through the shared SSH gateway
host (`ssh.cloud.morph.so` by default). From there the lifecycle is the normal
Crabbox SSH path: `warmup`, `run`, `ssh`, `status`, `list`, and `stop`.

## When to use

Use Morph when you already keep a reusable Morph snapshot and want normal
Crabbox sync-and-run behavior over SSH without running your own broker or VM
fleet. Choose AWS, Azure, GCP, or Hetzner instead when you need brokered spend
controls, desktop/VNC, browser, or code-server surfaces.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes, through the Morph SSH gateway |
| Crabbox sync (rsync over SSH) | Yes |
| Actions hydration | Yes |
| Desktop / browser / code | No |
| Tailscale | No |
| Coordinator (broker) | No, Morph always runs direct |

`--class`, `--type`, non-Linux `--target`, and `--tailscale` are rejected for
this provider.

Snapshots must provide `bash`, `git`, `rsync`, `tar`, and at least one of
`python3`, `python`, or `perl`; warmup verifies these before accepting the
instance.

## Auth

Keep the Morph API key in the environment or user config, never on the command
line:

```sh
export MORPH_API_KEY=...
```

Crabbox lookup order:

1. `CRABBOX_MORPH_API_KEY`
2. `MORPH_API_KEY`
3. `morph.apiKey` in user config

The API key is used as `Authorization: Bearer <token>` against the Morph REST
API. `crabbox doctor --provider morph` is read-only: it validates auth by
listing instances and fetching the configured snapshot.

## Configuration

```yaml
provider: morph
target: linux
morph:
  apiUrl: https://cloud.morph.so
  snapshot: snapshot_abc123
  sshGatewayHost: ssh.cloud.morph.so
  workRoot: /tmp/crabbox
  deleteOnRelease: false
  wakeOnSSH: true
```

Defaults:

- `apiUrl`: `https://cloud.morph.so`
- `sshGatewayHost`: `ssh.cloud.morph.so`
- `workRoot`: `/tmp/crabbox`
- `deleteOnRelease`: `false`
- `wakeOnSSH`: `true`

`snapshot` is required to create a new instance.

Flags:

```text
--morph-api-url
--morph-snapshot
--morph-ssh-gateway-host
--morph-work-root
--morph-delete-on-release
--morph-wake-on-ssh
```

Environment overrides:

```text
CRABBOX_MORPH_API_KEY      (or MORPH_API_KEY)
CRABBOX_MORPH_API_URL
CRABBOX_MORPH_SNAPSHOT
CRABBOX_MORPH_SSH_GATEWAY_HOST
CRABBOX_MORPH_WORK_ROOT
CRABBOX_MORPH_DELETE_ON_RELEASE
CRABBOX_MORPH_WAKE_ON_SSH
```

## Commands

```sh
crabbox warmup --provider morph --morph-snapshot snapshot_abc123
crabbox run --provider morph --morph-snapshot snapshot_abc123 -- pnpm test
crabbox ssh --provider morph --id blue-lobster
crabbox status --provider morph --id blue-lobster --wait
crabbox list --provider morph
crabbox stop --provider morph blue-lobster
```

## Lifecycle

1. Validate that the configured Morph snapshot exists.
2. Boot a new instance from that snapshot.
3. Write Crabbox metadata (`lease`, `slug`, `work_root`, TTL labels, and
   `provider=morph`) back onto the instance.
4. Set the provider-side TTL and optional `wake_on_ssh` policy.
5. Wait for the instance to become ready, fetch the Morph SSH private key, and
   store it in Crabbox's normal per-lease key path.
6. Connect as SSH user `<instance-id>@<sshGatewayHost>:22` and use the normal
   Crabbox sync/run flow.
7. On release, pause the instance by default. If `morph.deleteOnRelease` is
   true, Crabbox deletes it instead.

## Gotchas

- Morph uses API-returned credentials per instance. If the private key is
  passphrase-protected, Crabbox decrypts it in memory with the returned
  password, stores the usable key under the normal per-lease path with mode
  `0600`, and keeps it while the instance is paused for reuse. Deleting the
  instance removes the key. The password is not passed in command-line
  arguments or retained on disk.
- SSH host keys for the shared Morph gateway persist in
  `~/.config/crabbox/morph/known_hosts` (or the platform user config
  equivalent), so trust-on-first-use survives lease cleanup.
- `stop` pauses by default. Paused instances can be reused later by `run`,
  `ssh`, or `status --wait`; with `wakeOnSSH=true`, SSH can wake them through
  the gateway. The local claim and SSH key remain until the instance is deleted.
- `morph.snapshot` is required for new leases. Existing leases can still be
  resolved by lease id, slug, or instance id.
- The SSH hostname is the shared gateway, not the instance-specific hostname,
  so `status` shows the gateway target for SSH. Morph-reported networking remains
  available in `morph_hostname`, `morph_external_ip`, and `morph_internal_ip`
  labels when present.

## Live testing

Two opt-in entry points exercise the real Morph API. Neither runs in the
default `go test -race ./...` or `node --test scripts/*.test.js` CI jobs;
both are skipped or fail-fast when the API key is absent. Both force
`deleteOnRelease=true` so test instances are deleted instead of retained.

The Morph API key is read from the same contract as runtime auth:

1. `CRABBOX_MORPH_API_KEY`
2. `MORPH_API_KEY`
3. `morph.apiKey` in the user config

### Go integration test

```sh
export CRABBOX_MORPH_API_KEY=...
export CRABBOX_LIVE_MORPH_SNAPSHOT=snapshot_xxx
go test -tags smoke -run TestLiveMorphAcquireResolveTouchReleaseLease \
  -count=1 -v ./internal/providers/morph/
```

The test boots a real instance from the configured snapshot, exercises
`Acquire` → `Resolve` → `Touch` → `List` → `ReleaseLease`, and unconditionally
releases the lease on exit. It skips when `CRABBOX_MORPH_API_KEY` /
`MORPH_API_KEY` is unset or when `go test -short` is set.

### Live provider smoke

```sh
export CRABBOX_MORPH_API_KEY=...
export CRABBOX_LIVE_MORPH_SNAPSHOT=snapshot_xxx
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_LIVE=1 CRABBOX_LIVE_COORDINATOR=0 CRABBOX_LIVE_PROVIDERS=morph \
  scripts/live-smoke.sh
```

Optional knobs: `CRABBOX_LIVE_MORPH_SLUG` (default `morph-smoke-<pid>`),
`CRABBOX_LIVE_MORPH_TTL` (default `15m`), `CRABBOX_LIVE_MORPH_IDLE_TIMEOUT`
(default `5m`).

### Loading the key from a local file

Keep the key in a file outside the repository (for example
`~/Desktop/morph-crabbox/api.env.md`). The repository `.gitignore` blocks
`*.env` and `api.env*` so accidental `git add .` cannot leak it:

```sh
export CRABBOX_MORPH_API_KEY=$(tr -d '[:space:]' < ~/path/to/api.env.md)
```

> The key file is intentionally outside the repo. Do not commit it, do not
> copy it into `crabbox.yaml`, and do not echo it in CI logs. The PR diff
> must never contain the key bytes — verify with
> `git diff origin/main..HEAD | grep -E 'morph_[A-Za-z0-9]{20,}'` (should
> return nothing).

## Related docs

- [Provider backends](../provider-backends.md)
