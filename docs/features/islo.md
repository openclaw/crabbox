# Islo

Read when:

- choosing `provider: islo`;
- understanding what Crabbox owns versus what islo owns;
- diagnosing islo auth or sandbox errors.

Crabbox can target [islo.dev](https://islo.dev) sandboxes as the machine backend by linking against the [islo Go SDK](https://github.com/islo-labs/go-sdk) instead of shelling out to the `islo` CLI. Select it with `--provider islo` for one command, or set `provider: islo` in config when a repo should use it by default.

## One-Liners

```sh
export ISLO_API_KEY=ak_...
crabbox run --provider islo --islo-image docker.io/library/ubuntu:24.04 -- echo hello
crabbox warmup --provider islo --islo-image docker.io/library/python:3.12-slim
crabbox status --provider islo --id <sandbox-name> --wait
crabbox list --provider islo --json
crabbox stop --provider islo <sandbox-name>
```

Crabbox accepts the sandbox name directly, the `isb_<sandbox-name>` lease ID, or the friendly slug Crabbox prints when the sandbox was first claimed.

## Repo Config

```yaml
provider: islo
islo:
  image: docker.io/library/ubuntu:24.04
  workdir: /workspace/crabbox
  gatewayProfile: default
  env:
    NODE_ENV: production
```

Environment variables `CRABBOX_ISLO_IMAGE`, `CRABBOX_ISLO_WORKDIR`, `CRABBOX_ISLO_GATEWAY_PROFILE`, and `CRABBOX_ISLO_DEBUG` override the corresponding YAML keys.

## Auth

`ISLO_API_KEY` (typically `ak_...`) is the only credential Crabbox reads. Optional `ISLO_BASE_URL` overrides the default `https://api.islo.dev`. Crabbox does not call the Crabbox login broker, does not send work to the Cloudflare coordinator, and does not store the islo key on disk. The SDK exchanges the API key for a short-lived session token and refreshes it automatically before expiry.

## Forwarded Operations

| crabbox op | islo Go SDK call |
|---|---|
| `warmup` / `run` (acquire) | `Sandboxes.CreateSandbox` |
| `run` (exec) | direct SSE consumer on `POST /sandboxes/{name}/exec/stream` (reuses the SDK auth provider) |
| `status` | `Sandboxes.GetSandbox`; `--wait` polls until `status == "running"` |
| `list` | `Sandboxes.ListSandboxes` |
| `stop` | `Sandboxes.DeleteSandbox` |

The exec stream bypasses the SDK's JSON-coalescing wrapper so stdout/stderr deltas reach the user terminal as they arrive. The SDK still owns auth and retry semantics. If `go-sdk` later exposes a true streaming API, the consumer here should be removed.

## Ownership Boundary

- Islo owns sandbox provisioning, networking, idle expiry, image lifecycle, file transfer, and command execution.
- Crabbox owns local YAML/env config, friendly slugs, repo claims, provider selection, and final timing summaries.

Because islo owns workspace state, Crabbox sync flags do not apply: `--sync-only`, `--checksum`, and `--force-sync-large` return exit 2 for `provider: islo`. `--no-sync` is accepted but redundant. The run summary prints `sync=delegated`.

## Unsupported

- `--desktop`, `--browser`, `crabbox vnc`, `crabbox screenshot` — islo sandboxes are headless.
- `--actions-runner` — islo owns sandbox setup; GitHub Actions runner hydration is out of scope.

## Limits

The current SDK release does not expose the SSE stream as an iterator, so Crabbox issues the streaming request directly. Auth refresh and base URL still come from the SDK; only the response body parser is local. File a follow-up against `go-sdk` to expose `Sandboxes.ExecStream(ctx, ...) <-chan ExecLogLine` once that lands.

Related docs:

- [Providers](providers.md)
- [run command](../commands/run.md)
- [warmup command](../commands/warmup.md)
- [Source map](../source-map.md)
