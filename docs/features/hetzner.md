# Hetzner

Read this when you are:

- choosing Hetzner Cloud as the Crabbox provider;
- debugging Hetzner capacity, quotas, images, or SSH readiness;
- changing Hetzner provisioning code in the CLI or the coordinator.

Hetzner is a Linux-only managed provider. Crabbox creates a Hetzner Cloud
server, labels it with lease metadata (`crabbox=true` plus lease/slug/class and
related labels), waits for the standard SSH bootstrap contract, and optionally layers
on the desktop, browser, code, and Tailscale capabilities. It is one of the
four providers that can run **brokered** through the coordinator (alongside
`aws`, `azure`, and `gcp`); without a configured broker
it runs **direct** from the CLI against the Hetzner Cloud API.

## Targets

| Target  | Managed | Notes |
| ------- | ------- | ----- |
| Linux   | Yes     | Cloud-init bootstrap, SSH, rsync sync, optional desktop/browser/code/Tailscale. |
| Windows | No      | Use `aws` or `azure` for managed Windows, or `provider: ssh` for an existing Windows host. |
| macOS   | No      | Use `aws` (EC2 Mac), `parallels`, or `provider: ssh` for an existing Mac. |

Examples:

```sh
crabbox warmup --provider hetzner --class beast
crabbox run --provider hetzner --class standard -- pnpm test
crabbox warmup --provider hetzner --desktop --browser
crabbox vnc --id blue-lobster --open
```

## Server classes and types

`--class` selects a list of candidate server types. Crabbox tries them in order
and falls back to the next when Hetzner rejects a candidate for quota or
capacity (`dedicated_core_limit`, `resource_limit_exceeded`,
`server_type_not_available`, `location_not_available`). The default class is
`beast`.

```text
standard  ccx33, cpx62, cx53
fast      ccx43, cpx62, cx53
large     ccx53, ccx43, cpx62, cx53
beast     ccx63, ccx53, ccx43, cpx62, cx53
```

`--type` pins an explicit server type. It is tried first, but Crabbox still
falls back to the rest of the class candidates if that type is rejected for
quota or capacity, so a busy region does not strand a lease. Dedicated-core
types (the `ccx*` family) are the most likely to hit account quota.

## Image and location

The OS image follows `--os` (default `ubuntu:26.04`), mapped to the Hetzner
image name. Both `ubuntu:26.04` and `ubuntu:24.04` resolve to the
`ubuntu-24.04` Hetzner image today. The default location is `fsn1`.

Override the image, location, or a preexisting Hetzner SSH key via config or
environment variables (see below). Crabbox otherwise generates and uploads a
per-lease SSH key automatically.

## Credentials and configuration

**Direct mode** reads the API token from the environment:

```text
HCLOUD_TOKEN     # preferred
HETZNER_TOKEN    # fallback
```

One of these is required for any direct Hetzner operation.

**Brokered mode** stores the token as a Worker secret named `HETZNER_TOKEN`;
the CLI never sees it. Lease lifecycle calls go through the broker, while SSH,
rsync, and command execution still run directly from the CLI to the runner
host.

Optional config keys (file section `hetzner:` or `CRABBOX_HETZNER_*`
environment variables):

```text
CRABBOX_HETZNER_LOCATION   # hetzner.location  (default fsn1)
CRABBOX_HETZNER_IMAGE      # hetzner.image     (default per --os)
CRABBOX_HETZNER_SSH_KEY    # hetzner.sshKey    (reuse a named Hetzner key)
```

## Capabilities

The Hetzner adapter advertises `ssh`, `crabbox-sync`, `cleanup`, `desktop`,
`browser`, `code`, and `tailscale`.

- `--desktop` / `--browser` use the Linux VNC path: Xvfb, a lightweight desktop
  session, and x11vnc bound to `127.0.0.1:5900`. `crabbox vnc` opens an SSH
  local tunnel to it. See [Linux VNC](vnc-linux.md).
- `--code` provisions code-server, bridgeable into the portal with
  `crabbox code`.
- `--tailscale` joins the lease to a tailnet. In direct mode this requires an
  auth key in the configured `--tailscale-auth-key-env` variable; brokered mode
  uses the coordinator's OAuth secrets. See [Tailscale](tailscale.md).

## Cleanup

In brokered mode, expiry and teardown are owned by the coordinator's Durable
Object alarm. In direct mode, cleanup is best-effort through the `crabbox=true`
provider labels: `crabbox cleanup --provider hetzner` deletes expired
direct-provider servers and skips machines that were kept.

## Related docs

- [Providers](providers.md)
- [Capabilities](capabilities.md)
- [Linux VNC](vnc-linux.md)
- [Tailscale](tailscale.md)
- [Infrastructure](../infrastructure.md)
- [cleanup command](../commands/cleanup.md)
