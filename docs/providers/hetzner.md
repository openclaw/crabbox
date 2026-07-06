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
browser leases, code-server leases — when you do not need managed Windows or
macOS targets or cloud-specific capacity controls. For non-Linux targets or
provider-native snapshots, use [AWS](aws.md), [GCP](gcp.md), [Azure](azure.md),
or a container/sandbox provider instead.

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
- **Coordinator**: supported.

## Gotchas

- No managed Windows or macOS targets — Hetzner is Linux-only in Crabbox.
- Dedicated-core types (`ccx*`) can hit account quota. Prefer class fallback over
  pinning an exact `--type`.
- Direct mode has no coordinator alarm to reap expired boxes; run
  `crabbox cleanup --provider hetzner` (or `crabbox stop`) to release servers.

## Related docs

- [Provider backends](../provider-backends.md)
- [Linux VNC](../features/vnc-linux.md)
- [AWS](aws.md), [GCP](gcp.md), [Azure](azure.md)
