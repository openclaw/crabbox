# Cloudflare Dynamic Workers provider

Select with `provider: cloudflare-dynamic-workers` (aliases `cf-dynamic` and
`cfdw`) to run Cloudflare Workers module source through Cloudflare Dynamic
Workers. This is a **delegated-run** provider with `target=worker-runtime`: the
local CLI sends a JavaScript or TypeScript module to a deployed Crabbox loader
Worker, the loader creates or reuses a Dynamic Worker, invokes its `fetch`
handler, and returns the result. There is no Linux host and no SSH lease.

Use the separate [Cloudflare provider](cloudflare.md) when you need Cloudflare
Containers and Linux command execution.

## Capabilities at a glance

- **Target:** `worker-runtime`.
- **Supported commands:** `run`, `warmup`, `status`, `stop`, `list`, `doctor`,
  and local-claim `cleanup`.
- **Run input:** `--script <file>` or `--script-stdin` module source.
- **Coordinator:** never brokered. The CLI talks directly to the loader Worker.
- **Cache modes:** `one-shot`, `stable`, and `explicit`.
- **Egress modes:** `blocked` by default, or `intercept` when the loader exports
  the gateway bindings required by the Worker runner.
- **Not supported:** trailing `-- <command>` argv, POSIX shell, SSH, rsync,
  archive sync, VNC, browser desktop, code-server, port forwarding, Actions
  hydration, artifact download, `--fresh-pr`, `--checksum`, `--class`, and
  `--type`.

## Requirements

- A Cloudflare Workers account with Dynamic Workers enabled for the target
  account.
- Wrangler authenticated for that account.
- The deployed Crabbox Dynamic Workers loader from
  `worker/wrangler.cloudflare-dynamic-workers.jsonc`.
- A Workers KV namespace bound as `RUNS` for durable run metadata used by
  `status`, `list --refresh`, `stop`, and `cleanup`.
- The Worker secret `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN`.
- CLI-side `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL` and
  `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN`.

The Worker entrypoint is `worker/src/cloudflare-dynamic-worker-runner.ts`. It is
separate from the Cloudflare Containers runner and uses a `worker_loaders`
binding named `LOADER` plus a Workers KV namespace binding named `RUNS`.

## Configuration

Keep the bearer token out of repo YAML. Repo config can select the provider and
loader URL:

```yaml
provider: cloudflare-dynamic-workers
target: worker-runtime
cloudflareDynamicWorkers:
  loaderUrl: https://crabbox-cloudflare-dynamic-workers-runner.example.workers.dev
  compatibilityDate: "2026-06-12"
  compatibilityFlags:
    - nodejs_compat
  cacheMode: stable
  egress: blocked
  cpuMs: 50
  subrequests: 12
  timeoutSecs: 30
```

Config keys map to the typed `cloudflareDynamicWorkers` section:
`loaderUrl`, `token`, `compatibilityDate`, `compatibilityFlags`, `cacheMode`,
`egress`, `cpuMs`, `subrequests`, `timeoutSecs`, and `metadata`.

| Setting | Config key | Environment variable | Flag |
| --- | --- | --- | --- |
| Loader URL | `loaderUrl` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL` | `--cloudflare-dynamic-workers-url` |
| Loader URL alias | `loaderUrl` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_LOADER_URL` | `--cloudflare-dynamic-workers-url` |
| Token | `token` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN` | _(none, by design)_ |
| Compatibility date | `compatibilityDate` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_DATE` | `--cloudflare-dynamic-workers-compatibility-date` |
| Compatibility flags | `compatibilityFlags` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_FLAGS` | `--cloudflare-dynamic-workers-compatibility-flags` |
| Cache mode | `cacheMode` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CACHE_MODE` | `--cloudflare-dynamic-workers-cache` |
| Egress mode | `egress` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_EGRESS` | `--cloudflare-dynamic-workers-egress` |
| CPU limit | `cpuMs` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CPU_MS` | `--cloudflare-dynamic-workers-cpu-ms` |
| Subrequest limit | `subrequests` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_SUBREQUESTS` | `--cloudflare-dynamic-workers-subrequests` |
| Loader timeout | `timeoutSecs` | `CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TIMEOUT_SECS` | `--cloudflare-dynamic-workers-timeout-secs` |

The token is intentionally not exposed as a command-line flag because command
arguments can appear in shell history and process listings.

## Deploy

Install dependencies and verify the Worker before deploy:

```sh
npm ci --prefix worker
npm run check --prefix worker
npm run build:cloudflare-dynamic-workers --prefix worker
```

Set the loader bearer token as a Worker secret through stdin:

```sh
printf '%s' "$CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN" \
  | npx wrangler secret put CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN \
      --config worker/wrangler.cloudflare-dynamic-workers.jsonc
```

Create a KV namespace for run metadata and replace the placeholder `RUNS`
namespace IDs in `worker/wrangler.cloudflare-dynamic-workers.jsonc` before live
deploy:

```sh
npx wrangler kv namespace create crabbox-cloudflare-dynamic-workers-runs
npx wrangler kv namespace create crabbox-cloudflare-dynamic-workers-runs-preview --preview
```

Deploy the Dynamic Workers loader:

```sh
npm run deploy:cloudflare-dynamic-workers --prefix worker
```

Check readiness without creating a Dynamic Worker:

```sh
crabbox doctor --provider cloudflare-dynamic-workers
```

## Run modules

Dynamic Workers run Worker module source, not shell commands:

```js
export default {
  async fetch() {
    return new Response("hello from dynamic workers");
  },
};
```

Run the module from a file:

```sh
crabbox run \
  --provider cloudflare-dynamic-workers \
  --script ./worker-smoke.mjs \
  --timing-json
```

Or send source on stdin:

```sh
printf '%s\n' 'export default { fetch() { return new Response("ok") } }' \
  | crabbox run --provider cfdw --script-stdin
```

Trailing command argv is rejected because there is no POSIX shell:

```sh
crabbox run --provider cloudflare-dynamic-workers -- echo not-supported
```

## Cache and billing

`cacheMode` controls how the loader identifies Dynamic Workers:

- `one-shot` creates an uncached one-off run.
- `stable` uses a stable ID derived from module source and runtime settings so
  repeated same-code runs can reuse Cloudflare's Dynamic Worker cache.
- `explicit` requires `--id` and is the mode to use when an operator wants a
  named Worker identity and explicit cleanup with `crabbox stop`.

Cloudflare bills Dynamic Workers according to the platform's current pricing and
limits. Stable IDs can improve repeat-run startup behavior, but they should be
treated as a cache key, not as durable user data.

## Egress and limits

The default egress mode is `blocked`. In `blocked` mode the loader does not wire
an outbound gateway into the Dynamic Worker. `intercept` mode routes outbound
fetches through the loader's `HttpGateway` and `LogTailer` exports when the live
Cloudflare runtime supports those bindings.

Use `cpuMs`, `subrequests`, and `timeoutSecs` to bound execution. These are
runtime limits for a module invocation, not VM sizing knobs; `--class` and
`--type` are rejected for this provider.

## Lifecycle commands

- `warmup` checks loader readiness. With `--keep` or `--slug`, it also records a
  local claim for later `status`, `list`, or `stop`.
- `status` resolves a local claim or explicit run ID and asks the loader for run
  metadata.
- `list` reports local Dynamic Workers claims. Add `--refresh` to query loader
  metadata for each claim.
- `stop` deletes loader metadata for the run and removes the local claim. If the
  loader reports the run is missing, Crabbox removes the stale local claim.
- `cleanup` checks local claims and removes stale claims whose loader metadata
  is missing or terminal. `--dry-run` prints the same decisions without removing
  local state.

Cleanup is local-claim cleanup. It is not a global Cloudflare account inventory
sweeper.

## Live smoke

The live smoke harness is opt-in and non-mutating by default:

```sh
scripts/deploy-cloudflare-dynamic-workers-smoke.sh
```

Without explicit gates it exits successfully with:

```text
environment_blocked provider=cloudflare-dynamic-workers mutation=false reason=live_gate_missing
```

To allow live deploy and smoke, set both gates and the required Cloudflare
environment:

```sh
export CRABBOX_LIVE=1
export CRABBOX_LIVE_PROVIDERS=cloudflare-dynamic-workers
export CLOUDFLARE_ACCOUNT_ID=...
export CLOUDFLARE_API_TOKEN=...
export CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL=https://runner.example.workers.dev
export CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN=...
scripts/deploy-cloudflare-dynamic-workers-smoke.sh
```

The script classifies the result as one of:

- `environment_blocked` — gates, local tools, dependencies, or runtime setup are
  unavailable, and no live success is claimed.
- `auth_blocked` — required credentials are missing or rejected.
- `quota_blocked` — Cloudflare plan, quota, limit, or billing capacity prevents
  deploy or run.
- `live_cloudflare_dynamic_workers_smoke_passed` — deploy, doctor, run, status,
  list, stop, and cleanup completed.

The script never prints token values. Wrangler secrets are written through stdin.

## Limitations

- Dynamic Workers execute Worker-runtime module source only.
- There is no Linux shell, SSH target, filesystem sync, archive upload, VNC,
  browser desktop, code-server, port forwarding, or Actions hydration.
- `--download`, `--fresh-pr`, `--artifact-glob`, `--require-artifact`,
  `--checksum`, `--class`, and `--type` are unsupported.
- `intercept` egress requires loader exports and Cloudflare runtime support.
- Local `list` and `cleanup` are claim-based; they do not enumerate every
  Dynamic Worker in the Cloudflare account.

## Related docs

- [Cloudflare Containers provider](cloudflare.md)
- [Provider feature overview](../features/providers.md)
- [Configuration](../features/configuration.md)
- [run](../commands/run.md)
- [doctor](../commands/doctor.md)
