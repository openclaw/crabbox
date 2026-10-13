# extend

Extend an owned lease within its lifetime policy. Total lifetime always counts
from creation; extending does not reset the idle clock or revive an expired lease.

```sh
crabbox extend blue-lobster
crabbox extend blue-lobster --by 30m --reason 'finish tests'
crabbox extend cbx_abcdef123456 --ttl 2h --json
```

Supply exactly one positional ID or slug. `--by` and `--ttl` are mutually
exclusive; omitting both means `--by 30m`. Durations must be positive whole
seconds. `--reason` accepts at most 200 characters. Use `--provider` when normal
claim routing does not select the intended provider.

Brokered requests use `POST /v1/leases/{id}/extend`. The owner or an authenticated
admin may extend; shared manage access alone does not grant this authority.
Policy can deny extensions (403 `lease_extension_denied`). Inactive or expired
leases return 409 `lease_not_active`; a request that cannot increase total
lifetime after clamping returns 409 `lease_extension_not_advancing`. Invalid
bodies return 400. The API retains only the last ten explicit extension events.

Extensions also require fleet, owner, and organization budget headroom, including
admin requests. The coordinator reserves the extra cost at the lease's recorded
hourly estimate in the same transaction as the new TTL. Insufficient headroom
returns 403 `lease_extension_budget_exceeded`, reports the remaining headroom,
and leaves the lease unchanged. Partial extensions are never applied.

Providers with `leasePolicy.runtimeExtension: false`, including brokered Daytona,
return 409 `lease_extension_unsupported_provider`. Request a longer `--ttl` when
creating those leases. Their command heartbeats silently skip auto-extension.

Direct providers with a supported mutable label/tag path update the exact
claimed resource's `ttl_secs` and `expires_at` metadata, respecting `maxTTL`
(default 4h, configurable up to 24h with `CRABBOX_MAX_TTL`). Providers without
that capability report `extend is not supported for provider X`. The backend
`LeaseExtensionBackend.ExtendLease` boundary owns provider metadata writes.
For file-backed Linux guest guards, extension saves provider labels and the
local claim first, then refreshes `/etc/crabbox/lease-expiry` through the existing
SSH route. AWS, Azure, and GCP guards read metadata directly and need no SSH write.

If the guest refresh fails, the command exits nonzero and reports both deadlines:
the provider deadline has advanced, but the on-box guard still holds the old
deadline and will power the machine off then. Provider labels are retained.
Rerun the same command with the same arguments to retry the guest refresh;
that retry does not add lifetime again. The pending refresh is stored in the
exact local claim, so use the same local state for the retry.

Morph must accept its native deadline before the new total lifetime is published.
If metadata publication fails, rerun the same extension to retry without adding
the duration twice. Cleanup continues to use the old metadata deadline until
publication succeeds.

Human output shows the new expiry, remaining lifetime, and maximum lifetime;
`--json` returns `id`, `expiresAt`, `remainingLifetime`, `maxLifetime`, and
`runtimeExtension`. File-backed extensions also return `guestExpiresAt`; a failed
refresh adds `guestExpiryRefreshPending: true` and an `error` message, while
`expiresAt` remains the committed provider deadline. JSON output also exits
nonzero on this partial failure. See [lifecycle policy](../features/lifecycle-cleanup.md#ordinary-lifetime-policy).
