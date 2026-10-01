# Cloudflare provider

Select with `provider: cloudflare` (alias `cf`) to run Linux commands inside
Cloudflare Containers behind a Cloudflare Worker. This is a **delegated-run**
provider: the local CLI builds a repo archive, owns the local lease claim,
renders the command, and streams timing output, while the Worker runner creates
the container, receives the upload, executes the command, and tears the
container down. There is no SSH lease.

The runner uses the Containers `durable_object` scheduling policy: one
`CrabboxSandbox` Durable Object per lease starts its container through
`ctx.container`, picks the image and instance type per lease, and runs commands
through native `exec()`. Cached-image cold starts take about 1–2 seconds, which
makes this provider a good fit for short Linux test jobs and warm repeated
commands.
It is not suitable for SSH-oriented or interactive desktop workflows.

For Worker-runtime JavaScript or TypeScript module execution, use the separate
[Cloudflare Dynamic Workers provider](cloudflare-dynamic-workers.md)
(`provider: cloudflare-dynamic-workers`, aliases `cf-dynamic` and `cfdw`).
Dynamic Workers do not provide Linux shell execution, archive sync, SSH, VNC, or
ports; they run module source through the Cloudflare Workers runtime.

## Capabilities at a glance

- **Targets:** Linux only.
- **Supported commands:** `run`, `warmup`, `status`, `stop`, `list`, `doctor`,
  and claim-scoped `cleanup`.
- **Run sessions:** `run --keep --lease-output <path>` writes a reusable lease
  handle with an exact cleanup command.
- **Sync:** archive upload/extract (gzipped tar), not rsync.
- **Checkpoints:** native container filesystem snapshots through
  `crabbox checkpoint create` and `checkpoint fork`; see
  [Container snapshots](#container-snapshots).
- **Coordinator:** never brokered — this provider always runs direct from the
  CLI against its own Worker runner, independent of any `CRABBOX_COORDINATOR`
  broker.
- **Not supported:** SSH, VNC, browser desktop, code-server, Actions hydration,
  `--download`, `--fresh-pr`, `--artifact-glob`, `--require-artifact`, and
  `--checksum` (sync is archive-based, so there is no per-file checksum step).
  The provider also does not advertise a [pond](../features/pond.md) transport,
  so `pond peers` reports Cloudflare members as `transport=none`.

## Requirements

- A Cloudflare Workers Paid account with Durable Objects and Containers enabled.
- Wrangler authenticated for the target account.
- Docker (or a Docker-compatible daemon) available to Wrangler for image builds.
- The deployed Crabbox runner from `worker/wrangler.cloudflare.jsonc`.
- The Worker secret `CRABBOX_RUNNER_TOKEN`.
- CLI-side `CRABBOX_CLOUDFLARE_RUNNER_URL` and `CRABBOX_CLOUDFLARE_RUNNER_TOKEN`.

The Worker entrypoint is `worker/src/cloudflare-container-runner.ts`. The
container image is built from `worker/cloudflare-container.Dockerfile`; its
entrypoint only keeps the container alive (`tini -- sleep infinity`), and the
Durable Object runs uploads and commands through `ctx.container.exec()`.

Commands run as `timeout --kill-after=5s <ttl> /bin/bash -l <script>`, so a
timeout signals the whole process group and exits 124; group members that
ignore SIGTERM are killed 5 seconds later. `exec()` does not inherit the image
`ENV`; login-shell defaults such as `NPM_CONFIG_CACHE` live in
`/etc/profile.d/crabbox.sh` and apply only when the variable is not forwarded. A background process that keeps stdout or stderr
open (`sleep 30 & echo done`) does not hold the command open: after the command
exits, output keeps streaming until 300 ms pass without new bytes, for at most
5 seconds of reading. Output is read only as fast as the CLI reads the
response, with at most 256 KiB queued in the runner; time spent waiting for a
slow CLI does not count toward those limits.

## Runner toolchain and pnpm upgrades

The bundled image pins Node 24.21.0, Go 1.26.8, GitHub CLI 2.101.0, and
pnpm 12.5.1. These versions take effect when the runner image is deployed;
updating the local Crabbox CLI does not update an existing deployment.

The pnpm default advances from 10.24.0 to 12.5.1. Projects without a
`packageManager` field in `package.json` inherit the new default. Corepack
continues to honor explicit project pins: set `"packageManager": "pnpm@10.24.0"`
before deployment if a project still requires pnpm 10.

Before adopting pnpm 12, follow the [pnpm migration guide](https://pnpm.io/migration)
and [v12 release notes](https://github.com/pnpm/pnpm/releases/tag/v12.0.0).
Configuration under `package.json#pnpm` and non-registry/auth settings in
`.npmrc` move to `pnpm-workspace.yaml`; build-approval settings consolidate
into `allowBuilds`, and `npm_config_*` environment variables become
`pnpm_config_*`. Test dependency installation, frozen-lockfile reinstallation,
and the project's own build/test commands with the selected pnpm version before
deploying. Crabbox does not migrate project configuration automatically.

## Configuration

Repo config should select the runner URL and remote workdir only. Keep the
bearer token out of repo YAML.

```yaml
provider: cloudflare
cloudflare:
  apiUrl: https://crabbox-cloudflare-container-runner.example.workers.dev
  workdir: /workspace/crabbox
```

Config keys map to the typed `cloudflare` section: `apiUrl`, `token`, `image`,
and `workdir`. The corresponding environment variables and flags are:

| Setting    | Config key | Environment variable             | Flag                  |
| ---------- | ---------- | -------------------------------- | --------------------- |
| Runner URL | `apiUrl`   | `CRABBOX_CLOUDFLARE_RUNNER_URL`  | `--cloudflare-url`    |
| Image      | `image`    | `CRABBOX_CLOUDFLARE_IMAGE`       | `--cloudflare-image`  |
| Workdir    | `workdir`  | `CRABBOX_CLOUDFLARE_WORKDIR`     | `--cloudflare-workdir`|
| Token      | `token`    | `CRABBOX_CLOUDFLARE_RUNNER_TOKEN`| _(none, by design)_   |

`image` names an entry in the runner's Wrangler `containers[].images` map and
defaults to `default`. Add more named images there (for example a Python
toolchain) and select one per lease; a name the deployed runner does not know
fails creation with the list of configured names.

Keep the bearer token in a shell secret, credential manager, or user-level
config:

```sh
export CRABBOX_CLOUDFLARE_RUNNER_URL=https://runner.example.workers.dev
export CRABBOX_CLOUDFLARE_RUNNER_TOKEN=...
```

The token is intentionally **not** exposed as a command-line flag, because
command-line arguments can be captured in shell history and process listings.

All four bindings share one typed declaration. The loader retains its existing
user/repository YAML handling, including nonempty `token` input; this does not
change the advice to keep tokens out of repository files. Omitted, null, and empty
YAML strings preserve earlier values, while whitespace is accepted for later
validation. Environment overrides use raw nonempty values. Accepted URL/token
inputs keep their existing source classification; an explicit URL flag visit is
still recorded separately after successful provider flag application.

Runner redirects are followed only when they keep the configured scheme, host,
and effective port. Cross-origin redirects fail before command, environment, or
upload bodies can be replayed to another destination.
Runner error bodies and streamed error events redact the configured token and
bearer-shaped credentials before they reach CLI diagnostics.

The workdir defaults to `/workspace/crabbox` and must resolve to an absolute
path. Broad system paths (`/`, `/workspace`, `/usr`, `/var`, and similar) are
rejected; pick a dedicated subdirectory. A lease keeps the workdir it was
created with: `run --id` uses the workdir recorded in the lease's claim, not
the current configuration.

The CLI's configured workdir and runtime fallback share that default. The bundled
Worker keeps a separate HTTP-protocol fallback for requests that omit workdir;
the Go client sends its resolved workdir explicitly. Instance-type mapping,
URL-first client validation, authentication, and Worker lifecycle remain outside
the generated bindings.

Check the configured runner URL and token without creating a container:

```sh
crabbox doctor --provider cloudflare
```

## Deploy

Install dependencies and verify the Worker before deploy:

```sh
npm ci --prefix worker
npm run check --prefix worker
npm run build:cloudflare --prefix worker
```

Set the runner bearer token as a Worker secret:

```sh
printf '%s' "$CRABBOX_CLOUDFLARE_RUNNER_TOKEN" \
  | npx wrangler secret put CRABBOX_RUNNER_TOKEN \
      --config worker/wrangler.cloudflare.jsonc
```

Deploy the Worker and container image together:

```sh
npm run deploy:cloudflare --prefix worker
```

Each Worker version carries its image map. Running containers keep the image
they started with; new leases start on the newly deployed image. Right after a
deploy, creation can briefly fail with `image default is not configured` until
the new version's images are available; retry after a few seconds.

### Upgrading an existing runner

Upgrading from a runner deployed before the `durable_object` policy is a
one-time cutover. The `cloudflare-container-v3` migration deletes the six
per-instance-type classes, their Durable Object state, and their running
containers; Cloudflare does not move them to the new class.

1. Stop kept leases on the old runner: `crabbox list --provider cloudflare`,
   then `crabbox stop --provider cloudflare <lease>` for each.
2. Deploy the new runner with `npm run deploy:cloudflare --prefix worker`.
3. Run `crabbox cleanup --provider cloudflare`. Claims for leases you did not
   stop now return 404, and cleanup retires them; their workspaces are gone.
4. Delete the six legacy container applications. The migration removes their
   Durable Object classes but leaves the applications, which keep reporting
   active instances:

   ```sh
   npx wrangler containers list --config worker/wrangler.cloudflare.jsonc
   npx wrangler containers delete <application-id> \
     --config worker/wrangler.cloudflare.jsonc
   ```

   Delete each `<worker-name>-sandbox*` application and keep
   `<worker-name>-crabboxsandbox`.

For a repeatable local gate, deploy, and live smoke in one step, use:

```sh
scripts/deploy-cloudflare-smoke.sh
```

It expects `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN`,
`CRABBOX_CLOUDFLARE_RUNNER_TOKEN`, and `CRABBOX_CLOUDFLARE_RUNNER_URL` in the
environment. Set `CRABBOX_CLOUDFLARE_SKIP_DEPLOY=1` to run only the local checks
and live smoke, or `CRABBOX_CLOUDFLARE_SKIP_SMOKE=1` to stop after deploy.

Inspect the deployed container app:

```sh
npx wrangler containers list --config worker/wrangler.cloudflare.jsonc
npx wrangler containers info <container-application-id> \
  --config worker/wrangler.cloudflare.jsonc
```

## Instance types and capacity

`worker/wrangler.cloudflare.jsonc` defines one `CrabboxSandbox` class; each
lease passes its instance type to `ctx.container.start()`. Crabbox maps every
generic class to
`standard-4`, because the smaller Cloudflare tiers are far smaller than the
default Linux classes on other providers.

```text
--class tiny      standard-4
--class small     standard-4
--class standard  standard-4
--class fast      standard-4
--class large     standard-4
--class beast     standard-4
```

Pick a smaller container explicitly with
`--type standard-1|standard-2|standard-3|standard-4` for smoke tests or quota
control; anything else fails.

- `standard-1` (1/2 vCPU, 4 GiB, 8 GB disk) is the smallest type that runs the
  bundled image.
- `--type basic` still works but prints a deprecation warning and uses
  `standard-1`: the `durable_object` policy has no `basic` type (1/4 vCPU,
  1 GiB, 4 GB disk), and `standard-1` costs more per second.
- `--type lite` fails up front. On 2026-09-30, `lite` (1/16 vCPU, 256 MiB, 2 GB
  disk) failed to start the bundled image with a platform internal error, while
  Cloudflare's `cloudflare/debian-trixie` image started on it.
- Prefer `standard-*` types with more disk for dependency-heavy builds or tests;
  large module downloads can exhaust the smaller disks before the command starts.

`standard-4` is 4 vCPU, 12 GiB memory, and 20 GB disk. The `durable_object`
policy has no `max_instances` cap: running containers count against the
account's Containers limits. For current instance and account limits, see
<https://developers.cloudflare.com/containers/platform/limits/>

## Live smoke

With the runner URL and token configured, first exercise the deployed runner
without uploading the checkout:

```sh
crabbox run \
  --provider cloudflare \
  --no-sync \
  --timing-json \
  --shell \
  -- 'df -h / /tmp /workspace; printf "npm cache=%s\n" "${NPM_CONFIG_CACHE:-}"; printf "pnpm store="; pnpm config get store-dir'
```

That one-shot run cleans up automatically. Use `--keep` when you want to inspect
or reuse the same container, then stop it explicitly:

```sh
crabbox run \
  --provider cloudflare \
  --keep \
  --lease-output /tmp/cloudflare-session.json \
  --no-sync \
  --shell \
  -- 'uname -a; command -v go node pnpm gh'

cat /tmp/cloudflare-session.json
crabbox stop --provider cloudflare <lease-id-or-slug>
```

Then run a sync smoke from a checkout:

```sh
crabbox run \
  --provider cloudflare \
  --type standard-1 \
  --timing-json \
  --shell \
  -- 'test -f go.mod && rg -n "stopped_with_code" internal/providers/cloudflare'
```

## Container snapshots

`crabbox checkpoint create --id <lease>` captures the running container's whole
filesystem as a Cloudflare container snapshot (kind
`cloudflare-container-snapshot`) without stopping it, and
`crabbox checkpoint fork <checkpoint>` starts new leases from it:

```sh
crabbox warmup --provider cloudflare --type standard-1 --slug blue-crab
crabbox run --provider cloudflare --id blue-crab --shell -- 'pnpm install'
crabbox checkpoint create --provider cloudflare --id blue-crab --name deps
crabbox checkpoint fork chk_0123456789abcdef --count 4 --type standard-2 -- pnpm test
```

- Capture took about 8 seconds for a small change set; forks start in about
  1–5 seconds once the snapshot has propagated. For several seconds after
  capture, restoring on another Durable Object can fail with a platform
  internal error; the runner retries snapshot starts up to six times, 3
  seconds apart, within the 300-second readiness window.
- A fork keeps the checkpoint's workdir and may pick another instance type with
  `--type`. A fork command runs like `crabbox run` in that workdir and syncs the
  checkout, which replaces the workdir; state outside it, such as the npm and pnpm caches
  under `/var/cache/crabbox`, carries over. Like any lease, a fork whose
  container stops ends instead of restarting from the snapshot.
- Snapshots belong to the runner that captured them. Forking with another
  runner URL fails, and snapshots do not survive a new runner image: rebuild
  checkpoints after deploying an image change.
- Cloudflare has no snapshot lookup or delete API. `checkpoint inspect
  --verify` reports `unverified_ref`, snapshots expire 30 days after creation
  or their last restore, and `crabbox checkpoint delete --local-only` removes
  the local record. Snapshot storage pricing is not published yet.
- A snapshot holds everything on the container's filesystem, including tokens
  or credentials written there, until it expires. Keep secrets out of the
  filesystem before capture, or rotate them afterwards.
- `--strategy image` and `--mode image` are rejected; a container snapshot is a
  filesystem snapshot.
- Checkpoints need a lease this repository already claims; `--reclaim`,
  `--lease-id`, `--workdir`, and `--keep=false` are not supported, and there is
  no archive checkpoint mode.

## Behavior

- `run` creates or reuses a container Durable Object, uploads a gzipped archive
  of the local checkout (unless `--no-sync`), extracts it, then relays stdout,
  stderr, and exit status. Fresh runs prepare and size-check the full archive
  before creating a container; a small dirty delta does not bypass full-archive limits.
- With `sync.delete: true`, extraction uses a sibling staging directory and
  replaces `workdir` only after extraction succeeds. Failed admission, upload,
  or extraction preserves the old checkout, and exact temporary paths receive
  best-effort cleanup. With deletion disabled, extraction merges into `workdir`.
- Before upload, the provider checks remote disk headroom for both the archive
  and the extracted checkout, and fails early with a sizing hint if the selected
  type is too small. This check does not remove the old checkout to free space.
- `warmup` starts a container and leaves it alive until `crabbox stop` or the
  configured TTL/idle deadline expires. The runner sets the container
  inactivity timeout to the platform maximum of 6 hours, restores it as soon as
  a restarted Durable Object starts, and renews it from a Durable Object alarm
  at least hourly; the same alarm enforces the lease deadline.
- The first lease after deploying a new image waits for Cloudflare to pull it,
  which took about 2.5 minutes for the bundled image. Creation, upload, and exec
  wait up to 300 seconds for readiness, and the CLI waits up to 330 seconds for
  response headers. A container that fails to start is reported immediately.
- Canceling a command (Ctrl-C, or a dropped connection) sends SIGTERM to the
  command's process group, followed by SIGKILL after 5 seconds, even when the
  cancel arrives before the command has started or the shell exits on SIGTERM.
- A failed stdout or stderr transport ends the command with an error instead of
  a completion with missing output.
- Reuse, `status`, and `stop` resolve local Crabbox claims before calling the
  runner and reject raw sandbox IDs without a matching claim.
- Reuse and cleanup keep the captured local claim revision: another caller
  replacing or removing that claim prevents stale admission or teardown. This
  fences local claim writers, not remote operators recreating the same Durable
  Object identity; the runner has no conditional generation-delete API.
- Automatic cleanup finishes before the final result and timing record. A
  cleanup failure fails an otherwise successful run and retains its recovery
  claim; an existing command failure or cancellation stays the primary outcome.
  `--keep-on-failure` also retains fresh sessions after preparation or sync fails.
- `list` reports local Cloudflare claims. Add `--refresh` to check runner state
  for those claims. The runner intentionally does not expose a global container
  enumeration API.
- The default image includes Git, checksum-verified GitHub CLI (`gh`), `jq`,
  `ripgrep`, `curl`, Go, Node, and `pnpm`; repo-specific dependencies still
  belong to the repo setup command.
- npm and pnpm caches live under `/var/cache/crabbox`
  (`NPM_CONFIG_CACHE=/var/cache/crabbox/npm`, pnpm store
  `/var/cache/crabbox/pnpm`), and the container filesystem persists while the
  lease is active.
- The runner stores lease metadata in Durable Object storage and sets a Durable
  Object alarm at the earlier of `--ttl` or `--idle-timeout`. Uploads and
  command execution extend the idle deadline.
- If a lease's container stops before its deadline, the lease ends: `status`
  reports `stopped` with `stopReason`, and uploads and commands fail with HTTP
  410 instead of silently continuing in an empty workspace. `crabbox cleanup
  --provider cloudflare` retires the claim.
- `status` reports expired or stopped metadata without retiring the local claim:
  the runner may have stored that state before native destruction failed.
  `crabbox cleanup --provider cloudflare` checks local claims and confirms or
  retries native deletion for terminal containers before removing their claims.
  An HTTP 404 can retire the exact unchanged claim; other errors preserve it.
  `--dry-run` sends no DELETE requests and does not remove local claims; the
  runner's expiry policy still applies during status reads.

Containers run with Internet access. Crabbox does not install
`ctx.container.interceptOutboundHttp(s)` handlers by default.

## Limitations

- Only Linux delegated `run`, `warmup`, `status`, `stop`, `list`, `doctor`, and
  claim-scoped cleanup are supported.
- SSH, VNC, browser desktop, code-server, Actions hydration, `--download`, and
  `--fresh-pr` are not supported.
- `--checksum` is not supported, because sync uses archive upload/extract rather
  than rsync.
- The provider does not advertise a pond transport; `pond peers` reports
  Cloudflare members as `transport=none` rather than fabricating an endpoint.
- Cleanup cannot discover containers that have no local Crabbox claim.
- Container capacity is bounded by the target account's Cloudflare Containers
  limits.
- The `durable_object` scheduling policy is in public beta.

If a command stream ends before its completion event while the caller context is
canceled, Crabbox reports cancellation rather than a missing-completion error.
An accepted completion event and explicit stream-read errors retain their
existing precedence.
