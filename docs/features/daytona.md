# Daytona

Read when:

- choosing `provider: daytona`;
- configuring Daytona auth, snapshots, or SSH access;
- reviewing Daytona provider behavior.

`provider: daytona` provisions Daytona sandboxes from snapshots. `run` and
`warmup` use Daytona's SDK/toolbox for workspace upload and command execution;
`ssh` mints a short-lived Daytona SSH token only when interactive shell access is
requested.

## Auth

Run Daytona's CLI login:

```sh
daytona login --api-key ...
```

Crabbox uses the active Daytona CLI profile when no explicit Daytona auth
environment variables are set.

Alternatively, set one of:

```sh
export DAYTONA_API_KEY=...
```

or:

```sh
export DAYTONA_JWT_TOKEN=...
export DAYTONA_ORGANIZATION_ID=...
```

`DAYTONA_ORGANIZATION_ID` is required when JWT auth is used. `DAYTONA_API_URL`
or `daytona.apiUrl` can override the default `https://app.daytona.io/api`.
Explicit environment or Crabbox config values override the Daytona CLI profile.

## Config

Daytona's first Crabbox integration is snapshot-first. The snapshot owns CPU,
memory, disk, and installed tooling. Crabbox does not expose Daytona resource
flags in this mode.

```yaml
provider: daytona
target: linux
daytona:
  snapshot: crabbox-ready
  target: ""
  user: daytona
  workRoot: /home/daytona/crabbox
  sshGatewayHost: ssh.app.daytona.io # fallback when the API omits sshCommand
  sshAccessMinutes: 30
```

Equivalent flags:

```sh
crabbox warmup --provider daytona --daytona-snapshot crabbox-ready
crabbox run --provider daytona --id <slug> -- pnpm test
crabbox stop --provider daytona <slug>
```

## Behavior

- `warmup` creates a Daytona sandbox from `daytona.snapshot`, waits for the
  sandbox, records Crabbox labels, then prints a normal Crabbox lease ID and
  slug.
- `run --id` resolves a Daytona sandbox, uploads a Crabbox manifest archive
  through Daytona toolbox file APIs, extracts it in the sandbox, and executes the
  command through Daytona toolbox process APIs.
- `list`, `status`, and `stop` use Daytona sandbox labels to find Crabbox-owned
  sandboxes.
- `ssh` mints a fresh Daytona SSH token, parses the host and port returned by
  Daytona's `sshCommand`, and redacts the token as `<token>` unless
  `--show-secret` is used.

Daytona is a hybrid backend: core rendering, lease labels, sync manifests, and
repo claim checks stay Crabbox-owned, while the actual `run` transport is
Daytona SDK/toolbox. Actions runner hydration is not supported for Daytona
warmup because it requires a normal long-lived SSH runner host.
