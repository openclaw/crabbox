# ASCII Box Provider

Read when:

- choosing `provider: ascii-box`;
- configuring the ASCII Box API endpoint or workdir;
- changing `internal/providers/asciibox`.

[ASCII Box](https://box.ascii.dev) provides Ubuntu sandbox VMs. Crabbox uses the
documented `box --json` CLI as the control plane, lets `box ssh` prepare the
CLI-managed SSH key, and then runs normal Crabbox sync and commands over SSH.
The provider does not depend on private exec, upload, or command-stream REST
endpoints.

## When To Use

Use ASCII Box when commands should run in ASCII-managed Ubuntu sandboxes through
the `box` CLI's SSH endpoint. Use a delegated provider such as [Upstash Box](https://upstash.com/docs/box/overall/quickstart),
Modal, E2B, Islo, or Cloudflare when the provider owns command execution instead
of exposing SSH.

## Prerequisites

- Create an ASCII Box account at <https://box.ascii.dev>.
- Export the API key as `ASCII_BOX_API_KEY` or `CRABBOX_ASCII_BOX_API_KEY`.
- Install the official `box` CLI. Crabbox discovers the platform-specific config
  path through `box status --json`, writes a private config from the API key
  under its state directory, and does not require a pre-existing `box login`.

## Commands

```sh
crabbox warmup --provider ascii-box
crabbox run --provider ascii-box -- pnpm test
crabbox run --provider ascii-box --id blue-lobster --shell 'pnpm install && pnpm test'
crabbox status --provider ascii-box --id blue-lobster
crabbox stop --provider ascii-box blue-lobster
```

## Auth

```sh
export ASCII_BOX_API_KEY=...
```

`CRABBOX_ASCII_BOX_BASE_URL` or `asciiBox.baseUrl` can override the default
`https://ascii.dev`. Custom endpoints require HTTPS, except literal loopback
hosts may use HTTP. Userinfo, queries, and fragments are rejected before the
API key is written to Box CLI configuration or passed to the CLI.

## Config

```yaml
provider: ascii-box
target: linux
asciiBox:
  baseUrl: https://ascii.dev
  cliPath: box
  workdir: /home/user/crabbox
```

Provider flags:

```text
--ascii-box-base-url
--ascii-box-cli
--ascii-box-workdir
```

Environment overrides:

```text
CRABBOX_ASCII_BOX_API_KEY / ASCII_BOX_API_KEY
CRABBOX_ASCII_BOX_BASE_URL / ASCII_BOX_BASE_URL
CRABBOX_ASCII_BOX_CLI / BOX_CLI
CRABBOX_ASCII_BOX_HOME
CRABBOX_ASCII_BOX_WORKDIR
```

## Lifecycle

1. `crabbox warmup --provider ascii-box` creates a Box through `box new --json`,
   stores the returned Box id in a local lease claim, prepares the SSH key with
   `box ssh <id> -- true`, waits for SSH, and keeps the Box until
   `crabbox stop`. The default SSH key lives in the private Box CLI home
   (`CRABBOX_ASCII_BOX_HOME`, otherwise Crabbox state).
2. `crabbox run --provider ascii-box` provisions a Box for one run, or reuses an
   existing lease/slug/id, then uses the standard SSH sync and run path.
3. `crabbox status` resolves the local lease claim or raw Box id and reads Box
   state through `box info --json`.
4. `crabbox stop` releases the Box with `box stop --json`, removes the Box
   record with `box delete --json`, and removes the local lease claim.

## Limitations

- `--class`, `--type`, image, size, and keep-alive Box options are not exposed
  because the public CLI lifecycle surface does not document them.
- Desktop/VNC/code features are not advertised through Crabbox for this
  provider. Use the official Box tools directly for interactive sessions.
