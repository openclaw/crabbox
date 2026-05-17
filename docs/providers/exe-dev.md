# exe.dev Provider

Read when:

- choosing `provider: exe-dev`;
- configuring the exe.dev exec endpoint;
- changing `internal/providers/exedev`.

[exe.dev](https://exe.dev) is a delegated run provider with a single stateless
exec endpoint (`POST /exec`, see <https://exe.dev/docs/https-api>). Crabbox
`POST`s the user command as the request body and writes the response body to
stdout. The exe.dev API always returns JSON output (equivalent to `--json`);
the body is the SSH command output, streamed verbatim. There is no sandbox
lifecycle: every `run` constructs a fresh request and the service owns the
execution environment.

## When To Use

Use exe.dev for ad-hoc one-shot commands where you do not need workspace sync,
SSH, or a long-lived sandbox. Use E2B, Modal, or a Daytona/Cloudflare backend
when you need lease lifecycle, archive sync, or richer process transport.

## Commands

```sh
crabbox run --provider exe-dev --no-sync -- whoami
crabbox run --provider exe-dev --no-sync --shell 'uname -a && date'
```

`warmup`, `status`, `list`, and `stop` are not meaningful for a stateless exec
service; the backend rejects them with a clear message rather than pretending to
maintain sandboxes.

## Auth

```sh
export EXE_API_KEY=...   # required
```

`CRABBOX_EXE_API_KEY` is also accepted and wins over `EXE_API_KEY`. The token
is read from the environment only; the provider does not register a CLI flag
for the key. Do not pass it as a command-line argument.

The canonical exe.dev request shape is:

```sh
curl -X POST https://exe.dev/exec \
  -H "Authorization: Bearer $EXE_API_KEY" \
  -d 'whoami'
```

Crabbox sends the same `Authorization: Bearer $EXE_API_KEY` header and uses the
command string as the request body.

## Config

```yaml
provider: exe-dev
target: linux
exeDev:
  apiUrl: https://exe.dev
```

Provider flags:

```text
--exe-dev-url
```

Environment overrides:

```text
CRABBOX_EXE_API_KEY  (or EXE_API_KEY)
CRABBOX_EXE_API_URL  (or EXE_API_URL)
```

## Capabilities

- SSH: no.
- Crabbox sync: no. `--no-sync` is required.
- Provider sync: no.
- Desktop/browser/code: no.
- Actions hydration: no.
- Coordinator: no.

## Gotchas

- exe.dev does not expose sandbox state, so `--id`, `--keep`, `--reclaim`,
  `warmup`, `status`, and `stop` are rejected.
- The provider must run with `--no-sync`; there is no documented file upload
  endpoint, and the service does not maintain a working directory between
  requests.
- `/exec` has a 30-second server-side timeout (HTTP 504) and a 64KB request
  body limit (HTTP 413). It has no stdin and no pty, so interactive commands
  will not work.
- Response responses are always JSON-formatted command output (the API enables
  `--json` server-side). Pipe stdout through `jq` if you need to extract
  individual fields.
- Status mapping, per <https://exe.dev/docs/https-api>:
  - `2xx` -> command exited 0.
  - `422` -> command ran but exited non-zero; the body is streamed to stdout
    and `crabbox` exits non-zero (surfaced as `exeDevCommandFailedError`).
  - Other non-2xx (`400`, `401`, `403`, `404`, `405`, `413`, `429`, `500`,
    `504`) are transport-level failures surfaced as `exeDevAPIError`,
    mirroring the error shape used by other delegated providers.

Related docs:

- [Provider backends](../provider-backends.md)
