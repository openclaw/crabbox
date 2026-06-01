# pool

`crabbox pool` contains machine-pool helpers. `pool list` keeps the older
machine inventory alias. Ready-pool subcommands manage hydrated broker leases
that can be borrowed by `crabbox run --pool`.

```sh
crabbox pool ready
crabbox pool ready example/app/main/aws/linux/c6i.2xlarge
crabbox pool register example/app/main/aws/linux/c6i.2xlarge --id cbx_...
crabbox pool borrow example/app/main/aws/linux/c6i.2xlarge
crabbox pool return example/app/main/aws/linux/c6i.2xlarge --id cbx_... --result ready --borrow-token <token>
crabbox pool ensure example/app/main/aws/linux/c6i.2xlarge --min-ready 1 --create -- --provider aws --type c6i.2xlarge
```

## Ready Pools

Ready pools are broker records for already hydrated leases. The CLI registers a
lease after `prewarm` or `actions hydrate` has prepared it. Borrow marks one
ready entry busy. Return either makes it ready again or drains and releases it.
Manual returns for busy leases must pass the token printed by `pool borrow`.

## Subcommands

```text
pool list                 list provider machine inventory
pool ready [key]          list ready-pool entries
pool register <key>       register a hydrated lease
pool borrow <key>         borrow one ready lease
pool return <key>         return, drain, or release a borrowed lease
pool ensure <key>         check or create minimum ready capacity
```

`pool ensure --create` forwards arguments after `--` to `prewarm`, then adds
`--pool <key>` so the created lease registers back into the requested pool.
Forwarded `--repo` and `--ref` overrides are rejected; set the desired
repository/ref in config before ensuring the pool.

## See Also

- [run](run.md)
- [prewarm](prewarm.md)
- [Broker ready pools](../spec/broker.md)
