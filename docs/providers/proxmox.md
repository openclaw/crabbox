# Proxmox Provider

Read when:

- choosing `provider: proxmox`;
- setting up a Proxmox VE VM template for Crabbox;
- debugging Proxmox API tokens, clone tasks, guest agent IP discovery, or cleanup;
- changing `internal/providers/proxmox` or `internal/cli/proxmox.go`.

Proxmox is a direct SSH lease provider for Linux QEMU VMs. Crabbox clones a
configured Proxmox template, injects a per-lease SSH key through cloud-init,
uses the QEMU guest agent to discover the VM IP and run the Crabbox bootstrap,
then uses the normal SSH sync/run/release path.

It is direct-only today. The Crabbox coordinator does not broker Proxmox
credentials or capacity yet.

## When To Use

Use Proxmox when:

- your test capacity lives on a private Proxmox VE cluster;
- you want reusable local or lab infrastructure instead of public cloud VMs;
- your template already supports cloud-init and the QEMU guest agent.

Use AWS, Azure, Google Cloud, or Hetzner when you need brokered shared-team
leases. Use Static SSH when you already have a running host and do not want
Crabbox to clone or delete VMs.

Provider name:

```text
proxmox
```

## Template Requirements

The configured template must be a Linux QEMU VM template with:

- cloud-init drive configured;
- QEMU guest agent installed and enabled;
- DHCP networking or equivalent IP config;
- outbound package access for `apt-get`;
- enough permissions for the guest agent to run bootstrap commands as root.

Crabbox sets:

- `ciuser` to `proxmox.user` or `CRABBOX_PROXMOX_USER`;
- `sshkeys` to the generated per-lease public key;
- `ipconfig0=ip=dhcp`;
- `agent=enabled=1`;
- `description` with Crabbox lease labels for list/touch/cleanup;
- `tags=crabbox`.

If the guest agent does not report an IPv4 address or cannot execute the
bootstrap script, provisioning fails and the cloned VM is deleted.

## Quick Start

Create an API token in Proxmox and give it permission to clone, configure,
start, stop, delete, and inspect VMs on the target node/storage.

```sh
export CRABBOX_PROXMOX_API_URL=https://pve.example.test:8006
export CRABBOX_PROXMOX_TOKEN_ID='crabbox@pve!ci'
export CRABBOX_PROXMOX_TOKEN_SECRET='<token-secret>'
export CRABBOX_PROXMOX_NODE=pve1
export CRABBOX_PROXMOX_TEMPLATE_ID=9000

crabbox warmup --provider proxmox --keep
crabbox run --provider proxmox --no-sync -- echo proxmox-ok
crabbox ssh --provider proxmox --id blue-crab
crabbox stop --provider proxmox blue-crab
crabbox cleanup --provider proxmox
```

For self-signed private clusters, set `CRABBOX_PROXMOX_INSECURE_TLS=1` or pass
`--proxmox-insecure-tls`.

## Config

```yaml
provider: proxmox
target: linux
proxmox:
  apiUrl: https://pve.example.test:8006
  tokenId: crabbox@pve!ci
  tokenSecret: <token-secret>
  node: pve1
  templateId: 9000
  storage: local-lvm
  pool: crabbox
  bridge: vmbr0
  user: crabbox
  workRoot: /work/crabbox
  fullClone: true
  insecureTLS: false
```

Secret values should live in `~/.profile`, a private config file with `0600`
permissions, or your shell secret manager. Avoid passing token secrets as CLI
arguments.

Environment:

```text
CRABBOX_PROXMOX_API_URL
CRABBOX_PROXMOX_TOKEN_ID
CRABBOX_PROXMOX_TOKEN_SECRET
CRABBOX_PROXMOX_NODE
CRABBOX_PROXMOX_TEMPLATE_ID
CRABBOX_PROXMOX_STORAGE
CRABBOX_PROXMOX_POOL
CRABBOX_PROXMOX_BRIDGE
CRABBOX_PROXMOX_USER
CRABBOX_PROXMOX_WORK_ROOT
CRABBOX_PROXMOX_FULL_CLONE
CRABBOX_PROXMOX_INSECURE_TLS
```

Provider flags mirror the non-secret config fields:

```text
--proxmox-api-url
--proxmox-node
--proxmox-template-id
--proxmox-storage
--proxmox-pool
--proxmox-bridge
--proxmox-user
--proxmox-work-root
--proxmox-full-clone
--proxmox-insecure-tls
```

`--proxmox-token-secret` is intentionally not a flag so secrets do not appear in
shell history or process argv.

## Lifecycle

1. Allocate a Crabbox lease ID and friendly slug.
2. Ask Proxmox for the next VMID.
3. Clone `proxmox.templateId` on `proxmox.node`.
4. Configure cloud-init SSH user/key, DHCP, optional bridge/storage/pool, and
   Crabbox labels in the VM description.
5. Start the VM and wait for the QEMU guest agent.
6. Discover the first non-loopback IPv4 address from the guest agent.
7. Run the Crabbox Linux bootstrap through guest-agent exec.
8. Wait for SSH and `/usr/local/bin/crabbox-ready`.
9. Touch labels during runs and delete the VM on release unless the lease is kept.

Cleanup reads Crabbox labels from VM descriptions and only deletes expired
Crabbox-managed VMs.

## Troubleshooting

`proxmox tokenId/tokenSecret are required`

Set `CRABBOX_PROXMOX_TOKEN_ID` and `CRABBOX_PROXMOX_TOKEN_SECRET`, or put them
under `proxmox:` in a private Crabbox config file.

`timeout waiting for proxmox qemu guest agent`

Install and enable `qemu-guest-agent` in the template, then shut down and convert
the VM to a template again.

`no guest ipv4 address reported by qemu guest agent`

Check DHCP, bridge selection, VLANs, and whether the guest agent can see the
interface. If you override `--proxmox-bridge`, make sure the template NIC can
boot on that bridge.

`proxmox guest bootstrap exit=...`

The VM booted and the guest agent ran, but apt/bootstrap failed. SSH into a kept
lease if available, or check the Proxmox task and guest logs.

## Related

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Providers overview](../features/providers.md)
