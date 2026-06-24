# AGX Provider

Read this when you:

- pick `provider: agx`;
- configure the AGX workspace gateway, in-VM user, or work root;
- change `internal/providers/agx`.

AGX ([agx.so](https://www.agx.so)) runs fast-booting microVM sandboxes that you
reach over plain SSH through a workspace gateway —
`ssh <user>+<instance>@workspace.agx.so`. AGX's stated design is *"no SDK
required, no custom client — if it can ssh, it can work on AGX,"* so this
provider is **SSH-only**: it does not call any AGX control-plane API. Crabbox
builds the `<user>+<instance>` SSH target with your own SSH key, lets AGX
provision the microVM on connect, and then runs the standard Crabbox SSH flow —
slugs, per-repo claims, rsync sync, command execution, and `list`/`status`
rendering.

> **Early access.** AGX ships Summer 2026 and currently publishes no control-plane
> API, authentication contract, or CLI — only the SSH connection shape above and
> the "no SDK / no custom client" model. This provider commits only to that
> documented SSH interface. Auth therefore uses **your own SSH key** (the one you
> register with AGX during onboarding), `<instance>` is named after the Crabbox
> slug, and there is **no remote list or cleanup** (Crabbox tracks leases through
> local claims). When AGX publishes a management API, provisioning and cleanup
> can be revisited. Built on Loophole Labs' Firecracker/CRIU/[Drafter](https://github.com/loopholelabs/drafter)
> microVM stack.

## When to use

Reach for AGX when you want a quick, disposable Linux microVM with normal
Crabbox sync-and-run behavior and no infrastructure of your own, and you already
have an AGX-registered SSH key. Choose AWS, Azure, GCP, or Hetzner instead when
you need brokered fleet accounting, remote inventory and cleanup, a desktop or
VNC, code-server, provider firewall control, or cloud images — AGX exposes none
of those today.

## Capabilities

| Capability | Supported |
| --- | --- |
| OS targets | Linux only |
| SSH | Yes (AGX workspace gateway, your own key) |
| Crabbox sync (rsync) | Yes |
| Actions hydration | Yes (Linux SSH target) |
| Remote list / orphan cleanup | No (no AGX control-plane API; leases tracked via local claims) |
| Desktop / browser / code | No |
| Tailscale | No (SSH is exposed through the AGX gateway) |
| Coordinator (broker) | No (always direct from the CLI) |

`--class`, `--type`, and `--tailscale` are rejected. Only `target=linux` is
accepted.

## Auth

AGX authenticates SSH connections with **your own SSH key**, registered with AGX
during early-access onboarding. There is no API token. Crabbox uses the SSH key
from its standard configuration (`--ssh-key` / `ssh.key` in config / your SSH
agent), exactly as the Static SSH provider does. Crabbox does not mint a
per-lease key for AGX, because AGX would not trust an unregistered key.

## Configuration

```yaml
provider: agx
target: linux
ssh:
  key: ~/.ssh/id_ed25519   # the key you registered with AGX
agx:
  workspace: workspace.agx.so
  user: root
  workRoot: /root/crabbox
```

Defaults: workspace gateway `workspace.agx.so`, in-VM SSH user `root`, work root
`/root/crabbox`.

Flags:

- `--agx-workspace` — AGX SSH workspace gateway host.
- `--agx-user` — in-VM SSH login user (the `<user>` half of `<user>+<instance>`).
- `--agx-work-root` — remote work root.

Environment variables:

```text
CRABBOX_AGX_WORKSPACE / AGX_WORKSPACE
CRABBOX_AGX_USER / AGX_USER
CRABBOX_AGX_WORK_ROOT
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
```

For redacted PR evidence or maintainer validation, run the shared live-smoke
harness from a repo checkout on a machine with an AGX-registered SSH key:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=agx CRABBOX_LIVE_COORDINATOR=0 \
  CRABBOX_LIVE_REPO=/path/to/my-app scripts/live-smoke.sh
```

That path runs `doctor`, `warmup`, `status --wait`, `ssh` command rendering,
cache diagnostics, one synced command, recent history/log capture, and
`stop`. Because AGX has no remote inventory or delete API, successful proof
should show the local release message and the AGX gateway reclaim behavior
expected for idle sandboxes.

## Lifecycle

1. Allocate a Crabbox lease ID and slug; the slug becomes the AGX `<instance>`
   name (stable, so the lease reconnects to the same address).
2. Build the SSH target `<user>+<instance>@<workspace>` using your configured
   SSH key.
3. Connect; AGX provisions the microVM on connect (sub-second) and Crabbox waits
   until SSH is ready.
4. Record a local lease claim with the resolved endpoint.
5. Crabbox syncs the checkout and runs commands over SSH.
6. On release, Crabbox removes the local claim. There is no remote delete call;
   AGX reclaims idle sandboxes and persists critical state out of band.

## Gotchas

- An `--id` can be a Crabbox lease ID, a local slug, or an AGX instance name. If
  no local claim matches, Crabbox treats the id as an instance name and connects
  directly.
- `crabbox list` shows only leases this machine has claimed locally — there is no
  AGX inventory API to enumerate instances.
- SSH depends on the AGX gateway and on your SSH key being registered with AGX;
  an unregistered key fails authentication at the gateway.

## Related docs

- [Provider backends](../provider-backends.md)
- [Authoring a provider](../features/provider-authoring.md)
- [Static SSH provider](ssh.md) (the closest BYO-key model)
