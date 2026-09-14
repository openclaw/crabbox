# Hetzner Provider

Read this when you are:

- choosing `provider: hetzner`;
- debugging Hetzner capacity, quotas, images, locations, or SSH readiness;
- changing `internal/providers/hetzner` or brokered Hetzner provisioning in the
  coordinator (`worker/src/hetzner.ts`).

Hetzner is the Linux-only managed provider and the simplest managed path for
Crabbox. It is an **SSH lease** backend: Hetzner Cloud provisions the server,
and Crabbox then owns SSH readiness, sync, command execution, VNC tunnels, test
results, and cleanup.

## When to use it

Reach for Hetzner for fast, low-overhead Linux work — CI-style runs, desktop and
browser leases, code-server leases, and direct project-snapshot checkpoints —
when you do not need managed Windows or macOS targets or cloud-specific capacity
controls.

Hetzner is one of the five brokerable providers: it runs **direct from the CLI**
by default and goes **through the coordinator** only when a coordinator URL and
token are configured. See [Provider backends](../provider-backends.md) for the
brokered-vs-direct model.

## Commands

```sh
crabbox warmup --provider hetzner --class beast
crabbox run --provider hetzner --class standard -- pnpm test
crabbox warmup --provider hetzner --desktop --browser
crabbox ssh --provider hetzner --id swift-crab
crabbox checkpoint create --provider hetzner --id swift-crab --mode native
crabbox stop --provider hetzner swift-crab
```

`--id` accepts either the canonical lease id (`cbx_…`) or the friendly slug.

## Configuration

```yaml
provider: hetzner
target: linux
class: beast
hetzner:
  image: ubuntu-24.04
  location: fsn1
  sshKey: ""
```

Config keys (under `hetzner:`):

| Key        | Maps to        | Default                       | Notes |
|------------|----------------|-------------------------------|-------|
| `location` | `cfg.Location` | `fsn1`                        | Hetzner datacenter location. |
| `image`    | `cfg.Image`    | resolved from `--os` selector | Hetzner image slug. |
| `sshKey`   | provider key   | per-lease key                 | Optional named Hetzner SSH key; otherwise Crabbox manages one. |

### Direct-mode environment

Direct mode authenticates from the environment:

```text
HCLOUD_TOKEN            Hetzner Cloud API token (preferred)
HETZNER_TOKEN           Alternate name; used if HCLOUD_TOKEN is unset
CRABBOX_HETZNER_IMAGE   Override the image slug
CRABBOX_HETZNER_LOCATION Override the location
CRABBOX_HETZNER_SSH_KEY  Use a named Hetzner SSH key
```

One of `HCLOUD_TOKEN` or `HETZNER_TOKEN` is required for direct mode; without it
provisioning fails fast. In brokered mode the API token lives in the Worker, not
on the client.

## OS selector

Crabbox accepts the portable Linux selector `--os` (default `ubuntu:26.04`, also
`ubuntu:24.04`). Hetzner's public image catalog does not expose an Ubuntu 26.04
slug yet, so **both** `ubuntu:26.04` and `ubuntu:24.04` currently resolve to the
Hetzner image `ubuntu-24.04`. If proof must actually run on Ubuntu 26.04, use
AWS, GCP, Azure, or a container provider, whose image maps already point at a
26.04 image.

## Lifecycle

1. Generate or reuse the per-lease SSH key; register it with Hetzner.
2. Pick the configured location, image, and the class's server-type candidates.
3. Create the server with Crabbox labels (with region/capacity fallback).
4. Wait for an IP, then for SSH and the `crabbox-ready` bootstrap marker.
5. Mark the server `state=ready` and hand off to core sync/run over SSH.
6. Delete the server (and managed SSH key) on release, `cleanup`, or — in
   brokered mode — coordinator expiry.

## Native checkpoints

Direct Linux leases with a real numeric Hetzner server ID can create
`hetzner-snapshot` checkpoints with `--mode native` or an explicit
`--strategy disk-snapshot`. The default `--mode auto --strategy auto` retains
the direct-provider archive fallback. `--strategy image` is rejected because
Hetzner's native primitive is a project snapshot.

Before creation Crabbox re-reads the server, verifies canonical remote labels
and the exact local lease claim, holds that claim fence while it runs
`cloud-init clean --logs; sync`, and starts snapshot creation. The local record
stores the numeric snapshot ID, actual source location and architecture, state,
and source server/lease/checkpoint binding metadata. `--wait=false` records the
creating state; wait failures retain a partial record once Hetzner has returned
the snapshot ID.

Verify and delete re-read the image and require `type=snapshot` plus every
recorded ownership and binding label before deletion. `checkpoint delete`
deletes the provider snapshot before local metadata; `--local-only` preserves
the project snapshot. `checkpoint fork` boots through the normal Hetzner create
path with the numeric snapshot ID, recorded location, and compatible
architecture. Explicit `--class` and `--type` fork overrides still apply.

Brokered Hetzner leases continue to use workspace archives. The coordinator
does not implement Hetzner snapshot creation or promotion. A direct
`crabbox image delete <snapshot-id> --provider hetzner` is available only when
exactly one local `hetzner-snapshot` record claims that ID; it reuses the same
remote verification and removes that record only after successful deletion.

Direct destructive operations require both canonical remote ownership labels
and the exact local claim bound to the Hetzner server ID. A weakly labeled,
unclaimed, or stale-claim server remains visible only through Hetzner's own
tools; Crabbox will not adopt it during cleanup. To recover intentionally lost
local state, first inspect the canonical server and explicitly reclaim it
through a normal reuse command before stopping it. Failed server or SSH-key
cleanup retains the claim for an exact retry.

## Classes and server types

Classes expand to an ordered list of Hetzner server types; provisioning tries
each in turn until one has capacity:

```text
tiny      ccx13, cpx22, cx23
small     ccx23, cpx32, cx33
standard  ccx33, cpx62, cx53
fast      ccx43, cpx62, cx53
large     ccx53, ccx43, cpx62, cx53
beast     ccx63, ccx53, ccx43, cpx62, cx53
```

The default class is `beast`. An explicit `--type` pins one exact server type
with no fallback; class-based provisioning falls back across the candidate list
when Hetzner reports a capacity or quota error.

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Desktop / browser / code**: yes, Linux-only (`--desktop`, `--browser`,
  `--code`). See [Linux VNC](../features/vnc-linux.md).
- **Tailscale**: yes on managed Linux leases. Direct `--tailscale` requires a
  Tailscale auth key in the configured `authKeyEnv`; brokered mode uses
  coordinator-side OAuth secrets.
- **Actions hydration**: yes (Linux SSH leases).
- **Cleanup**: yes.
- **Native checkpoint / fork**: yes for direct Linux leases; brokered leases use
  workspace archives.
- **Coordinator**: supported.

## Gotchas

Brokered release acknowledgement queues cleanup. A durable journal records
validated delete-action success plus exact server absence before owned key
cleanup, or a distinct already-absent observation before any known dispatch.
Inspect it with `crabbox inspect --json`; `cleanupStatus` remains the overall
completion signal. Pending actions and lost acknowledgements cannot be resolved
by GET 404 alone. Definitive key-only creation failures can retry the retained
owned key ID without a server receipt. See [cleanup confirmation](../features/lifecycle-cleanup.md#brokered-hetzner-cleanup-confirmation).

- No managed Windows or macOS targets — Hetzner is Linux-only in Crabbox.
- Dedicated-core types (`ccx*`) can hit account quota. Prefer class fallback over
  pinning an exact `--type`.
- Direct mode has no coordinator alarm to reap expired boxes; run
  `crabbox cleanup --provider hetzner` (or `crabbox stop`) to release servers.

## Related docs

- [Provider backends](../provider-backends.md)
- [Linux VNC](../features/vnc-linux.md)
- [AWS](aws.md), [GCP](gcp.md), [Azure](azure.md)
