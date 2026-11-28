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
| Fixed lease IDs | Yes (`warmup --lease-id`) |

`--class`, `--type`, and `--tailscale` are rejected: Sprites owns VM sizing and
exposes SSH only through its proxy. Only `target=linux` is accepted.

## Auth

Crabbox needs a Sprites API token. Keep it in the environment;
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

The `sprite` CLI must also be on `PATH`: Crabbox runs `sprite --version` before
creating a lease, and uses `sprite proxy` for SSH and `sprite exec` for SSH
bootstrap. Crabbox passes its resolved token and API URL to both child
processes, overriding the CLI's saved context and ambient `SPRITE_URL`.
No separate CLI login is required for Crabbox-managed commands. Tokens are
not placed in command-line arguments, generated SSH configuration, or lease claims.

Use `crabbox connect --provider sprites --id <slug>` for an interactive session
with those same credentials. `crabbox ssh` prints a standalone OpenSSH command;
before running that command yourself, configure the Sprite CLI with the same
`SPRITE_TOKEN` and `SPRITES_API_URL` (and unset any conflicting `SPRITE_URL`).

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

`CRABBOX_SPRITES_API_URL` wins over `SPRITES_API_URL`. Custom API URLs must use
HTTPS, except literal loopback hosts may use HTTP. Userinfo, queries, and
fragments are rejected, and authenticated requests never follow redirects to a
different scheme, host, or port. The work root must be a
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

1. Verify the `sprite` CLI is present (`sprite --version`).
2. Create a sprite named `crabbox-<slug>`, labeled `crabbox`, `provider-sprites`,
   `lease-<lease-id>`, and `slug-<slug>`.
3. Generate a per-lease Crabbox SSH key.
4. Bootstrap inside the sprite: install OpenSSH server, Git, rsync, tar, and
   python3 if missing, add the public key for user `sprite`, and start `sshd`.
5. Return an SSH target with `ProxyCommand=sprite proxy -s %h -W 22` and wait
   until SSH is ready.
6. Crabbox syncs the checkout and runs commands over SSH.
7. On release, require the exact local lease claim, API endpoint, sprite name,
   and live lease ownership labels before deleting the sprite. Successful
   deletion removes the fenced claim and local key; failed deletion preserves
   the claim for a safe retry.

Reuse validates the saved API endpoint, immutable Sprite ID, organization,
and lease ownership before installing keys or running bootstrap. A replacement
Sprite with the same name is not silently adopted, even with `--reclaim`.

Plain `status` and `inspect` do not wake the Sprite, generate keys, install
packages, or start services. When the API reports the Sprite `running`, they
probe SSH with the existing key and report real readiness; otherwise they report
`ready=false` without contacting the Sprite. `status --wait` probes SSH in any
state; it can wake the Sprite but does not bootstrap it. Use a normal
reuse command such as `run --id <slug>` when SSH bootstrap needs to be retried.

Status and list project native identity and state without inventing creation
or expiry timestamps. When an associated local claim exists, they show its
recorded idle timeout, TTL, retention, and last use; otherwise those values
remain unknown. Reader configuration does not replace saved policy, and a read
does not count as lease activity. Recorded TTL is local policy, not a native
Sprites deletion deadline.

If a Sprite has already been deleted, `stop` confirms the token's organization
against the saved claim and rechecks absence before removing the local claim
and key. Claims without the original organization and immutable ID are preserved,
as are claims when the account differs or verification fails.

### Staying awake

While a lease is in use, Crabbox heartbeats upsert a `crabbox-lease` task through
the in-Sprite Tasks API. The task keeps the Sprite running so background
processes and their outbound connections remain reachable. Its expiry follows
the lease idle window and is refreshed by each heartbeat; after the last
heartbeat, it expires within that idle window so an idle lease stops billing
compute. Sprites caps task expiry at one hour.

Raw names and provider labels only discover sprites; they never authorize
deletion. Explicit `--reclaim` can adopt a verified sprite through a normal
reuse command before it can be stopped. `stop --force` is unavailable because
Sprites cannot independently prove ownership of an arbitrary lost-claim sprite.

Normal reuse without a local ownership claim requires `--reclaim` even when all
Crabbox labels match. Adoption requires an immutable provider resource ID;
read-only status and inspection do not create a claim or bootstrap the Sprite.

## Fixed lease IDs

```sh
crabbox warmup --provider sprites --lease-id cbx_123456789abc
crabbox warmup --provider sprites --lease-id cbx_123456789abc
crabbox stop --provider sprites cbx_123456789abc
```

Sprites has no idempotency key, but Sprite names are unique per organization
and a duplicate create is rejected with HTTP 409. A fixed lease therefore uses a
deterministic name derived from the canonical lease ID and slug, and the shared
fixed-lease engine durably records the intent, API endpoint, token
organization, and a random per-attempt nonce before sending the create. The
create request carries the usual lease labels plus `attempt-<nonce>`; Sprites
stores labels atomically with the Sprite.

Crabbox never treats the create reply as identity evidence. After every create,
and on every replay, it reads the Sprite by name and adopts it only when its
name, organization, lease and slug labels, and attempt nonce all match the
recorded intent. It then binds the immutable Sprite `id`; later reads must
match it exactly. A Sprite with the same name but any other labels, nonce,
organization, or `id` produces `lease_id_conflict` and is never deleted.

| Interrupted step | Replay outcome |
| --- | --- |
| Create reply lost, Sprite exists | Adopted after attestation; no second create |
| Create never landed (404 for the name) | The same attempt is resubmitted |
| Sprite bound, then deleted out of band | `lease_id_conflict`; `stop` records absence |

A 404 only proves absence after the token's organization is confirmed to match
the claim, since a token for another organization sees every foreign Sprite as
missing. If a create outcome is still unresolved, `stop` refuses and asks for a
replay with the same lease ID first.

Release re-reads and re-attests the Sprite immediately before deleting it by
name, then requires a 404 before publishing a `released` tombstone. A failed or
unconfirmed delete keeps the claim for retry. The tombstone keeps the lease ID
from being reused for a new Sprite: later warmups with it fail, `status` reports
`released`, and repeat `stop` calls succeed without touching the API.

The Sprites API addresses a Sprite only by name: there is no delete or read by
immutable `id` and no conditional request. Two limits follow. Re-attestation
narrows, but cannot close, the window between the last check and the delete in
which a same-name replacement could appear. And a Sprite deleted out of band
before its `id` was bound is indistinguishable from a create that never landed,
so a replay recreates it; a lease still has at most one live Sprite. Once the
`id` is bound, the same disappearance is reported as `lease_id_conflict`.

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
`list`, or `stop` command when the `sprite` CLI is missing. With
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
- `status --wait` confirms SSH readiness for the Linux lease.
- `ssh` prints a command that includes `ProxyCommand=sprite proxy -s %h -W 22`.
- `run` prints `crabbox-sprites-ok`.
- `list` shows Crabbox-owned sprites (those whose name starts with `crabbox-` or
  that carry the `crabbox` / `lease-cbx-*` labels).
- `stop` deletes the sprite and removes the local claim and key.

## Gotchas

- An `--id` can be a Crabbox lease ID, a local slug, a `spr_<sprite-name>` ID, or
  a raw sprite name.
- A raw sprite that Crabbox did not create can only be adopted with `--reclaim`.
- If `sprite proxy` cannot connect, SSH readiness and execution fail even when
  the Sprites API can see the sprite. Plain `status` of a sprite that is not
  running still uses only the API.

## Related docs

- [Feature: Sprites](../features/sprites.md)
- [Provider backends](../provider-backends.md)
