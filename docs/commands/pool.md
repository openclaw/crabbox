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
pool identity <key>       generate an identity from an existing AWS or GCP image-backed lease
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

## Opt-in typed pools

Typed pools are a separate, provider-scoped and image-pinned protocol; they do
not replace legacy provider-neutral pools or let providers share a typed
cohort. Generate a supported identity from an existing active AWS or GCP Linux
lease:

```sh
crabbox pool identity example/app/main/linux \
  --id cbx_0123456789ab \
  --cache-compatibility node-22-pnpm-10 > pool-identity.json

crabbox pool register example/app/main/linux \
  --id cbx_0123456789ab --identity-file pool-identity.json
crabbox pool ready example/app/main/linux --identity-file pool-identity.json
crabbox pool borrow example/app/main/linux --identity-file pool-identity.json
crabbox pool return example/app/main/linux --id cbx_0123456789ab \
  --borrow-token <token> --identity-file pool-identity.json
crabbox pool ensure example/app/main/linux --min-ready 2 --max-ready 2 \
  --identity-file pool-identity.json --create -- \
  --provider aws --type c6i.4xlarge
```

`pool register --cache-compatibility node-22-pnpm-10` can also derive and
register the identity automatically. `--cache-compatibility` is trusted operator
metadata compared exactly; it is neither independently verified nor a security
attestation. The coordinator independently derives the immutable source from
the lease: AWS uses the AMI and region; GCP uses the numeric image or
disk-snapshot ID and its exact source project/collection namespace. GCP's
execution project is required evidence but is not part of the source identity,
and the launch zone does not split a cohort. The coordinator also validates
persisted canonical architecture and recomputes a length-framed UTF-8 hash of
the exact repo/ref/commit/fingerprint metadata. Regenerate the identity when any
bound repository input changes.

Supported leases are AWS Linux with an authoritative immutable AMI and matching
region, or GCP Linux with an authoritative boot-image or disk-snapshot source,
numeric resource ID, and execution project. Both require persisted canonical
architecture. GCP machine-image checkpoints, older leases missing evidence,
Azure, and other providers fail closed. Typed operations against an older
coordinator report that typed pools are unsupported; they never fall back to
legacy capacity.

## See Also

- [run](run.md)
- [prewarm](prewarm.md)
- [Broker ready pools](../spec/broker.md)
