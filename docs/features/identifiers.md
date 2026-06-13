# Identifiers

Read this when you are:

- changing how Crabbox names leases, slugs, runs, or claims;
- debugging "why does `crabbox run --id <x>` not find this lease?";
- adding a new lookup form (a slug, a provider ID, anything that should resolve
  to a lease).

Crabbox names every long-lived thing twice: once with a stable canonical ID
that machines compare, and once with a friendly slug that people type. This
page lists each identifier, where it comes from, and how `--id` lookup resolves
across them.

## Lease ID

Canonical lease IDs look like:

```text
cbx_abcdef123456
```

The format is fixed: the literal `cbx_` prefix followed by 12 lowercase hex
characters. `newLeaseID` mints one from 6 random bytes, and the regex
`^cbx_[a-f0-9]{12}$` (`isCanonicalLeaseID`) decides whether a value is a
canonical ID; anything that fails the pattern is treated as a slug.

The CLI mints a provisional lease ID before calling the broker. The broker may
return a different final ID (when the Worker dedupes a retried request, for
example). When that happens, the CLI moves the local SSH key directory from the
provisional ID to the final ID with `MoveStoredTestboxKey` (an atomic
`os.Rename` of the per-lease directory) and re-keys the claim and other
references accordingly.

Provider resources reference the lease ID through a Crabbox label (the label key
is literally `lease`):

```text
lease=cbx_abcdef123456
```

Crabbox-created machines also carry a `crabbox=true` marker label. `crabbox list`
and `crabbox cleanup` discover machines by that marker and then read the `lease`
label to map a provider machine back to a Crabbox lease.

## Slug

Slugs are friendly, human-typeable lease names. They look like:

```text
blue-lobster
amber-crab
silver-shrimp
```

By default a slug is generated from a stable hash of the lease ID
(`newLeaseSlug`), so the same lease always gets the same generated slug. The
vocabulary is deliberately small (14 adjectives x 8 nouns = 112 base
combinations) to match Crabbox's small-fleet model. Lease-creating commands can
request a custom slug with `--slug <name>`:

```sh
crabbox warmup --slug update-flow-smoke
crabbox run --slug update-flow-smoke -- pnpm test:changed
crabbox checkpoint fork chk_abc123def456 --slug update-flow-smoke
```

`--slug` is creation-time metadata, not a rename. It is honored only when
Crabbox is creating a new lease; existing leases keep their assigned slug.

Slugs are normalized everywhere they are accepted. `normalizeLeaseSlug`
lowercases, keeps only `[a-z0-9]`, collapses every other run of characters into
a single `-`, and trims leading and trailing dashes — so `Blue_Lobster` and
`BLUE-LOBSTER` both resolve to `blue-lobster`. A requested slug must contain at
least one letter or digit and is capped at 41 characters after normalization, so
collision suffixes and provider names stay portable.

When a requested or generated slug collides with an existing active lease (a
matching server label or a matching local claim), `slugWithCollisionSuffix`
appends a 4-hex suffix derived from a per-attempt seed:

```text
blue-lobster-1f3a
```

Allocation tries up to 20 suffixed candidates before settling. Collisions are
rare in normal use — a single user's active leases seldom approach the 112 base
slugs.

## Provider Name

Each managed lease also gets a per-provider resource name that includes the slug
and a hash of the lease ID, so the provider console shows something legible:

```text
crabbox-blue-lobster-7f8a2c1d
```

This is what appears as the EC2 `Name` tag, the Hetzner server name, the Daytona
sandbox name, and so on. It comes from `leaseProviderName(leaseID, slug)`; when
the slug is empty the function falls back to `crabbox-cbx-abcdef123456` (the
lease ID with `_` rewritten to `-`).

## Run ID

Each `crabbox run` against a coordinator also gets a durable run handle:

```text
run_abcdef123456
```

Like lease IDs, run IDs are the `run_` prefix plus 12 lowercase hex characters
(`newRunID` in the coordinator, from 6 random bytes). The coordinator mints the run
record before the lease is acquired, so events can be appended for leasing
failures, sync failures, and command output even when the run never reaches
command-start. A run ID is stable across a single invocation; retrying the same
command produces a new run.

`crabbox history`, `crabbox events`, `crabbox attach`, `crabbox logs`, and
`crabbox results` all accept run IDs. Slugs do not resolve to runs — only to
leases.

## Local Claims

Reusable leases get a JSON claim file under the Crabbox state directory:

```text
$XDG_STATE_HOME/crabbox/claims/cbx_abcdef123456.json
```

When `XDG_STATE_HOME` is unset, the state directory sits next to the user config
directory: `~/Library/Application Support/crabbox/state/claims` on macOS or
`~/.config/crabbox/state/claims` on Linux.

A claim payload looks like:

```json
{
  "leaseID": "cbx_abcdef123456",
  "slug": "blue-lobster",
  "provider": "aws",
  "repoRoot": "/Users/alice/Projects/my-app",
  "claimedAt": "2026-05-07T07:42:18Z",
  "lastUsedAt": "2026-05-07T07:55:12Z",
  "idleTimeoutSeconds": 1800
}
```

Claims do three things:

- bind a lease to one repo so wrappers and agents do not silently reuse a lease
  against a different checkout;
- give `crabbox run --id blue-lobster` a slug-to-canonical-ID translation
  without round-tripping the broker;
- power "is this lease still mine?" checks before destructive operations such as
  `stop`, `cleanup`, and `actions register`.

A conflicting claim (same lease, different `repoRoot`) refuses commands by
default with a `use --reclaim` error; `--reclaim` overrides the check and
rewrites the claim atomically.

Static SSH leases (`provider: ssh`) record extra endpoint fields in the claim —
`staticHost`, `staticUser`, `staticPort`, `staticWorkRoot`, `targetOS`, and
`windowsMode` — so the resolver knows the lease bypasses the coordinator and can
reconnect without re-provisioning. Claims may also cache a resolved endpoint
(`sshHost`, `sshPort`, `tailscaleIPv4`, `tailscaleFQDN`, `bridgeURL`) and the
`pond` label once the lease is up.

## SSH Key Storage

Per-lease SSH key directories are keyed by lease ID, under the user config
directory (not the state directory):

```text
~/.config/crabbox/testboxes/cbx_abcdef123456/id_ed25519
~/.config/crabbox/testboxes/cbx_abcdef123456/id_ed25519.pub
```

Keys are `ed25519` by default; AWS and Azure Windows leases use a 4096-bit RSA
key instead (`ensureTestboxKeyForConfig`). The provisional-to-final lease ID
move renames the whole directory so the private key, public key, and any
`known_hosts` entries migrate together. The provider key name registered with
the cloud account is `crabbox-cbx-abcdef123456` (`providerKeyForLease`).

## Resolving An Identifier

`crabbox <command> --id <value>` accepts:

- a canonical `cbx_...` lease ID;
- a normalized slug — `blue-lobster`, `Blue Lobster`, and `BLUE_LOBSTER` all
  resolve to the same lease;
- in coordinator mode, the slug as the broker knows it (case-insensitive).

Resolution order:

1. Read the local claim store. `resolveLeaseClaim` first tries the literal
   identifier as a claim filename, then scans `claims/*.json` for any claim
   whose `leaseID` or normalized `slug` matches.
2. If a matching claim exists, use its `leaseID` as the canonical handle.
3. If no claim is found and a coordinator is configured, ask the coordinator to
   resolve the identifier (slug or canonical ID).
4. For static SSH and direct-provider modes, fall back to the provider's
   `Resolve` implementation on `SSHLeaseBackend`.

The first source that returns a hit wins. This is why `--id blue-lobster` works
from any directory once a warmup ran in some other repo — the local claim
translates the slug to a lease ID before the broker is ever involved.

## Identifier Lifetime

```text
provisional lease ID  newLeaseID() before the broker call
final lease ID        broker may return a different ID; key dir + claim re-keyed to it
slug                  computed on first lease creation, stable for that lease
provider name         derived from final lease ID + slug
run ID                minted per crabbox run when a coordinator is configured
```

Slugs are not reserved after a lease ends. The next lease that happens to hash
to the same base slug will reuse it; the small vocabulary makes that possible
but uncommon in practice.

Related docs:

- [Coordinator](coordinator.md)
- [SSH keys](ssh-keys.md)
- [Lifecycle cleanup](lifecycle-cleanup.md)
- [Source map](../source-map.md)
