# Hetzner Provider

Read when:

- choosing `provider: hetzner`;
- debugging Hetzner capacity, quotas, images, locations, or SSH readiness;
- changing `internal/providers/hetzner` or brokered Hetzner provisioning.

Hetzner is the Linux-only managed provider. It is an SSH lease backend: Hetzner
creates the server, then Crabbox owns SSH readiness, sync, command execution,
VNC tunnels, results, and cleanup.

## When To Use

Use Hetzner for fast Linux CI-style work when you do not need managed Windows,
macOS, or EC2-specific capacity controls. It is the simplest managed path for
Linux desktop/browser leases.

## Commands

```sh
crabbox warmup --provider hetzner --class beast
crabbox run --provider hetzner --class standard -- pnpm test
crabbox warmup --provider hetzner --desktop --browser
crabbox ssh --provider hetzner --id blue-lobster
crabbox stop --provider hetzner blue-lobster
```

## Config

```yaml
provider: hetzner
target: linux
class: beast
hetzner:
  image: ubuntu-24.04
  location: fsn1
  sshKey: ""
```

Important direct-mode environment:

```text
HCLOUD_TOKEN
HETZNER_TOKEN
CRABBOX_HETZNER_IMAGE
CRABBOX_HETZNER_LOCATION
CRABBOX_HETZNER_SSH_KEY
```

Brokered Hetzner credentials belong in the Worker.

## OS Selector

Crabbox accepts the portable Linux selector `--os ubuntu:26.04`, but Hetzner's
current public image catalog does not expose an Ubuntu 26.04 image slug. Until
that exists, `ubuntu:26.04` leases on Hetzner provision `ubuntu-24.04`. Use AWS,
GCP, Azure, or a container provider when proof must actually run on Ubuntu 26.04.

## Lifecycle

1. Import or reuse the lease SSH key.
2. Pick the configured location, image, and class server-type candidates.
3. Create a Hetzner server with Crabbox labels.
4. Wait for SSH and `crabbox-ready`.
5. Let core sync and run over SSH.
6. Delete on release, cleanup, or coordinator expiry.

## Classes

```text
standard  ccx33, cpx62, cx53
fast      ccx43, cpx62, cx53
large     ccx53, ccx43, cpx62, cx53
beast     ccx63, ccx53, ccx43, cpx62, cx53
```

Explicit `--type` is exact. Class-based provisioning can fall back across the
candidate list when Hetzner rejects capacity or quota.

## Capabilities

- SSH: yes.
- Crabbox sync: yes.
- Desktop/browser/code: Linux only.
- Tailscale: Linux managed leases.
- Actions hydration: yes, Linux SSH leases.
- Coordinator: yes.

## Gotchas

- Hetzner does not provide managed Windows or macOS targets in Crabbox.
- Dedicated-core types can hit account quota. Use class fallback before pinning
  exact types.
- Direct mode has no coordinator alarm; use `crabbox cleanup`.

Related docs:

- [Feature: Hetzner](../features/hetzner.md)
- [Linux VNC](../features/vnc-linux.md)
- [Provider backends](../provider-backends.md)
