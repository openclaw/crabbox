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

AWS fixed leases bind their scope to the caller account recorded at creation.
Heartbeat validates that account and the exact live instance before writing
tags, while holding the unchanged local claim. Renewals preserve native
ownership tags, including the full fixed-create fingerprint.
Waiting for the mutation lock honors request cancellation.

## Idle timeout

`--idle-timeout <duration>` optionally replaces the lease's idle window while
refreshing it. The value must be positive. Omitting the flag preserves the
current direct-provider timeout when it is available in lease metadata and
omits the coordinator heartbeat override. Direct static and local-runtime
providers persist the refreshed timestamps, expiry, and any explicit timeout
replacement in the exact claim so a fresh CLI process observes the same state.
Heartbeat preserves existing provider metadata, including ownership fingerprints
and empty attestation fields; it changes only lifecycle values.

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
