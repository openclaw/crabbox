# Linode Provider

Read this when you are:

- choosing `provider: linode`;
- validating a direct Linode SSH lease;
- changing `internal/providers/linode` or the guarded live smoke.

Linode is a Linux-only **SSH lease** provider. Crabbox creates a Linode
instance, injects a per-lease SSH key through Linode metadata/cloud-init, writes
Crabbox ownership metadata as namespaced Linode tags, waits for SSH/bootstrap
readiness, and then uses the normal Crabbox SSH sync/run/stop/cleanup path.

Linode is **direct-only** in this release. It does not run through the
coordinator, so the local CLI must have a Linode API token and
direct cleanup remains the operator's responsibility.

## When To Use It

Use Linode for simple Linux lease work when a Linode instance is the desired
execution surface and direct local credentials are acceptable. Prefer AWS,
Azure, GCP, or Hetzner when you need a brokered team path, coordinator-side
credentials, or cloud-specific capacity and cost accounting.

## Commands

```sh
crabbox warmup --provider linode --class standard
crabbox run --provider linode --type g6-standard-1 -- pnpm test
crabbox ssh --provider linode --id my-app
crabbox stop --provider linode my-app
crabbox cleanup --provider linode --dry-run
```

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the
numeric Linode instance id. `--type` is the exact Linode type slug; there is no
separate Linode size flag.

## Configuration

```yaml
provider: linode
target: linux
class: standard
linode:
  region: us-ord
  image: linode/ubuntu24.04
  type: g6-standard-1
  firewall: ""
  sshCIDRs: []
```

Config keys under `linode:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `region` | `cfg.Linode.Region` | `us-ord` | Linode region slug. |
| `image` | `cfg.Linode.Image` | `linode/ubuntu24.04` | Linode image slug. |
| `type` | `cfg.Linode.Type` | `g6-standard-1` | Linode instance type slug. |
| `firewall` | `cfg.Linode.FirewallID` | empty | Optional numeric existing Linode firewall id to attach at create time. |
| `sshCIDRs` | `cfg.Linode.SSHCIDRs` | empty | Reserved for firewall-aware follow-up work; Phase 1 does not create firewall rules. |

The portable `--os ubuntu:24.04` selector maps to `linode/ubuntu24.04`. Linode
does not currently offer the portable default Ubuntu 26.04 image in this
provider, so provisioning with an explicit `--os ubuntu:26.04` is rejected
unless `linode.image` or `CRABBOX_LINODE_IMAGE` provides an explicit image slug.
Validation occurs when Crabbox acquires a new Linode, after CLI overrides;
`config show`, provider overrides, and cleanup commands remain available when
the configured portable selector is unsupported.

Linode leases default to `root` on SSH port `22` with no fallback port. Explicit
generic `ssh.user` and `ssh.port` values remain authoritative. The effective
values appear in `crabbox config show` without retaining defaults from another
provider.

Environment overrides:

```text
LINODE_TOKEN                 Linode API token for direct mode
CRABBOX_LINODE_REGION        Override the region slug
CRABBOX_LINODE_IMAGE         Override the image slug
CRABBOX_LINODE_TYPE          Override the instance type slug
CRABBOX_LINODE_FIREWALL      Optional numeric existing firewall id
CRABBOX_LINODE_SSH_CIDRS     Comma-separated SSH CIDRs, reserved for firewall follow-up
```

Do not pass the Linode token as a command-line argument. Keep it in the
environment or in a local secret manager.

## Token Scopes

The Phase 1 provider uses account identity, Linode instances, tags, metadata
user-data, and optional existing firewalls. A custom Linode token needs at
least:

```text
account:read_only
linodes:read_write
```

Add firewall access when using `linode.firewall` /
`CRABBOX_LINODE_FIREWALL`. Crabbox uses account identity to bind local cleanup
claims to the creating Linode account's stable EUUID and refuses claim-only
deletion after an account switch.

If a live smoke fails with a permission error, keep the error output secret-safe
and adjust token scopes before retrying. Do not broaden scopes inside scripts.

## Lifecycle

1. Generate a per-lease SSH key under the Crabbox testbox key directory.
2. Create a Linode instance with `region`, `image`, `type`, authorized SSH key,
   cloud-init `user_data`, optional existing `firewall`, and Crabbox tags.
3. Wait for a public IPv4 address and Crabbox SSH bootstrap readiness.
4. Add ready-state Crabbox tags and claim the lease locally.
5. Run normal Crabbox sync/run/ssh workflows over SSH.
6. Delete the Linode instance on `stop`; `cleanup` deletes only resources with a
   complete Crabbox Linode ownership tag set.

If instance creation returns an indeterminate transport or server failure,
Crabbox retains the SSH credentials and records a pending local recovery claim.
`crabbox stop --provider linode <lease-or-slug>` reconciles that claim and
deletes a late-created Linode when the API exposes it. Empty inventory is not
treated as proof that creation failed while the outcome remains indeterminate:
Crabbox retains the claim and credentials and asks the operator to retry rather
than risk orphaning a billed instance without its SSH key.

## Ownership And Cleanup

Crabbox encodes owned leases with Linode tags such as:

```text
crabbox
crabbox:provider:linode
crabbox:lease:cbx_abcdef123456
crabbox:slug:my-app
crabbox:target:linux
crabbox:expires_at:<unix-seconds>
```

Release and cleanup require a complete ownership predicate: Crabbox marker,
provider marker, lease id, slug, and Linux target. Linodes with partial,
foreign, or malformed Crabbox-like tags are skipped/refused.

Tag updates replace only Crabbox's namespaced tags and preserve unrelated
operator tags already attached to the instance. Direct mode has no coordinator
alarm. Use:

```sh
crabbox list --provider linode --json
crabbox cleanup --provider linode --dry-run
crabbox cleanup --provider linode
```

## Firewall Scope

Phase 1 does not create or manage Linode firewall rules. If `linode.firewall`
or `CRABBOX_LINODE_FIREWALL` is set to a numeric firewall id, Crabbox attaches
that existing firewall when creating the instance. Crabbox reads the account's
new-Linode interface setting and attaches the firewall either to the legacy
Linode or its public Linode interface as required. Operators own the firewall
policy, including SSH source restrictions.

Use account-default firewall policy, an existing firewall, short TTLs, and
cleanup dry-runs for validation. `linode.sshCIDRs` and
`CRABBOX_LINODE_SSH_CIDRS` are reserved for follow-up firewall work and do not
create ingress rules in this phase.

## Guarded Live Smoke

The repeatable live check is opt-in:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=linode scripts/live-linode-smoke.sh
```

The script builds `bin/crabbox`, uses `LINODE_TOKEN` from the environment,
requires an empty Crabbox-owned Linode inventory, creates a small
`g6-standard-1` instance, waits for ready status, runs `echo ok`, verifies
`list --json`, stops the lease, runs dry-run cleanup, and verifies the
Crabbox-owned inventory is empty afterward.

Final classifications include:

```text
classification=live_linode_smoke_passed
classification=environment_blocked
classification=quota_blocked
classification=validation_failed
```

If cleanup fails, use the reported slug and Crabbox tags to inspect the instance
in `crabbox list --provider linode --json` or the Linode console.

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Tailscale**: yes through the standard Linux cloud-init path when a direct
  Tailscale auth key is configured.
- **Desktop / browser / code**: not advertised in Phase 1.
- **Cleanup**: yes, tag-owned only.
- **Coordinator**: never; direct CLI only.

## Gotchas

- `linode` is direct-only. Coordinator secrets and cost accounting do not
  cover these instances.
- `--type` must be a valid Linode type slug such as `g6-standard-1`.
- Crabbox decodes the highest-priority state when stale tags coexist, but
  cleanup relies on `expires_at` and ownership tags, not tag order.
- Phase 1 can attach an existing firewall by id, but it does not create or
  mutate firewall rules. Restrict SSH exposure through account/default firewall
  policy where needed, and prefer short TTLs for live validation.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Provider feature overview](../features/providers.md)
- [Operations](../operations.md)
