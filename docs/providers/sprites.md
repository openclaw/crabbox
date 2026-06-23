# Sprites Provider

Read this when you:

- pick `provider: sprites`;
- configure a Sprites token, API URL, or work root;
- change `internal/providers/sprites`.

Sprites is an SSH-lease provider for short-lived Linux microVMs. Crabbox creates
a sprite through the Sprites API, bootstraps OpenSSH inside it, and then reaches
it over SSH using `sprite proxy` as the `ProxyCommand`. From there everything is
the standard Crabbox SSH flow: Crabbox owns slugs, per-repo claims, per-lease SSH
keys, rsync sync, command execution, and `list`/`status` rendering.

## When to use

Reach for Sprites when you want a quick, disposable Linux microVM with normal
Crabbox sync-and-run behavior and no infrastructure of your own. Choose AWS,
Azure, GCP, or Hetzner instead when you need brokered fleet accounting, a desktop
or VNC, code-server, provider firewall control, or cloud images — Sprites
supports none of those.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes (via `sprite proxy`) |
| Crabbox sync (rsync) | Yes |
| Actions hydration | Yes (Linux SSH target) |
| Desktop / browser / code | No |
| Tailscale | No (SSH is exposed through `sprite proxy`) |
| Coordinator (broker) | No (always direct from the CLI) |

`--class`, `--type`, and `--tailscale` are rejected: Sprites owns VM sizing and
exposes SSH only through its proxy. Only `target=linux` is accepted.

## Auth

Crabbox needs a Sprites API token. Keep it in the environment or user config;
never pass it as a command-line argument.

```sh
export SPRITES_TOKEN=...
```

Token lookup, in priority order:

1. `CRABBOX_SPRITES_TOKEN`
2. `SPRITES_TOKEN`
3. `SPRITE_TOKEN`
4. `SETUP_SPRITE_TOKEN`

`SPRITE_TOKEN` and `SETUP_SPRITE_TOKEN` exist for compatibility with the Sprites
installer. A missing token fails the lease before any API call.

The authenticated `sprite` CLI must also be on `PATH`: Crabbox runs
`sprite --version` before creating a lease, and uses `sprite proxy` for SSH and
`sprite exec` for the one-time SSH bootstrap.

## Configuration

```yaml
provider: sprites
target: linux
sprites:
  apiUrl: https://api.sprites.dev
  workRoot: /home/sprite/crabbox
```

Defaults: API URL `https://api.sprites.dev`, work root `/home/sprite/crabbox`.

Flags:

- `--sprites-api-url` — Sprites API URL.
- `--sprites-work-root` — remote work root.

Environment variables:

```text
CRABBOX_SPRITES_TOKEN
SPRITES_TOKEN
SPRITE_TOKEN
SETUP_SPRITE_TOKEN
CRABBOX_SPRITES_API_URL
SPRITES_API_URL
CRABBOX_SPRITES_WORK_ROOT
```

`CRABBOX_SPRITES_API_URL` wins over `SPRITES_API_URL`. The work root must be a
dedicated absolute path; broad roots such as `/`, `/home`, `/home/sprite`,
`/tmp`, `/etc`, `/usr`, `/var`, and similar system directories are rejected
before sync.

## Commands

```sh
crabbox warmup --provider sprites
crabbox run --provider sprites -- pnpm test
crabbox ssh --provider sprites --id swift-crab
crabbox status --provider sprites --id swift-crab
crabbox stop --provider sprites swift-crab
crabbox list --provider sprites
```

## Lifecycle

1. Verify the `sprite` CLI is present and authenticated (`sprite --version`).
2. Create a sprite named `crabbox-<slug>`, labeled `crabbox`, `provider-sprites`,
   `lease-<lease-id>`, and `slug-<slug>`.
3. Generate a per-lease Crabbox SSH key.
4. Bootstrap inside the sprite: install OpenSSH server, Git, rsync, tar, and
   python3 if missing, add the public key for user `sprite`, and start `sshd`.
5. Return an SSH target with `ProxyCommand=sprite proxy -s %h -W 22` and wait
   until SSH is ready.
6. Crabbox syncs the checkout and runs commands over SSH.
7. On release, delete the sprite and remove the local claim and key (unless the
   lease is kept).

## Live smoke

Run a live smoke when you change Sprites lifecycle, SSH bootstrap, the proxy
command, or cleanup behavior.

```sh
export SPRITES_TOKEN=...
go build -trimpath -o bin/crabbox ./cmd/crabbox
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=sprites \
CRABBOX_LIVE_REPO=/path/to/my-app \
scripts/live-smoke.sh
```

The shared harness exits before any Sprites `warmup`, `status`, `ssh`, `run`,
`list`, or `stop` command when the authenticated `sprite` CLI is missing. With
the CLI and token configured, it creates one short-lived sprite, waits for SSH,
verifies `ssh`, runs one command, lists normalized Sprites inventory, and stops
the lease.

For manual debugging, run the same lifecycle directly:

```sh
export SPRITES_TOKEN=...
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox warmup --provider sprites --timing-json
lease=<slug-or-cbx_id-from-warmup-output>

bin/crabbox status --provider sprites --id "$lease" --wait
bin/crabbox ssh --provider sprites --id "$lease"
bin/crabbox run --provider sprites --id "$lease" --shell 'echo crabbox-sprites-ok'
bin/crabbox list --provider sprites
bin/crabbox stop --provider sprites "$lease"
```

Expected results:

- `warmup` creates a `crabbox-<slug>` sprite and prints `provider=sprites`, a
  Crabbox lease ID, a slug, and the sprite name.
- `status --wait` reports a running Linux lease.
- `ssh` prints a command that includes `ProxyCommand=sprite proxy -s %h -W 22`.
- `run` prints `crabbox-sprites-ok`.
- `list` shows Crabbox-owned sprites (those whose name starts with `crabbox-` or
  that carry the `crabbox` / `lease-cbx-*` labels).
- `stop` deletes the sprite and removes the local claim and key.

## Gotchas

- An `--id` can be a Crabbox lease ID, a local slug, a `spr_<sprite-name>` ID, or
  a raw sprite name.
- A raw sprite that Crabbox did not create can only be adopted with `--reclaim`.
- If `sprite proxy` cannot connect, `status --wait`, `run`, and `ssh` fail even
  when the Sprites API can see the sprite — SSH depends on the local CLI.

## Related docs

- [Feature: Sprites](../features/sprites.md)
- [Provider backends](../provider-backends.md)
