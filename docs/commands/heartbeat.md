# heartbeat

`crabbox heartbeat` refreshes the idle deadline for one owned lease and prints
the resulting lease state. It is intended for external drivers that keep a
lease busy over SSH without running commands through `crabbox run`.

```sh
crabbox heartbeat swift-crab
crabbox heartbeat --id swift-crab
crabbox heartbeat --id swift-crab --idle-timeout 90m
crabbox heartbeat --id cbx_abcdef123456 --provider aws --json
```

## Identifying the lease

Supply exactly one identifier, either as the positional argument or with
`--id`; combining the two is a usage error. The identifier accepts a canonical
`cbx_...` lease ID or an active slug. When
`--provider` is omitted, Crabbox uses the same local-claim routing as `status`
and `inspect`; an explicit provider still wins.

## Coordinator and direct-provider behavior

For managed coordinator leases, the command sends exactly one request through
the existing lease heartbeat endpoint. The configured owner credentials and
provider binding are unchanged, so unknown, unowned, expired, released, or
otherwise terminal leases retain the coordinator's normal failure response.

`broker.mode: registered` has both coordinator and direct-provider expiry
state. The command therefore requires the exact direct claim, sends one
coordinator heartbeat, and calls the provider's existing `Touch` capability
with the same idle timeout before reporting success.

Without a coordinator, Crabbox resolves the direct SSH lease and calls the
provider's existing `Touch` capability. This fallback requires an exact local
claim for the canonical provider. Static scopes must match the configured
provider scope and live resource identity exactly. Providers with a dynamic
runtime scope may hydrate their recorded context, but must validate the live
endpoint/daemon identity and exact resource before authorizing. The provider
then compare-and-swaps the carried claim snapshot, so a disappeared or replaced
claim is never recreated or overwritten. Terminal leases are rejected before
touch. Providers without lease-touch support fail with
`provider=<name> does not support lease heartbeat`.

## Delegated providers

A delegated-run provider has no Crabbox-managed SSH lease to touch. Providers
that advertise the `lease-heartbeat` feature keep the lease alive through their
own API instead: the command hands the identifier to the provider, which makes
a cheap authenticated call intended to register activity on the lease. The
provider owns every check on this path — Crabbox does no lease resolution,
claim check, or state validation of its own before delegating.

Because the provider owns the lease's idle policy here, the command reports the
idle window the provider reports back, and reports nothing at all when the
provider does not report one. It never substitutes the local `idle_timeout`
configuration, which does not describe such a lease. For the same reason
`--idle-timeout` is refused on this path with
`provider=<name> does not support replacing the lease idle timeout while heartbeating`,
rather than accepted and silently ignored.

This path writes no lease deadline of any kind, so nothing it does can move a
lease's expiry in either direction, and it persists nothing locally: the
reported `lastTouchedAt` is when the provider observed the lease, so `crabbox
claims` keeps showing the claim's previous `lastUsed`. What the provider's call
defers on its own side is the provider's business; see the provider's own page.

A coordinator-registered broker keeps the coordinator path above, because the
coordinator remains the source of truth for expiry wherever a coordinator lease
can exist. Providers declared `CoordinatorNever` can never hold one, so they use
the delegated path regardless of broker mode. Providers that do not advertise
the feature keep failing with
`provider=<name> does not support lease heartbeat`.

## Idle timeout

`--idle-timeout <duration>` optionally replaces the lease's idle window while
refreshing it, on the coordinator and direct-provider paths. It is not
supported on the delegated-provider path above. The value must be positive.
Omitting the flag preserves the
current direct-provider timeout when it is available in lease metadata and
omits the coordinator heartbeat override. Direct static and local-runtime
providers persist the refreshed timestamps, expiry, and any explicit timeout
replacement in the exact claim so a fresh CLI process observes the same state.

## Output

Human output is one line with the lease ID, slug, provider, state, idle timeout,
last-touch timestamp, and expiry. `--json` emits the same fields as a JSON
object.

## Flags

```text
--id <lease-id-or-slug>
--provider <provider>
--idle-timeout <duration>
--json
```

## See also

- [`status`](status.md) — read the current lease state without touching it.
- [`inspect`](inspect.md) — inspect full lease and provider details.
- [`run`](run.md) — run a command with automatic background heartbeats.
