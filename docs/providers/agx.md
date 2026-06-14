# AGX Provider

Read this when you:

- pick `provider: agx`;
- configure an AGX API key, workspace gateway, or work root;
- change `internal/providers/agx`.

AGX ([agx.so](https://www.agx.so)) runs fast-booting microVM sandboxes that
expose plain SSH through a workspace gateway — `ssh <user>+<instance>@workspace.agx.so`.
Crabbox creates an instance through the AGX control plane with a Crabbox-managed
per-lease SSH key, then reaches it over SSH at the gateway. From there
everything is the standard Crabbox SSH flow: Crabbox owns slugs, per-repo
claims, per-lease SSH keys, rsync sync, command execution, and `list`/`status`
rendering.

> AGX is early access (shipping Summer 2026) and does not yet publish a stable
> control-plane contract. The instance lifecycle API modeled here
> (`/v1/instances`) is provisional and may change as AGX stabilizes; the SSH
> transport (`<user>+<instance>@<workspace>`) follows AGX's documented
> connection shape. Override `--agx-api-url` / `--agx-workspace` to track the
> published endpoints.

## When to use

Reach for AGX when you want a quick, disposable Linux microVM with normal
Crabbox sync-and-run behavior and no infrastructure of your own. Choose AWS,
Azure, GCP, or Hetzner instead when you need brokered fleet accounting, a
desktop or VNC, code-server, provider firewall control, or cloud images — AGX
supports none of those today.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes (AGX workspace gateway) |
| Crabbox sync (rsync) | Yes |
| Actions hydration | Yes (Linux SSH target) |
| Orphan cleanup | Yes (`crabbox cleanup`) |
| Desktop / browser / code | No |
| Tailscale | No (SSH is exposed through the AGX gateway) |
| Coordinator (broker) | No (always direct from the CLI) |

`--class`, `--type`, and `--tailscale` are rejected: AGX owns VM sizing and
exposes SSH only through its gateway. Only `target=linux` is accepted.

## Auth

Crabbox needs an AGX API key. Keep it in the environment; never pass it as a
command-line argument and never persist it to a config file.

```sh
export AGX_API_KEY=...
```

Key lookup, in priority order:

1. `CRABBOX_AGX_API_KEY`
2. `AGX_API_KEY`
3. `AGX_TOKEN`

A missing key fails the lease before any API call. The key is sent only in the
`Authorization: Bearer` header.

## Configuration

```yaml
provider: agx
target: linux
agx:
  apiUrl: https://api.agx.so
  workspace: workspace.agx.so
  user: root
  workRoot: /root/crabbox
  region: ""
  image: ""
```

Defaults: API URL `https://api.agx.so`, workspace gateway `workspace.agx.so`,
in-VM SSH user `root`, work root `/root/crabbox`.

Flags:

- `--agx-api-url` — AGX control-plane API URL.
- `--agx-workspace` — AGX SSH workspace gateway host.
- `--agx-user` — in-VM SSH login user (the `<user>` half of `<user>+<instance>`).
- `--agx-work-root` — remote work root.
- `--agx-region` — AGX region (optional).
- `--agx-image` — AGX base image or snapshot (optional).

Environment variables:

```text
CRABBOX_AGX_API_KEY
AGX_API_KEY
AGX_TOKEN
CRABBOX_AGX_API_URL / AGX_API_URL
CRABBOX_AGX_WORKSPACE / AGX_WORKSPACE
CRABBOX_AGX_USER / AGX_USER
CRABBOX_AGX_WORK_ROOT
CRABBOX_AGX_REGION / AGX_REGION
CRABBOX_AGX_IMAGE / AGX_IMAGE
```

The work root must be a dedicated absolute path; broad roots such as `/`,
`/home`, `/root`, `/tmp`, `/etc`, `/usr`, `/var`, and similar system directories
are rejected before sync.

## Commands

```sh
crabbox warmup --provider agx
crabbox run --provider agx -- pnpm test
crabbox ssh --provider agx --id swift-crab
crabbox status --provider agx --id swift-crab
crabbox stop --provider agx swift-crab
crabbox list --provider agx
crabbox cleanup --provider agx --dry-run
```

## Lifecycle

1. Generate a per-lease Crabbox SSH key.
2. Create an AGX instance named `crabbox-<slug>` through the control plane,
   registering the public key and labeling it `crabbox`, `provider=agx`,
   `crabbox-lease=<lease-id>`, and `crabbox-slug=<slug>`.
3. Build an SSH target for `<user>+<instance>@<workspace>` and wait until SSH is
   ready.
4. Crabbox syncs the checkout and runs commands over SSH.
5. On release, delete the instance and remove the local claim and key (unless the
   lease is kept). `crabbox cleanup` deletes only expired Crabbox-labeled
   instances and honors `--dry-run`.

## Live smoke

Run a live smoke when you change AGX lifecycle, the SSH gateway mapping, or
cleanup behavior.

```sh
export AGX_API_KEY=...
go build -trimpath -o bin/crabbox ./cmd/crabbox

bin/crabbox warmup --provider agx --timing-json
lease=<slug-or-cbx_id-from-warmup-output>

bin/crabbox status --provider agx --id "$lease" --wait
bin/crabbox ssh --provider agx --id "$lease"
bin/crabbox run --provider agx --id "$lease" --shell 'echo crabbox-agx-ok'
bin/crabbox list --provider agx
bin/crabbox stop --provider agx "$lease"
```

Expected results:

- `warmup` creates a `crabbox-<slug>` instance and prints `provider=agx`, a
  Crabbox lease ID, a slug, and the instance id.
- `status --wait` reports a running Linux lease.
- `ssh` prints a command whose user is `<user>+<instance>` against the workspace
  gateway.
- `run` prints `crabbox-agx-ok`.
- `list` shows Crabbox-owned instances (those carrying the `crabbox` /
  `created_by=crabbox` labels).
- `stop` deletes the instance and removes the local claim and key.

## Gotchas

- An `--id` can be a Crabbox lease ID, a local slug, or a raw AGX instance id.
- A raw instance that Crabbox did not create can only be adopted with
  `--reclaim`.
- SSH depends on the AGX gateway: if the gateway cannot route to the instance,
  `status --wait`, `run`, and `ssh` fail even when the control plane can still
  see the instance.

## Related docs

- [Provider backends](../provider-backends.md)
- [Authoring a provider](../features/provider-authoring.md)
