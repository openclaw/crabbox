# Daytona Provider

Read this when you are:

- choosing `provider: daytona`;
- configuring Daytona API auth, snapshots, or SSH access;
- changing `internal/providers/daytona`.

Daytona is an SSH-lease provider with two data planes. Direct `run` and `warmup`
create the sandbox from a Daytona snapshot and drive sync and command execution
through the Daytona SDK/toolbox APIs. With a coordinator configured, the Worker
creates the sandbox and mints an expiring SSH access token; the CLI then uses
normal SSH and rsync without receiving the Daytona API key.

## When to use

Use Daytona when the box image should come from a Daytona snapshot and command
execution should stay inside Daytona's toolbox APIs. Reach for AWS, Hetzner, or
the static `ssh` provider instead when you need a normal long-lived SSH lease for
Actions hydration, desktop/VNC, or `code` workflows.

Use brokered Daytona when clients should share centrally managed Daytona
capacity without receiving the API key. Brokered Daytona supports normal
SSH/sync/run, but not workspaces, ready pools, Actions hydration,
desktop/browser/code, or Tailscale.

## Commands

```sh
crabbox warmup --provider daytona --daytona-snapshot crabbox-ready
crabbox run --provider daytona --daytona-snapshot crabbox-ready -- pnpm test
crabbox run --provider daytona --id swift-crab -- pnpm test:changed
crabbox ssh --provider daytona --id swift-crab
crabbox stop --provider daytona swift-crab
```

## Native snapshots and forks

Direct Daytona leases support filesystem snapshots through `checkpoint`:

```sh
crabbox checkpoint create --provider daytona --id swift-crab \
  --mode native --name after-install --no-reboot=false
crabbox checkpoint inspect chk_<id> --verify
crabbox checkpoint fork chk_<id> --slug snapshot-fork
crabbox run --provider daytona --id snapshot-fork -- pnpm test
crabbox checkpoint delete chk_<id>
```

Capture flushes and stops a running source, waits for the snapshot to become
active, then restarts the source. A stopped source stays stopped. The default
`--no-reboot=true` refuses to stop a running source; explicitly allow the
interruption with `--no-reboot=false`. The snapshot barrier applies even with
`--wait=false`, bounded by `--wait-timeout`. Memory and running processes are
not captured. Forks start independent sandboxes and relocate the saved workspace
into the new lease's workdir.

The local `daytona-snapshot` record binds the exact snapshot ID, organization,
API endpoint, and source sandbox. Provider names include the checkpoint ID to
avoid collisions. Verify, fork, and delete reject identity or scope mismatches;
deletion waits for provider-confirmed absence. A missing snapshot on the initial
lookup does not authorize removal of the local record: use `--local-only` only
after checking the account and endpoint. Failed or uncertain captures retain a
recovery record when a snapshot may exist. If capture completion cannot be
confirmed, Crabbox leaves the source stopped and reports the source and snapshot
to inspect before restarting it.

Native capture currently requires direct Daytona credentials; it is not exposed
through the coordinator. Use `--mode native` (or an explicit strategy); default
auto mode retains the existing workspace-archive behavior. Snapshots may contain
credentials and other filesystem state and incur storage charges until deleted.

## Live Smoke

The shared live-smoke harness can validate Daytona without a coordinator:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=daytona CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

The smoke requires a snapshot through `CRABBOX_DAYTONA_SNAPSHOT`,
`DAYTONA_SNAPSHOT`, or `daytona.snapshot`. It exits before any Daytona `run`,
`list`, `warmup`, or `stop` command when the snapshot is missing, so
credentialless machines can verify the guard without mutating provider state.
With a snapshot configured, the harness runs one delegated Daytona command and
then lists normalized Daytona inventory.

For a coordinator deployment, store `DAYTONA_CRABBOX_KEY` as a Worker secret.
The optional `CRABBOX_DAYTONA_SNAPSHOT` Worker variable selects a shared
snapshot; when it is empty, the Daytona account default is used.

Clients need only normal Crabbox broker authentication. They do not need a
Daytona CLI profile or Daytona API environment variables. Verify the fallback
without creating a sandbox:

```sh
crabbox doctor --provider daytona
```

The broker readiness check performs a read-only sandbox inventory request and
reports `daytona-fallback` with `client_auth=crabbox`,
`control_plane=coordinator`, `data_plane=ssh-rsync`, and `mutation=false`.

## Auth

Crabbox reads the active Daytona CLI profile when no Daytona auth values are set
in the environment or config:

```sh
daytona login --api-key ...
```

You can also supply explicit API-key auth:

```sh
export DAYTONA_API_KEY=...
```

or JWT auth:

```sh
export DAYTONA_JWT_TOKEN=...
export DAYTONA_ORGANIZATION_ID=...
```

`DAYTONA_ORGANIZATION_ID` is required with JWT auth. Explicit environment values
(or Crabbox config values) override the Daytona CLI profile.

Each auth variable also has a `CRABBOX_`-prefixed form that takes precedence over
the unprefixed one: `CRABBOX_DAYTONA_API_KEY`, `CRABBOX_DAYTONA_JWT_TOKEN`,
`CRABBOX_DAYTONA_ORGANIZATION_ID`, and `CRABBOX_DAYTONA_API_URL`.

## Config

```yaml
provider: daytona
target: linux
daytona:
  apiUrl: https://app.daytona.io/api
  snapshot: crabbox-ready
  target: ""
  user: daytona
  workRoot: /home/daytona/crabbox
  sshGatewayHost: ssh.app.daytona.io
  sshAccessMinutes: 30
```

The values above are the built-in defaults except for `snapshot` and `target`,
which are empty by default.

Provider flags:

```text
--daytona-api-url
--daytona-snapshot
--daytona-target
--daytona-user
--daytona-work-root
--daytona-ssh-gateway-host
--daytona-ssh-access-minutes
```

The non-auth settings can also be set through environment variables:
`CRABBOX_DAYTONA_SNAPSHOT`, `CRABBOX_DAYTONA_TARGET`, `CRABBOX_DAYTONA_USER`,
`CRABBOX_DAYTONA_WORK_ROOT`, `CRABBOX_DAYTONA_SSH_GATEWAY_HOST`, and
`CRABBOX_DAYTONA_SSH_ACCESS_MINUTES`.

## Managed lifecycle

Brokered allocation includes native wall-clock TTL even for kept or explicitly
retained sandboxes. The coordinator records returned UUIDs before readiness and
confirms exact-resource deletion in the original allocation context. A lost
create response remains visibly unresolved; name/label matches and elapsed TTL
are not deletion proof. Legacy records without scope and changed credentials
require operator resolution. See [managed Daytona cleanup](../features/lifecycle-cleanup.md#managed-daytona-cleanup)
for lifetime, key-rotation, and recovery behavior.

## Direct lifecycle

Direct control-plane HTTP requests have a 60-second default whole-request
timeout, including response-body reads. Earlier caller cancellation or deadlines
still apply. Toolbox execution and archive uploads keep their caller-controlled
lifetimes rather than inheriting this control-plane budget.

1. Create or resolve a Daytona sandbox from `daytona.snapshot`.
2. Create private previews, configure Daytona's native wall-clock TTL and idle
   auto-stop interval, and store Crabbox labels and an exact local repo claim.
   The adapter records allocation before waiting for readiness, so a failed
   startup is rolled back. A lost create response is reconciled by the unique
   sandbox name and verified ownership, without allocating again. Failed
   cleanup retains a recovery claim and reports the exact sandbox and lease IDs.
3. For `run`, build the Crabbox sync manifest, stream a gzipped tar archive to
   the Daytona toolbox upload endpoint, extract it in the sandbox, and execute
   the command through the Daytona process APIs. Remote process timeouts are
   derived from the caller's context deadline, rounded up to whole seconds, and
   capped at Daytona's maximum supported value; callers without a deadline use
   that maximum. Toolbox HTTP requests are canceled by their request context
   without an independent client-wide timeout.
   Sync prunes only deleted manifest-owned source paths; dependencies, caches,
   and other remote-only files survive ordinary resyncs. The next manifest is
   published only after successful extraction. Active sync and execution
   refresh Daytona activity at least every 30 seconds so quiet commands do not
   trigger idle auto-stop.
4. For `ssh`, request short-lived SSH access (TTL `daytona.sshAccessMinutes`),
   parse Daytona's `sshCommand`, and redact the token in normal output.
5. Delete the sandbox on release unless the lease is kept.

`--ttl` is a hard upper bound even while commands run or a lease is kept.
Daytona lifetime settings use whole minutes, so positive durations are rounded
up. Idle auto-stop preserves the sandbox filesystem; native TTL ultimately
deletes the sandbox. `heartbeat --idle-timeout` changes the provider's auto-stop
policy as well as Crabbox metadata. Status readiness comes from Daytona's live
state, never a previously stored `ready` label. Explicit stop and rollback wait
for confirmed provider deletion, with a bounded cleanup deadline.
If `stop` cannot resolve the claimed sandbox, it returns the lookup error and
preserves the local recovery claim. A missing sandbox in the current account or
API endpoint does not prove deletion in the original scope, even after native TTL.

API, toolbox, and archive-upload clients refuse redirects that change scheme,
host, or effective port. Custom endpoints require HTTPS except for loopback
development endpoints.

## Capabilities

- Provider kind: SSH-lease (Linux only).
- SSH: yes, via a short-lived Daytona SSH access token.
- Sync: direct mode uses Daytona toolbox archive sync; brokered mode uses normal
  Crabbox rsync over SSH.
- Desktop / browser / code: no — Daytona has no Crabbox VNC or `code` surface.
- Actions hydration: no.
- Coordinator (broker): yes for Linux SSH/sync/run. The coordinator owns the
  API key and rotates the lease's SSH token.
- Broker readiness: read-only Daytona inventory plus explicit coordinator and
  SSH/rsync data-plane diagnostics; no sandbox is created.
- Native checkpoint / fork / snapshot: yes, for direct Linux leases; filesystem
  capture only. In-place native restore and memory capture are not supported.

## Gotchas

- Direct mode requires `daytona.snapshot` (or `--daytona-snapshot`). Brokered
  mode uses the coordinator's optional `CRABBOX_DAYTONA_SNAPSHOT`.
- Brokered release and rollback are idempotent when the owned sandbox was
  already deleted or expired.
- `--class` and `--type` are rejected; size the sandbox through the snapshot.
- `--id <sandbox-id-or-slug>` is required to address an existing sandbox.
- Daytona `run` is delegated to the toolbox APIs; it is not core-over-SSH
  execution. Because of that, the following `run` options are rejected:
  `--checksum`, `--full-resync`,
  `--fresh-pr`, `--script` / `--script-stdin`, `--env-helper`,
  `--capture-stdout` / `--capture-stderr`, `--capture-on-fail`, `--download`,
  `--artifact-glob`, `--require-artifact`, `--emit-proof`, and `--stop-after`.
- Use `--sync-only` to pre-upload the archive into a kept sandbox before a later
  command. Large-sync guardrails still apply; `--force-sync-large` is honored
  for intentional large archive syncs.
- `--actions-runner` is rejected because it needs a normal SSH lease host.
- `--keep-on-failure` keeps a newly created failed sandbox until Daytona
  auto-stop or an explicit `crabbox stop`.

## Related docs

- [Feature: Daytona](../features/daytona.md)
- [Provider backends](../provider-backends.md)
