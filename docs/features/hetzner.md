# Hetzner

Read when:

- choosing Hetzner as the Crabbox provider;
- debugging Hetzner capacity, quotas, images, or SSH readiness;
- changing Hetzner provisioning code in the CLI or Worker.

Hetzner is Crabbox's Linux-only managed provider. It creates Ubuntu servers,
labels them with Crabbox lease metadata, bootstraps the normal SSH/sync
contract, and optionally adds Linux desktop/browser capability.

## Targets

| Target | Managed | Notes |
| --- | --- | --- |
| Linux | Yes | Cloud-init bootstrap, SSH, rsync, optional desktop/browser. |
| Windows | No | Use AWS for managed Windows or `provider: ssh` for an existing Windows host. |
| macOS | No | Use AWS EC2 Mac or `provider: ssh` for an existing Mac. |

Examples:

```sh
crabbox warmup --provider hetzner --class beast
crabbox run --provider hetzner --class standard -- pnpm test
crabbox warmup --provider hetzner --desktop --browser
crabbox vnc --id blue-lobster --open
```

## Classes

```text
standard  ccx33, cpx62, cx53
fast      ccx43, cpx62, cx53
large     ccx53, ccx43, cpx62, cx53
beast     ccx63, ccx53, ccx43, cpx62, cx53
```

Dedicated-core types can hit account quota. Crabbox falls back through the
configured server types when Hetzner rejects a candidate for capacity or quota.
Explicit `--type` is exact and fails clearly when the type cannot be created.

## Broker Secrets And Env

Worker secret:

```text
HETZNER_TOKEN
```

Direct/provider env and config:

```text
HCLOUD_TOKEN
HETZNER_TOKEN
CRABBOX_HETZNER_IMAGE
CRABBOX_HETZNER_LOCATION
CRABBOX_HETZNER_SSH_KEY
```

## Desktop

Hetzner desktop leases use the Linux VNC path: Xvfb, a lightweight desktop
session, x11vnc bound to `127.0.0.1:5900`, and an SSH local tunnel created by
`crabbox vnc`. Hetzner does not manage Windows desktop boxes in Crabbox.

## Cleanup

Brokered cleanup belongs to the Durable Object alarm. Direct cleanup is
best-effort through provider labels and `crabbox cleanup`; it skips kept
machines and deletes expired direct-provider leftovers.

Related docs:

- [Providers](providers.md)
- [Linux VNC](vnc-linux.md)
- [Infrastructure](../infrastructure.md)
- [cleanup command](../commands/cleanup.md)
