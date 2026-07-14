# Vultr Provider

Read this when you are:

- choosing `provider: vultr`;
- validating a direct Vultr SSH lease;
- changing `internal/providers/vultr` or the guarded live smoke.

Vultr is a Linux-only **SSH lease** provider. Crabbox creates a Vultr instance,
creates or reuses a per-lease Vultr SSH key, injects Crabbox cloud-init
`user_data`, writes Crabbox ownership metadata as namespaced Vultr tags, waits
for SSH/bootstrap readiness, and then uses the normal Crabbox
SSH sync/run/stop/cleanup path.

Vultr is **direct-only** in this release. It does not run through the
coordinator, so the local CLI must have a Vultr API key and direct cleanup
remains the operator's responsibility.

## When To Use It

Use Vultr for simple Linux lease work when a Vultr instance is the desired
execution surface and direct local credentials are acceptable. Prefer AWS,
Azure, GCP, or Hetzner when you need a brokered team path, coordinator-side
credentials, or cloud-specific capacity and cost accounting.

## Commands

```sh
crabbox warmup --provider vultr --class standard
crabbox run --provider vultr --type vc2-1c-1gb -- pnpm test
crabbox ssh --provider vultr --id my-app
crabbox stop --provider vultr my-app
crabbox cleanup --provider vultr --dry-run
```

`--id` accepts the canonical lease id (`cbx_...`), the friendly slug, or the
Vultr instance UUID. `--type` is the exact Vultr plan id; there is no separate
Vultr size flag.

## Configuration

```yaml
provider: vultr
target: linux
class: standard
vultr:
  region: ewr
  os: ""
  image: ""
  snapshot: ""
  firewallGroup: ""
  vpcIds: []
  sshCIDRs: []
  userScheme: root
```

Config keys under `vultr:`:

| Key | Maps to | Default | Notes |
| --- | --- | --- | --- |
| `region` | `cfg.Vultr.Region` | `ewr` | Vultr region id. |
| `os` | `cfg.Vultr.OS` | auto-resolved | Exact numeric Vultr OS id. |
| `image` | `cfg.Vultr.Image` | empty | Optional Vultr application/image id. |
| `snapshot` | `cfg.Vultr.Snapshot` | empty | Optional Vultr snapshot id. |
| `firewallGroup` | `cfg.Vultr.FirewallGroup` | empty | Optional existing Vultr firewall group id to attach at create time. |
| `vpcIds` | `cfg.Vultr.VPCIDs` | empty | Optional VPC ids to attach at create time. |
| `sshCIDRs` | `cfg.Vultr.SSHCIDRs` | empty | Reserved for firewall-aware follow-up work; Phase 1 does not create firewall rules. |
| `userScheme` | `cfg.Vultr.UserScheme` | `root` | Vultr instance user scheme: `root` or `limited`. |

Set exactly one boot source: `vultr.os`, `vultr.image`, or `vultr.snapshot`.
When no boot source is configured, Crabbox queries Vultr's OS catalog and
selects a portable Ubuntu 24.04 Linux OS id. If that lookup cannot find a
matching OS, set one explicit boot source. `vultr.os` is an exact numeric OS id,
not a portable name.

Vultr leases default to SSH user `root` on port `22`. When
`vultr.userScheme: limited` is configured and no generic SSH user is set,
Crabbox uses SSH user `limited`. Explicit generic `ssh.user` and `ssh.port`
values remain authoritative. The effective values appear in
`crabbox config show` without retaining defaults from another provider.

Environment overrides:

```text
VULTR_API_KEY                   Vultr API key for direct mode
CRABBOX_VULTR_REGION            Override the region id
CRABBOX_VULTR_OS                Override the exact numeric OS id
CRABBOX_VULTR_IMAGE             Override the image id
CRABBOX_VULTR_SNAPSHOT          Override the snapshot id
CRABBOX_VULTR_FIREWALL_GROUP    Optional existing firewall group id
CRABBOX_VULTR_VPC_IDS           Comma-separated VPC ids
CRABBOX_VULTR_SSH_CIDRS         Comma-separated SSH CIDRs, reserved for firewall follow-up
CRABBOX_VULTR_USER_SCHEME       Override the user scheme (`root` or `limited`)
```

Do not pass the Vultr API key as a command-line argument. Keep it in the
environment or in a local secret manager.

## Token Permissions

The Phase 1 provider uses account identity, organizations, instances, OS
catalog lookup, SSH keys, tags, and optional existing firewall/VPC attachment.
Use a Vultr API key with access to read account identity and create, inspect,
tag, update, and delete the relevant instance and SSH-key resources. Add access
for existing firewall groups and VPCs when using `vultr.firewallGroup` or
`vultr.vpcIds`.

Crabbox binds local cleanup claims to the creating Vultr account or organization
identity and refuses claim-only deletion after an account switch. If a live
smoke fails with a permission error, keep the error output secret-safe and
adjust token permissions before retrying. Do not broaden permissions inside
scripts.

## Lifecycle

1. Generate a per-lease SSH key under the Crabbox testbox key directory.
2. Create or reuse the matching Vultr account SSH key.
3. Create a Vultr instance with `region`, boot source, plan, SSH key,
   cloud-init `user_data`, optional existing `firewallGroup`, optional
   `vpcIds`, and Crabbox tags.
4. Wait for a public IPv4 address and Crabbox SSH bootstrap readiness.
5. Add ready-state Crabbox tags and claim the lease locally.
6. Run normal Crabbox sync/run/ssh workflows over SSH.
7. Delete the instance and managed SSH key on `stop`; `cleanup` deletes only
   resources with complete Crabbox Vultr ownership tags and an exact account-
   and instance-bound local claim.

If instance or SSH-key creation returns an indeterminate transport or server
failure, Crabbox retains the SSH credentials and records a pending local
recovery claim. `crabbox stop --provider vultr <lease-or-slug>` reconciles that
claim and deletes a late-created instance or key when Vultr exposes it. Empty
inventory is not treated as proof that creation failed while the outcome
remains indeterminate: Crabbox retains the claim and credentials and asks the
operator to retry rather than risk orphaning a billed instance without its SSH
key.

## Ownership And Cleanup

Vultr tags are flat strings, not key/value labels. Crabbox encodes owned leases
as tags such as:

```text
crabbox
crabbox:provider:vultr
crabbox:lease:cbx_abcdef123456
crabbox:slug:my-app
crabbox:target:linux
crabbox:expires_at:<unix-seconds>
```

Read-only inventory requires a complete ownership predicate: Crabbox marker,
provider marker, canonical lease id, slug, and Linux target. Reuse and deletion
also require an exact local claim for the same Vultr account, lease, and
instance UUID. Instances with partial, foreign, malformed, claimless, or
mismatched ownership are skipped or refused; a claimless instance must first
be adopted through explicit supported `--reclaim` reuse.

Direct mode has no coordinator alarm. Use:

```sh
crabbox list --provider vultr --json
crabbox cleanup --provider vultr --dry-run
crabbox cleanup --provider vultr
```

## Firewall And Network Scope

Phase 1 can attach an existing Vultr firewall group by id, but it does not
create or manage firewall rules. If `vultr.sshCIDRs` or
`CRABBOX_VULTR_SSH_CIDRS` is set without `vultr.firewallGroup`, provisioning
fails closed because managed firewall creation is not implemented yet.

Operators own firewall and VPC policy, including SSH source restrictions. Use
account-default network policy, an existing firewall group, short TTLs, and
cleanup dry-runs for validation.

## Guarded Live Smoke

The repeatable live check is opt-in:

```sh
CRABBOX_LIVE=1 CRABBOX_LIVE_PROVIDERS=vultr scripts/live-vultr-smoke.sh
```

The script builds `bin/crabbox`, uses `VULTR_API_KEY` from the environment,
requires an empty Crabbox-owned Vultr inventory, creates one small
`vc2-1c-1gb` instance, waits for ready status, runs `echo ok`, verifies
`list --json`, stops the lease, runs dry-run cleanup, and verifies the
Crabbox-owned inventory is empty afterward.

Final classifications include:

```text
classification=live_vultr_smoke_passed
classification=environment_blocked
classification=quota_blocked
classification=validation_failed
```

Missing opt-in flags, missing credentials, API access that is disabled until
the account is ready, and authentication setup failures are
`environment_blocked`. Quota, capacity, rate-limit, and funding failures are
`quota_blocked`. JSON shape or command-sequence contract failures are
`validation_failed`.

If cleanup fails, use the reported slug and Crabbox tags to inspect the
instance in `crabbox list --provider vultr --json` or the Vultr console.

## Capabilities

- **SSH** and **Crabbox sync**: yes.
- **Tailscale**: not advertised in Phase 1.
- **Desktop / browser / code**: not advertised in Phase 1.
- **Cleanup**: yes, tag-owned only.
- **Coordinator**: never; direct CLI only.

## Gotchas

- `vultr` is direct-only. Coordinator secrets and cost accounting do not cover
  these instances.
- `--type` must be a valid Vultr plan id such as `vc2-1c-1gb`.
- `vultr.os` must be a numeric OS id. Use `vultr.image` or `vultr.snapshot`
  instead when booting a non-OS source.
- Phase 1 can attach an existing firewall group by id, but it does not create
  or mutate firewall rules.
- Vultr may return `default_password` and full `user_data` fields in API
  responses; Crabbox redacts those from provider errors and public logs should
  not retain raw API payloads.

## Related Docs

- [Provider reference](README.md)
- [Provider backends](../provider-backends.md)
- [Operations](../operations.md)
