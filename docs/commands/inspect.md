# inspect

`crabbox inspect` prints the full record for a single lease: state, provider,
server identity, the resolved SSH command, idle/expiry timing, Tailscale
metadata, and the provider labels attached to the box. Reach for it when
something looks wrong and you want every detail in one place.

```sh
crabbox inspect --id blue-lobster
crabbox inspect --id blue-lobster --network tailscale
crabbox inspect --id blue-lobster --json
crabbox inspect --provider namespace-devbox --id blue-lobster
crabbox inspect --provider ssh --target windows --windows-mode wsl2 --static-host win-dev.local
```

You can also pass the lease id or slug as a positional argument instead of
`--id`:

```sh
crabbox inspect blue-lobster
```

When `--provider` is omitted, an exact local claim or an unambiguous claimed
slug selects its recorded provider before provider initialization. An explicit
`--provider` remains authoritative; ambiguous claimed slugs require a canonical
lease ID or `--provider`, while missing claims keep the configured-provider
fallback.

## Output

Human output prints one `key=value` line per field, followed by any Tailscale
metadata (when the lease has Tailscale enabled) and one `label.<name>=<value>`
line per provider label.

```text
id=cbx_abcdef123456
slug=blue-lobster
provider=aws
target=linux
windows_mode=-
state=active
server=i-0abcdef0123456789
host=203.0.113.10
network=public
ssh=~/.config/crabbox/testboxes/cbx_abcdef123456/id_ed25519 -p 2222 crabbox@203.0.113.10
ssh_fallback_ports=22
idle_for=12m4s
idle_timeout=30m0s
last_touched=2026-05-07T07:55:12Z
expires=2026-05-07T08:25:12Z
tailscale.state=ok
tailscale.hostname=blue-lobster
tailscale.fqdn=blue-lobster.tail-scale.ts.net
tailscale.ipv4=100.64.0.5
tailscale.tags=tag:crabbox
label.target=linux
label.state=active
```

The `ssh=` line shows the connection for the selected `--network` mode (the
key path, port, user, and host). Empty fields render as `-`.

`--json` prints the structured status record (the same shape returned by
[`status`](status.md)), including non-secret Tailscale metadata and the full
label map. Secrets such as broker tokens, provider keys, and VNC passwords are
never included in either output mode.

Brokered Hetzner leases may include versioned `providerCleanup` in JSON output.
It binds the provider, lease ID and numeric server ID, retains dispatch and
action status when known, and records a confirmation method and timestamp after
server cleanup is observed. A recorded action must succeed before exact server
absence can confirm its deletion. `already-absent` is a distinct, weaker basis
for a server missing before any recorded dispatch. A confirmation may coexist
with failed SSH-key cleanup; use `cleanupStatus` for overall release finality.
Definitive key-only creation failures need no server receipt; their retained
no-resource evidence authorizes only exact owned-key cleanup. Missing evidence
on historical server records means no recorded confirmation. Stored
host/server fields and `hasHost` describe the retained record, not current
provider existence. Inspect reads this receipt from the broker without provider
credentials or CLI-side provider polling.

An exact rejected Hetzner DELETE can leave only the provider/lease/server binding
in `providerCleanup`, with the rejection in the ordinary cleanup error fields.
That is retryable debt, not server confirmation; the broker rechecks ownership
after backoff. Keep local credentials and evidence while cleanup is unresolved.
See [recovery guidance](../features/lifecycle-cleanup.md#brokered-hetzner-cleanup-confirmation)
before acting on historical records or missing acknowledgement authority.

[Local Container](../providers/local-container.md#memory-failure-evidence)
adds fresh, read-only `diagnostic.memory.*` labels to the returned view. They
describe actual container settings and, when available, total RAM from its
verified runtime route, not free memory or a universal effective limit. These
facts are not persisted into claims or container labels, and do not reconstruct
a previous command's OOM evidence. For a deleted one-shot container, use the
run's timing/failure-bundle snapshot instead.

For brokered leases, labels describe the current coordinator lease record,
including allocation identity, target, capacity, timing, and capability facts.
They are not a fresh read of provider tags. Only the documented
`providerMetadata` fields below are refreshed from the provider during
coordinator inspection.

Brokered JSON records also expose the coordinator's provider-cleanup state:

- `cleanupStatus`: for released leases, `pending`, `failed`, `complete`, or
  `retained`. This is computed from the current lifecycle record, not persisted
  as a separate state or used as provider deletion authority.
- `cleanupStartedAt`: cleanup has started but is not yet terminal.
- `cleanupError`: cleanup remains unconfirmed; this can include legacy diagnostics
  for pending creation as well as observed failures.
- `cleanupRetryAt`: the coordinator scheduled another cleanup attempt.
- `releaseDeletesServer`: whether release is intended to delete the provider
  resource.

Cleanup is terminal under Crabbox's coordinator predicate only when `state` is
`released`, `cleanupStartedAt`, `cleanupError`, and `cleanupRetryAt` are all
absent, and `releaseDeletesServer` is either omitted or `true`. An explicit
`releaseDeletesServer: false` means the provider resource was intentionally
retained and must not be treated as deletion-confirmed. Omitted and `false` are
therefore distinct states.

`pending` includes an allocation response or cleanup attempt still being
observed. An explicit stop keeps observing that state within its existing
five-minute bound instead of treating it as a provider failure. A real cleanup
failure or uncertain abandoned allocation remains `failed`; local claims and
SSH files are retained. Older coordinators omit `cleanupStatus`, and clients
continue using the existing conservative metadata checks. The original
diagnostics remain available so older clients also fail closed.

These fields report the coordinator's lifecycle observation. They are not an
independent provider inventory check. `complete` does not override remaining
cleanup metadata or an explicit retained-resource flag.

### Azure cleanup identity diagnostics

For a blocked brokered Azure lease, the authenticated coordinator API offers an
explicit read-only `GET /v1/leases/{id}/cleanup`. Use the canonical lease ID and
an existing owner, manage-share, or admin credential. View-only shares and device
tokens cannot inspect cleanup identities; unsupported providers and registered
leases return HTTP 501. This is an API diagnostic, not an `inspect` CLI flag.

The response's `inspection` contains the retained cleanup claim's stable identity
and deletion progress, fresh canonical VM/NIC/public-IP/disk identities and
ownership-match classifications, and the original provider scope. Raw resource
bodies, tags, bootstrap data, credentials, and operation URLs are not returned.
`identityMatches` compares the stable resource set with recorded deletion progress;
it is not an ownership verdict or authorization to delete. `ownership: unclaimed`
means ownership labels are absent, not that the resource is safe to adopt.

`claimUnchanged: false` means cleanup changed the claim during the reads;
`identityMatches` is then `null`, as it is when no stable baseline was recorded.
Provider observations are not an atomic inventory. Inspection never creates,
updates, clears, or accepts a cleanup identity, and never issues a resource
mutation. Keep local claims and SSH artifacts until normal stop confirms cleanup.
The optional `claimFingerprint` binds an explicit recovery request to the exact
stored claim; do not use it when `claimUnchanged` is false. A retained
`recoveryAudit`, when present, distinguishes operator-acknowledged absence from
provider-confirmed deletion progress.

### Audited Azure cleanup recovery

If historical public-IP completion evidence was lost, an owner or admin may
explicitly accept its absence **in the original provider scope**. This does not
prove Azure deleted the public IP rather than moving it elsewhere, and must not
be used when that distinction still requires investigation. Manage-share and
device credentials cannot authorize this exception.

After reviewing a fresh cleanup inspection, send the authenticated API request:

```http
POST /v1/leases/{id}/cleanup
Content-Type: application/json

{"action":"acknowledge-missing-resource","expectedClaimFingerprint":"<claimFingerprint from inspection>"}
```

Azure accepts only an expired, blocked lease with no active cleanup attempt:
its complete version-2 ordinary cleanup baseline must retain all four original
identities and successful VM/NIC deletion progress. Fresh exact-scope GETs must
show VM, NIC, and public IP absent, with only the original owned, detached disk
remaining in the original region. Pending operations, incomplete claims,
replacements, conflicting ownership, changed attachments, and stale fingerprints
are rejected. Explicit retained-resource disposition is also rejected, and the
lease binding and eligibility are rechecked after provider reads, immediately
before the atomic recovery commit. No resource is created, tagged, or deleted
by this request.

The transaction preserves the original baseline and actual DELETE receipts,
persists a separate audit with basis `operator-confirmed-public-ip-absence`, and
promotes the claim to version 3 so older workers reject it. The acknowledgement
is never inserted into the actual deletion receipt list. The same fingerprint is
idempotent; another claim cannot overwrite the audit. Normal `crabbox stop` then
rechecks every survivor before deleting the original disk and retains local
artifacts until cleanup is confirmed. The audit survives removal of the completed
cleanup claim and remains available through the cleanup diagnostic.

`identityMatches` accounts for this explicitly acknowledged absence on a recovered
claim; consult `recoveryAudit` for its basis. It is still not deletion authority.
Orphan/provisioning continuation paths cannot replace the recovered baseline;
only ordinary release consumes this recovery.

### Additional provider metadata

For coordinator leases whose provider can inject an SSH host key before first
boot, JSON also includes `sshHostKey`. Its value is exactly the public host-key
algorithm and base64 payload, without a hostname or comment:

```json
{
  "sshHostKey": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA..."
}
```

The key is the authoritative public half generated for provisioning, not a key
learned later through `known_hosts` or `ssh-keyscan`. The field is omitted when
the provider cannot inject a host key before boot.

For Islo leases, JSON also includes the API-assigned sandbox ID as
`providerResourceId`:

```json
{
  "serverId": "my-app-7f3a91",
  "providerResourceId": "0195f3d2-5c1a-7c39-9c1e-6f0f2b7a41cd"
}
```

For Islo, `serverId` remains the sandbox name and `providerResourceId` identifies
its resource generation. Other providers retain their existing `serverId`
semantics and may omit `providerResourceId`; omission does not imply that their
resource names are immutable.

When an Islo name fallback does not report the resource ID the lease claims,
`labels.islo_resource_id_mismatch` is set to
`true`, `labels.islo_claimed_resource_id` reports the id the lease claims, and
`providerResourceId` is omitted entirely rather than attributed to a resource
the lease does not own. The lease is not ready, and `status --wait` fails without
running remote Tailscale checks. A fallback that reports the claimed ID sets
neither label. Malformed by-ID responses fail rather than falling back to a
different resource.

AWS leases also include authoritative provider metadata sourced from EC2
`DescribeInstances`. Brokered inspection requests a fresh coordinator-side
lookup; direct inspection uses the local AWS client:

```json
{
  "providerMetadata": {
    "instanceProfileAttached": false
  }
}
```

Consumers can use this boolean to fail closed when a workload must not receive
an IAM instance profile. The field is omitted when the backend cannot attest
the association state.

## Flags

```text
--id <lease-id-or-slug>      lease to inspect (required); also accepted as a positional argument
--provider <name>            override the configured provider (e.g. aws, hetzner, ssh, namespace-devbox)
--target linux|macos|windows target OS
--windows-mode normal|wsl2   Windows execution mode
--static-host <host>         static SSH host (provider=ssh)
--static-user <user>         static SSH user override
--static-port <port>         static SSH port override
--static-work-root <path>    static target work root
--network auto|tailscale|public  which address the resolved SSH line prints
--json                       print the structured JSON record
```

## inspect vs status vs list

- `inspect` is the long-form record for one lease, including provider labels
  and the resolved SSH command.
- [`status`](status.md) is the shorter "is this lease healthy right now"
  check, with optional `--wait` and bounded telemetry.
- [`list`](list.md) is the table view across many leases, scoped by owner/org
  or fleet-wide for admins.

Use `inspect` when something is unexpected and you want all the detail at once.
Use `status` when an automation needs a quick liveness check. Use `list` when
you are hunting for a specific lease across the pool.

Related docs:

- [status](status.md)
- [list](list.md)
- [ssh](ssh.md)
- [Identifiers](../features/identifiers.md)
- [Network and reachability](../features/network.md)
