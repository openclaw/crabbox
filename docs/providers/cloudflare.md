# Cloudflare Provider

Use `provider: cloudflare` when Crabbox should run commands through a
Cloudflare Worker backed by a custom Cloudflare Containers image. The provider
also accepts the short alias `cf`.

Cloudflare is a delegated run provider. Crabbox owns local repo archive
creation, local lease claims, timing output, command rendering, and friendly
slugs. A small Worker runner owns container creation, file upload, command
execution, and teardown.

## Requirements

- A Cloudflare account with Workers, Durable Objects, and Containers access.
- Wrangler authenticated for deploys.
- Docker or a Docker-compatible CLI/daemon available to Wrangler for container
  image builds.
- A deployed Crabbox Cloudflare runner with `CRABBOX_RUNNER_TOKEN` set
  as a Worker secret.

The Worker runner lives in `worker/src/cloudflare-container-runner.ts`. The
container image is built from `worker/cloudflare-container.Dockerfile` and starts
the HTTP runner in `worker/cloudflare-container-runner`. The deploy config is
`worker/wrangler.cloudflare.jsonc`.

## Configuration

```yaml
provider: cloudflare
cloudflare:
  apiUrl: https://crabbox-cloudflare-container-runner.example.workers.dev
  workdir: /workspace/crabbox
```

Keep the bearer token in `CRABBOX_CLOUDFLARE_RUNNER_TOKEN` or user-level
config, not in repo YAML. `CRABBOX_CLOUDFLARE_RUNNER_URL` can also provide the
runner URL.

With the token already available from `CRABBOX_CLOUDFLARE_RUNNER_TOKEN` or user
config, the runner URL can also be supplied as a flag:

```sh
crabbox run \
  --provider cloudflare \
  --cloudflare-url https://runner.example.workers.dev \
  -- pnpm test
```

## Runner Deploy

Install Worker dependencies and verify the runner:

```sh
npm ci --prefix worker
npm run check --prefix worker
npm run build:cloudflare --prefix worker
```

Deploy with:

```sh
npm run deploy:cloudflare --prefix worker
```

The deploy script uses Wrangler's immediate container rollout mode so a Worker
deploy updates the backing container image and `instance_type` in the same
operation. When deploying manually, include the same flag:

```sh
npx wrangler deploy \
  --config worker/wrangler.cloudflare.jsonc \
  --containers-rollout=immediate
```

Then set the bearer token:

```sh
printf '%s' "$CRABBOX_CLOUDFLARE_RUNNER_TOKEN" \
  | npx wrangler secret put CRABBOX_RUNNER_TOKEN \
      --config worker/wrangler.cloudflare.jsonc
```

The checked-in runner config defines one Durable Object binding per predefined
Cloudflare container instance type. Crabbox maps `--class standard|fast|large|beast`
to `standard-1|standard-2|standard-3|standard-4`; `beast` is the default and has
the largest predefined disk budget. Use `--type lite|basic|standard-1|standard-2|standard-3|standard-4`
to pin a specific Cloudflare type.

When `--provider cloudflare` is used to override a repo config written for
another provider, Crabbox defaults Cloudflare back to `beast` unless `--class`
or `--type` is also set. This avoids inheriting a generic `class: standard`
repo default into a small Cloudflare container by accident.

Check the active container app after deploy:

```sh
npx wrangler containers list \
  --config worker/wrangler.cloudflare.jsonc
npx wrangler containers info <container-application-id> \
  --config worker/wrangler.cloudflare.jsonc
```

## Live Smoke

With `CRABBOX_CLOUDFLARE_RUNNER_TOKEN` available and `cloudflare.apiUrl` set,
start with a no-sync smoke so the runner, token, image, disk, and package cache
settings are exercised before uploading a repository archive:

```sh
crabbox run \
  --provider cloudflare \
  --no-sync \
  --timing-json \
  --shell \
  -- 'df -h / /tmp /workspace; printf "npm cache=%s\n" "${NPM_CONFIG_CACHE:-}"; printf "pnpm store="; pnpm config get store-dir'
```

Stop the printed lease ID when the smoke is complete:

```sh
crabbox stop --provider cloudflare <lease-id>
```

## Behavior

- `run` creates or reuses a Container Durable Object, uploads a gzipped archive
  of the local checkout, extracts it into `workdir`, and relays command output
  and exit status back to the CLI.
- Before uploading an archive, the provider checks remote disk headroom for the
  compressed archive plus extracted checkout and fails early with a sizing hint
  when the selected instance type is too small.
- `warmup` creates the container and starts the runner. The workdir is prepared
  when a later sync or no-sync run uses the lease. Warmed containers remain
  alive until `crabbox stop` or the configured TTL/idle deadline expires.
- `status` and `stop` resolve Crabbox's local claim and call the runner.
- `list` reports local Cloudflare claims. Cloudflare does not expose a
  global container listing API through the runner.
- `worker/cloudflare-container.Dockerfile` is the default Crabbox runner image.
  Operators can replace it in Wrangler config when they need a different
  language or toolchain baseline.
- The default image includes common repo-test tools such as Git, GitHub CLI,
  `jq`, `ripgrep`, Go, Node, and `pnpm`; project-specific dependencies still
  belong to the repo's own setup commands.
- npm and pnpm package caches live under `/var/cache/crabbox` so a warmed
  container can reuse package downloads across repeated commands.
- Warmed containers keep their container filesystem between commands while the
  lease is active. Use that as the provider's cache layer for cloned
  repositories, package stores, and generated setup state.
- The runner stores lease metadata in the Container Durable Object and schedules
  cleanup at the earlier of `--ttl` or `--idle-timeout`. Activity on file upload
  or command execution extends the idle deadline. When the deadline passes, the
  runner destroys the container and marks the lease expired.
- `crabbox cleanup --provider cloudflare` cannot discover every remote
  container, but it checks local Cloudflare claims and removes entries
  whose runner state is expired, stopped, or missing.

## Limitations

- SSH, VNC, browser desktop, code-server, Actions hydration, and `--download`
  are not supported.
- `--fresh-pr` is not supported for delegated archive sync.
- `--checksum` is not supported because the provider uses archive upload and
  extraction instead of Crabbox rsync.
- Container size classes are controlled by the bindings in
  `worker/wrangler.cloudflare.jsonc`. Choose `max_instances` values that match
  the account's Cloudflare Containers limits.
- Cloudflare can roll container changes separately from Worker script changes.
  Use `npm run deploy:cloudflare --prefix worker` or pass
  `--containers-rollout=immediate` when running Wrangler directly.
