# heartbeat

`crabbox heartbeat` refreshes the idle deadline for one owned lease and prints
the resulting lease state. It is intended for external drivers that keep a
lease busy over SSH without running commands through `crabbox run`.

```sh
crabbox heartbeat --id swift-crab
crabbox heartbeat --id swift-crab --idle-timeout 90m
crabbox heartbeat --id cbx_abcdef123456 --provider aws --json
```

## Identifying the lease

`--id` accepts a canonical `cbx_...` lease ID or an active slug. When
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
claim that matches the provider scope and live resource identity, and it
rejects terminal leases before touching them. Providers without lease-touch
support fail with `provider=<name> does not support lease heartbeat`.

## Idle timeout

`--idle-timeout <duration>` optionally replaces the lease's idle window while
refreshing it. The value must be positive. Omitting the flag preserves the
current direct-provider timeout when it is available in lease metadata and
omits the coordinator heartbeat override.

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
