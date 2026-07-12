# Sprites

Read this when you:

- choose `provider: sprites`;
- debug Sprites token resolution, the `sprite` CLI, the SSH proxy, or bootstrap;
- change Sprites lease creation, status, sync, or cleanup.

`provider: sprites` provisions a Sprites Linux microVM and adapts it into a
normal Crabbox SSH lease. Sprites owns the microVM lifecycle and the
`sprite proxy` transport. Crabbox owns local config, slugs, repo claims,
per-lease SSH keys, rsync-based sync, command execution, timing summaries, and
normalized `list`/`status` output. There is no Crabbox coordinator (broker)
path for Sprites — it always runs direct from the CLI.

## Auth

Set a Sprites token through the environment or user config. Do not commit
tokens to repo config.

```sh
export SPRITES_TOKEN=...
```

Crabbox resolves the token from the first set of these, in order:

1. `CRABBOX_SPRITES_TOKEN`
2. `SPRITES_TOKEN`
3. `SPRITE_TOKEN`
4. `SETUP_SPRITE_TOKEN`
5. `sprites.token` in config

Install and authenticate the Sprites CLI before first use. Crabbox calls the
Sprites HTTP API for sprite create/get/list/delete, and shells out to the local
`sprite` CLI for `sprite --version` (a readiness check), `sprite exec` (running
the SSH bootstrap inside the microVM), and `sprite proxy` (the SSH transport).

## Config

```yaml
provider: sprites
target: linux
sprites:
  apiUrl: https://api.sprites.dev
  workRoot: /home/sprite/crabbox
```

Defaults: `apiUrl` is `https://api.sprites.dev` and `workRoot` is
`/home/sprite/crabbox`. The API URL and work root also read from the
environment:

- `CRABBOX_SPRITES_API_URL` or `SPRITES_API_URL` → `sprites.apiUrl`
- `CRABBOX_SPRITES_WORK_ROOT` → `sprites.workRoot`

Custom API URLs require HTTPS unless the host is literal loopback. Userinfo,
queries, and fragments are rejected, and authenticated requests cannot follow
redirects to another origin.

Equivalent one-off flags:

```sh
crabbox warmup --provider sprites
crabbox run --provider sprites --sprites-work-root /home/sprite/crabbox -- pnpm test
crabbox run --provider sprites --sprites-api-url https://api.sprites.dev -- pnpm test
crabbox ssh --provider sprites --id <slug>
crabbox status --provider sprites --id <slug>
crabbox stop --provider sprites <slug>
```

## Behavior

- `warmup` creates a sprite named `crabbox-<...>` and a local Crabbox claim.
- During bootstrap Crabbox ensures OpenSSH server, Git, rsync, tar, and python3
  are installed (via `apt-get` when missing), appends the per-lease public key
  to `/home/sprite/.ssh/authorized_keys`, and starts `sshd` — registering it as
  a `sprite-env` service when that tool is available so it survives restarts.
- The lease SSH user is `sprite`.
- `run` creates or reuses a sprite, syncs the current Git manifest over SSH, and
  runs the command through Crabbox's standard SSH executor.
- `ssh` prints a command that uses `sprite proxy -s %h -W 22` as the SSH
  `ProxyCommand`.
- `status`, `list`, and `stop` operate on Sprites resources mapped to local
  claims or provider labels; `list` only shows sprites whose name starts with
  `crabbox-`.
- `stop` deletes the sprite and removes the local claim after provider cleanup
  succeeds.

## Boundaries

- Linux only.
- No coordinator; auth is local/provider-native.
- No VNC, desktop, browser, or code-server.
- `--tailscale` is rejected: Sprites exposes SSH through `sprite proxy`.
- `--class` and `--type` do not apply to Sprites.
- Actions hydration works, since the sprite is a normal Linux SSH target.

## Troubleshooting

- `provider=sprites requires SPRITES_TOKEN, SPRITE_TOKEN, SETUP_SPRITE_TOKEN, or CRABBOX_SPRITES_TOKEN`:
  set one of those tokens (or `sprites.token` in config).
- `provider=sprites requires the sprite CLI on PATH and authenticated`: install
  the authenticated Sprites CLI and ensure `sprite` is on `PATH`. Crabbox probes
  this with `sprite --version`.
- `sprite proxy` failures mean SSH cannot reach the microVM even when API calls
  succeed. Run `crabbox status --provider sprites --id <slug> --wait` to retry
  the idempotent SSH bootstrap.
- Slow first boot usually means package install inside the sprite is still
  running. Kept leases reuse the installed OpenSSH/rsync packages.
- The work root must resolve to a dedicated absolute path (broad paths such as
  `/`, `/home`, `/tmp` are rejected). Prefer a subdirectory under the sprite
  user's home, for example `/home/sprite/crabbox`.

## Related docs

- [Provider: Sprites](../providers/sprites.md)
- [Provider Reference](../providers/README.md)
- [Provider backends](../provider-backends.md)
