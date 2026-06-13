# Cloudflare provider

Select with `provider: cloudflare` (alias `cf`) to run Linux commands inside
Cloudflare Containers behind a Cloudflare Worker. This is a **delegated-run**
provider: the local CLI builds a repo archive, owns the local lease claim,
renders the command, and streams timing output, while the Worker runner creates
the container, receives the upload, executes the command, and tears the
container down. There is no SSH lease.

Cloudflare Containers run behind container-enabled Durable Objects, which makes
this provider a good fit for short Linux test jobs and warm repeated commands.
It is not suitable for SSH-oriented or interactive desktop workflows.

For Worker-runtime JavaScript or TypeScript module execution, use the separate
[Cloudflare Dynamic Workers provider](cloudflare-dynamic-workers.md)
(`provider: cloudflare-dynamic-workers`, aliases `cf-dynamic` and `cfdw`).
Dynamic Workers do not provide Linux shell execution, archive sync, SSH, VNC, or
ports; they run module source through the Cloudflare Workers runtime.

## Capabilities at a glance

- **Targets:** Linux only.
- **Supported commands:** `run`, `warmup`, `status`, `stop`, `list`, `doctor`,
  and local-claim `cleanup`.
- **Sync:** archive upload/extract (gzipped tar), not rsync.
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
container image is built from `worker/cloudflare-container.Dockerfile` and runs
the Go HTTP runner in `worker/cloudflare-container-runner`.

## Configuration

Repo config should select the runner URL and remote workdir only. Keep the
bearer token out of repo YAML.

```yaml
provider: cloudflare
cloudflare:
  apiUrl: https://crabbox-cloudflare-container-runner.example.workers.dev
  workdir: /workspace/crabbox
```

Config keys map to the typed `cloudflare` section: `apiUrl`, `token`, and
`workdir`. The corresponding environment variables and flags are:

| Setting    | Config key | Environment variable             | Flag                  |
| ---------- | ---------- | -------------------------------- | --------------------- |
| Runner URL | `apiUrl`   | `CRABBOX_CLOUDFLARE_RUNNER_URL`  | `--cloudflare-url`    |
| Workdir    | `workdir`  | `CRABBOX_CLOUDFLARE_WORKDIR`     | `--cloudflare-workdir`|
| Token      | `token`    | `CRABBOX_CLOUDFLARE_RUNNER_TOKEN`| _(none, by design)_   |

Keep the bearer token in a shell secret, credential manager, or user-level
config:

```sh
export CRABBOX_CLOUDFLARE_RUNNER_URL=https://runner.example.workers.dev
export CRABBOX_CLOUDFLARE_RUNNER_TOKEN=...
```

The token is intentionally **not** exposed as a command-line flag, because
command-line arguments can be captured in shell history and process listings.

The workdir defaults to `/workspace/crabbox` and must resolve to an absolute
path. Broad system paths (`/`, `/workspace`, `/usr`, `/var`, and similar) are
rejected; pick a dedicated subdirectory.

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

The `deploy:cloudflare` script passes `--containers-rollout=immediate` so Worker
and container changes roll out together. If you call Wrangler directly, include
that flag:

```sh
npx wrangler deploy \
  --config worker/wrangler.cloudflare.jsonc \
  --containers-rollout=immediate
```

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

`worker/wrangler.cloudflare.jsonc` defines one Durable Object class per
predefined Cloudflare instance type. Crabbox maps every generic class to
`standard-4`, because the smaller Cloudflare tiers are far smaller than the
default Linux classes on other providers.

```text
--class standard  standard-4
--class fast      standard-4
--class large     standard-4
--class beast     standard-4
```

Pick a smaller container explicitly with
`--type lite|basic|standard-1|standard-2|standard-3|standard-4` for smoke tests
or quota control. `--type` accepts only these six values; anything else fails.

- `lite` suits no-sync and quick command smoke tests.
- `basic` or a `standard-*` type is the right choice for archive sync.
- Prefer `standard-*` for dependency-heavy builds or tests; large module
  downloads can exhaust the smaller container disks before the command starts.

Cloudflare's current predefined types range from `lite` to `standard-4`;
`standard-4` is 4 vCPU, 12 GiB memory, and 20 GB disk. Each class is capped at
`max_instances: 4` in `worker/wrangler.cloudflare.jsonc`; change that value when
the account should allow more or fewer concurrent containers. For current
instance and account limits, see the Cloudflare Containers limits docs:
<https://developers.cloudflare.com/containers/platform-details/limits/>

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
  --no-sync \
  --shell \
  -- 'uname -a; command -v go node pnpm gh'

crabbox stop --provider cloudflare <lease-id-or-slug>
```

Then run a sync smoke from a checkout:

```sh
crabbox run \
  --provider cloudflare \
  --type basic \
  --timing-json \
  --shell \
  -- 'test -f go.mod && rg -n "stopped_with_code" internal/providers/cloudflare'
```

## Behavior

- `run` creates or reuses a container Durable Object, prepares `workdir`,
  uploads a gzipped archive of the local checkout (unless `--no-sync`), extracts
  it, then relays stdout, stderr, and exit status.
- Before upload, the provider checks remote disk headroom for both the archive
  and the extracted checkout, and fails early with a sizing hint if the selected
  type is too small.
- `warmup` starts a container and leaves it alive until `crabbox stop` or the
  configured TTL/idle deadline expires.
- `status` and `stop` resolve local Crabbox claims, then call the runner.
- `list` reports local Cloudflare claims. Add `--refresh` to check runner state
  for those claims. The runner intentionally does not expose a global container
  enumeration API.
- The default image includes Git, GitHub CLI (`gh`), `jq`, `ripgrep`, `curl`,
  Go, Node, and `pnpm`; repo-specific dependencies still belong to the repo
  setup command.
- npm and pnpm caches live under `/var/cache/crabbox`
  (`NPM_CONFIG_CACHE=/var/cache/crabbox/npm`, pnpm store
  `/var/cache/crabbox/pnpm`), and the container filesystem persists while the
  lease is active.
- The runner stores lease metadata in Durable Object storage and schedules
  cleanup at the earlier of `--ttl` or `--idle-timeout`. Uploads and command
  execution extend the idle deadline.
- `crabbox cleanup --provider cloudflare` only checks local claims. It removes
  claims whose runner state is expired, stopped, or missing.

Cloudflare Containers can also reach Worker bindings through outbound handlers.
Crabbox does not wire those by default, but custom runner images can add them:
<https://developers.cloudflare.com/containers/platform-details/workers-connections/>

## Limitations

- Only Linux delegated `run`, `warmup`, `status`, `stop`, `list`, `doctor`, and
  local-claim cleanup are supported.
- SSH, VNC, browser desktop, code-server, Actions hydration, `--download`, and
  `--fresh-pr` are not supported.
- `--checksum` is not supported, because sync uses archive upload/extract rather
  than rsync.
- The provider does not advertise a pond transport; `pond peers` reports
  Cloudflare members as `transport=none` rather than fabricating an endpoint.
- Cleanup cannot discover containers that have no local Crabbox claim.
- Container capacity is bounded by the checked-in Wrangler bindings
  (`max_instances`) and the target account's Cloudflare Containers limits.
