# pool

`crabbox pool` contains machine-pool helpers. `pool list` keeps the older
machine inventory alias. Ready-pool subcommands manage hydrated broker leases
that can be borrowed by `crabbox run --pool`.

```sh
crabbox pool ready
crabbox pool ready example/app/main/linux
crabbox pool register example/app/main/linux --id cbx_... --compatibility-key linux-16-vcpu
crabbox pool borrow example/app/main/linux --compatibility-key linux-16-vcpu
crabbox pool heartbeat example/app/main/linux --id cbx_... --borrow-token <token>
crabbox pool return example/app/main/linux --id cbx_... --result ready --borrow-token <token>
crabbox pool ensure example/app/main/linux --min-ready 2 --max-ready 4 --compatibility-key linux-16-vcpu --create -- --provider aws --type c6i.4xlarge
```

## Ready Pools

Ready pools are broker records for already hydrated leases. The CLI registers a
lease after `prewarm` or `actions hydrate` has prepared it. Borrow marks one
ready entry busy. Return either makes it ready again or drains and releases it.
Manual returns for busy leases must pass the token printed by `pool borrow`.
`crabbox run --pool` negotiates heartbeat enforcement and sends heartbeats
automatically. Manual and older-client borrows remain deadline-free unless
they send `pool heartbeat`; the first successful heartbeat opts that borrow in.
An opted-in abandoned borrow is quarantined and cannot become ready again
without being drained.

## Subcommands

```text
pool list                 list provider machine inventory
pool ready [key]          list ready-pool entries
pool register <key>       register a hydrated lease
pool borrow <key>         borrow one ready lease
pool heartbeat <key>      refresh a borrowed lease deadline
pool return <key>         return, drain, or release a borrowed lease
pool ensure <key>         reconcile desired ready capacity
```

`pool ensure` persists `--min-ready` and `--max-ready` with the coordinator.
With `--create`, each keeper first obtains an atomic fill claim, then forwards
arguments after `--` to `prewarm`. Concurrent keepers count active claims
toward the maximum, so they cannot double-provision the same missing slot.
An issued claim remains valid until registration, explicit release, or expiry;
later policy or capacity changes block new claims but never revoke in-flight
provisioning. `pool ensure` succeeds only when the actual `ready` count reaches
`--min-ready`. Another keeper's in-flight claim is reported but does not make
the command succeed.
Forwarded `--repo` and `--ref` overrides are rejected; set the desired
repository/ref in config before ensuring the pool.

During a rolling upgrade, a new CLI falls back once to the legacy client-side
count-then-create algorithm when the coordinator returns 404 or 405 for the
reconcile route. The notice is printed once to stderr. That fallback preserves
the older `--min-ready` behavior but cannot enforce atomic claims, `--max-ready`,
or compatibility keys until the coordinator is upgraded. A new CLI also stops
sending borrow heartbeats after the first unsupported-route response from an
older coordinator.

`--compatibility-key` names a provider-neutral capability and size class. For
example, compatible AWS and Azure 16-vCPU shapes can share `linux-16-vcpu`
under one logical pool key while incompatible entries and fill claims remain
separate.

## See Also

- [run](run.md)
- [prewarm](prewarm.md)
- [Broker ready pools](../spec/broker.md)
