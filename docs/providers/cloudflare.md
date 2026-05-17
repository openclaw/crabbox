# Cloudflare Provider

Use `provider: cloudflare` or `provider: cf` to run Linux commands through a
Cloudflare Worker backed by Cloudflare Containers. This is a delegated run
provider: the local CLI builds the repo archive, owns local lease claims, renders
commands, and streams timing output; the Worker runner creates the container,
uploads files, executes commands, and tears the container down.

Cloudflare Containers run behind container-enabled Durable Objects. That makes
the provider a good fit for short Linux test jobs and warm repeated commands,
but not for SSH-oriented workflows.

## Requirements

- Cloudflare Workers Paid account with Durable Objects and Containers enabled.
- Wrangler authenticated for the target account.
- Docker or a Docker-compatible daemon available to Wrangler for image builds.
- Deployed Crabbox runner from `worker/wrangler.cloudflare.jsonc`.
- Worker secret `CRABBOX_RUNNER_TOKEN`.
- CLI-side `CRABBOX_CLOUDFLARE_RUNNER_URL` and
  `CRABBOX_CLOUDFLARE_RUNNER_TOKEN`.

The Worker entrypoint is `worker/src/cloudflare-container-runner.ts`. The
container image is built from `worker/cloudflare-container.Dockerfile` and runs
the Go HTTP runner in `worker/cloudflare-container-runner`.

## Configuration

Repo config should select the runner and remote workdir only:

```yaml
provider: cloudflare
cloudflare:
  apiUrl: https://crabbox-cloudflare-container-runner.example.workers.dev
  workdir: /workspace/crabbox
```

Keep the bearer token in a shell secret, credential manager, or user-level
config. Do not commit it to repo YAML.

```sh
export CRABBOX_CLOUDFLARE_RUNNER_URL=https://runner.example.workers.dev
export CRABBOX_CLOUDFLARE_RUNNER_TOKEN=...
```

`--cloudflare-url` can override the runner URL for one command. The runner token
is intentionally not exposed as a command-line flag because command-line
arguments can be captured in shell history and process listings.

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

Deploy the Worker and container image:

```sh
npm run deploy:cloudflare --prefix worker
```

For a repeatable local gate, deploy, and live smoke, use:

```sh
scripts/deploy-cloudflare-smoke.sh
```

It expects `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN`,
`CRABBOX_CLOUDFLARE_RUNNER_TOKEN`, and `CRABBOX_CLOUDFLARE_RUNNER_URL` in the
environment. Set `CRABBOX_CLOUDFLARE_SKIP_DEPLOY=1` to run only the local checks
and live smoke, or `CRABBOX_CLOUDFLARE_SKIP_SMOKE=1` to stop after deploy.

The deploy script passes `--containers-rollout=immediate` so Worker and
container changes roll out together. If you call Wrangler directly, include that
flag:

```sh
npx wrangler deploy \
  --config worker/wrangler.cloudflare.jsonc \
  --containers-rollout=immediate
```

Check the deployed container app:

```sh
npx wrangler containers list --config worker/wrangler.cloudflare.jsonc
npx wrangler containers info <container-application-id> \
  --config worker/wrangler.cloudflare.jsonc
```

## Capacity

`worker/wrangler.cloudflare.jsonc` defines one Durable Object class per
predefined Cloudflare instance type. Crabbox maps all generic classes to
`standard-4` because the smaller Cloudflare tiers are much smaller than the
default Linux classes on other providers.

```text
--class standard  standard-4
--class fast      standard-4
--class large     standard-4
--class beast     standard-4
```

Use `--type lite|basic|standard-1|standard-2|standard-3|standard-4` for smaller
smoke tests or quota control. Change `max_instances` in
`worker/wrangler.cloudflare.jsonc` when the account should allow more or fewer
concurrent containers.

`lite` is intended for no-sync and quick command smoke tests. Use `basic` or a
`standard-*` type for archive sync, and prefer `standard-*` for dependency-heavy
builds or tests. Large module downloads can exhaust the smaller container disks
before the command itself starts.

Cloudflare's current predefined instance types range from `lite` to
`standard-4`; `standard-4` is 4 vCPU, 12 GiB memory, and 20 GB disk. See
Cloudflare's Containers limits docs for current instance and account limits:
https://developers.cloudflare.com/containers/platform-details/limits/

## Live Smoke

With the runner URL and token configured, first test the deployed runner without
uploading the checkout:

```sh
crabbox run \
  --provider cloudflare \
  --no-sync \
  --timing-json \
  --shell \
  -- 'df -h / /tmp /workspace; printf "npm cache=%s\n" "${NPM_CONFIG_CACHE:-}"; printf "pnpm store="; pnpm config get store-dir'
```

That one-shot run cleans up automatically. Use `--keep` when you want to inspect
or reuse the same container:

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
  uploads a gzipped archive of the local checkout unless `--no-sync` is set,
  extracts it, then relays stdout, stderr, and exit status.
- Before upload, the provider checks remote disk headroom for both the archive
  and extracted checkout and fails early with a sizing hint if the selected type
  is too small.
- `warmup` starts a container and leaves it alive until `crabbox stop` or the
  configured TTL/idle deadline expires.
- `status` and `stop` resolve local Crabbox claims, then call the runner.
- `list` reports local Cloudflare claims. Add `--refresh` to check runner state
  for those claims. The runner intentionally does not expose a global container
  enumeration API.
- The default image includes Git, GitHub CLI, `jq`, `ripgrep`, Go, Node, and
  `pnpm`; repo-specific dependencies still belong to the repo setup command.
- npm and pnpm caches live under `/var/cache/crabbox`, and the container
  filesystem persists while the lease is active.
- The runner stores lease metadata in Durable Object storage and schedules
  cleanup at the earlier of `--ttl` or `--idle-timeout`. Uploads and command
  execution extend the idle deadline.
- `crabbox cleanup --provider cloudflare` only checks local claims. It removes
  claims whose runner state is expired, stopped, or missing.

Cloudflare Containers can also access Worker bindings through outbound handlers;
Crabbox does not wire those by default, but custom runner images can add them:
https://developers.cloudflare.com/containers/platform-details/workers-connections/

## Limitations

- Linux delegated `run`, `warmup`, `status`, `stop`, `list`, `doctor`, and
  local-claim cleanup are supported.
- SSH, VNC, browser desktop, code-server, Actions hydration, `--download`, and
  `--fresh-pr` are not supported.
- `--checksum` is not supported because sync uses archive upload/extract rather
  than rsync.
- Cleanup cannot discover containers that do not have a local Crabbox claim.
- Container capacity is bounded by the checked-in Wrangler bindings and the
  target account's Cloudflare Containers limits.
