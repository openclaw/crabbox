# DigitalOcean Provider

Read this when you are:

- choosing `provider: digitalocean`;
- validating a direct DigitalOcean Droplet lease;
- changing `internal/providers/digitalocean` or the guarded live smoke.

DigitalOcean is a Linux-only **SSH lease** provider. Crabbox creates a Droplet,
injects a per-lease SSH key, writes Crabbox ownership metadata as namespaced
DigitalOcean tags, waits for SSH/bootstrap readiness, and then uses the normal
Crabbox SSH sync/run/stop/cleanup path.

DigitalOcean is **direct-only** in this release. It does not run through the
coordinator, so the local CLI must have a DigitalOcean API token
and direct cleanup remains the operator's responsibility.

## When To Use It

Use DigitalOcean for simple Linux lease work when a Droplet is the desired
execution surface and direct local credentials are acceptable. Prefer AWS,
Azure, GCP, or Hetzner when you need a brokered team path, coordinator-side
credentials, or cloud-specific capacity and cost accounting.

## Commands

```sh
crabbox warmup --provider digitalocean --class standard
crabbox run --provider digitalocean --type s-1vcpu-1gb -- pnpm test
crabbox ssh --provider digitalocean --id my-app
crabbox stop --provider digitalocean my-app
crabbox cleanup --provider digitalocean --dry-run
```

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the
numeric DigitalOcean Droplet id. `--type` is the exact DigitalOcean Droplet size
slug; there is no separate DigitalOcean size flag.

## Configuration

```yaml
provider: digitalocean
target: linux
class: standard
digitalocean:
  region: nyc3
  image: ubuntu-24-04-x64
  vpc: ""
  sshCIDRs: []
```

Config keys under `digitalocean:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `region` | `cfg.DigitalOcean.Region` | `nyc3` | DigitalOcean region slug. |
| `image` | `cfg.DigitalOcean.Image` | `ubuntu-24-04-x64` | Droplet image slug. |
| `vpc` | `cfg.DigitalOcean.VPCUUID` | empty | Optional VPC UUID for Droplet placement. |
| `sshCIDRs` | `cfg.DigitalOcean.SSHCIDRs` | empty | Reserved for firewall-aware follow-up work; Phase 1 does not create firewalls. |

The portable `--os ubuntu:24.04` selector maps to `ubuntu-24-04-x64`.
DigitalOcean does not currently offer the portable default Ubuntu 26.04 image,
so provisioning with an explicit `--os ubuntu:26.04` is rejected unless
`digitalocean.image` or `CRABBOX_DIGITALOCEAN_IMAGE` provides an explicit image
slug. Validation occurs when Crabbox acquires a new Droplet, after CLI
overrides; `config show`, provider overrides, and cleanup commands remain
available when the configured portable selector is unsupported.

DigitalOcean leases default to `root` on SSH port `22` with no fallback port.
Explicit generic `ssh.user` and `ssh.port` values remain authoritative. The
effective values appear in `crabbox config show` without retaining defaults
from another provider when a command overrides the provider.

Environment overrides:

```text
DIGITALOCEAN_TOKEN                 DigitalOcean API token for direct mode
CRABBOX_DIGITALOCEAN_REGION        Override the region slug
CRABBOX_DIGITALOCEAN_IMAGE         Override the image slug
CRABBOX_DIGITALOCEAN_VPC           Override the VPC UUID
CRABBOX_DIGITALOCEAN_SSH_CIDRS     Comma-separated SSH CIDRs, reserved for firewall follow-up
```

Do not pass the DigitalOcean token as a command-line argument. Keep it in the
environment or in a local secret manager.

## Token Scopes

The Phase 1 provider uses Droplets, SSH keys, and tags. A custom DigitalOcean
token needs at least:

```text
account:read
droplet:read
droplet:create
droplet:delete
droplet:update
regions:read
sizes:read
actions:read
image:read
ssh_key:read
ssh_key:create
ssh_key:delete
tag:read
tag:create
tag:delete
```

DigitalOcean lists `regions:read`, `sizes:read`, `actions:read`, and
`image:read` as required dependencies of the Droplet read/create scopes.
Crabbox uses `account:read` to bind local cleanup claims to the creating
DigitalOcean user or team and refuses claim-only deletion after an account
switch.
If a live smoke fails with a permission error, keep the error output secret-safe
and adjust token scopes before retrying. Add `vpc:read` when selecting an
explicit VPC. Do not broaden scopes inside scripts.

## Lifecycle

1. Generate a per-lease SSH key under the Crabbox testbox key directory.
2. Create or reuse the matching DigitalOcean account SSH key.
3. Create a Droplet with `region`, `image`, `size`, SSH key, cloud-init
   `user_data`, and Crabbox tags.
4. Wait for a public IPv4 address and Crabbox SSH bootstrap readiness.
5. Add ready-state Crabbox tags and claim the lease locally.
6. Run normal Crabbox sync/run/ssh workflows over SSH.
7. Delete the Droplet and managed SSH key on `stop`; `cleanup` deletes only
   resources with complete Crabbox DigitalOcean ownership tags and an exact
   account- and Droplet-bound local claim. Failed post-delete or key-only
   rollback cleanup retains that claim so `stop` can retry it.

If Droplet creation returns an indeterminate transport or server failure,
Crabbox retains the SSH credentials and records a pending local recovery claim.
`crabbox stop --provider digitalocean <lease-or-slug>` reconciles that claim and
deletes a late-created Droplet when DigitalOcean exposes it. Empty inventory is
not treated as proof that creation failed: while the outcome remains
indeterminate, Crabbox retains the claim and credentials and asks the operator
to retry rather than risk orphaning a billed Droplet without its SSH key.

## Ownership And Cleanup

DigitalOcean tags are flat strings, not key/value labels. Crabbox encodes owned
leases as tags such as:

```text
crabbox
crabbox:provider:digitalocean
crabbox:lease:cbx_abcdef123456
crabbox:slug:my-app
crabbox:target:linux
crabbox:expires_at:<unix-seconds>
```

Read-only inventory requires a complete ownership predicate: Crabbox marker,
provider marker, canonical lease id, slug, and Linux target. Reuse and deletion
also require an exact local claim for the same DigitalOcean account, lease, and
Droplet id. Droplets with partial, foreign, malformed, claimless, or mismatched
ownership are skipped or refused; a claimless Droplet must first be adopted
through explicit supported `--reclaim` reuse.

Tag updates apply only the set difference. Crabbox detaches obsolete tags from
the Droplet but does not delete account-level tag objects: DigitalOcean tag
deletion can also untag images, volumes, snapshots, and databases that the token
cannot necessarily inspect. Unused Crabbox tag objects may therefore remain in
the account.

Direct mode has no coordinator alarm. Use:

```sh
crabbox list --provider digitalocean --json
crabbox cleanup --provider digitalocean --dry-run
crabbox cleanup --provider digitalocean
```

## Guarded Live Smoke

The repeatable live check is opt-in:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=digitalocean scripts/live-smoke.sh
```

The top-level smoke dispatches to the provider-specific script:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=digitalocean scripts/live-digitalocean-smoke.sh
```

The script builds `bin/crabbox` unless `CRABBOX_BIN` points at an existing
binary, uses `DIGITALOCEAN_TOKEN` from the environment,
requires an empty Crabbox-owned DigitalOcean inventory, creates a small
`s-1vcpu-1gb` Droplet, waits for ready status, runs `echo ok`, verifies
`list --json`, stops the lease, runs dry-run cleanup, and verifies the
Crabbox-owned inventory is empty afterward.

Final classifications include:

```text
classification=live_digitalocean_smoke_passed
classification=environment_blocked
classification=quota_blocked
classification=validation_failed
```

If cleanup fails, use the reported slug and Crabbox tags to inspect the Droplet
in `crabbox list --provider digitalocean --json` or the DigitalOcean console.

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Tailscale**: yes through the standard Linux cloud-init path when a direct
  Tailscale auth key is configured.
- **Desktop / browser / code**: not advertised in Phase 1.
- **Cleanup**: yes, tag-owned only.
- **Coordinator**: never; direct CLI only.

## Gotchas

- `digitalocean` is direct-only. Coordinator secrets and cost accounting do
  not cover these Droplets.
- `--type` must be a valid Droplet size slug such as `s-1vcpu-1gb`.
- Crabbox decodes the highest-priority state when stale tags coexist, but
  cleanup relies on `expires_at` and ownership tags, not tag order.
- Phase 1 does not create firewalls. Restrict SSH exposure through account/VPC
  policy where needed, and prefer short TTLs for live validation.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Operations](../operations.md)
